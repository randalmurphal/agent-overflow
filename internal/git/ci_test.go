package git

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"agent-overflow/internal/testutil/mockexec"
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
	mockexec.Write(t, filepath.Join(binDir, "glab"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("gitlab").ListPRCIJobs(t.TempDir(), "group/repo", 12, nil, nil)
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
	mockexec.Write(t, filepath.Join(binDir, "glab"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("gitlab").ListPRCIJobs(t.TempDir(), "group/repo", 12, nil, nil)
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
  echo '{"statusCheckRollup":[{"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/111/job/901","startedAt":"2026-07-06T14:08:19Z","completedAt":"2026-07-06T14:08:47Z"},{"__typename":"StatusContext","context":"codecov/patch","state":"SUCCESS","targetUrl":"https://codecov.example/x"}]}'
  ;;
api)
  echo '{"total_count":1,"jobs":[{"id":901,"run_id":111,"name":"build","status":"completed","conclusion":"failure","started_at":"2026-07-06T14:08:19Z","completed_at":"2026-07-06T14:08:47Z","html_url":"https://github.com/o/r/actions/runs/111/job/901","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Build","status":"completed","conclusion":"failure"}]}]}'
  ;;
*)
  echo "unexpected subcommand $1" 1>&2
  exit 1
  ;;
esac
`
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	core := NewCore()
	pipeline, err := core.ForgeByID("github").ListPRCIJobs(t.TempDir(), "o/r", 5, nil, []string{"901"})
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
	if len(job.Steps) != 2 || job.Steps[0].Number != 1 || job.Steps[0].Name != "Set up job" || job.Steps[0].Status != CIStatusSuccess || job.Steps[1].Status != CIStatusFailed {
		t.Fatalf("steps = %+v", job.Steps)
	}
	ext := pipeline.Stages[1]
	if ext.Name != githubCIExternalStage || len(ext.Jobs) != 1 || ext.Jobs[0].LogsAvailable {
		t.Fatalf("external stage = %+v", ext)
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
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
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

// githubStepsMock is a gh that answers the rollup from the file rollup
// names and a run's REST jobs list from jobsDir/<run id>.json, recording
// every api path it is asked for.
func githubStepsMock(t *testing.T) (rollup, jobsDir string, apiCalls func() []string) {
	t.Helper()
	dir := t.TempDir()
	rollup = filepath.Join(dir, "rollup.json")
	jobsDir = filepath.Join(dir, "jobs")
	calls := filepath.Join(dir, "api-calls")
	if err := os.Mkdir(jobsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
pr)
  cat "` + rollup + `"
  ;;
api)
  echo "$2" >> "` + calls + `"
  run=$(echo "$2" | sed -n 's|^repos/o/r/actions/runs/\([0-9]*\)/jobs?per_page=100$|\1|p')
  if [ -z "$run" ] || [ ! -f "` + jobsDir + `/$run.json" ]; then
    echo '{"message":"Not Found"}'
    echo "gh: Not Found (HTTP 404)" 1>&2
    exit 1
  fi
  cat "` + jobsDir + `/$run.json"
  ;;
*)
  echo "unexpected subcommand $1" 1>&2
  exit 1
  ;;
esac
`
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return rollup, jobsDir, func() []string {
		data, _ := os.ReadFile(calls)
		return strings.Fields(string(data))
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGitHubListPRCIJobsReadsStepsOnlyForFollowedJobs: the rollup lists
// every job with its status and duration; steps cost one REST jobs list
// per run holding a followed job whose steps can still change, and land
// on the followed job alone. Once that job completed and the previous
// observation holds its settled steps, they are reused with no call; a
// re-run (the check back in progress) reads them again.
func TestGitHubListPRCIJobsReadsStepsOnlyForFollowedJobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	rollup, jobsDir, apiCalls := githubStepsMock(t)
	const runningRollup = `{"statusCheckRollup":[
{"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/111/job/901","startedAt":"2026-07-06T14:08:19Z","completedAt":"2026-07-06T14:08:47Z"},
{"__typename":"CheckRun","name":"lint","workflowName":"Lint","status":"IN_PROGRESS","conclusion":"","detailsUrl":"https://github.com/o/r/actions/runs/222/job/902","startedAt":"2026-07-06T14:08:19Z","completedAt":"0001-01-01T00:00:00Z"},
{"__typename":"CheckRun","name":"vet","workflowName":"Lint","status":"QUEUED","conclusion":"","detailsUrl":"https://github.com/o/r/actions/runs/222/job/903","startedAt":"0001-01-01T00:00:00Z","completedAt":"0001-01-01T00:00:00Z"}]}`
	const doneRollup = `{"statusCheckRollup":[
{"__typename":"CheckRun","name":"build","workflowName":"CI","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/111/job/901","startedAt":"2026-07-06T14:08:19Z","completedAt":"2026-07-06T14:08:47Z"},
{"__typename":"CheckRun","name":"lint","workflowName":"Lint","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://github.com/o/r/actions/runs/222/job/902","startedAt":"2026-07-06T14:08:19Z","completedAt":"2026-07-06T14:09:19Z"},
{"__typename":"CheckRun","name":"vet","workflowName":"Lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/222/job/903","startedAt":"2026-07-06T14:09:19Z","completedAt":"2026-07-06T14:09:29Z"}]}`
	const runningJobs = `{"total_count":2,"jobs":[
{"id":902,"name":"lint","status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Lint","status":"in_progress","conclusion":null}]},
{"id":903,"name":"vet","status":"queued","conclusion":null,"steps":[]}]}`
	const doneJobs = `{"total_count":2,"jobs":[
{"id":902,"name":"lint","status":"completed","conclusion":"failure","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Lint","status":"completed","conclusion":"failure"}]},
{"id":903,"name":"vet","status":"completed","conclusion":"success","steps":[{"number":1,"name":"Vet","status":"completed","conclusion":"success"}]}]}`
	writeTestFile(t, rollup, runningRollup)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), runningJobs)
	forge := NewCore().ForgeByID("github")
	list := func(prev *CIPipeline, stepsFor ...string) CIPipeline {
		t.Helper()
		pipeline, err := forge.ListPRCIJobs(t.TempDir(), "o/r", 5, prev, stepsFor)
		if err != nil {
			t.Fatalf("ListPRCIJobs: %v", err)
		}
		return pipeline
	}

	// Nothing followed: the rollup alone, with statuses and durations.
	plain := list(nil)
	if got := apiCalls(); len(got) != 0 {
		t.Fatalf("an unfollowed pipeline read %v", got)
	}
	if len(plain.Stages) != 2 || plain.Stages[0].Name != "CI" || plain.Stages[1].Name != "Lint" || plain.Status != CIStatusRunning {
		t.Fatalf("stages = %+v", plain.Stages)
	}
	if build := plain.Stages[0].Jobs[0]; build.Status != CIStatusSuccess || build.DurationSeconds != 28 || build.Steps != nil {
		t.Fatalf("build = %+v", build)
	}
	if lint, vet := plain.Stages[1].Jobs[0], plain.Stages[1].Jobs[1]; lint.Status != CIStatusRunning || lint.DurationSeconds != 0 || vet.Status != CIStatusPending || vet.LogsAvailable {
		t.Fatalf("lint = %+v, vet = %+v", lint, vet)
	}

	// A followed running job: one jobs list for its run, steps on it alone.
	first := list(&plain, "902")
	if got := apiCalls(); len(got) != 1 || got[0] != "repos/o/r/actions/runs/222/jobs?per_page=100" {
		t.Fatalf("api calls = %v, want one jobs list of run 222", got)
	}
	lint := FindCIJob(first, "902")
	if len(lint.Steps) != 2 || lint.Steps[1].Name != "Lint" || lint.Steps[1].Status != CIStatusRunning {
		t.Fatalf("lint steps = %+v", lint.Steps)
	}
	if FindCIJob(first, "903").Steps != nil || FindCIJob(first, "901").Steps != nil {
		t.Fatalf("an unfollowed job carries steps: %+v", first.Stages)
	}

	// Still running: its steps can change, so they are read again.
	second := list(&first, "902")
	if got := apiCalls(); len(got) != 2 {
		t.Fatalf("api calls = %v, want a second read while the job runs", got)
	}

	// The job completes: read once more, since prev saw it running.
	writeTestFile(t, rollup, doneRollup)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), doneJobs)
	third := list(&second, "902")
	if got := apiCalls(); len(got) != 3 {
		t.Fatalf("api calls = %v, want a read for the completion", got)
	}
	if lint := FindCIJob(third, "902"); lint.Status != CIStatusFailed || lint.DurationSeconds != 60 || lint.Steps[1].Status != CIStatusFailed {
		t.Fatalf("completed lint = %+v", lint)
	}

	// Completed and settled in prev: reused, no call.
	fourth := list(&third, "902")
	if got := apiCalls(); len(got) != 3 {
		t.Fatalf("api calls = %v, want settled steps reused", got)
	}
	if lint := FindCIJob(fourth, "902"); len(lint.Steps) != 2 || lint.Steps[1].Status != CIStatusFailed {
		t.Fatalf("reused lint steps = %+v", lint.Steps)
	}

	// A re-run puts the check back in progress: read again.
	writeTestFile(t, rollup, runningRollup)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), runningJobs)
	rerun := list(&fourth, "902")
	if got := apiCalls(); len(got) != 4 {
		t.Fatalf("api calls = %v, want a read for the re-run", got)
	}
	if lint := FindCIJob(rerun, "902"); lint.Steps[1].Status != CIStatusRunning {
		t.Fatalf("re-run lint steps = %+v", lint.Steps)
	}
}

