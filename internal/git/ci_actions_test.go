package git

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"agent-overflow/internal/forgeapi"
)

// githubActionsRollup is a PR head's rollup across two workflow runs: run
// 111 completed with a failed and a passing job, run 222 still running,
// and an external status.
func githubActionsRollup() string {
	return strings.Join([]string{
		githubCheckRun("unit", "CI", "COMPLETED", "FAILURE", "111", "901", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z"),
		githubCheckRun("lint", "CI", "COMPLETED", "SUCCESS", "111", "902", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z"),
		githubCheckRun("deploy", "Deploy", "IN_PROGRESS", "", "222", "903", "2026-07-06T14:02:00Z", ""),
		githubCheckRun("smoke", "Deploy", "COMPLETED", "FAILURE", "222", "907", "2026-07-06T14:02:00Z", "2026-07-06T14:03:00Z"),
		`{"__typename":"StatusContext","context":"codecov/patch","state":"FAILURE","targetUrl":"https://codecov.example/x","createdAt":"2026-07-06T14:08:19Z"}`,
	}, ",")
}

// githubActionsCore answers PRTick with rollup and viewerPermission
// permission, and each run's artifacts list from artifacts (none when
// absent); every request is recorded.
func githubActionsCore(t *testing.T, rollup, permission string, artifacts map[string]string) (*Core, *forgeAPICalls) {
	t.Helper()
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Op == "PRTick" {
			return githubData(`{"repository":{"viewerPermission":` + permission + `,"pullRequest":{"number":5,"headRefName":"feature","commits":{"nodes":[{"commit":{"id":"C","oid":"abc","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + rollup + `]}}}}]}}}}`)
		}
		path, _, _ := strings.Cut(call.Path, "?")
		if run, ok := strings.CutSuffix(strings.TrimPrefix(path, "repos/o/r/actions/runs/"), "/artifacts"); ok && call.Method == http.MethodGet {
			if body, ok := artifacts[run]; ok {
				return forgeAPIAnswer{Body: body}
			}
			return forgeAPIAnswer{Body: `{"total_count":0,"artifacts":[]}`}
		}
		return forgeUnexpected(t, call)
	})
}

// The capability flags come from the read the pump already makes: a
// failed job of a completed run is retryable, a job of a live run is not
// (GitHub re-runs a job of a completed run only), the live run makes the
// pipeline cancelable and the completed one's failure retryable; an
// external check carries nothing; a viewer below write is denied each.
// Artifacts are read once per completed run and job set.
func TestGitHubCIActionFlags(t *testing.T) {
	t.Parallel()
	core, calls := githubActionsCore(t, githubActionsRollup(), `"WRITE"`, map[string]string{
		"111": `{"total_count":1,"artifacts":[{"id":5,"name":"coverage","size_in_bytes":10,"expired":false}]}`,
	})
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil {
		t.Fatalf("ListPRCIJobs: %v", err)
	}
	unit, lint, deploy := FindCIJob(pipeline, "901"), FindCIJob(pipeline, "902"), FindCIJob(pipeline, "903")
	if unit.RunID != "111" || unit.Actions != (CIJobActions{Retry: CIActionAllowed}) {
		t.Fatalf("failed job of a completed run = %+v", unit)
	}
	if lint.Actions != (CIJobActions{}) || deploy.Actions != (CIJobActions{}) {
		t.Fatalf("passing and running jobs = %+v / %+v, want no action", lint.Actions, deploy.Actions)
	}
	if smoke := FindCIJob(pipeline, "907"); smoke.Actions != (CIJobActions{}) {
		t.Fatalf("a failed job of a live run = %+v, want no retry until the run completes", smoke.Actions)
	}
	external := pipeline.Stages[len(pipeline.Stages)-1].Jobs[0]
	if external.ID != "" || external.Actions != (CIJobActions{}) {
		t.Fatalf("external check = %+v", external)
	}
	if want := (CIPipelineActions{RetryFailed: CIActionAllowed, Cancel: CIActionAllowed, Artifacts: CIActionAllowed}); pipeline.Actions != want {
		t.Fatalf("pipeline actions = %+v, want %+v", pipeline.Actions, want)
	}
	if got := fmt.Sprint(withoutPRTick(calls.ops())); got != "[GET repos/o/r/actions/runs/111/artifacts?per_page=100]" {
		t.Fatalf("requests = %s, want the completed run's artifacts alone", got)
	}
	// The same runs and jobs read again cost no artifacts read.
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), &pipeline, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(withoutPRTick(calls.ops())); got != 1 {
		t.Fatalf("artifacts reads after a second tick = %d, want 1", got)
	}

	reader, _ := githubActionsCore(t, githubActionsRollup(), `"READ"`, nil)
	denied, err := reader.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if FindCIJob(denied, "901").Actions.Retry != CIActionDenied || denied.Actions != (CIPipelineActions{RetryFailed: CIActionDenied, Cancel: CIActionDenied}) {
		t.Fatalf("a reader's actions = %+v / %+v", FindCIJob(denied, "901").Actions, denied.Actions)
	}
}

func withoutPRTick(ops []string) []string {
	var out []string
	for _, op := range ops {
		if op != "PRTick" {
			out = append(out, op)
		}
	}
	return out
}

// A run re-run with new job ids is a new job set, whose artifacts are
// read again.
func TestGitHubCIArtifactsReadAgainForANewAttempt(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	rollup := githubCheckRun("unit", "CI", "COMPLETED", "FAILURE", "111", "901", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z")
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		if call.Op == "PRTick" {
			return githubData(`{"repository":{"viewerPermission":"WRITE","pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + rollup + `]}}}}]}}}}`)
		}
		return forgeAPIAnswer{Body: `{"total_count":0,"artifacts":[]}`}
	})
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	rollup = githubCheckRun("unit", "CI", "COMPLETED", "SUCCESS", "111", "905", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z")
	mu.Unlock()
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(withoutPRTick(calls.ops())); got != 2 {
		t.Fatalf("artifacts reads = %d, want one per attempt", got)
	}
}

