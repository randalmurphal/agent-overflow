package main

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// ciActionsFixture is a GitHub PR whose head run failed and keeps an
// artifact, and a GitLab merge request whose head pipeline holds a failed
// build with an archive, a manual job and a running one.
func ciActionsFixture() forgefake.Fixture {
	return forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/ci", Pulls: []forgefake.Pull{{
			Number: 4, Title: "CI", HeadRef: "ci",
			CI: &forgefake.Pipeline{ID: 300, Name: "CI", Jobs: []forgefake.Job{{ID: 3001, Name: "unit", Status: "failed"}, {ID: 3002, Name: "lint", Status: "success"}},
				Artifacts: []forgefake.Artifact{{ID: 61, Name: "coverage", Text: "PK-gh"}}},
		}}},
		{Forge: "gitlab", Project: "grp/ci", ID: 5151, Pulls: []forgefake.Pull{{
			Number: 8, Title: "CI", HeadRef: "ci",
			CI: &forgefake.Pipeline{ID: 80, Jobs: []forgefake.Job{
				{ID: 8001, Name: "unit", Stage: "test", Status: "failed", Artifacts: &forgefake.Artifact{Text: "PK-gl"}},
				{ID: 8002, Name: "release", Stage: "deploy", Status: "manual"},
				{ID: 8003, Name: "e2e", Stage: "test", Status: "running"},
			}},
		}}},
	}}
}

