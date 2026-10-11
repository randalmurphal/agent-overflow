package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/forgeapi"
)

// The GitLab forge over an httptest GitLab (newForgeAPICore). Paths are
// relative to the REST base /api/v4/, query included.

const gitlabTestProject = "projects/grp%2Fsub%2Fapp"

func gitlabTestRef(number int) PRReference {
	return PRReference{Forge: "gitlab", Host: testForgeHost, Namespace: "grp/sub", Repo: "app", Number: number}
}

// gitlabTestMR is a merge request with a diff head of "head2" and the
// head pipeline 77, or none when pipeline is 0.
func gitlabTestMR(number int, pipeline int) string {
	head := "null"
	if pipeline > 0 {
		head = fmt.Sprintf(`{"id":%d,"status":"running","web_url":"https://gitlab.example/p/%d"}`, pipeline, pipeline)
	}
	return fmt.Sprintf(`{"iid":%d,"title":"MR","description":"Body","source_branch":"feature","target_branch":"main",
		"sha":"head2","web_url":"https://gitlab.example/grp/sub/app/-/merge_requests/%d","state":"opened","draft":false,
		"changes_count":"3","has_conflicts":false,"detailed_merge_status":"mergeable","author":{"username":"author","name":"Author"},
		"diff_refs":{"base_sha":"base","head_sha":"head2","start_sha":"start"},"head_pipeline":%s}`, number, number, head)
}

// gitlabDiscussion is one positioned discussion whose note was written
// on headSHA.
func gitlabDiscussion(id, headSHA string) string {
	return fmt.Sprintf(`{"id":%q,"notes":[{"id":1,"body":"note %s","system":false,"created_at":"t","resolvable":true,"resolved":false,
		"author":{"username":"rev","name":"Rev"},"position":{"head_sha":%q,"new_path":"a.go","old_path":"a.go","position_type":"text","new_line":4}}]}`, id, id, headSHA)
}

func gitlabJobs(from, n int) string {
	jobs := make([]string, 0, n)
	for i := from; i < from+n; i++ {
		jobs = append(jobs, fmt.Sprintf(`{"id":%d,"name":"job%d","stage":"test","status":"success","started_at":"2026-01-01T00:00:00Z"}`, i, i))
	}
	return "[" + strings.Join(jobs, ",") + "]"
}

// gitlabPaths lists the method and path of every request.
func gitlabPaths(calls *forgeAPICalls) []string {
	var out []string
	for _, call := range calls.all() {
		out = append(out, call.Method+" "+call.Path)
	}
	return out
}

func countPrefix(paths []string, prefix string) int {
	n := 0
	for _, path := range paths {
		if path == prefix || strings.HasPrefix(path, prefix+"?") {
			n++
		}
	}
	return n
}

// A read that wants every part reads the merge request once: the detail
// adds the approvals, the threads the discussions and CI the head
// pipeline's jobs, all on the reference's host with the GitLab token
// header.
func TestGitLabReadPRReadsTheMergeRequestOnce(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 77)}
		case mr + "/approvals":
			return forgeAPIAnswer{Body: `{"approved_by":[{"approved_at":"t","user":{"username":"lead","name":"Lead"}}]}`}
		case mr + "/discussions?per_page=100":
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion("d1", "head2") + "]", Header: http.Header{"X-Next-Page": {"2"}}}
		case mr + "/discussions?page=2&per_page=100":
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion("d2", "head1") + "]", Header: http.Header{"X-Next-Page": {""}}}
		case gitlabTestProject + "/pipelines/77/jobs?per_page=100":
			return forgeAPIAnswer{Body: gitlabJobs(1, 3)}
		}
		return forgeUnexpected(t, call)
	})
	read, err := core.ReadPR(t.Context(), gitlabTestRef(7), allParts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths := gitlabPaths(calls)
	if countPrefix(paths, "GET "+mr) != 1 || len(paths) != 5 {
		t.Fatalf("requests = %q, want one MR read, the approvals, two discussion pages and one jobs page", paths)
	}
	for _, call := range calls.all() {
		if call.Host != testForgeHost || call.Header.Get("Private-Token") != "test-token" {
			t.Fatalf("request %s went to %q with Private-Token %q", call.Path, call.Host, call.Header.Get("Private-Token"))
		}
	}
	if d := read.Detail; d.Number != 7 || d.HeadSHA != "head2" || d.ReviewDecision != "APPROVED" || len(d.LatestReviews) != 1 ||
		d.Checks.Total != 1 || d.ChangedFiles != 3 || d.DiffRefs == nil || d.DiffRefs.StartSHA != "start" {
		t.Fatalf("detail = %+v", d)
	}
	if len(read.Threads) != 2 || read.Threads[0].IsOutdated || !read.Threads[1].IsOutdated {
		t.Fatalf("threads = %+v, want d1 current and d2 outdated against the MR's head", read.Threads)
	}
	if !read.HasCI || read.CI.URL != "https://gitlab.example/p/77" || len(read.CI.Stages) != 1 || len(read.CI.Stages[0].Jobs) != 3 {
		t.Fatalf("pipeline = %+v", read.CI)
	}
}