// githubActionsPipeline is a PR pipeline over runs 111 (completed, a
// failure), 222 (running) and 333 (completed, a cancel).
func githubActionsPipeline() CIPipeline {
	return CIPipeline{Stages: []CIStage{{Name: "CI", Jobs: []CIJob{
		{ID: "901", RunID: "111", Status: CIStatusFailed},
		{ID: "903", RunID: "222", Status: CIStatusRunning},
		{ID: "904", RunID: "333", Status: CIStatusCanceled},
		{ID: "905", RunID: "333", Status: CIStatusSuccess},
	}}}}
}

// Each GitHub action is the documented request: a job re-run, the
// failed jobs of each completed run with a failure, the cancel of each
// live run.
func TestGitHubCIActionRequests(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Method != http.MethodPost || len(call.Body) != 0 {
			return forgeUnexpected(t, call)
		}
		if strings.HasSuffix(call.Path, "/cancel") {
			return forgeAPIAnswer{Status: http.StatusAccepted, Body: `{}`}
		}
		return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
	})
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	for _, step := range []struct {
		kind CIActionKind
		job  string
	}{{CIActionRetryJob, "901"}, {CIActionRetryFailed, ""}, {CIActionCancelPipeline, ""}} {
		if err := core.RunCIAction(t.Context(), repo, step.kind, step.job, githubActionsPipeline()); err != nil {
			t.Fatalf("%s: %v", step.kind, err)
		}
	}
	want := "[POST repos/o/r/actions/jobs/901/rerun POST repos/o/r/actions/runs/111/rerun-failed-jobs POST repos/o/r/actions/runs/333/rerun-failed-jobs POST repos/o/r/actions/runs/222/cancel]"
	if got := fmt.Sprint(calls.ops()); got != want {
		t.Fatalf("requests = %s\nwant %s", got, want)
	}
	if err := core.RunCIAction(t.Context(), repo, CIActionCancelJob, "903", githubActionsPipeline()); err == nil {
		t.Fatal("a GitHub job cancel was sent")
	}
}

