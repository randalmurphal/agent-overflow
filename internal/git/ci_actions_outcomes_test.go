package git

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// countPath counts the recorded requests to path (its query included).
func countPath(calls *forgeAPICalls, path string) int {
	n := 0
	for _, call := range calls.all() {
		if call.Path == path {
			n++
		}
	}
	return n
}

const githubRun111Artifacts = "repos/o/r/actions/runs/111/artifacts?per_page=100"

// A failed artifacts read keeps the CI read: the jobs and every flag the
// artifacts do not decide stay, the artifacts flag is absent, and the
// pipeline says why in one sentence. The failure is not asked again
// every tick, only once its backoff is due, and its sentence clears with
// the next good answer.
func TestGitHubCIActionsUnavailableKeepsTheJobs(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	refuse := true
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case call.Op == "PRTick":
			rollup := githubCheckRun("unit", "CI", "COMPLETED", "FAILURE", "111", "901", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z")
			return githubData(`{"repository":{"viewerPermission":"WRITE","pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + rollup + `]}}}}]}}}}`)
		case call.Path == githubRun111Artifacts && refuse:
			return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"Resource not accessible by integration"}`}
		case call.Path == githubRun111Artifacts:
			return forgeAPIAnswer{Body: `{"total_count":1,"artifacts":[{"id":5,"name":"coverage","size_in_bytes":10,"expired":false}]}`}
		}
		return forgeUnexpected(t, call)
	})
	clock := &testClockCore{t: time.Now()}
	core.nowFn = clock.now
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil {
		t.Fatalf("a refused artifacts read failed the CI read: %v", err)
	}
	unit := FindCIJob(pipeline, "901")
	if unit == nil || unit.Actions.Retry != CIActionAllowed || pipeline.Actions != (CIPipelineActions{RetryFailed: CIActionAllowed}) {
		t.Fatalf("the read without artifacts = %+v / %+v", unit, pipeline.Actions)
	}
	if want := "Retry, cancel and artifact downloads are not shown: this login cannot read the Actions permissions or artifacts of o/r on forge.example."; pipeline.ActionsUnavailable != want {
		t.Fatalf("ActionsUnavailable = %q, want %q", pipeline.ActionsUnavailable, want)
	}
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), &pipeline, nil); err != nil {
		t.Fatal(err)
	}
	if n := countPath(calls, githubRun111Artifacts); n != 1 {
		t.Fatalf("artifacts reads before the backoff is due = %d, want 1", n)
	}
	mu.Lock()
	refuse = false
	mu.Unlock()
	clock.advance(time.Minute)
	again, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), &pipeline, nil)
	if err != nil || countPath(calls, githubRun111Artifacts) != 2 || again.ActionsUnavailable != "" || again.Actions.Artifacts != CIActionAllowed {
		t.Fatalf("once the backoff is due = %+v, %v (%d reads)", again, err, countPath(calls, githubRun111Artifacts))
	}
}

// githubRunReadCore answers ReadPipeline of run 400 (completed, a failed
// job 4001, created at created) with permission for GET repos/o/r and no
// artifacts.
func githubRunReadCore(t *testing.T, created string, permission forgeAPIAnswer) (*Core, *forgeAPICalls) {
	t.Helper()
	return newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "repos/o/r/actions/runs/400":
			return forgeAPIAnswer{Body: `{"id":400,"name":"CI","head_branch":"main","head_sha":"abc","status":"completed","conclusion":"failure","created_at":"` + created + `","html_url":"https://github.com/o/r/actions/runs/400"}`}
		case strings.HasPrefix(call.Path, "repos/o/r/actions/runs/400/jobs?"):
			return forgeAPIAnswer{Body: `{"total_count":1,"jobs":[{"id":4001,"name":"unit","status":"completed","conclusion":"failure","started_at":"2026-10-20T10:00:00Z","completed_at":"2026-10-20T10:00:30Z","html_url":"https://github.com/o/r/actions/runs/400/job/4001","steps":[]}]}`}
		case call.Path == "repos/o/r":
			return permission
		case call.Path == "repos/o/r/actions/runs/400/artifacts?per_page=100":
			return forgeAPIAnswer{Body: `{"total_count":0,"artifacts":[]}`}
		}
		return forgeUnexpected(t, call)
	})
}

