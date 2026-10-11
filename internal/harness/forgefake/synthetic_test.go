package forgefake

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func githubLogs(t *testing.T, srv *httptest.Server, job string, header http.Header) httpAnswer {
	t.Helper()
	all := http.Header{"Authorization": {"Bearer " + testAPIToken}}
	for k, v := range header {
		all[k] = v
	}
	return httpRequest(t, srv, "GET", "github.com", "/github/rest/repos/a/b/actions/jobs/"+job+"/logs", "", all)
}

func gitlabTrace(t *testing.T, srv *httptest.Server, job string, header http.Header) httpAnswer {
	t.Helper()
	all := http.Header{"Private-Token": {testAPIToken}}
	for k, v := range header {
		all[k] = v
	}
	return httpRequest(t, srv, "GET", "gitlab.com", "/gitlab/api/v4/projects/g%2Ft/jobs/"+job+"/trace", "", all)
}

// TestSyntheticGitHubLogIsGeneratedAndRanged: a synthetic GitHub log is
// generated per request with each step's time and group line, and served
// as the log blob serves: 206 for a start range, the whole body for a
// suffix range, 416 past the end, ETag and 304 as for a seeded log.
func TestSyntheticGitHubLogIsGeneratedAndRanged(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	_, err := seedJSON(e, `{"repos":[{"forge":"github","project":"a/b","pulls":[{"number":1,"title":"x","ci":{"jobs":[
{"id":510,"name":"big","status":"success","synthetic":{"lineBytes":100,"sections":[{"name":"Set up job","lines":3},{"name":"Run tests","lines":200000}]},
 "steps":[{"name":"Set up job","status":"success","startedAt":"2026-01-01T00:00:00Z","completedAt":"2026-01-01T00:00:02Z"},
          {"name":"Run tests","status":"success","startedAt":"2026-01-01T00:00:02Z","completedAt":"2026-01-01T00:09:00Z"}]}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	whole := githubLogs(t, srv, "510", nil)
	if whole.status != 200 || lastInvocation(t, e).Unhandled {
		t.Fatalf("synthetic log = %d %v", whole.status, lastInvocation(t, e).Detail)
	}
	if want := 2 + 3*100 + len("2026-01-01T00:00:02.0000000Z ##[group]Run tests\n") + 200000*100 + len("2026-01-01T00:00:00.0000000Z ##[group]Set up job\n") - 2; len(whole.body) != want {
		t.Fatalf("synthetic log is %d bytes, want %d", len(whole.body), want)
	}
	if got := whole.header.Get("Content-Length"); got != strconv.Itoa(len(whole.body)) {
		t.Fatalf("Content-Length %q for %d bytes", got, len(whole.body))
	}
	lines := strings.Split(strings.TrimSuffix(whole.body, "\n"), "\n")
	if lines[0] != "2026-01-01T00:00:00.0000000Z ##[group]Set up job" || lines[4] != "2026-01-01T00:00:02.0000000Z ##[group]Run tests" {
		t.Fatalf("group lines = %q, %q", lines[0], lines[4])
	}
	last := lines[len(lines)-1]
	if len(last) != 99 || !strings.HasPrefix(last, "2026-01-01T00:00:02.0000000Z run_tests_1 line 200000 ") {
		t.Fatalf("last line = %q", last)
	}
	etag := whole.header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if got := githubLogs(t, srv, "510", http.Header{"If-None-Match": {etag}}); got.status != 304 || got.body != "" {
		t.Fatalf("revalidated = %d %d bytes, want 304", got.status, len(got.body))
	}

	from := len(whole.body) - 12345
	part := githubLogs(t, srv, "510", http.Header{"Range": {fmt.Sprintf("bytes=%d-", from)}})
	if part.status != 206 || part.body != whole.body[from:] || lastInvocation(t, e).Unhandled {
		t.Fatalf("start range = %d with %d bytes, want 206 with the last 12345", part.status, len(part.body))
	}
	if got, want := part.header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", from, len(whole.body)-1, len(whole.body)); got != want {
		t.Fatalf("Content-Range = %q, want %q", got, want)
	}
	if part.header.Get("ETag") != etag {
		t.Fatalf("206 ETag = %q, want %q", part.header.Get("ETag"), etag)
	}
	if got := githubLogs(t, srv, "510", http.Header{"Range": {"bytes=-100"}}); got.status != 200 || len(got.body) != len(whole.body) {
		t.Fatalf("suffix range = %d with %d bytes, want the whole body", got.status, len(got.body))
	}
	if got := githubLogs(t, srv, "510", http.Header{"Range": {fmt.Sprintf("bytes=%d-", len(whole.body))}}); got.status != 416 {
		t.Fatalf("range past the end = %d, want 416", got.status)
	}
}

// TestSyntheticGitLabTraceGrowsWhileRunning: a running synthetic trace
// grows by growLines on every request and ignores Range (200, whole
// body); once the job completed it is whole, honors a start range and
// refuses a suffix one.
func TestSyntheticGitLabTraceGrowsWhileRunning(t *testing.T) {
	t.Parallel()
	e := New(Options{APIToken: testAPIToken})
	fixture, err := seedJSON(e, `{"repos":[{"forge":"gitlab","project":"g/t","pulls":[{"number":1,"title":"x","ci":{"jobs":[
{"id":610,"name":"big","status":"running","synthetic":{"runningLines":4,"growLines":3,"sections":[{"name":"step_script","lines":20}]}}]}}]}]}`)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	srv := testServer(t, e)
	first := gitlabTrace(t, srv, "610", nil)
	if first.status != 200 || strings.Count(first.body, "\n") != 4 {
		t.Fatalf("first running trace = %d %q, want 4 lines", first.status, first.body)
	}
	if !strings.Contains(first.body, "section_start:1767225600:step_script_0\r\x1b[0Kstep_script\n") {
		t.Fatalf("first trace has no section start: %q", first.body)
	}
	second := gitlabTrace(t, srv, "610", http.Header{"Range": {"bytes=10-"}, "If-None-Match": {first.header.Get("ETag")}})
	if second.status != 200 || strings.Count(second.body, "\n") != 7 || !strings.HasPrefix(second.body, first.body) {
		t.Fatalf("second running trace = %d %q, want the whole grown trace", second.status, second.body)
	}
	if second.header.Get("ETag") == first.header.Get("ETag") {
		t.Fatal("a grown trace kept its ETag")
	}
	fixture.Repos[0].Pulls[0].CI.Jobs[0].Status = "success"
	if _, err := e.Seed(fixture); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	whole := gitlabTrace(t, srv, "610", nil)
	if whole.status != 200 || strings.Count(whole.body, "\n") != 23 || !strings.HasSuffix(whole.body, "section_end:1767225601:step_script_0\r\x1b[0K\n") {
		t.Fatalf("completed trace = %d %q", whole.status, whole.body)
	}
	part := gitlabTrace(t, srv, "610", http.Header{"Range": {"bytes=100-"}})
	if part.status != 206 || part.body != whole.body[100:] {
		t.Fatalf("completed trace start range = %d %q", part.status, part.body)
	}
	if got := gitlabTrace(t, srv, "610", http.Header{"Range": {"bytes=-10"}}); got.status != 400 || lastInvocation(t, e).Unhandled {
		t.Fatalf("suffix range on a completed trace = %d, want a handled 400", got.status)
	}
}