// A pipeline action some runs refuse after others took it is one partial
// failure, worded with the refusal; when every run refuses, the first
// refusal is the answer. A 409 on a cancel is a refusal of the cancel,
// not a moved head.
func TestGitHubCIPartialFailure(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Path == "repos/o/r/actions/runs/333/rerun-failed-jobs" {
			return forgeAPIAnswer{Status: http.StatusConflict, Body: `{"message":"This workflow run cannot be rerun."}`}
		}
		return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
	})
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	err := core.RunCIAction(t.Context(), repo, CIActionRetryFailed, "", githubActionsPipeline())
	var partial *CIPartialError
	if !errors.As(err, &partial) || partial.Done != 1 || partial.Total != 2 {
		t.Fatalf("err = %v, want 1 of 2 runs", err)
	}
	explained := ExplainCIError(repo, "the checks of o/r#5", ActionRetryFailed, err)
	var sentence *ForgeActionError
	if !errors.As(explained, &sentence) || sentence.Message != "Of the 2 workflow runs of the checks of o/r#5 asked to retry the failed jobs, GitHub took it on 1 and refused it on 1: The checks of o/r#5 can no longer be retried. Refresh to see its current state." {
		t.Fatalf("sentence = %v", explained)
	}

	refused, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusConflict, Body: `{"message":"Cannot cancel a workflow run that is completed."}`}
	})
	pipeline := githubActionsPipeline()
	pipeline.Stages[0].Jobs = append(pipeline.Stages[0].Jobs, CIJob{ID: "906", RunID: "444", Status: CIStatusPending})
	err = refused.RunCIAction(t.Context(), repo, CIActionCancelPipeline, "", pipeline)
	if errors.As(err, &partial) {
		t.Fatalf("every run refused, got a partial failure %v", err)
	}
	if !errors.As(ExplainCIError(repo, "pipeline 222 of o/r", ActionCancelCI, err), &sentence) || sentence.Kind != ForgeErrorRefused ||
		sentence.Message != "GitHub refused to cancel pipeline 222 of o/r: Cannot cancel a workflow run that is completed" {
		t.Fatalf("409 sentence = %+v", sentence)
	}
}

// Every GitLab action is a bodiless POST to its documented path.
func TestGitLabCIActionRequests(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		if call.Method != http.MethodPost || len(call.Body) != 0 {
			return forgeUnexpected(t, call)
		}
		return forgeAPIAnswer{Status: http.StatusCreated, Body: `{"id":1}`}
	})
	repo := gitlabTestRepo()
	pipeline := CIPipeline{ID: "77"}
	for _, step := range []struct {
		kind CIActionKind
		job  string
	}{{CIActionRetryJob, "11"}, {CIActionCancelJob, "12"}, {CIActionPlayJob, "13"}, {CIActionRetryFailed, ""}, {CIActionCancelPipeline, ""}} {
		if err := core.RunCIAction(t.Context(), repo, step.kind, step.job, pipeline); err != nil {
			t.Fatalf("%s: %v", step.kind, err)
		}
	}
	p := "projects/grp%2Fsub%2Fapp"
	want := fmt.Sprintf("[POST %[1]s/jobs/11/retry POST %[1]s/jobs/12/cancel POST %[1]s/jobs/13/play POST %[1]s/pipelines/77/retry POST %[1]s/pipelines/77/cancel]", p)
	if got := fmt.Sprint(calls.ops()); got != want {
		t.Fatalf("requests = %s\nwant %s", got, want)
	}
	if err := core.RunCIAction(t.Context(), repo, CIActionRetryJob, "../1", pipeline); err == nil {
		t.Fatal("a job id that is not a number reached the forge")
	}
	if err := core.RunCIAction(t.Context(), repo, CIActionRetryFailed, "", CIPipeline{}); err == nil {
		t.Fatal("a GitLab pipeline action without the pipeline id was sent")
	}
}