// A failed permission read keeps ReadPipeline's jobs, with no
// permission-gated action and the sentence; the rate limit's wording is
// the vocabulary's.
func TestGitHubReadPipelineWithoutPermission(t *testing.T) {
	t.Parallel()
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	core, calls := githubRunReadCore(t, time.Now().UTC().Format(time.RFC3339), forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"Server Error"}`})
	pipeline, err := core.ReadPipeline(t.Context(), repo, "400", nil, nil)
	if err != nil {
		t.Fatalf("a failed permission read failed ReadPipeline: %v", err)
	}
	if job := FindCIJob(pipeline, "4001"); job == nil || job.Actions != (CIJobActions{}) || pipeline.Actions != (CIPipelineActions{}) {
		t.Fatalf("pipeline without its permission = %+v", pipeline)
	}
	if !strings.HasPrefix(pipeline.ActionsUnavailable, "Retry, cancel and artifact downloads are not shown for now. ") {
		t.Fatalf("ActionsUnavailable = %q", pipeline.ActionsUnavailable)
	}
	if _, err := core.ReadPipeline(t.Context(), repo, "400", nil, nil); err != nil || countPath(calls, "repos/o/r") != 1 {
		t.Fatalf("the permission was asked again before its backoff: %d reads, %v", countPath(calls, "repos/o/r"), err)
	}
}

// GitHub re-runs a workflow run up to 30 days after it was created.
// ReadPipeline's run carries its creation, so a run past that offers no
// retry; the PR checks carry none, so GitHub's refusal reads as the run
// being too old.
func TestGitHubRerunWindow(t *testing.T) {
	t.Parallel()
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	push := forgeAPIAnswer{Body: `{"id":1,"permissions":{"admin":false,"maintain":false,"push":true}}`}
	for _, tc := range []struct {
		age  time.Duration
		want CIAction
	}{{29 * 24 * time.Hour, CIActionAllowed}, {31 * 24 * time.Hour, ""}} {
		core, _ := githubRunReadCore(t, time.Now().Add(-tc.age).UTC().Format(time.RFC3339), push)
		pipeline, err := core.ReadPipeline(t.Context(), repo, "400", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := FindCIJob(pipeline, "4001").Actions.Retry; got != tc.want || pipeline.Actions.RetryFailed != tc.want {
			t.Errorf("a run %s old: retry %q, retry failed %q, want %q", tc.age, got, pipeline.Actions.RetryFailed, tc.want)
		}
	}

	refusing, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		return forgeAPIAnswer{Status: http.StatusForbidden, Body: `{"message":"Unable to re-run this workflow run because it was created over a month ago"}`}
	})
	err := refusing.RunCIAction(t.Context(), repo, CIActionRetryJob, "901", githubActionsPipeline())
	var sentence *ForgeActionError
	if !errors.As(ExplainCIError(repo, CIJobSubject(repo, "unit"), ActionRetryJob, err), &sentence) || sentence.Kind != ForgeErrorRefused ||
		sentence.Message != "Job unit of o/r is more than 30 days old, so it can no longer be retried. Start a new run on the forge." {
		t.Fatalf("too old = %+v (%v)", sentence, err)
	}
}

// A write whose answer was lost may have been applied: it is posted once,
// reported as unconfirmed rather than as refused, and a pipeline action
// over several runs counts each run's outcome.
func TestCIWriteOutcomes(t *testing.T) {
	t.Parallel()
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch call.Path {
		case "repos/o/r/actions/runs/333/rerun-failed-jobs", "repos/o/r/actions/jobs/901/rerun":
			return forgeAPIAnswer{Status: http.StatusBadGateway, Body: `{"message":"Bad gateway"}`}
		case "projects/grp%2Fsub%2Fapp/jobs/11/retry":
			return forgeAPIAnswer{Status: http.StatusInternalServerError, Body: `{"message":"500 Internal Server Error"}`}
		}
		return forgeAPIAnswer{Status: http.StatusCreated, Body: `{}`}
	})
	err := core.RunCIAction(t.Context(), repo, CIActionRetryFailed, "", githubActionsPipeline())
	partial, ok := errors.AsType[*CIPartialError](err)
	if !ok || partial.Done != 1 || partial.Unconfirmed != 1 || partial.Total != 2 || !partial.Applied() {
		t.Fatalf("one run confirmed and one lost = %v", err)
	}
	if n := countPath(calls, "repos/o/r/actions/runs/333/rerun-failed-jobs"); n != 1 {
		t.Fatalf("the unconfirmed write was posted %d times", n)
	}
	var sentence *ForgeActionError
	if !errors.As(ExplainCIError(repo, "the checks of o/r#5", ActionRetryFailed, err), &sentence) ||
		sentence.Message != "Of the 2 workflow runs of the checks of o/r#5 asked to retry the failed jobs, GitHub took it on 1 and did not confirm it on 1, which may have taken it." {
		t.Fatalf("mixed sentence = %+v", sentence)
	}

	err = core.RunCIAction(t.Context(), repo, CIActionRetryJob, "901", githubActionsPipeline())
	if partial, ok := errors.AsType[*CIPartialError](err); !ok || partial.Unconfirmed != 1 || partial.Total != 1 {
		t.Fatalf("a lost job re-run = %v, want one unconfirmed write", err)
	}
	if !errors.As(ExplainCIError(repo, CIJobSubject(repo, "unit"), ActionRetryJob, err), &sentence) ||
		sentence.Message != "GitHub did not confirm the request to retry job unit of o/r, so it may have been applied. The CI view shows what GitHub did." {
		t.Fatalf("unconfirmed sentence = %+v", sentence)
	}
	if err := core.RunCIAction(t.Context(), gitlabTestRepo(), CIActionRetryJob, "11", CIPipeline{}); func() bool {
		partial, ok := errors.AsType[*CIPartialError](err)
		return !ok || partial.Unconfirmed != 1
	}() {
		t.Fatalf("a GitLab 500 = %v, want one unconfirmed write", err)
	}
}

// GitLab's cancel permission decides cancel on its own: a project can
// restrict cancel past update (restrict_pipeline_cancellation_role).
func TestGitLabCancelFollowsTheCancelPermission(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "projects/grp%2Fsub%2Fapp/pipelines/77":
			return forgeAPIAnswer{Body: `{"id":77,"status":"running","ref":"main","sha":"abc","web_url":"https://gitlab.example/p/77"}`}
		case call.Op == "PipelineJobs":
			page := gitlabJobsPage([]string{"test"}, "",
				gitlabJobNode("Ci::Build", 12, "lint", "test", "RUNNING", `"startedAt":"2026-10-20T10:00:00Z","cancelable":true,"userPermissions":{"updateBuild":true,"cancelBuild":false,"readJobArtifacts":true}`))
			body := strings.Replace(page.Body, `"pipeline":{`, `"pipeline":{"retryable":false,"cancelable":true,"userPermissions":{"updatePipeline":true,"cancelPipeline":false},`, 1)
			return forgeAPIAnswer{Body: body}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "77", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := FindCIJob(pipeline, "12").Actions.Cancel; got != CIActionDenied || pipeline.Actions.Cancel != CIActionDenied {
		t.Fatalf("cancel restricted past update: job %q, pipeline %q, want denied", got, pipeline.Actions.Cancel)
	}
}

// A GitLab before 16.6 has no cancel permission fields, and a field the
// schema lacks fails the whole document: the read falls back to the
// document without them, where update decides cancel, and the host is
// read that way from then on.
func TestGitLabPipelineJobsWithoutTheCancelPermissions(t *testing.T) {
	t.Parallel()
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Path == "projects/grp%2Fsub%2Fapp/pipelines/77":
			return forgeAPIAnswer{Body: `{"id":77,"status":"running","ref":"main","sha":"abc","web_url":"https://gitlab.example/p/77"}`}
		case call.Op == "PipelineJobs" && strings.Contains(call.Query, "cancelBuild"):
			return forgeAPIAnswer{Body: `{"errors":[{"message":"Field 'cancelPipeline' doesn't exist on type 'PipelinePermissions'","locations":[{"line":5,"column":40}],"path":["query PipelineJobs","project","pipeline","userPermissions","cancelPipeline"],"extensions":{"code":"undefinedField","typeName":"PipelinePermissions","fieldName":"cancelPipeline"}},{"message":"Field 'cancelBuild' doesn't exist on type 'JobPermissions'","extensions":{"code":"undefinedField","typeName":"JobPermissions","fieldName":"cancelBuild"}}]}`}
		case call.Op == "PipelineJobs":
			if strings.Contains(call.Query, "cancelPipeline") {
				t.Errorf("the fallback document still selects cancelPipeline")
			}
			page := gitlabJobsPage([]string{"test"}, "",
				gitlabJobNode("Ci::Build", 12, "lint", "test", "RUNNING", `"startedAt":"2026-10-20T10:00:00Z","cancelable":true,"userPermissions":{"updateBuild":true,"readJobArtifacts":true}`))
			body := strings.Replace(page.Body, `"pipeline":{`, `"pipeline":{"retryable":false,"cancelable":true,"userPermissions":{"updatePipeline":true},`, 1)
			return forgeAPIAnswer{Body: body}
		}
		return forgeUnexpected(t, call)
	})
	for range 2 {
		pipeline, err := core.ReadPipeline(t.Context(), gitlabTestRepo(), "77", nil, nil)
		if err != nil {
			t.Fatalf("ReadPipeline on an older GitLab: %v", err)
		}
		if got := FindCIJob(pipeline, "12").Actions.Cancel; got != CIActionAllowed || pipeline.Actions.Cancel != CIActionAllowed {
			t.Fatalf("cancel on an older GitLab: job %q, pipeline %q, want allowed by update", got, pipeline.Actions.Cancel)
		}
	}
	jobsReads := 0
	for _, op := range calls.ops() {
		if op == "PipelineJobs" {
			jobsReads++
		}
	}
	if jobsReads != 3 {
		t.Fatalf("PipelineJobs reads = %d, want the refused one, its fallback and the second read's fallback alone", jobsReads)
	}
}

// The artifacts flag is not remembered past the earliest expiry the list
// reported, and a download that finds the artifacts expired drops it so
// the next read asks again. A first page of expired artifacts does not
// hide a usable one on the next.
func TestGitHubArtifactsFlagFollowsExpiry(t *testing.T) {
	t.Parallel()
	start := time.Now()
	var mu sync.Mutex
	expired := false
	core, calls := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case call.Op == "PRTick":
			rollup := githubCheckRun("unit", "CI", "COMPLETED", "SUCCESS", "111", "901", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z")
			return githubData(`{"repository":{"viewerPermission":"WRITE","pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + rollup + `]}}}}]}}}}`)
		case strings.HasPrefix(call.Path, "repos/o/r/actions/runs/111/artifacts?"):
			return forgeAPIAnswer{Body: fmt.Sprintf(`{"total_count":1,"artifacts":[{"id":5,"name":"coverage","size_in_bytes":10,"expired":%t,"expires_at":%q}]}`, expired, start.Add(2*time.Minute).UTC().Format(time.RFC3339))}
		}
		return forgeUnexpected(t, call)
	})
	clock := &testClockCore{t: start}
	core.nowFn = clock.now
	reads := func() int { return countPath(calls, githubRun111Artifacts) }
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil || pipeline.Actions.Artifacts != CIActionAllowed {
		t.Fatalf("first read = %+v, %v", pipeline.Actions, err)
	}
	clock.advance(time.Minute)
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil); err != nil || reads() != 1 {
		t.Fatalf("before the expiry: %d reads, %v", reads(), err)
	}
	clock.advance(2 * time.Minute)
	mu.Lock()
	expired = true
	mu.Unlock()
	after, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil || reads() != 2 || after.Actions.Artifacts != "" {
		t.Fatalf("past the earliest expiry: %+v after %d reads, %v", after.Actions, reads(), err)
	}

	// A download that finds them expired drops the remembered answer.
	mu.Lock()
	expired = false
	mu.Unlock()
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil); err != nil || reads() != 2 {
		t.Fatalf("within the answer's life: %d reads, %v", reads(), err)
	}
	mu.Lock()
	expired = true
	mu.Unlock()
	repo := RepoReference{Forge: "github", Host: testForgeHost, Namespace: "o", Repo: "r"}
	if _, err := core.ListCIArtifacts(t.Context(), repo, CIArtifactsRequest{RunIDs: []string{"111"}}); !errors.Is(err, ErrCIArtifactExpired) {
		t.Fatalf("download of expired artifacts = %v", err)
	}
	before := reads()
	if _, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil); err != nil || reads() != before+1 {
		t.Fatalf("after the download found them expired: %d reads, want %d, %v", reads(), before+1, err)
	}
}