// A part not wanted costs no request; a merge request without a head
// pipeline costs no jobs read.
func TestGitLabReadPRRequestsOnlyTheWantedParts(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var pipeline atomic.Int32
	pipeline.Store(77)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, int(pipeline.Load()))}
		case call.Path == mr+"/approvals":
			return forgeAPIAnswer{Body: `{"approved_by":[]}`}
		case strings.HasPrefix(call.Path, mr+"/discussions?"):
			return forgeAPIAnswer{Body: `[]`}
		case strings.HasPrefix(call.Path, gitlabTestProject+"/pipelines/77/jobs?"):
			return forgeAPIAnswer{Body: gitlabJobs(1, 1)}
		}
		return forgeUnexpected(t, call)
	})
	cases := []struct {
		want     PRReadParts
		pipeline int32
		paths    []string
	}{
		{PRReadParts{Detail: true}, 77, []string{"GET " + mr, "GET " + mr + "/approvals"}},
		{PRReadParts{Threads: true}, 77, []string{"GET " + mr, "GET " + mr + "/discussions?per_page=100"}},
		{PRReadParts{CI: true}, 77, []string{"GET " + mr, "GET " + gitlabTestProject + "/pipelines/77/jobs?per_page=100"}},
		{PRReadParts{CI: true}, 0, []string{"GET " + mr}},
	}
	seen := 0
	for _, tc := range cases {
		pipeline.Store(tc.pipeline)
		read, err := core.ReadPR(t.Context(), gitlabTestRef(7), tc.want, nil, nil)
		if err != nil {
			t.Fatalf("%+v: %v", tc.want, err)
		}
		if read.HasCI != tc.want.CI {
			t.Fatalf("%+v: HasCI = %v", tc.want, read.HasCI)
		}
		paths := gitlabPaths(calls)[seen:]
		seen += len(paths)
		if strings.Join(paths, "\n") != strings.Join(tc.paths, "\n") {
			t.Fatalf("%+v (pipeline %d): requests = %q, want %q", tc.want, tc.pipeline, paths, tc.paths)
		}
	}
	if _, err := core.ReadPR(t.Context(), gitlabTestRef(7), PRReadParts{}, nil, nil); err == nil {
		t.Fatal("a read naming no part was sent")
	}
}

// The discussions are read page after page; a next page that does not
// advance ends the read instead of looping on it.
func TestGitLabThreadsStopOnANextPageThatDoesNotAdvance(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case strings.HasPrefix(call.Path, mr+"/discussions?"):
			return forgeAPIAnswer{Body: "[" + gitlabDiscussion("d", "head2") + "]", Header: http.Header{"X-Next-Page": {"1"}}}
		}
		return forgeUnexpected(t, call)
	})
	threads, err := core.ListReviewThreads(t.Context(), gitlabTestRef(7))
	if err != nil || len(threads) != 1 {
		t.Fatalf("ListReviewThreads = %+v, %v", threads, err)
	}
	if n := len(calls.all()); n != 2 {
		t.Fatalf("requests = %q, want the MR and one discussions page", gitlabPaths(calls))
	}
}

