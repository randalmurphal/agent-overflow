package git

import (
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeCIStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status, conclusion, want string
	}{
		{"COMPLETED", "SUCCESS", CIStatusSuccess},
		{"completed", "failure", CIStatusFailed},
		{"completed", "timed_out", CIStatusFailed},
		{"completed", "cancelled", CIStatusCanceled},
		{"completed", "skipped", CIStatusSkipped},
		{"completed", "neutral", CIStatusNeutral},
		{"completed", "", CIStatusNeutral},
		{"IN_PROGRESS", "", CIStatusRunning},
		{"queued", "", CIStatusPending},
		// GitLab single-status forms.
		{"success", "", CIStatusSuccess},
		{"failed", "", CIStatusFailed},
		{"running", "", CIStatusRunning},
		{"created", "", CIStatusPending},
		{"waiting_for_resource", "", CIStatusPending},
		{"manual", "", CIStatusManual},
		{"canceled", "", CIStatusCanceled},
		// Unknown states pass through lowercased, not blank.
		{"weird_new_state", "", "weird_new_state"},
	}
	for _, c := range cases {
		if got := NormalizeCIStatus(c.status, c.conclusion); got != c.want {
			t.Errorf("NormalizeCIStatus(%q, %q) = %q, want %q", c.status, c.conclusion, got, c.want)
		}
	}
}

func TestAggregateCIStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		statuses []string
		want     string
	}{
		{nil, CIStatusSkipped},
		{[]string{CIStatusSuccess, CIStatusSkipped}, CIStatusSuccess},
		{[]string{CIStatusSuccess, CIStatusFailed, CIStatusRunning}, CIStatusFailed},
		{[]string{CIStatusSuccess, CIStatusRunning}, CIStatusRunning},
		{[]string{CIStatusSuccess, CIStatusPending}, CIStatusPending},
		{[]string{CIStatusSuccess, CIStatusManual}, CIStatusManual},
		{[]string{CIStatusSkipped, CIStatusNeutral}, CIStatusSkipped},
		// Unknown states outrank success so they stay visible.
		{[]string{CIStatusSuccess, "weird"}, "weird"},
	}
	for _, c := range cases {
		if got := AggregateCIStatus(c.statuses); got != c.want {
			t.Errorf("AggregateCIStatus(%v) = %q, want %q", c.statuses, got, c.want)
		}
	}
}

func TestValidateCIJobID(t *testing.T) {
	t.Parallel()
	if err := ValidateCIJobID("15208089088"); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	for _, bad := range []string{"", "abc", "12/logs", "-1", "1 2", "999999999999999999999"} {
		if err := ValidateCIJobID(bad); err == nil {
			t.Errorf("ValidateCIJobID(%q) accepted, want error", bad)
		}
	}
}

func TestGroupGitLabJobsByStage(t *testing.T) {
	t.Parallel()
	started := "2026-07-06T19:11:33Z"
	duration := 42.5
	// Newest-first order, as the API returns.
	raw := []gitlabCIJobRaw{
		{ID: 30, Name: "docs-check", Stage: "docs", Status: "manual", AllowFailure: true},
		{ID: 20, Name: "unit", Stage: "test", Status: "failed", Duration: &duration, StartedAt: &started, WebURL: "https://x/j/20"},
		{ID: 21, Name: "lint", Stage: "test", Status: "success", StartedAt: &started},
		{ID: 10, Name: "compile", Stage: "build", Status: "success", StartedAt: &started},
	}
	stages := groupGitLabJobsByStage(raw)

	names := make([]string, len(stages))
	for i, s := range stages {
		names[i] = s.Name
	}
	if strings.Join(names, ",") != "build,test,docs" {
		t.Fatalf("stage order = %v, want build,test,docs", names)
	}
	if stages[1].Status != CIStatusFailed {
		t.Fatalf("test stage status = %q, want failed", stages[1].Status)
	}
	if stages[2].Status != CIStatusManual {
		t.Fatalf("docs stage status = %q, want manual", stages[2].Status)
	}
	unit := stages[1].Jobs[0]
	if unit.ID != "20" || unit.DurationSeconds != 42.5 || !unit.LogsAvailable || unit.URL != "https://x/j/20" {
		t.Fatalf("unit job = %+v", unit)
	}
	manual := stages[2].Jobs[0]
	if manual.LogsAvailable {
		t.Fatal("manual (never started) job must not advertise logs")
	}
	if !manual.AllowFailure {
		t.Fatal("allow_failure not carried")
	}
}