// TestGitHubListPRCIJobsRereadsStepsReadWhileTheJobRan: steps read while
// the job ran are not reused once it completes, even when every step read
// then had finished (a job between steps lists only those it started).
func TestGitHubListPRCIJobsRereadsStepsReadWhileTheJobRan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	rollup, jobsDir, apiCalls := githubStepsMock(t)
	writeTestFile(t, rollup, `{"statusCheckRollup":[{"__typename":"CheckRun","name":"lint","workflowName":"Lint","status":"IN_PROGRESS","detailsUrl":"https://github.com/o/r/actions/runs/222/job/902"}]}`)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), `{"total_count":1,"jobs":[{"id":902,"status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"}]}]}`)
	forge := NewCore().ForgeByID("github")
	first, err := forge.ListPRCIJobs(t.TempDir(), "o/r", 5, nil, []string{"902"})
	if err != nil {
		t.Fatalf("first ListPRCIJobs: %v", err)
	}
	writeTestFile(t, rollup, `{"statusCheckRollup":[{"__typename":"CheckRun","name":"lint","workflowName":"Lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/222/job/902"}]}`)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), `{"total_count":1,"jobs":[{"id":902,"status":"completed","conclusion":"success","steps":[{"number":1,"name":"Set up job","status":"completed","conclusion":"success"},{"number":2,"name":"Lint","status":"completed","conclusion":"success"}]}]}`)
	second, err := forge.ListPRCIJobs(t.TempDir(), "o/r", 5, &first, []string{"902"})
	if err != nil {
		t.Fatalf("second ListPRCIJobs: %v", err)
	}
	if got := apiCalls(); len(got) != 2 {
		t.Fatalf("api calls = %v, want the completed job's steps read again", got)
	}
	if steps := FindCIJob(second, "902").Steps; len(steps) != 2 {
		t.Fatalf("steps = %+v, want both", steps)
	}
}