// The pipeline's jobs are paged by X-Next-Page until a short page, and at
// most gitlabCIJobsMaxPages pages are read.
func TestGitLabPipelineJobsPageToTheCap(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var short atomic.Bool
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == mr {
			return forgeAPIAnswer{Body: gitlabTestMR(7, 77)}
		}
		u, err := url.Parse(call.Path)
		if err != nil || u.EscapedPath() != gitlabTestProject+"/pipelines/77/jobs" {
			return forgeUnexpected(t, call)
		}
		page, _ := strconv.Atoi(u.Query().Get("page"))
		page = max(page, 1)
		if short.Load() && page == 2 {
			return forgeAPIAnswer{Body: gitlabJobs(5000, 10)}
		}
		return forgeAPIAnswer{Body: gitlabJobs(page*1000, gitlabPageSize), Header: http.Header{"X-Next-Page": {strconv.Itoa(page + 1)}}}
	})
	pipeline, err := core.ListPRCIJobs(t.Context(), gitlabTestRef(7), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(pipeline.Stages[0].Jobs); n != gitlabCIJobsMaxPages*gitlabPageSize {
		t.Fatalf("jobs = %d, want %d", n, gitlabCIJobsMaxPages*gitlabPageSize)
	}
	if n := countPrefix(gitlabPaths(calls), "GET "+gitlabTestProject+"/pipelines/77/jobs"); n != gitlabCIJobsMaxPages {
		t.Fatalf("jobs pages = %d, want %d", n, gitlabCIJobsMaxPages)
	}
	short.Store(true)
	before := len(calls.all())
	pipeline, err = core.ListPRCIJobs(t.Context(), gitlabTestRef(7), nil, nil)
	if err != nil || len(pipeline.Stages[0].Jobs) != gitlabPageSize+10 {
		t.Fatalf("short second page: %v, %v", pipeline, err)
	}
	if n := len(calls.all()) - before; n != 3 {
		t.Fatalf("requests = %d, want the MR and two jobs pages", n)
	}
}

// A merge request GitLab does not have is the transport's 404, which the
// pump reads as the PR gone; any other failure is its status error.
func TestGitLabReadPRReturnsTheForgeError(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	core, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: int(status.Load()), Body: `{"message":"404 Merge Request Not Found"}`}
	})
	status.Store(http.StatusNotFound)
	if _, err := core.ReadPR(t.Context(), gitlabTestRef(7), allParts, nil, nil); !errors.Is(err, forgeapi.ErrNotFound) {
		t.Fatalf("ReadPR = %v, want the 404", err)
	}
	status.Store(http.StatusUnauthorized)
	_, err := core.ReadPR(t.Context(), gitlabTestRef(7), allParts, nil, nil)
	if setup, ok := errors.AsType[*forgeapi.SetupError](err); !ok || setup.Kind != forgeapi.SetupUnauthenticated || setup.Binary != "glab" {
		t.Fatalf("ReadPR after 401 = %v, want the unauthenticated glab setup error", err)
	}
}

// A Core without a forge API transport refuses GitLab requests with
// ErrNoForgeAPI rather than reaching anything.
func TestGitLabWithoutATransportIsErrNoForgeAPI(t *testing.T) {
	t.Parallel()
	core := NewCore()
	ref := gitlabTestRef(1)
	if _, err := core.ReadPR(t.Context(), ref, allParts, nil, nil); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("ReadPR = %v", err)
	}
	if _, err := core.GetCIJobLog(t.Context(), ref, CIJobLogRequest{JobID: "9"}); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("GetCIJobLog = %v", err)
	}
	if _, err := core.SubmitReview(t.Context(), ref, SubmitReviewRequest{Verdict: ReviewVerdictApprove}); !errors.Is(err, ErrNoForgeAPI) {
		t.Fatalf("SubmitReview = %v", err)
	}
}