func TestSplitGitHubChecks(t *testing.T) {
	t.Parallel()
	checks := []CheckStatus{
		{Kind: "CheckRun", Name: "build", Workflow: "CI", Status: "COMPLETED", Conclusion: "SUCCESS", DetailsURL: "https://github.com/o/r/actions/runs/111/job/901", StartedAt: "2026-07-06T14:08:19Z", CompletedAt: "2026-07-06T14:08:47Z"},
		{Kind: "CheckRun", Name: "lint", Workflow: "CI", Status: "QUEUED", DetailsURL: "https://github.com/o/r/actions/runs/111/job/902"},
		{Kind: "CheckRun", Name: "scan", Workflow: "Code Scanning", Status: "IN_PROGRESS", DetailsURL: "https://github.com/o/r/actions/runs/222/job/903", StartedAt: "2026-07-06T14:08:19Z"},
		{Kind: "CheckRun", Name: "deploy", Workflow: "Code Scanning", Status: "COMPLETED", Conclusion: "CANCELLED", DetailsURL: "https://github.com/o/r/actions/runs/222/job/904"},
		{Kind: "CheckRun", Name: "vendor-bot", DetailsURL: "https://vendor.example/checks/1", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Kind: "StatusContext", Name: "codecov/patch", Status: "SUCCESS", DetailsURL: "https://codecov.example/x"},
	}
	runs, external := splitGitHubChecks(checks)
	if len(runs) != 2 || runs[0].id != "111" || runs[0].workflow != "CI" || runs[1].id != "222" || runs[1].workflow != "Code Scanning" {
		t.Fatalf("runs = %+v, want 111 (CI) and 222 (Code Scanning)", runs)
	}
	build, lint, scan := runs[0].jobs[0], runs[0].jobs[1], runs[1].jobs[0]
	if build.ID != "901" || build.Name != "build" || build.Status != CIStatusSuccess || build.DurationSeconds != 28 || !build.LogsAvailable ||
		build.URL != "https://github.com/o/r/actions/runs/111/job/901" || build.Steps != nil {
		t.Fatalf("build = %+v", build)
	}
	if lint.ID != "902" || lint.Status != CIStatusPending || lint.LogsAvailable {
		t.Fatalf("queued lint = %+v, want pending without logs", lint)
	}
	if scan.ID != "903" || scan.Status != CIStatusRunning || !scan.LogsAvailable {
		t.Fatalf("scan = %+v", scan)
	}
	// Cancelled while queued: the job never ran, so it never has a log.
	deploy := runs[1].jobs[1]
	if deploy.ID != "904" || deploy.Status != CIStatusCanceled || deploy.LogsAvailable {
		t.Fatalf("never-started deploy = %+v, want canceled without logs", deploy)
	}
	if len(external) != 2 {
		t.Fatalf("external = %d entries, want 2", len(external))
	}
	if external[0].LogsAvailable || external[1].LogsAvailable {
		t.Fatal("external checks must not advertise logs")
	}
	if external[0].Status != CIStatusSuccess {
		t.Fatalf("external[0].Status = %q, want success", external[0].Status)
	}
}

// The pipeline is the MR's head pipeline, its jobs grouped into stages
// in creation order although the jobs list answers newest first.
func TestGitLabListPRCIJobs(t *testing.T) {
	t.Parallel()
	mr := "projects/group%2Frepo/merge_requests/12"
	var pipeline atomic.Value
	pipeline.Store(`{"id":77,"status":"failed","web_url":"https://gl/p/77"}`)
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case mr:
			return forgeAPIAnswer{Body: `{"iid":12,"head_pipeline":` + pipeline.Load().(string) + `}`}
		case "projects/group%2Frepo/pipelines/77/jobs?per_page=100":
			return forgeAPIAnswer{Body: `[{"id":20,"name":"unit","stage":"test","status":"failed","duration":10.0,"web_url":"https://gl/j/20","allow_failure":false,"started_at":"2026-07-06T19:11:33Z"},{"id":10,"name":"compile","stage":"build","status":"success","duration":5.0,"web_url":"https://gl/j/10","allow_failure":false,"started_at":"2026-07-06T19:10:33Z"}]`}
		}
		return forgeUnexpected(t, call)
	})
	ref := PRReference{Forge: "gitlab", Host: testForgeHost, Namespace: "group", Repo: "repo", Number: 12}
	got, err := core.ListPRCIJobs(t.Context(), ref, nil, nil)
	if err != nil {
		t.Fatalf("ListPRCIJobs returned error: %v", err)
	}
	if got.Status != CIStatusFailed || got.URL != "https://gl/p/77" {
		t.Fatalf("pipeline = %+v", got)
	}
	if len(got.Stages) != 2 || got.Stages[0].Name != "build" || got.Stages[1].Name != "test" || !got.Stages[1].Jobs[0].LogsAvailable {
		t.Fatalf("stages = %+v", got.Stages)
	}
	pipeline.Store(`null`)
	if got, err = core.ListPRCIJobs(t.Context(), ref, nil, nil); err != nil || got.Status != "" || len(got.Stages) != 0 {
		t.Fatalf("no head pipeline = %+v, %v; want an empty pipeline", got, err)
	}
}