// GitLab's own answer decides each flag: a failed retryable build, a
// running cancelable one, a manual job the viewer may not play, a build
// with its archive, a bridge (nothing), and the pipeline's retryable
// and cancelable with the viewer's permissions.
func TestGitLabCIActionFlags(t *testing.T) {
	t.Parallel()
	perms := func(update, cancel, artifacts bool) string {
		return fmt.Sprintf(`"userPermissions":{"updateBuild":%t,"cancelBuild":%t,"readJobArtifacts":%t}`, update, cancel, artifacts)
	}
	started := `"startedAt":"2026-10-20T10:00:00Z"`
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "projects/grp%2Fsub%2Fapp/pipelines/77":
			return forgeAPIAnswer{Body: `{"id":77,"status":"running","ref":"main","sha":"abc","web_url":"https://gitlab.example/p/77"}`}
		case call.Op == "PipelineJobs":
			page := gitlabJobsPage([]string{"test", "deploy"}, "",
				gitlabJobNode("Ci::Build", 11, "unit", "test", "FAILED", started+`,"retryable":true,`+perms(true, true, true)+`,"artifacts":{"nodes":[{"fileType":"TRACE"},{"fileType":"ARCHIVE"}]}`),
				gitlabJobNode("Ci::Build", 12, "lint", "test", "RUNNING", started+`,"cancelable":true,`+perms(false, true, true)+`,"artifacts":{"nodes":[{"fileType":"TRACE"}]}`),
				gitlabJobNode("Ci::Build", 13, "release", "deploy", "MANUAL", `"playable":true,"canPlayJob":false,`+perms(true, true, true)+`,"artifacts":{"nodes":[]}`),
				gitlabJobNode("Ci::Bridge", 14, "child", "deploy", "FAILED", `"retryable":true,`+perms(true, true, true)+`,"artifacts":null`),
			)
			body := strings.Replace(page.Body, `"pipeline":{`, `"pipeline":{"retryable":true,"cancelable":true,"userPermissions":{"updatePipeline":false,"cancelPipeline":true},`, 1)
			return forgeAPIAnswer{Body: body}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "77", nil, nil)
	if err != nil {
		t.Fatalf("ReadPipeline: %v", err)
	}
	for id, want := range map[string]CIJobActions{
		"11": {Retry: CIActionAllowed, Artifacts: CIActionAllowed},
		"12": {Cancel: CIActionAllowed},
		"13": {Play: CIActionDenied},
	} {
		if got := FindCIJob(pipeline, id).Actions; got != want {
			t.Errorf("job %s actions = %+v, want %+v", id, got, want)
		}
	}
	for _, stage := range pipeline.Stages {
		for _, job := range stage.Jobs {
			if job.ID == "" && job.Actions != (CIJobActions{}) {
				t.Errorf("bridge %s carries actions %+v", job.Name, job.Actions)
			}
		}
	}
	// Running: cancelable, and not yet retryable though GitLab says so.
	if pipeline.Actions != (CIPipelineActions{Cancel: CIActionAllowed}) {
		t.Fatalf("pipeline actions = %+v", pipeline.Actions)
	}
	if got := gitlabPipelineActions(CIPipeline{Status: CIStatusFailed, Stages: []CIStage{{Jobs: []CIJob{{ID: "1", Status: CIStatusFailed}}}}}, gitlabCIPipelineNode{Retryable: true}); got != (CIPipelineActions{RetryFailed: CIActionDenied}) {
		t.Fatalf("a finished failed pipeline without updatePipeline = %+v", got)
	}
}