// A review reads the MR alone for its diff refs and head, posts each draft
// note as a JSON object, publishes them, and approves the head it read. An
// approval that fails after the notes were published is a partial submit.
func TestGitLabSubmitReview(t *testing.T) {
	t.Parallel()
	mr := gitlabTestProject + "/merge_requests/7"
	var failApprove atomic.Bool
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Method + " " + call.Path {
		case "GET " + mr:
			return forgeAPIAnswer{Body: gitlabTestMR(7, 0)}
		case "POST " + mr + "/draft_notes":
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":1}`}
		case "POST " + mr + "/draft_notes/bulk_publish":
			return forgeAPIAnswer{Status: http.StatusNoContent}
		case "POST " + mr + "/approve":
			if failApprove.Load() {
				return forgeAPIAnswer{Status: http.StatusUnprocessableEntity, Body: `{"message":"head changed"}`}
			}
			return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
		}
		return forgeUnexpected(t, call)
	})
	line, old := 4, 9
	review := SubmitReviewRequest{Verdict: ReviewVerdictApprove, Body: "Looks good", Comments: []ReviewLineComment{
		{Path: "a.go", Body: "nit", Line: &line, Side: "right"},
		{Path: "b.go", Body: "gone", Line: &old, Side: "left"},
	}}
	result, err := core.SubmitReview(t.Context(), gitlabTestRef(7), review)
	if err != nil || !result.PostedReview {
		t.Fatalf("SubmitReview = %+v, %v", result, err)
	}
	want := []string{"GET " + mr, "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes", "POST " + mr + "/draft_notes/bulk_publish", "POST " + mr + "/approve"}
	if got := gitlabPaths(calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	sent := calls.all()
	var summary, right, left struct {
		Note     string             `json:"note"`
		Position *gitlabPositionRaw `json:"position"`
	}
	if err := json.Unmarshal(sent[1].Body, &summary); err != nil || summary.Note != "Looks good" || summary.Position != nil {
		t.Fatalf("summary note body = %s (%v)", sent[1].Body, err)
	}
	if err := json.Unmarshal(sent[2].Body, &right); err != nil || right.Note != "nit" || right.Position == nil || right.Position.PositionType != "text" ||
		right.Position.HeadSHA != "head2" || right.Position.BaseSHA != "base" || right.Position.StartSHA != "start" ||
		right.Position.NewLine == nil || *right.Position.NewLine != 4 || right.Position.OldLine != nil {
		t.Fatalf("right-side note body = %s (%v)", sent[2].Body, err)
	}
	if err := json.Unmarshal(sent[3].Body, &left); err != nil || left.Position == nil || left.Position.OldLine == nil || *left.Position.OldLine != 9 || left.Position.NewLine != nil {
		t.Fatalf("left-side note body = %s (%v)", sent[3].Body, err)
	}
	for _, call := range sent[1:4] {
		if call.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("draft note Content-Type = %q", call.Header.Get("Content-Type"))
		}
	}
	if len(sent[4].Body) != 0 {
		t.Fatalf("bulk publish body = %q, want none", sent[4].Body)
	}
	var approve map[string]string
	if err := json.Unmarshal(sent[5].Body, &approve); err != nil || len(approve) != 1 || approve["sha"] != "head2" {
		t.Fatalf("approve body = %s (%v), want the head the MR read named", sent[5].Body, err)
	}

	failApprove.Store(true)
	_, err = core.SubmitReview(t.Context(), gitlabTestRef(7), review)
	partial, ok := errors.AsType[*PartialSubmitError](err)
	if !ok || !partial.PostedReview {
		t.Fatalf("failed approval after publishing = %v, want a partial submit", err)
	}
	// An approval alone that fails posted nothing.
	_, err = core.SubmitReview(t.Context(), gitlabTestRef(7), SubmitReviewRequest{Verdict: ReviewVerdictApprove})
	if _, partial := errors.AsType[*PartialSubmitError](err); partial || err == nil {
		t.Fatalf("failed bare approval = %v, want a plain error", err)
	}
}