func TestStripCISectionMarkers(t *testing.T) {
	t.Parallel()
	cleaned := cleanGitLabTrace("Running with gitlab-runner\n" +
		"section_start:1714557600:step_script\r\x1b[0K\x1b[36;1mExecuting\x1b[0;m\n" +
		"$ make test\n" +
		"\x1b[0Ksection_end:1714557605:step_script\r\x1b[0K\n" +
		"section_start:1714557605:after[collapsed=true]\r\x1b[0Kdone\n" +
		"section_end:1714557606:after\r\x1b[0K\n" +
		"section_start without a time stays\n")
	want := "Running with gitlab-runner\n" +
		"\x1b[36;1mExecuting\x1b[0;m\n" +
		"$ make test\n" +
		"done\n" +
		"section_start without a time stays\n"
	if got := StripCISectionMarkers(cleaned); got != want {
		t.Fatalf("StripCISectionMarkers = %q, want %q", got, want)
	}
	plain := "2026-10-11T00:04:31.5805438Z ##[group]Run make\n"
	if got := StripCISectionMarkers(plain); got != plain {
		t.Fatalf("a log without markers changed: %q", got)
	}
}

func TestCleanGitLabTrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "timestamped stream prefix keeps timestamp, drops flags",
			raw:  "2026-06-26T22:39:47.158568Z 00O echo hello",
			want: "2026-06-26T22:39:47.158568Z echo hello",
		},
		{
			name: "marker-only timestamped line keeps the bare marker",
			raw: "2026-06-26T22:39:47.158568Z 00O+\x1b[0Ksection_start:1782513587:upload_artifacts_on_success\n" +
				"2026-06-26T22:39:47.158568Z 00O \x1b[0;33mUploading artifacts\x1b[0;m",
			want: "section_start:1782513587:upload_artifacts_on_success\n" +
				"2026-06-26T22:39:47.158568Z \x1b[0;33mUploading artifacts\x1b[0;m",
		},
		{
			name: "inline section marker puts its CR-erased header on the next line",
			raw:  "section_start:1714557600:step_script\r\x1b[0K\x1b[36;1mRunning steps\x1b[0;m",
			want: "section_start:1714557600:step_script\n\x1b[36;1mRunning steps\x1b[0;m",
		},
		{
			name: "section marker keeps its options",
			raw:  "section_start:1714557600:cleanup[collapsed=true]\r\x1b[0Kdone",
			want: "section_start:1714557600:cleanup[collapsed=true]\ndone",
		},
		{
			name: "an end and a start on one line keep their order",
			raw:  "\x1b[0Ksection_end:1714557605:prepare\r\x1b[0K\x1b[0Ksection_start:1714557605:step_script\r\x1b[0K\x1b[0K\x1b[36;1mExecuting\x1b[0;m",
			want: "section_end:1714557605:prepare\nsection_start:1714557605:step_script\n\x1b[36;1mExecuting\x1b[0;m",
		},
		{
			name: "output without a final newline stays before the end marker",
			raw:  "2026-06-26T22:39:47.158568Z 00O last words\x1b[0Ksection_end:1782513590:step_script\r\x1b[0K",
			want: "2026-06-26T22:39:47.158568Z last words\nsection_end:1782513590:step_script",
		},
		{
			name: "carriage-return progress overwrite keeps the final frame",
			raw:  "Downloading  10%\rDownloading  60%\rDownloading 100%",
			want: "Downloading 100%",
		},
		{
			name: "blank lines and plain lines pass through",
			raw:  "line one\n\nline two",
			want: "line one\n\nline two",
		},
		{
			name: "erase-in-line escapes are stripped",
			raw:  "\x1b[0Kfoo \x1b[Kbar",
			want: "foo bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanGitLabTrace(tt.raw); got != tt.want {
				t.Fatalf("cleanGitLabTrace = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetCIJobLogValidatesTheJobID(t *testing.T) {
	t.Parallel()
	if _, err := NewCore().ForgeByID("gitlab").GetCIJobLog(t.Context(), testPRRef("g/r", 1), CIJobLogRequest{JobID: "abc"}); err == nil {
		t.Fatal("expected error for non-numeric job id (gitlab)")
	}
}
