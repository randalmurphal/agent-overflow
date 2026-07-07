package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeCIStatus(t *testing.T) {
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
	checks := []CheckStatus{
		{Kind: "CheckRun", Name: "build", Workflow: "CI", DetailsURL: "https://github.com/o/r/actions/runs/111/job/901"},
		{Kind: "CheckRun", Name: "lint", Workflow: "CI", DetailsURL: "https://github.com/o/r/actions/runs/111/job/902"},
		{Kind: "CheckRun", Name: "scan", Workflow: "Code Scanning", DetailsURL: "https://github.com/o/r/actions/runs/222/job/903"},
		{Kind: "CheckRun", Name: "vendor-bot", DetailsURL: "https://vendor.example/checks/1", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Kind: "StatusContext", Name: "codecov/patch", Status: "SUCCESS", DetailsURL: "https://codecov.example/x"},
	}
	runIDs, external := splitGitHubChecks(checks)
	if strings.Join(runIDs, ",") != "111,222" {
		t.Fatalf("runIDs = %v, want [111 222]", runIDs)
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

func TestGitLabListPRCIJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	// Dispatch on the endpoint argument: MR view vs pipeline jobs.
	script := `#!/bin/sh
case "$2" in
*pipelines/77/jobs*)
  echo '[{"id":20,"name":"unit","stage":"test","status":"failed","duration":10.0,"web_url":"https://gl/j/20","allow_failure":false,"started_at":"2026-07-06T19:11:33Z"},{"id":10,"name":"compile","stage":"build","status":"success","duration":5.0,"web_url":"https://gl/j/10","allow_failure":false,"started_at":"2026-07-06T19:10:33Z"}]'
  ;;
*merge_requests/12*)
  echo '{"iid":12,"head_pipeline":{"id":77,"status":"failed","web_url":"https://gl/p/77"}}'
  ;;
*)
  echo "unexpected endpoint $2" 1>&2
  exit 1
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "glab"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("gitlab").ListPRCIJobs(t.TempDir(), "group/repo", 12)
	if err != nil {
		t.Fatalf("ListPRCIJobs returned error: %v", err)
	}
	if pipeline.Status != CIStatusFailed {
		t.Fatalf("pipeline.Status = %q, want failed", pipeline.Status)
	}
	if pipeline.URL != "https://gl/p/77" {
		t.Fatalf("pipeline.URL = %q", pipeline.URL)
	}
	if len(pipeline.Stages) != 2 || pipeline.Stages[0].Name != "build" || pipeline.Stages[1].Name != "test" {
		t.Fatalf("stages = %+v", pipeline.Stages)
	}
}

func TestGitLabListPRCIJobsNoPipeline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock glab is unix-only")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\necho '{\"iid\":12,\"head_pipeline\":null}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "glab"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock glab: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("gitlab").ListPRCIJobs(t.TempDir(), "group/repo", 12)
	if err != nil {
		t.Fatalf("ListPRCIJobs returned error: %v", err)
	}
	if pipeline.Status != "" || len(pipeline.Stages) != 0 {
		t.Fatalf("expected empty pipeline, got %+v", pipeline)
	}
}

func TestGitHubListPRCIJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
pr)
  echo '{"statusCheckRollup":[{"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/111/job/901"},{"__typename":"StatusContext","context":"codecov/patch","state":"SUCCESS","targetUrl":"https://codecov.example/x"}]}'
  ;;
run)
  echo '{"workflowName":"CI","jobs":[{"databaseId":901,"name":"build","status":"completed","conclusion":"failure","startedAt":"2026-07-06T14:08:19Z","completedAt":"2026-07-06T14:08:47Z","url":"https://github.com/o/r/actions/runs/111/job/901","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Build","status":"completed","conclusion":"failure"}]}]}'
  ;;
*)
  echo "unexpected subcommand $1" 1>&2
  exit 1
  ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("github").ListPRCIJobs(t.TempDir(), "o/r", 5)
	if err != nil {
		t.Fatalf("ListPRCIJobs returned error: %v", err)
	}
	if pipeline.Status != CIStatusFailed {
		t.Fatalf("pipeline.Status = %q, want failed", pipeline.Status)
	}
	if len(pipeline.Stages) != 2 {
		t.Fatalf("stages = %+v, want CI + External", pipeline.Stages)
	}
	ci := pipeline.Stages[0]
	if ci.Name != "CI" || ci.Status != CIStatusFailed || len(ci.Jobs) != 1 {
		t.Fatalf("CI stage = %+v", ci)
	}
	job := ci.Jobs[0]
	if job.ID != "901" || !job.LogsAvailable || job.DurationSeconds != 28 {
		t.Fatalf("job = %+v", job)
	}
	if len(job.Steps) != 2 || job.Steps[1].Status != CIStatusFailed {
		t.Fatalf("steps = %+v", job.Steps)
	}
	ext := pipeline.Stages[1]
	if ext.Name != githubCIExternalStage || len(ext.Jobs) != 1 || ext.Jobs[0].LogsAvailable {
		t.Fatalf("external stage = %+v", ext)
	}
}

func TestCleanGitLabTrace(t *testing.T) {
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
			name: "marker-only timestamped line vanishes",
			raw: "2026-06-26T22:39:47.158568Z 00O+\x1b[0Ksection_start:1782513587:upload_artifacts_on_success\n" +
				"2026-06-26T22:39:47.158568Z 00O \x1b[0;33mUploading artifacts\x1b[0;m",
			want: "2026-06-26T22:39:47.158568Z \x1b[0;33mUploading artifacts\x1b[0;m",
		},
		{
			name: "inline section marker with CR-erased header survives",
			raw:  "section_start:1714557600:step_script\r\x1b[0K\x1b[36;1mRunning steps\x1b[0;m",
			want: "\x1b[36;1mRunning steps\x1b[0;m",
		},
		{
			name: "section marker with options",
			raw:  "section_start:1714557600:cleanup[collapsed=true]\r\x1b[0Kdone",
			want: "done",
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

func TestGetCIJobLogStripsBOMAndValidatesID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\nprintf '\\357\\273\\277log line one\\n'\n"
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write mock gh: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	log, err := core.ForgeByID("github").GetCIJobLog(t.TempDir(), "o/r", "901")
	if err != nil {
		t.Fatalf("GetCIJobLog returned error: %v", err)
	}
	if log != "log line one\n" {
		t.Fatalf("log = %q, want BOM stripped", log)
	}

	if _, err := core.ForgeByID("github").GetCIJobLog(t.TempDir(), "o/r", "901/logs"); err == nil {
		t.Fatal("expected error for non-numeric job id")
	}
	if _, err := core.ForgeByID("gitlab").GetCIJobLog(t.TempDir(), "g/r", "abc"); err == nil {
		t.Fatal("expected error for non-numeric job id (gitlab)")
	}
}