// A reply posts {"body"} to the discussion's notes; resolving PUTs
// {"resolved"} on the discussion. The discussion id is one path segment.
func TestGitLabReplyAndResolve(t *testing.T) {
	t.Parallel()
	discussion := gitlabTestProject + "/merge_requests/7/discussions/abc%2Fdef"
	var mu sync.Mutex
	status := http.StatusOK
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		switch call.Method + " " + call.Path {
		case "POST " + discussion + "/notes", "PUT " + discussion:
			return forgeAPIAnswer{Status: status, Body: `{"message":"answer"}`}
		}
		return forgeUnexpected(t, call)
	})
	ref := gitlabTestRef(7)
	if err := core.ReplyToThread(t.Context(), ref, "abc/def", 0, "Thanks"); err != nil {
		t.Fatal(err)
	}
	for _, resolved := range []bool{true, false} {
		if err := core.SetThreadResolved(t.Context(), ref, "abc/def", resolved); err != nil {
			t.Fatal(err)
		}
	}
	sent := calls.all()
	if len(sent) != 3 || string(sent[0].Body) != `{"body":"Thanks"}` || string(sent[1].Body) != `{"resolved":true}` || string(sent[2].Body) != `{"resolved":false}` {
		t.Fatalf("requests = %q", gitlabPaths(calls))
	}
	mu.Lock()
	status = http.StatusBadRequest
	mu.Unlock()
	if err := core.SetThreadResolved(t.Context(), ref, "abc/def", true); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("refused resolve = %v, want the forge's 400", err)
	}
	if err := core.ReplyToThread(t.Context(), ref, " ", 0, "x"); err == nil {
		t.Fatal("a reply without a discussion id was sent")
	}
	if err := core.ReplyToThread(t.Context(), ref, "abc", 0, " "); err == nil {
		t.Fatal("an empty reply was sent")
	}
	if n := len(calls.all()); n != 4 {
		t.Fatalf("requests = %d, want the refused input to send nothing", n)
	}
}

// A trace is cleaned; a 404 is ErrCIJobLogNotFound carrying the forge's
// error, and any other failure is not.
func TestGitLabJobLog(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(http.StatusOK)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path != gitlabTestProject+"/jobs/901/trace" {
			return forgeUnexpected(t, call)
		}
		if s := int(status.Load()); s != http.StatusOK {
			return forgeAPIAnswer{Status: s, Body: `{"message":"` + http.StatusText(s) + `"}`}
		}
		return forgeAPIAnswer{Body: "section_start:1:x\r\x1b[0Kline one\n", Header: http.Header{"Content-Type": {"text/plain"}}}
	})
	read, err := core.GetCIJobLog(t.Context(), gitlabTestRef(1), CIJobLogRequest{JobID: "901"})
	log := read.Text
	if err != nil || log != "section_start:1:x\nline one\n" {
		t.Fatalf("GetCIJobLog = %q, %v", log, err)
	}
	if accept := calls.all()[0].Header.Get("Accept"); accept != "*/*" {
		t.Fatalf("trace Accept = %q", accept)
	}
	if _, err := core.GetCIJobLog(t.Context(), gitlabTestRef(1), CIJobLogRequest{JobID: "../901"}); err == nil {
		t.Fatal("a non-numeric job id was requested")
	}
	for _, tc := range []struct {
		status   int
		notFound bool
	}{{http.StatusNotFound, true}, {http.StatusInternalServerError, false}, {http.StatusForbidden, false}} {
		status.Store(int32(tc.status))
		_, err := core.GetCIJobLog(t.Context(), gitlabTestRef(1), CIJobLogRequest{JobID: "901"})
		var statusErr *forgeapi.StatusError
		if errors.Is(err, ErrCIJobLogNotFound) != tc.notFound || !errors.As(err, &statusErr) || statusErr.Status != tc.status {
			t.Fatalf("HTTP %d: GetCIJobLog = %v, want ErrCIJobLogNotFound %v over the *StatusError", tc.status, err, tc.notFound)
		}
	}
}

