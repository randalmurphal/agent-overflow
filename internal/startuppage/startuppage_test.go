package startuppage

import (
	"encoding/json"
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/startupprogress"
)

// TestReportFollowsTheLaunch: before a launch the report has no clock; a
// launch starts the clock; the reports supply the status, step and the
// update being finished; a new launch forgets the previous report.
func TestReportFollowsTheLaunch(t *testing.T) {
	var status Status
	t0 := time.Unix(1_700_000_000, 0)
	if got := status.Report(t0); got != (Report{Title: "Starting Agent Overflow"}) {
		t.Fatalf("report before a launch = %+v", got)
	}

	status.Begin(t0)
	if got := status.Report(t0.Add(1500 * time.Millisecond)); got.ElapsedMs != 1500 || got.Phase != "" {
		t.Fatalf("report before the first progress = %+v", got)
	}

	status.SetProgress(startupprogress.Progress{Phase: "store.migrate", Detail: "Applying migration 3 of 7 v101", Step: 3, Steps: 7, UpdatingTo: "1.2.3"})
	want := Report{
		Title: "Updating Agent Overflow", Status: "Finishing update to v1.2.3: applying migration 3 of 7 v101",
		Phase: "store.migrate", Step: 3, Steps: 7, ElapsedMs: 12_000,
	}
	if got := status.Report(t0.Add(12 * time.Second)); got != want {
		t.Fatalf("report = %+v, want %+v", got, want)
	}

	status.ClearProgress()
	if got := status.Report(t0.Add(13 * time.Second)); got.Phase != "" || got.Title != "Starting Agent Overflow" || got.ElapsedMs != 13_000 {
		t.Fatalf("a cleared report = %+v, want the clock alone", got)
	}

	status.SetProgress(startupprogress.Progress{Phase: "store.open"})
	status.Begin(t0.Add(time.Minute))
	if got := status.Report(t0.Add(time.Minute)); got.Phase != "" || got.Title != "Starting Agent Overflow" || got.ElapsedMs != 0 {
		t.Fatalf("a new launch kept the old report: %+v", got)
	}
}

// TestServeAnswersItsRoutesUncached: the loading page polls the report
// through its script, so it updates without a reload; the failure route
// serves the current page; none of it is cacheable; and any other path is
// left to the caller untouched.
func TestServeAnswersItsRoutesUncached(t *testing.T) {
	report := Report{Title: "Starting Agent Overflow", Status: "Applying migration 1 of 2 a", Phase: "store.migrate", Step: 1, Steps: 2, ElapsedMs: 42}
	failure := []byte("first failure")
	loading := Loading("Waiting <for> it")
	serve := func(path string) (*httptest.ResponseRecorder, bool) {
		rec := httptest.NewRecorder()
		served := Serve(rec, httptest.NewRequest("GET", path, nil), loading,
			func() Report { return report }, func() []byte { return failure })
		return rec, served
	}
	get := func(path, contentType string) string {
		t.Helper()
		rec, served := serve(path)
		if !served {
			t.Fatalf("%s was not served", path)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s can be cached", path)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, contentType) {
			t.Fatalf("%s content type = %q, want %s", path, ct, contentType)
		}
		return rec.Body.String()
	}

	page := get(LoadingPath, "text/html")
	for _, want := range []string{`<script src="/loading.js">`, `id="ao-loading-title"`, `id="ao-loading-meta"`,
		`<div class="label" id="ao-loading-status">Waiting &lt;for&gt; it</div>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("the loading page lacks %s: %s", want, page)
		}
	}
	script := get(ScriptPath, "text/javascript")
	for _, want := range []string{`fetch("/loading.json"`, "setTimeout(poll, 500)", "r.status", "r.steps", "r.elapsedMs", `" \u00b7 "`} {
		if !strings.Contains(script, want) {
			t.Fatalf("the script lacks %s", want)
		}
	}
	var got Report
	if err := json.Unmarshal([]byte(get(ReportPath, "application/json")), &got); err != nil || got != report {
		t.Fatalf("the report = %+v (%v), want %+v", got, err, report)
	}
	report.Step = 2
	if err := json.Unmarshal([]byte(get(ReportPath, "application/json")), &got); err != nil || got.Step != 2 {
		t.Fatalf("the report did not follow the launch: %+v %v", got, err)
	}
	if body := get(FailurePath, "text/html"); body != "first failure" {
		t.Fatalf("the failure route served %q", body)
	}
	failure = []byte("second failure")
	if body := get(FailurePath, "text/html"); body != "second failure" {
		t.Fatalf("the failure route kept an old page: %q", body)
	}

	rec, served := serve("/picker")
	if served || rec.Body.Len() != 0 || len(rec.Header()) != 0 {
		t.Fatalf("an unknown path was answered: served=%v body=%q header=%v", served, rec.Body.String(), rec.Header())
	}
}

// TestFailurePageEscapesItsCopyAndOffersRetryOnlyWhenAsked: every value is
// escaped, the page names where the details are, and only a page with a
// Retry method has the button, which calls that method the way Wails
// registers a bound call.
func TestFailurePageEscapesItsCopyAndOffersRetryOnlyWhenAsked(t *testing.T) {
	plain := string(Failure{Title: "Title <b>", Detail: "Reason: <script>x</script>", Action: "Do <this>", Log: `/home/u/<log>`}.HTML())
	for _, want := range []string{
		"<h1>Title &lt;b&gt;</h1><p>Reason: &lt;script&gt;x&lt;/script&gt;</p><p>Do &lt;this&gt;</p>",
		"<p>Startup details: <code>/home/u/&lt;log&gt;</code></p>",
	} {
		if !strings.Contains(plain, want) {
			t.Fatalf("the page lacks %s: %s", want, plain)
		}
	}
	if strings.Contains(plain, `id="ao-retry"`) || strings.Contains(plain, "<script") {
		t.Fatalf("a page without Retry offers it: %s", plain)
	}
	if strings.Contains(string(Failure{Title: "t", Detail: "d", Log: "l"}.HTML()), "<p></p>") {
		t.Fatal("a page without an action renders an empty paragraph")
	}

	method := `main.helper"App.RetryMigration`
	retry := string(Failure{Title: "t", Detail: "d", Log: "l", Retry: method}.HTML())
	if n := strings.Count(retry, `<button id="ao-retry"`); n != 1 {
		t.Fatalf("the page has %d Retry buttons", n)
	}
	if want := `var method = "` + template.JSEscapeString(method) + `";`; !strings.Contains(retry, want) {
		t.Fatalf("the page does not call %s: %s", want, retry)
	}
	if !strings.Contains(retry, `"/wails/runtime"`) || !strings.Contains(retry, `args: { "call-id": callId, methodName: method, args: [] }`) {
		t.Fatalf("the Retry call is not Wails' CallBinding shape: %s", retry)
	}
}
