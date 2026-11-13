package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/forgeapi"
	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// ciActionsRepos are a GitHub repository and a GitLab project each with
// a finished pipeline holding a failed and a passing job (GitHub run 400
// keeps an artifact, GitLab's failed build an archive), a running one, a
// GitLab manual job, and a GitLab merge request whose head pipeline is
// pipeline 81.
func ciActionsRepos() []forgefake.Repo {
	return []forgefake.Repo{
		{Forge: "github", Project: "acme/acts", Pipelines: []forgefake.Pipeline{
			{ID: 400, Name: "CI", Ref: "main", Jobs: []forgefake.Job{{ID: 4001, Name: "unit", Status: "failed"}, {ID: 4002, Name: "lint", Status: "success"}},
				Artifacts: []forgefake.Artifact{{ID: 71, Name: "coverage", Text: "PK-gh-coverage"}}},
			{ID: 401, Name: "CI", Ref: "feature", Jobs: []forgefake.Job{{ID: 4011, Name: "unit", Status: "running"}}},
		}},
		{Forge: "gitlab", Project: "grp/acts", ID: 8181,
			Pulls: []forgefake.Pull{{Number: 3, Title: "Acts", HeadRef: "acts", CI: &forgefake.Pipeline{ID: 81, Jobs: []forgefake.Job{
				{ID: 8101, Name: "unit", Stage: "test", Status: "failed", Artifacts: &forgefake.Artifact{Text: "PK-gl-unit"}},
				{ID: 8102, Name: "lint", Stage: "test", Status: "success"},
			}}}},
			Pipelines: []forgefake.Pipeline{
				{ID: 40, Ref: "main", Jobs: []forgefake.Job{{ID: 4101, Name: "unit", Stage: "test", Status: "failed"}, {ID: 4102, Name: "lint", Stage: "test", Status: "success"}}},
				{ID: 41, Ref: "feature", Jobs: []forgefake.Job{{ID: 4111, Name: "unit", Stage: "test", Status: "running"}, {ID: 4112, Name: "release", Stage: "deploy", Status: "manual"}}},
			}},
	}
}

var (
	ciActionsGitHub = gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "acts"}
	ciActionsGitLab = gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "acts"}
)

// newCIActionsApp is an App whose git core reads the fake forge seeded
// with repos, with the pumps' own cadences out of the way.
func newCIActionsApp(t *testing.T, repos []forgefake.Repo) (*App, *forgefake.Engine) {
	t.Helper()
	app := NewApp()
	// Registered first, so it runs after every subscription's release.
	t.Cleanup(app.prUpdates.wg.Wait)
	app.configDir = t.TempDir()
	app.prUpdates.interval = time.Hour
	app.prUpdates.ciLiveInterval = time.Hour
	engine := forgefake.New(forgefake.Options{APIToken: mergeForgeToken})
	if _, err := engine.Seed(forgefake.Fixture{Repos: repos}); err != nil {
		t.Fatal(err)
	}
	useForgefake(t, app, engine)
	return app, engine
}

func subscribeCIPipeline(t *testing.T, app *App, repo gitops.RepoReference, id string) PipelineSubscriptionResult {
	t.Helper()
	sub, err := app.SubscribePipeline(context.Background(), repo, id)
	if err != nil || sub.Pipeline == nil {
		t.Fatalf("SubscribePipeline(%s) = %+v, %v", id, sub, err)
	}
	t.Cleanup(func() { _ = app.UnsubscribePipeline(context.Background(), sub.ID) })
	return sub
}

// pipelineState is the pipeline a subscription's pump holds and its
// sequence.
func pipelineState(app *App, subscriptionID string) (gitops.CIPipeline, uint64) {
	app.prUpdates.mu.Lock()
	defer app.prUpdates.mu.Unlock()
	pump := app.prUpdates.pipelineHandles[subscriptionID].pump
	return pump.ci, pump.seq
}

func forgeRoutes(engine *forgefake.Engine) []string {
	var routes []string
	for _, inv := range engine.Invocations(0).Invocations {
		routes = append(routes, inv.Route)
	}
	return routes
}

func countRoute(engine *forgefake.Engine, route string) int {
	n := 0
	for _, inv := range engine.Invocations(0).Invocations {
		if inv.Route == route {
			n++
		}
	}
	return n
}