// A job log read with the ETag of the text the caller holds is
// conditional: a matching ETag answers 304, which reports NotModified with
// no text, and a moved log answers the new text and its ETag. Both forges.
func TestJobLogRevalidatesItsETag(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		forge string
		ref   PRReference
		path  string
	}{
		{"gitlab", gitlabTestRef(1), gitlabTestProject + "/jobs/901/trace"},
		{"github", githubTestRef(1), "repos/o/r/actions/jobs/901/logs"},
	} {
		var current atomic.Value
		current.Store(`"v1"`)
		core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
			if call.Path != tc.path {
				return forgeUnexpected(t, call)
			}
			etag := current.Load().(string)
			if call.Header.Get("If-None-Match") == etag {
				return forgeAPIAnswer{Status: http.StatusNotModified, Header: http.Header{"ETag": {etag}}}
			}
			return forgeAPIAnswer{Body: "log " + etag + "\n", Header: http.Header{"Content-Type": {"text/plain"}, "ETag": {etag}}}
		})
		first, err := core.GetCIJobLog(t.Context(), tc.ref, CIJobLogRequest{JobID: "901"})
		if err != nil || first.NotModified || first.ETag != `"v1"` || first.Text != `log "v1"`+"\n" {
			t.Fatalf("%s: unconditional read = %+v, %v", tc.forge, first, err)
		}
		same, err := core.GetCIJobLog(t.Context(), tc.ref, CIJobLogRequest{JobID: "901", ETag: first.ETag})
		if err != nil || !same.NotModified || same.Text != "" || same.ETag != `"v1"` {
			t.Fatalf("%s: read with a matching ETag = %+v, %v; want NotModified", tc.forge, same, err)
		}
		current.Store(`"v2"`)
		moved, err := core.GetCIJobLog(t.Context(), tc.ref, CIJobLogRequest{JobID: "901", ETag: first.ETag})
		if err != nil || moved.NotModified || moved.ETag != `"v2"` || moved.Text != `log "v2"`+"\n" {
			t.Fatalf("%s: read after the log moved = %+v, %v", tc.forge, moved, err)
		}
		sent := calls.all()
		if len(sent) != 3 || sent[0].Header.Get("If-None-Match") != "" || sent[1].Header.Get("If-None-Match") != `"v1"` || sent[2].Header.Get("If-None-Match") != `"v1"` {
			t.Fatalf("%s: If-None-Match per request = %q", tc.forge, []string{sent[0].Header.Get("If-None-Match"), sent[1].Header.Get("If-None-Match"), sent[2].Header.Get("If-None-Match")})
		}
	}
}

// A trace over maxCILogBytes keeps its tail from a whole line. A running
// GitLab trace ignores Range and answers the whole body again, which the
// tail buffer cuts.
func TestGitLabJobLogKeepsTheTail(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("y", 99) + "\n"
	body := strings.Repeat(line, maxCILogBytes/len(line)+50)
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Body: body, Header: http.Header{"Content-Type": {"text/plain"}, "Content-Length": {strconv.Itoa(len(body))}}}
	})
	read, err := core.GetCIJobLog(t.Context(), gitlabTestRef(1), CIJobLogRequest{JobID: "901"})
	log := read.Text
	if err != nil {
		t.Fatal(err)
	}
	if len(log) > maxCILogBytes || len(log) < maxCILogBytes-len(line) || !strings.HasPrefix(log, "yyy") || !strings.HasSuffix(body, log) || len(log)%len(line) != 0 {
		t.Fatalf("tail of %d bytes, want whole lines within %d", len(log), maxCILogBytes)
	}
	sent := calls.all()
	if len(sent) != 2 || sent[1].Header.Get("Range") == "" {
		t.Fatalf("requests = %d, want the probe and the Range", len(sent))
	}
}