// The app's CI reads and actions against the fake, both forges: the
// capability flags the reads carry, a retry that comes back as the new
// pending attempt under a new id, a GitLab play and job cancel,
// GitHub's run cancel refused once the run completed, and each forge's
// artifact download.
func TestCIActionsThroughTheAppsForgeCode(t *testing.T) {
	t.Parallel()
	r := newRig(t, ciActionsFixture())

	gh := gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "ci", Number: 4}
	pipeline, err := r.core.ListPRCIJobs(t.Context(), gh, nil, nil)
	if err != nil {
		t.Fatalf("GitHub ListPRCIJobs: %v", err)
	}
	unit := gitops.FindCIJob(pipeline, "3001")
	if unit == nil || unit.RunID != "300" || unit.Actions.Retry != gitops.CIActionAllowed ||
		pipeline.Actions != (gitops.CIPipelineActions{RetryFailed: gitops.CIActionAllowed, Artifacts: gitops.CIActionAllowed}) {
		t.Fatalf("GitHub flags = %+v / %+v", unit, pipeline.Actions)
	}
	if err := r.core.RunCIAction(t.Context(), gh.Repository(), gitops.CIActionRetryJob, "3001", pipeline); err != nil {
		t.Fatalf("GitHub retry: %v", err)
	}
	after, err := r.core.ListPRCIJobs(t.Context(), gh, &pipeline, nil)
	if err != nil {
		t.Fatal(err)
	}
	retries := jobsNamed(after, "CI", unit.Name)
	if len(retries) != 1 || retries[0].Status != gitops.CIStatusPending || retries[0].RunID != unit.RunID || !idAbove(t, retries[0].ID, unit.ID) ||
		gitops.FindCIJob(after, "3001") != nil || after.Actions.Cancel != gitops.CIActionAllowed {
		t.Fatalf("GitHub after the retry = %+v", after)
	}
	retry := retries[0]
	if _, err := r.engine.AdvanceJob(forgefake.JobAdvance{Forge: "github", Project: "acme/ci", ID: mustID(t, retry.ID), Status: "success"}); err != nil {
		t.Fatal(err)
	}
	done, err := r.core.ListPRCIJobs(t.Context(), gh, &after, nil)
	if err != nil || done.Status != gitops.CIStatusSuccess || done.Actions.Cancel != "" {
		t.Fatalf("GitHub once the attempt passed = %+v, %v", done, err)
	}
	err = r.core.RunCIAction(t.Context(), gh.Repository(), gitops.CIActionCancelPipeline, "", after)
	if sentence := explainedCI(t, gh.Repository(), gitops.ActionCancelCI, err); sentence.Kind != gitops.ForgeErrorRefused {
		t.Fatalf("cancel of a completed run = %+v", sentence)
	}
	artifacts, err := r.core.ListCIArtifacts(t.Context(), gh.Repository(), gitops.CIArtifactsRequest{RunIDs: []string{"300"}})
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("GitHub artifacts = %+v, %v", artifacts, err)
	}
	var buf bytes.Buffer
	if err := r.core.StreamCIArtifact(t.Context(), gh.Repository(), artifacts[0], &buf, 1<<20); err != nil || buf.String() != "PK-gh" {
		t.Fatalf("GitHub artifact = %q, %v", buf.String(), err)
	}

	gl := gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "ci", Number: 8}
	pipeline, err = r.core.ListPRCIJobs(t.Context(), gl, nil, nil)
	if err != nil {
		t.Fatalf("GitLab ListPRCIJobs: %v", err)
	}
	for id, want := range map[string]gitops.CIJobActions{
		"8001": {Retry: gitops.CIActionAllowed, Artifacts: gitops.CIActionAllowed},
		"8002": {Play: gitops.CIActionAllowed},
		"8003": {Cancel: gitops.CIActionAllowed},
	} {
		if got := gitops.FindCIJob(pipeline, id).Actions; got != want {
			t.Errorf("GitLab job %s = %+v, want %+v", id, got, want)
		}
	}
	if pipeline.Actions != (gitops.CIPipelineActions{Cancel: gitops.CIActionAllowed}) {
		t.Fatalf("GitLab pipeline actions = %+v", pipeline.Actions)
	}
	for _, step := range []struct {
		kind gitops.CIActionKind
		job  string
	}{{gitops.CIActionRetryJob, "8001"}, {gitops.CIActionPlayJob, "8002"}, {gitops.CIActionCancelJob, "8003"}} {
		if err := r.core.RunCIAction(t.Context(), gl.Repository(), step.kind, step.job, pipeline); err != nil {
			t.Fatalf("GitLab %s: %v", step.kind, err)
		}
	}
	after, err = r.core.ListPRCIJobs(t.Context(), gl, &pipeline, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := gitops.FindCIJob(pipeline, "8001")
	// The retry is in the same pipeline, under an id above the failed job's.
	if retries := jobsNamed(after, "test", old.Name); len(retries) != 1 || retries[0].Status != gitops.CIStatusPending || !idAbove(t, retries[0].ID, old.ID) ||
		after.ID == "" || after.ID != pipeline.ID || gitops.FindCIJob(after, "8001") != nil {
		t.Fatalf("GitLab retry = %+v", after)
	}
	if release := gitops.FindCIJob(after, "8002"); release.Status != gitops.CIStatusPending {
		t.Fatalf("played job = %+v", release)
	}
	if e2e := gitops.FindCIJob(after, "8003"); e2e.Status != gitops.CIStatusCanceled {
		t.Fatalf("canceled job = %+v", e2e)
	}
	err = r.core.RunCIAction(t.Context(), gl.Repository(), gitops.CIActionRetryJob, "8001", pipeline)
	if sentence := explainedCI(t, gl.Repository(), gitops.ActionRetryJob, err); sentence.Kind != gitops.ForgeErrorRefused || sentence.Message != "Job unit of grp/ci can no longer be retried. Refresh to see its current state." {
		t.Fatalf("a retry of the replaced job = %+v", sentence)
	}
	buf.Reset()
	if err := r.core.StreamCIArtifact(t.Context(), gl.Repository(), gitops.CIArtifact{JobID: "8001"}, &buf, 1<<20); err != nil || buf.String() != "PK-gl" {
		t.Fatalf("GitLab archive = %q, %v", buf.String(), err)
	}
	if err := r.core.StreamCIArtifact(t.Context(), gl.Repository(), gitops.CIArtifact{JobID: "8002"}, &buf, 1<<20); !errors.Is(err, gitops.ErrNoCIArtifacts) {
		t.Fatalf("a job without an archive = %v", err)
	}
	for _, inv := range r.engine.Invocations(0).Invocations {
		if inv.Unhandled {
			t.Errorf("unhandled: %s", inv.Detail)
		}
	}
}

func explainedCI(t *testing.T, repo gitops.RepoReference, action gitops.ForgeAction, err error) *gitops.ForgeActionError {
	t.Helper()
	if err == nil {
		t.Fatal("the action succeeded")
	}
	var sentence *gitops.ForgeActionError
	if !errors.As(gitops.ExplainCIError(repo, gitops.CIJobSubject(repo, "unit"), action, err), &sentence) {
		t.Fatalf("not explained: %v", err)
	}
	return sentence
}

func mustID(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	for _, c := range id {
		if c < '0' || c > '9' {
			t.Fatalf("id %q", id)
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// idAbove reports whether the decimal job id id is above old.
func idAbove(t *testing.T, id, old string) bool {
	t.Helper()
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		t.Fatalf("job id %q: %v", id, err)
	}
	o, err := strconv.ParseUint(old, 10, 64)
	if err != nil {
		t.Fatalf("job id %q: %v", old, err)
	}
	return n > o
}

// jobsNamed returns the jobs of stage in pipeline that are named name.
func jobsNamed(pipeline gitops.CIPipeline, stage, name string) []gitops.CIJob {
	var jobs []gitops.CIJob
	for _, s := range pipeline.Stages {
		if s.Name != stage {
			continue
		}
		for _, job := range s.Jobs {
			if job.Name == name {
				jobs = append(jobs, job)
			}
		}
	}
	return jobs
}