// A retry from the Pipelines tab runs on the forge and is answered once
// the pump polled: the pump's pipeline is the forge's answer (the failed
// job's retry, pending, under a new id) and the answer's sequence is the
// pump's after that poll, on both forges. A cancel ends the running
// pipeline the same way, and GitLab's manual job starts on play.
func TestCIActionsSettleOnThePumpsPoll(t *testing.T) {
	t.Parallel()
	app, engine := newCIActionsApp(t, ciActionsRepos())
	for _, tc := range []struct {
		repo                      gitops.RepoReference
		finished, running, failed string
		stage, retryRoute         string
	}{
		{ciActionsGitHub, "400", "401", "4001", "CI", "gh api job rerun"},
		{ciActionsGitLab, "40", "41", "4101", "test", "glab api job retry"},
	} {
		sub := subscribeCIPipeline(t, app, tc.repo, tc.finished)
		old := gitops.FindCIJob(*sub.Pipeline, tc.failed)
		if old == nil || old.Actions.Retry != gitops.CIActionAllowed || sub.Pipeline.Actions.RetryFailed != gitops.CIActionAllowed {
			t.Fatalf("%s: the failed job is not retryable: %+v", tc.repo.Forge, sub.Pipeline)
		}
		result, err := app.RunPipelineAction(context.Background(), sub.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: tc.failed})
		if err != nil {
			t.Fatalf("%s retry: %v", tc.repo.Forge, err)
		}
		pipeline, seq := pipelineState(app, sub.ID)
		retry := gitops.FindCIJobRetry(pipeline, tc.stage, *old)
		if retry == nil || retry.Status != gitops.CIStatusPending || result.Seq != seq || result.Seq <= sub.Seq || countRoute(engine, tc.retryRoute) != 1 {
			t.Fatalf("%s after the retry: seq %d (pump %d, subscribe %d), retry %+v, routes %v", tc.repo.Forge, result.Seq, seq, sub.Seq, retry, forgeRoutes(engine))
		}

		live := subscribeCIPipeline(t, app, tc.repo, tc.running)
		if live.Pipeline.Actions.Cancel != gitops.CIActionAllowed {
			t.Fatalf("%s: the running pipeline is not cancelable: %+v", tc.repo.Forge, live.Pipeline.Actions)
		}
		if _, err := app.RunPipelineAction(context.Background(), live.ID, CIActionRequest{Kind: gitops.CIActionCancelPipeline}); err != nil {
			t.Fatalf("%s cancel: %v", tc.repo.Forge, err)
		}
		canceled, _ := pipelineState(app, live.ID)
		if canceled.Status != gitops.CIStatusCanceled || canceled.Actions.Cancel != "" {
			t.Fatalf("%s after the cancel = %+v", tc.repo.Forge, canceled)
		}
	}

	live, _ := pipelineState(app, subscribeCIPipeline(t, app, ciActionsGitLab, "41").ID)
	sub := subscribeCIPipeline(t, app, ciActionsGitLab, "41")
	if gitops.FindCIJob(live, "4112").Actions.Play != gitops.CIActionAllowed {
		t.Fatalf("the manual job is not playable: %+v", gitops.FindCIJob(live, "4112"))
	}
	if _, err := app.RunPipelineAction(context.Background(), sub.ID, CIActionRequest{Kind: gitops.CIActionPlayJob, JobID: "4112"}); err != nil {
		t.Fatalf("play: %v", err)
	}
	if played, _ := pipelineState(app, sub.ID); gitops.FindCIJob(played, "4112").Status != gitops.CIStatusPending {
		t.Fatalf("the played job = %+v", gitops.FindCIJob(played, "4112"))
	}
	expectForgeCallsHandled(t, engine)
}