// The repository-scoped lists address the project cwd's origin names, on
// its host: the open MR by its escaped source branch, merged heads paged
// at a fixed page size up to the limit.
func TestGitLabOriginScopedLists(t *testing.T) {
	t.Parallel()
	list := gitlabTestProject + "/merge_requests"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		u, err := url.Parse(call.Path)
		if err != nil || u.EscapedPath() != list {
			return forgeUnexpected(t, call)
		}
		q := u.Query()
		switch q.Get("state") {
		case "opened":
			if q.Get("source_branch") == "none" {
				return forgeAPIAnswer{Body: `null`}
			}
			return forgeAPIAnswer{Body: `[{"web_url":"https://gitlab.com/grp/sub/app/-/merge_requests/3","iid":3,"title":"Fix","state":"opened"}]`}
		case "merged":
			perPage, _ := strconv.Atoi(q.Get("per_page"))
			page, _ := strconv.Atoi(q.Get("page"))
			page = max(page, 1)
			n := perPage
			if page == 2 {
				n = 50
			}
			rows := make([]string, 0, n)
			for i := range n {
				rows = append(rows, fmt.Sprintf(`{"source_branch":"p%d-%d","sha":"s%d","web_url":"u"}`, page, i, i))
			}
			return forgeAPIAnswer{Body: "[" + strings.Join(rows, ",") + "]", Header: http.Header{"X-Next-Page": {strconv.Itoa(page + 1)}}}
		}
		return forgeUnexpected(t, call)
	})
	cwd := t.TempDir()
	if forge := core.recordOrigin(cwd, originIdentity{url: "git@gitlab.com:grp/sub/app.git", known: true}, core.nowFn()); forge != "gitlab" {
		t.Fatalf("origin classified as %q", forge)
	}
	pulls, err := core.ListOpenPRs(t.Context(), cwd, "feature/x y")
	if err != nil || len(pulls) != 1 || pulls[0].Number != 3 || pulls[0].State != "open" || pulls[0].URL == "" {
		t.Fatalf("ListOpenPRs = %+v, %v", pulls, err)
	}
	if pulls, err := core.ListOpenPRs(t.Context(), cwd, "none"); err != nil || len(pulls) != 0 {
		t.Fatalf("ListOpenPRs(none) = %+v, %v", pulls, err)
	}
	heads, err := core.ListMergedPRHeads(t.Context(), cwd, 200)
	if err != nil || len(heads) != 150 || heads[0].HeadRefName != "p1-0" || heads[100].HeadRefName != "p2-0" {
		t.Fatalf("ListMergedPRHeads(200) = %d heads, %v", len(heads), err)
	}
	heads, err = core.ListMergedPRHeads(t.Context(), cwd, 30)
	if err != nil || len(heads) != 30 {
		t.Fatalf("ListMergedPRHeads(30) = %d heads, %v", len(heads), err)
	}
	want := []string{
		"GET " + list + "?per_page=1&source_branch=feature%2Fx+y&state=opened&view=simple",
		"GET " + list + "?per_page=1&source_branch=none&state=opened&view=simple",
		"GET " + list + "?order_by=updated_at&per_page=100&sort=desc&state=merged",
		"GET " + list + "?order_by=updated_at&page=2&per_page=100&sort=desc&state=merged",
		"GET " + list + "?order_by=updated_at&per_page=30&sort=desc&state=merged",
	}
	if got := gitlabPaths(calls); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, call := range calls.all() {
		if call.Host != "gitlab.com" {
			t.Fatalf("list went to %q, want the origin's host", call.Host)
		}
	}
}

// A refused list is the transport's status error; a checkout whose origin
// is unknown or not GitLab fails with *OriginUnknownError and sends
// nothing.
func TestGitLabOriginScopedListFailures(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Project Not Found"}`}
	})
	forge := core.ForgeByID("gitlab")
	cwd := t.TempDir()
	core.recordOrigin(cwd, originIdentity{url: "https://gitlab.com/grp/app.git", known: true}, core.nowFn())
	if _, err := forge.ListOpenPRs(t.Context(), cwd, "b"); !errors.Is(err, forgeapi.ErrNotFound) {
		t.Fatalf("ListOpenPRs on a missing project = %v, want its 404", err)
	}
	sent := len(calls.all())
	for name, origin := range map[string]string{"no origin": "", "github": "https://github.com/acme/repo.git", "no project": "https://gitlab.com/grp"} {
		cwd := t.TempDir()
		if origin != "" {
			core.recordOrigin(cwd, originIdentity{url: origin, known: true}, core.nowFn())
		}
		var unknown *OriginUnknownError
		if _, err := forge.ListOpenPRs(t.Context(), cwd, "b"); !errors.As(err, &unknown) || unknown.Cwd != cwd {
			t.Fatalf("%s: ListOpenPRs = %v, want *OriginUnknownError", name, err)
		}
		if _, err := forge.ListMergedPRHeads(t.Context(), cwd, 5); !errors.As(err, &unknown) {
			t.Fatalf("%s: ListMergedPRHeads = %v, want *OriginUnknownError", name, err)
		}
	}
	if n := len(calls.all()); n != sent {
		t.Fatalf("requests = %q", gitlabPaths(calls))
	}
}