// The refusals the CI actions meet read as the error vocabulary: a token
// without the scope, a job that is gone, a job GitLab no longer retries,
// a job GitLab cannot play.
func TestCIActionRefusalSentences(t *testing.T) {
	t.Parallel()
	answers := map[string]forgeAPIAnswer{
		"jobs/1/retry": {Status: http.StatusForbidden, Body: `{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api"}`},
		"jobs/2/retry": {Status: http.StatusNotFound, Body: `{"message":"404 Job Not Found"}`},
		"jobs/3/retry": {Status: http.StatusForbidden, Body: `{"message":"403 Forbidden - Job is not retryable"}`},
		"jobs/4/play":  {Status: http.StatusBadRequest, Body: `{"message":"400 Bad request - Unplayable Job"}`},
	}
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		path := strings.TrimPrefix(call.Path, "projects/grp%2Fsub%2Fapp/")
		if answer, ok := answers[path]; ok {
			return answer
		}
		if path == "user" {
			return forgeAPIAnswer{Body: `{"id":1,"username":"v"}`}
		}
		return forgeUnexpected(t, call)
	})
	repo := gitlabTestRepo()
	for _, tc := range []struct {
		kind     CIActionKind
		job      string
		wantKind string
		want     string
	}{
		{CIActionRetryJob, "1", ForgeErrorForbidden, "Your forge.example token lacks the `api` scope, so the app cannot retry job unit of grp/sub/app. Create a token with that scope and log glab in with it."},
		{CIActionRetryJob, "2", ForgeErrorNotFound, "Job unit of grp/sub/app was not found on forge.example, or this login cannot see it."},
		{CIActionRetryJob, "3", ForgeErrorRefused, "Job unit of grp/sub/app can no longer be retried. Refresh to see its current state."},
		{CIActionPlayJob, "4", ForgeErrorRefused, "Job unit of grp/sub/app can no longer be run. Refresh to see its current state."},
	} {
		err := core.RunCIAction(t.Context(), repo, tc.kind, tc.job, CIPipeline{})
		var sentence *ForgeActionError
		if !errors.As(ExplainCIError(repo, CIJobSubject(repo, "unit"), tc.kind.ForgeAction(), err), &sentence) || sentence.Kind != tc.wantKind || sentence.Message != tc.want {
			t.Errorf("%s %s: %+v (%v), want %s %q", tc.kind, tc.job, sentence, err, tc.wantKind, tc.want)
		}
	}
}

// Artifacts: GitHub lists each run's and streams each zip, skipping the
// expired ones and refusing one listed past the cap before any request;
// GitLab streams the job archive, a 404 meaning it has none.
func TestCIArtifactDownloads(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case "repos/o/r/actions/runs/111/artifacts?per_page=100":
			return forgeAPIAnswer{Body: `{"total_count":3,"artifacts":[{"id":5,"name":"coverage","size_in_bytes":4,"expired":false},{"id":6,"name":"old","size_in_bytes":4,"expired":true},{"id":7,"name":"big","size_in_bytes":4096,"expired":false}]}`}
		case "repos/o/r/actions/runs/222/artifacts?per_page=100":
			return forgeAPIAnswer{Body: `{"total_count":1,"artifacts":[{"id":8,"name":"gone","size_in_bytes":4,"expired":true}]}`}
		case "repos/o/r/actions/artifacts/5/zip":
			return forgeAPIAnswer{Header: http.Header{"Content-Type": {"application/zip"}}, Body: "PK\x03\x04"}
		case "repos/o/r/actions/artifacts/9/zip":
			return forgeAPIAnswer{Status: http.StatusGone, Body: `{"message":"Artifact has expired"}`}
		case "projects/grp%2Fsub%2Fapp/jobs/11/artifacts":
			return forgeAPIAnswer{Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: "PK-gitlab"}
		case "projects/grp%2Fsub%2Fapp/jobs/12/artifacts":
			return forgeAPIAnswer{Status: http.StatusNotFound, Body: `{"message":"404 Not Found"}`}
		}
		return forgeUnexpected(t, call)
	})
	gh := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	artifacts, err := core.ListCIArtifacts(t.Context(), gh, CIArtifactsRequest{RunIDs: []string{"111"}})
	if err != nil || len(artifacts) != 2 || artifacts[0].Name != "coverage" || artifacts[1].Name != "big" {
		t.Fatalf("ListCIArtifacts = %+v, %v", artifacts, err)
	}
	var buf bytes.Buffer
	if err := core.StreamCIArtifact(t.Context(), gh, artifacts[0], &buf, 1024); err != nil || buf.String() != "PK\x03\x04" {
		t.Fatalf("StreamCIArtifact = %q, %v", buf.String(), err)
	}
	before := len(calls.all())
	if err := core.StreamCIArtifact(t.Context(), gh, artifacts[1], &buf, 1024); !errors.Is(err, forgeapi.ErrBodyTooLarge) || len(calls.all()) != before {
		t.Fatalf("an artifact listed past the cap = %v after %d requests", err, len(calls.all())-before)
	}
	if err := core.StreamCIArtifact(t.Context(), gh, CIArtifact{ID: "9", Name: "x"}, &buf, 1024); !errors.Is(err, ErrCIArtifactExpired) {
		t.Fatalf("a 410 = %v, want ErrCIArtifactExpired", err)
	}
	if _, err := core.ListCIArtifacts(t.Context(), gh, CIArtifactsRequest{RunIDs: []string{"222"}}); !errors.Is(err, ErrCIArtifactExpired) {
		t.Fatalf("only expired artifacts = %v", err)
	}

	gl := gitlabTestRepo()
	buf.Reset()
	if err := core.StreamCIArtifact(t.Context(), gl, CIArtifact{JobID: "11", Name: "artifacts.zip"}, &buf, 1024); err != nil || buf.String() != "PK-gitlab" {
		t.Fatalf("GitLab archive = %q, %v", buf.String(), err)
	}
	if err := core.StreamCIArtifact(t.Context(), gl, CIArtifact{JobID: "12"}, &buf, 1024); !errors.Is(err, ErrNoCIArtifacts) {
		t.Fatalf("a GitLab job without an archive = %v", err)
	}
	if _, err := core.ListCIArtifacts(t.Context(), gl, CIArtifactsRequest{JobID: "11", RunIDs: []string{"1"}}); err == nil {
		t.Fatal("a request naming a job and runs was accepted")
	}
}