// TestGitHubListPRCIJobsRereadsStepsTheJobsListLeftLive: the jobs list can
// trail the rollup, so steps read live for a job the rollup already calls
// complete are not reused.
func TestGitHubListPRCIJobsRereadsStepsTheJobsListLeftLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	rollup, jobsDir, apiCalls := githubStepsMock(t)
	writeTestFile(t, rollup, `{"statusCheckRollup":[{"__typename":"CheckRun","name":"lint","workflowName":"Lint","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://github.com/o/r/actions/runs/222/job/902"}]}`)
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), `{"total_count":1,"jobs":[{"id":902,"status":"in_progress","conclusion":null,"steps":[{"number":1,"name":"Lint","status":"in_progress","conclusion":null}]}]}`)
	forge := NewCore().ForgeByID("github")
	first, err := forge.ListPRCIJobs(t.TempDir(), "o/r", 5, nil, []string{"902"})
	if err != nil {
		t.Fatalf("first ListPRCIJobs: %v", err)
	}
	writeTestFile(t, filepath.Join(jobsDir, "222.json"), `{"total_count":1,"jobs":[{"id":902,"status":"completed","conclusion":"success","steps":[{"number":1,"name":"Lint","status":"completed","conclusion":"success"}]}]}`)
	second, err := forge.ListPRCIJobs(t.TempDir(), "o/r", 5, &first, []string{"902"})
	if err != nil {
		t.Fatalf("second ListPRCIJobs: %v", err)
	}
	if got := apiCalls(); len(got) != 2 {
		t.Fatalf("api calls = %v, want the trailing steps read again", got)
	}
	if steps := FindCIJob(second, "902").Steps; len(steps) != 1 || steps[0].Status != CIStatusSuccess {
		t.Fatalf("steps = %+v", steps)
	}
}