const testUploadSecretB = "fedcba9876543210fedcba9876543210"

// A GitLab upload is a transport request for the project's uploads path,
// byte for byte, on the reference's host with its port; a larger body
// than the cap is refused.
func TestGitLabFetchAttachment(t *testing.T) {
	t.Parallel()
	content := "\x1b\x00binary\xff"
	upload := "projects/grp%2Fsub%2Fwidget/uploads/" + testUploadSecretB + "/Screen%20Shot.png"
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path != upload {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Body: content, Header: http.Header{"Content-Type": {"image/png"}}}
	})
	ref := PRReference{Forge: "gitlab", Host: "gitlab.example.com:8443", Namespace: "group/x", Repo: "y", Number: 4}
	href := "https://gitlab.example.com:8443/grp/sub/widget/uploads/" + testUploadSecretB + "/Screen%20Shot.png"
	data, name, err := core.FetchAttachment(t.Context(), ref, href, 1<<20)
	if err != nil || string(data) != content || name != "Screen Shot.png" {
		t.Fatalf("FetchAttachment = %q %q %v", data, name, err)
	}
	sent := calls.all()
	if len(sent) != 1 || sent[0].Host != "gitlab.example.com:8443" || sent[0].Header.Get("Accept") != "*/*" || sent[0].Header.Get("Private-Token") != "test-token" {
		t.Fatalf("attachment request = %+v", sent)
	}
	if _, _, err := core.FetchAttachment(t.Context(), ref, href, 4); err == nil || err.Error() != "attachment is larger than 4 bytes" {
		t.Fatalf("over-cap attachment = %v", err)
	}
}

// A failed upload fetch keeps the shape of its failure for errors.Is and
// errors.As, and its message never carries the upload secret: not from a
// status error, not from a dropped connection.
func TestGitLabFetchAttachmentFailureHidesTheSecret(t *testing.T) {
	t.Parallel()
	var mode atomic.Value
	core, _ := newForgeAPICore(t, func(forgeAPICall) forgeAPIAnswer {
		switch mode.Load() {
		case "drop":
			panic(http.ErrAbortHandler)
		case "unauthorized":
			return forgeAPIAnswer{Status: http.StatusUnauthorized, Body: `{"message":"401 Unauthorized"}`}
		}
		return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 File Not Found"}`}
	})
	ref := PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "g", Repo: "r", Number: 1}
	href := "/uploads/" + testUploadSecretB + "/a.png"
	check := func(name string, match func(error) bool) {
		t.Helper()
		_, _, err := core.FetchAttachment(t.Context(), ref, href, 1<<20)
		if err == nil || !match(err) {
			t.Fatalf("%s: FetchAttachment = %v", name, err)
		}
		if strings.Contains(err.Error(), testUploadSecretB) {
			t.Fatalf("%s: error %q carries the upload secret", name, err)
		}
	}
	mode.Store("missing")
	check("404", func(err error) bool {
		return errors.Is(err, forgeapi.ErrNotFound) && strings.Contains(err.Error(), "<attachment>")
	})
	mode.Store("drop")
	check("dropped", func(err error) bool { _, ok := errors.AsType[*forgeapi.TransientError](err); return ok })
	mode.Store("unauthorized")
	check("401", func(err error) bool {
		setup, ok := errors.AsType[*forgeapi.SetupError](err)
		return ok && setup.Kind == forgeapi.SetupUnauthenticated && setup.Binary == "glab"
	})
}