// An action the pipeline on screen does not allow is refused before any
// forge request, in the vocabulary: one that no longer applies, one the
// viewer may not take, a job the pipeline does not list, and a second
// action while one is pending on the pipeline.
func TestCIActionsRefusedBeforeTheForge(t *testing.T) {
	t.Parallel()
	repos := ciActionsRepos()
	repos[1].Merge = &forgefake.RepoMerge{Permission: "reporter"}
	app, engine := newCIActionsApp(t, repos)
	gh := subscribeCIPipeline(t, app, ciActionsGitHub, "400")
	gl := subscribeCIPipeline(t, app, ciActionsGitLab, "40")
	before := len(engine.Invocations(0).Invocations)
	for _, tc := range []struct {
		name, sub string
		req       CIActionRequest
		kind      string
		message   string
	}{
		{"passing job", gh.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4002"}, gitops.ForgeErrorRefused, "You can no longer retry job lint of acme/acts. Refresh to see its current state."},
		{"GitHub job cancel", gh.ID, CIActionRequest{Kind: gitops.CIActionCancelJob, JobID: "4001"}, gitops.ForgeErrorRefused, "You can no longer cancel job unit of acme/acts. Refresh to see its current state."},
		{"finished pipeline cancel", gh.ID, CIActionRequest{Kind: gitops.CIActionCancelPipeline}, gitops.ForgeErrorRefused, "You can no longer cancel pipeline 400 of acme/acts. Refresh to see its current state."},
		{"reporter", gl.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4101"}, gitops.ForgeErrorForbidden, "This login may not retry job unit of grp/acts."},
		{"unknown job", gh.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "9"}, gitops.ForgeErrorNotFound, "This job is no longer in the pipeline. Refresh to see its current jobs."},
	} {
		_, err := app.RunPipelineAction(context.Background(), tc.sub, tc.req)
		var sentence *gitops.ForgeActionError
		if !errors.As(err, &sentence) || sentence.Kind != tc.kind || sentence.Message != tc.message {
			t.Errorf("%s: %v, want %s %q", tc.name, err, tc.kind, tc.message)
		}
	}
	if _, err := app.RunPipelineAction(context.Background(), gh.ID, CIActionRequest{Kind: gitops.CIActionRetryFailed, JobID: "4001"}); err == nil {
		t.Error("a pipeline action naming a job was accepted")
	}
	if _, err := app.RunPipelineAction(context.Background(), "nope", CIActionRequest{Kind: gitops.CIActionRetryFailed}); !errors.Is(err, errPRUpdatesUnknownSubscription) {
		t.Errorf("unknown subscription = %v", err)
	}

	// One action at a time per pipeline: a second, here on another
	// subscription of the same pipeline, is refused while the first runs.
	other := subscribeCIPipeline(t, app, ciActionsGitHub, "400")
	app.prUpdates.mu.Lock()
	app.prUpdates.ciActions = map[string]bool{pipelineKey(ciActionsGitHub, "400"): true}
	app.prUpdates.mu.Unlock()
	_, err := app.RunPipelineAction(context.Background(), other.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4001"})
	var sentence *gitops.ForgeActionError
	if !errors.As(err, &sentence) || sentence.Message != "Another action on this pipeline is still running. Wait for it to finish." {
		t.Fatalf("a second action while one is pending = %v", err)
	}
	if after := len(engine.Invocations(0).Invocations); after != before {
		t.Fatalf("refused actions reached the forge: %v", forgeRoutes(engine)[before:])
	}
	app.prUpdates.mu.Lock()
	clear(app.prUpdates.ciActions)
	app.prUpdates.mu.Unlock()
	if _, err := app.RunPipelineAction(context.Background(), other.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4001"}); err != nil {
		t.Fatalf("the action once the first settled: %v", err)
	}
	app.prUpdates.mu.Lock()
	pending := len(app.prUpdates.ciActions)
	app.prUpdates.mu.Unlock()
	if pending != 0 {
		t.Fatalf("a settled action is still pending: %d", pending)
	}
}

// The forge's refusal of an action reaches the caller as its sentence,
// and the pipeline is not marked pending after it.
func TestCIActionForgeRefusal(t *testing.T) {
	t.Parallel()
	app, engine := newCIActionsApp(t, ciActionsRepos())
	sub := subscribeCIPipeline(t, app, ciActionsGitLab, "40")
	if err := engine.SetRefusal(forgefake.Refusal{Route: "glab api job retry", Status: 403, Body: []byte(`{"error":"insufficient_scope","error_description":"The request requires higher privileges than provided by the access token.","scope":"api"}`), Times: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := app.RunPipelineAction(context.Background(), sub.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4101"})
	var sentence *gitops.ForgeActionError
	if !errors.As(err, &sentence) || sentence.Kind != gitops.ForgeErrorForbidden || !strings.Contains(sentence.Message, "lacks the `api` scope, so the app cannot retry job unit of grp/acts") {
		t.Fatalf("refusal = %v", err)
	}
	if _, err := app.RunPipelineAction(context.Background(), sub.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "4101"}); err != nil {
		t.Fatalf("the action after a refusal: %v", err)
	}
}

// An action from the review pane runs on the PR's head pipeline and has
// the Pipelines tab's pump on the same pipeline poll too, so both show
// the forge's answer.
func TestCIActionFromThePRReachesThePipelinePump(t *testing.T) {
	t.Parallel()
	app, _ := newCIActionsApp(t, ciActionsRepos())
	pr := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "acts", Number: 3}
	sub, err := app.SubscribePRUpdates(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.UnsubscribePRUpdates(context.Background(), sub.ID) })
	pipelineSub := subscribeCIPipeline(t, app, ciActionsGitLab, "81")
	deadline := time.Now().Add(5 * time.Second)
	for {
		app.prUpdates.mu.Lock()
		known := app.prUpdates.handles[sub.ID].pump.ciKnown
		app.prUpdates.mu.Unlock()
		if known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the PR pump never read its pipeline")
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, err := app.RunPRCIAction(context.Background(), sub.ID, CIActionRequest{Kind: gitops.CIActionRetryJob, JobID: "8101"})
	if err != nil || result.Seq == 0 {
		t.Fatalf("RunPRCIAction = %+v, %v", result, err)
	}
	for {
		pipeline, _ := pipelineState(app, pipelineSub.ID)
		if gitops.FindCIJob(pipeline, "8101") == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Pipelines tab's pump still shows the retried job: %+v", pipeline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Artifacts download to files under the app data directory: a GitHub
// run's artifacts and a GitLab job's archive, the bytes the forge
// served; a job with none reads as its sentence.
func TestSaveCIArtifacts(t *testing.T) {
	t.Parallel()
	app, _ := newCIActionsApp(t, ciActionsRepos())
	for _, tc := range []struct {
		repo    gitops.RepoReference
		req     gitops.CIArtifactsRequest
		subject string
		name    string
		content string
	}{
		{ciActionsGitHub, gitops.CIArtifactsRequest{RunIDs: []string{"400"}}, "CI", "github-acme-acts-400-71-coverage.zip", "PK-gh-coverage"},
		{ciActionsGitLab, gitops.CIArtifactsRequest{JobID: "8101"}, "unit", "gitlab-grp-acts-8101-unit.zip", "PK-gl-unit"},
	} {
		saved, err := app.SaveCIArtifacts(context.Background(), tc.repo, tc.req, tc.subject)
		if err != nil || len(saved.Paths) != 1 || filepath.Base(saved.Paths[0]) != tc.name || saved.Dir != filepath.Join(app.configDir, "ci-artifacts") {
			t.Fatalf("%s SaveCIArtifacts = %+v, %v", tc.repo.Forge, saved, err)
		}
		if body, err := os.ReadFile(saved.Paths[0]); err != nil || string(body) != tc.content {
			t.Fatalf("%s saved file = %q, %v", tc.repo.Forge, body, err)
		}
	}
	_, err := app.SaveCIArtifacts(context.Background(), ciActionsGitLab, gitops.CIArtifactsRequest{JobID: "8102"}, "lint")
	var sentence *gitops.ForgeActionError
	if !errors.As(err, &sentence) || sentence.Message != "Job lint of grp/acts has no artifacts to download on GitLab." {
		t.Fatalf("a job without artifacts = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(app.configDir, "ci-artifacts"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".save-") {
			t.Fatalf("a failed download left %s", entry.Name())
		}
	}
	if _, err := app.SaveCIArtifacts(context.Background(), ciActionsGitLab, gitops.CIArtifactsRequest{JobID: "../x"}, "x"); err == nil {
		t.Fatal("a job id that is not a number was accepted")
	}
}

// An artifact past the cap is named in its sentence, and a failure after
// some files were saved says where they are.
func TestCIArtifactFailureSentences(t *testing.T) {
	t.Parallel()
	app := &App{}
	artifact := gitops.CIArtifact{ID: "7", Name: "coverage", RunID: "400"}
	tooLarge := fmt.Errorf("artifact coverage: %w", forgeapi.ErrBodyTooLarge)
	var sentence *gitops.ForgeActionError
	err := app.explainArtifactFailure(ciActionsGitHub, "pipeline 400 of acme/acts", artifact, CIArtifactsSaved{}, tooLarge)
	if !errors.As(err, &sentence) || sentence.Kind != gitops.ForgeErrorRefused || sentence.Message != "The artifact coverage of pipeline 400 of acme/acts is larger than the 1024 MiB the app downloads. Download it from the forge." {
		t.Fatalf("too large = %v", err)
	}
	saved := CIArtifactsSaved{Dir: "/data/ci-artifacts", Paths: []string{"/data/ci-artifacts/a.zip"}}
	err = app.explainArtifactFailure(ciActionsGitHub, "pipeline 400 of acme/acts", artifact, saved, gitops.ErrCIArtifactExpired)
	if !errors.As(err, &sentence) || sentence.Kind != gitops.ForgeErrorNotFound || sentence.Message != "Saved 1 artifacts to /data/ci-artifacts before the download stopped. The artifacts of pipeline 400 of acme/acts have expired on GitHub." {
		t.Fatalf("after a saved file = %v", err)
	}
}