// TestGitHubListPRCIJobsPagesToTheFollowedJob: a run past 100 jobs is read
// page by page until the followed job turns up.
func TestGitHubListPRCIJobsPagesToTheFollowedJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock gh is unix-only")
	}
	binDir := t.TempDir()
	calls := filepath.Join(t.TempDir(), "api-calls")
	var page1 strings.Builder
	page1.WriteString(`{"total_count":101,"jobs":[`)
	for i := range 100 {
		if i > 0 {
			page1.WriteString(",")
		}
		page1.WriteString(`{"id":` + strconv.Itoa(1000+i) + `,"steps":[]}`)
	}
	page1.WriteString(`]}`)
	script := `#!/bin/sh
case "$1" in
pr)
  echo '{"statusCheckRollup":[{"__typename":"CheckRun","name":"last","workflowName":"Matrix","status":"IN_PROGRESS","detailsUrl":"https://github.com/o/r/actions/runs/333/job/2000"}]}'
  ;;
api)
  echo "$2" >> "` + calls + `"
  case "$2" in
  *page=2) echo '{"total_count":101,"jobs":[{"id":2000,"steps":[{"number":1,"name":"Run","status":"in_progress"}]}]}' ;;
  *) echo '` + page1.String() + `' ;;
  esac
  ;;
esac
`
	mockexec.Write(t, filepath.Join(binDir, "gh"), script)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	pipeline, err := NewCore().ForgeByID("github").ListPRCIJobs(t.TempDir(), "o/r", 5, nil, []string{"2000"})
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	data, _ := os.ReadFile(calls)
	if got := strings.Fields(string(data)); len(got) != 2 || got[1] != "repos/o/r/actions/runs/333/jobs?per_page=100&page=2" {
		t.Fatalf("api calls = %v, want two pages", got)
	}
	if steps := FindCIJob(pipeline, "2000").Steps; len(steps) != 1 || steps[0].Status != CIStatusRunning {
		t.Fatalf("steps = %+v", steps)
	}
}

// TestGetCIJobLogNotFoundIsTyped: a job log the forge answers 404 for is
// ErrCIJobLogNotFound on both forges, carrying the forge's message; any
// other failure is not, and an authentication failure stays a setup error.
func TestGetCIJobLogNotFoundIsTyped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script mock CLIs are unix-only")
	}
	cases := []struct {
		forge, cli, stdout, stderr string
		notFound, setup            bool
	}{
		{"github", "gh", `{"message":"Not Found","status":"404"}`, "gh: Not Found (HTTP 404)", true, false},
		{"gitlab", "glab", `{"message":"404 Job Not Found"}`, "glab: 404 Not found (HTTP 404)", true, false},
		{"github", "gh", `{"message":"Server Error"}`, "gh: Server Error (HTTP 500)", false, false},
		{"gitlab", "glab", "", "glab: 500 Internal Server Error (HTTP 500)", false, false},
		{"github", "gh", "", "To get started with GitHub CLI, please run:  gh auth login", false, true},
	}
	for _, tc := range cases {
		binDir := t.TempDir()
		script := "#!/bin/sh\necho '" + tc.stdout + "'\necho '" + tc.stderr + "' 1>&2\nexit 1\n"
		mockexec.Write(t, filepath.Join(binDir, tc.cli), script)
		t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		_, err := NewCore().ForgeByID(tc.forge).GetCIJobLog(t.TempDir(), "o/r", "901")
		if err == nil {
			t.Fatalf("%s %q: no error", tc.forge, tc.stderr)
		}
		if got := errors.Is(err, ErrCIJobLogNotFound); got != tc.notFound {
			t.Errorf("%s %q: errors.Is(ErrCIJobLogNotFound) = %v, want %v (%v)", tc.forge, tc.stderr, got, tc.notFound, err)
		}
		if _, setup := errors.AsType[*ForgeSetupError](err); setup != tc.setup {
			t.Errorf("%s %q: setup error = %v, want %v (%v)", tc.forge, tc.stderr, setup, tc.setup, err)
		}
		if !tc.setup && !strings.Contains(err.Error(), strings.TrimSpace(tc.stderr)) {
			t.Errorf("%s: error %q lost the forge's message %q", tc.forge, err, tc.stderr)
		}
	}
}