// A run whose first artifacts page holds only expired ones is read on
// until a usable one is found.
func TestGitHubArtifactsFlagReadsPastExpiredPages(t *testing.T) {
	t.Parallel()
	core, _ := newForgeAPICore(t, func(call forgeAPICall) forgeAPIAnswer {
		switch {
		case call.Op == "PRTick":
			rollup := githubCheckRun("unit", "CI", "COMPLETED", "SUCCESS", "111", "901", "2026-07-06T14:00:00Z", "2026-07-06T14:01:00Z")
			return githubData(`{"repository":{"viewerPermission":"WRITE","pullRequest":{"number":5,"commits":{"nodes":[{"commit":{"id":"C","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":false},"nodes":[` + rollup + `]}}}}]}}}}`)
		case call.Path == githubRun111Artifacts:
			items := make([]string, 100)
			for i := range items {
				items[i] = fmt.Sprintf(`{"id":%d,"name":"old-%d","size_in_bytes":1,"expired":true}`, i+1, i)
			}
			next := "<" + call.RESTBase + "repos/o/r/actions/runs/111/artifacts?per_page=100&page=2>; rel=\"next\""
			return forgeAPIAnswer{Header: http.Header{"Link": {next}}, Body: `{"total_count":101,"artifacts":[` + strings.Join(items, ",") + `]}`}
		case call.Path == "repos/o/r/actions/runs/111/artifacts?per_page=100&page=2":
			return forgeAPIAnswer{Body: `{"total_count":101,"artifacts":[{"id":200,"name":"coverage","size_in_bytes":1,"expired":false}]}`}
		}
		return forgeUnexpected(t, call)
	})
	pipeline, err := core.ListPRCIJobs(t.Context(), githubTestRef(5), nil, nil)
	if err != nil || pipeline.Actions.Artifacts != CIActionAllowed {
		t.Fatalf("a usable artifact past an expired page = %+v, %v", pipeline.Actions, err)
	}
}