// A log view open on a retried job follows it only when the job left
// the read, the read is the job's own pipeline (GitLab: the same pipeline
// id; GitHub: the same workflow run), and exactly one job of the same
// stage and name has a higher id. Otherwise it does not follow.
func TestFindCIJobRetry(t *testing.T) {
	t.Parallel()
	gitlab := func(id string, jobs ...CIJob) CIPipeline {
		return CIPipeline{ID: id, Stages: []CIStage{{Name: "test", Jobs: jobs}}}
	}
	old := CIJob{ID: "20", Name: "unit"}
	if got := FindCIJobRetry(gitlab("77", CIJob{ID: "35", Name: "unit"}, CIJob{ID: "40", Name: "lint"}), "77", "test", old); got == nil || got.ID != "35" {
		t.Fatalf("GitLab retry in the same pipeline = %+v, want 35", got)
	}
	// A new commit's pipeline 78 holds a job of the same stage and name.
	if got := FindCIJobRetry(gitlab("78", CIJob{ID: "21", Name: "unit"}), "77", "test", old); got != nil {
		t.Fatalf("followed into another pipeline: %+v", got)
	}
	// The open job is still in the read: nothing replaced it.
	if got := FindCIJobRetry(gitlab("77", old, CIJob{ID: "35", Name: "unit"}), "77", "test", old); got != nil {
		t.Fatalf("followed while the job is still there: %+v", got)
	}
	// Two candidates: no telling which took over.
	if got := FindCIJobRetry(gitlab("77", CIJob{ID: "35", Name: "unit"}, CIJob{ID: "36", Name: "unit"}), "77", "test", old); got != nil {
		t.Fatalf("followed one of two candidates: %+v", got)
	}

	// GitHub: the run is the pipeline. A PR's checks have no id.
	run := func(jobs ...CIJob) CIPipeline { return CIPipeline{Stages: []CIStage{{Name: "CI", Jobs: jobs}}} }
	ghOld := CIJob{ID: "20", RunID: "1", Name: "unit"}
	if got := FindCIJobRetry(run(CIJob{ID: "45", RunID: "1", Name: "unit"}, CIJob{ID: "60", RunID: "2", Name: "unit"}), "", "CI", ghOld); got == nil || got.ID != "45" {
		t.Fatalf("GitHub retry in the same run = %+v, want 45", got)
	}
	if got := FindCIJobRetry(run(CIJob{ID: "60", RunID: "2", Name: "unit"}), "", "CI", ghOld); got != nil {
		t.Fatalf("followed into another run: %+v", got)
	}
	// Two distinct jobs of one run share a display name and both remain.
	twins := run(CIJob{ID: "20", RunID: "1", Name: "unit"}, CIJob{ID: "25", RunID: "1", Name: "unit"})
	if got := FindCIJobRetry(twins, "", "CI", ghOld); got != nil {
		t.Fatalf("followed a twin that is not a retry: %+v", got)
	}
}
