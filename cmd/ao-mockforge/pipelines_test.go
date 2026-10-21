package main

import (
	"strings"
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

func pipelinesFixture() forgefake.Fixture {
	return forgefake.Fixture{Repos: []forgefake.Repo{
		{Forge: "github", Project: "acme/pipes", Pulls: []forgefake.Pull{
			{Number: 5, Title: "Feature", HeadRef: "feature", HeadSHA: strings.Repeat("5", 40),
				CI: &forgefake.Pipeline{ID: 300, Name: "CI", Jobs: []forgefake.Job{
					{ID: 3001, Name: "unit", Status: "running", Steps: []forgefake.Step{{Name: "test", Status: "running", StartedAt: "2026-01-01T00:00:01Z"}}},
					{ID: 3002, Name: "lint", Status: "success"},
				}}},
		}, Pipelines: []forgefake.Pipeline{
			{ID: 100, Name: "CI", Ref: "main", Title: "Merge #4", CreatedAt: "2026-10-01T00:00:00Z", Jobs: []forgefake.Job{{ID: 1001, Name: "unit", Status: "failed", Log: "boom"}}},
			{ID: 200, Name: "Release", RefKind: "tag", Ref: "v1.0.0", Jobs: []forgefake.Job{{ID: 2001, Name: "publish", Status: "success"}}},
		}},
		{Forge: "gitlab", Project: "grp/pipes", ID: 6600, Pulls: []forgefake.Pull{
			{Number: 9, Title: "Feature", HeadRef: "feature", HeadSHA: strings.Repeat("9", 40),
				CI: &forgefake.Pipeline{ID: 30, Jobs: []forgefake.Job{{ID: 3101, Name: "unit", Stage: "test", Status: "pending"}}}},
		}, Pipelines: []forgefake.Pipeline{
			{ID: 10, Ref: "main", Source: "schedule", Jobs: []forgefake.Job{{ID: 1101, Name: "nightly", Status: "success", Log: "fine"}}},
			{ID: 20, RefKind: "tag", Ref: "v2.0.0", Jobs: []forgefake.Job{
				{ID: 2101, Name: "build", Stage: "build", Status: "success"}, {ID: 2102, Name: "ship", Stage: "deploy", Status: "failed"},
			}},
		}},
	}}
}

// The pipelines of a repository on any ref list and read through the
// app's own forge code, and a pull's CI read names the ref and commit it
// ran on.
func TestPipelinesThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, pipelinesFixture())
	gh := gitops.RepoReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "pipes"}
	runs, err := r.core.ListPipelines(t.Context(), gh, gitops.PipelineListFilter{})
	if err != nil || len(runs) != 3 {
		t.Fatalf("gh ListPipelines = %+v, %v", runs, err)
	}
	if pr := runs[0]; pr.ID != "300" || pr.RefKind != gitops.PipelineRefMergeRequest || pr.MRNumber != 5 || pr.Ref != "feature" ||
		pr.Status != gitops.CIStatusRunning || pr.SHA != strings.Repeat("5", 40) || pr.Source != "pull_request" || pr.Name != "CI" ||
		pr.URL != "https://github.com/acme/pipes/actions/runs/300" {
		t.Errorf("gh pull run = %+v", pr)
	}
	if tag := runs[1]; tag.RefKind != gitops.PipelineRefTag || tag.Ref != "v1.0.0" || tag.Status != gitops.CIStatusSuccess {
		t.Errorf("gh release run = %+v", tag)
	}
	if push := runs[2]; push.RefKind != gitops.PipelineRefBranch || push.Title != "Merge #4" || push.Status != gitops.CIStatusFailed || push.CreatedAt != "2026-10-01T00:00:00Z" {
		t.Errorf("gh push run = %+v", push)
	}
	failed, err := r.core.ListPipelines(t.Context(), gh, gitops.PipelineListFilter{Ref: "main", Status: gitops.CIStatusFailed, Limit: 5})
	if err != nil || len(failed) != 1 || failed[0].ID != "100" {
		t.Fatalf("gh filtered ListPipelines = %+v, %v", failed, err)
	}
	run, err := r.core.ReadPipeline(t.Context(), gh, "300", nil, []string{"3001"})
	if err != nil || run.ID != "300" || run.Ref != "feature" || run.SHA != strings.Repeat("5", 40) || len(run.Stages) != 1 || run.Stages[0].Name != "CI" {
		t.Fatalf("gh ReadPipeline = %+v, %v", run, err)
	}
	if jobs := run.Stages[0].Jobs; len(jobs) != 2 || len(jobs[0].Steps) != 1 || jobs[1].Steps != nil || !jobs[0].LogsAvailable || run.Status != gitops.CIStatusRunning {
		t.Errorf("gh run jobs = %+v", jobs)
	}
	log, err := streamLog(t, r.core, gh, gitops.CIJobLogRequest{JobID: "1001"})
	if err != nil || !strings.HasSuffix(log.text, "boom") {
		t.Errorf("gh repository pipeline log = %+v, %v", log, err)
	}
	ghCI, err := r.core.ListPRCIJobs(t.Context(), gitops.PRReference{Forge: "github", Host: "github.com", Namespace: "acme", Repo: "pipes", Number: 5}, nil, nil)
	if err != nil || ghCI.Ref != "feature" || ghCI.SHA != strings.Repeat("5", 40) || ghCI.ID != "" {
		t.Errorf("gh PR CI = %+v, %v", ghCI, err)
	}

	gl := gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "pipes"}
	pipelines, err := r.core.ListPipelines(t.Context(), gl, gitops.PipelineListFilter{})
	if err != nil || len(pipelines) != 3 {
		t.Fatalf("glab ListPipelines = %+v, %v", pipelines, err)
	}
	if mr := pipelines[0]; mr.ID != "30" || mr.RefKind != gitops.PipelineRefMergeRequest || mr.MRNumber != 9 || mr.Status != gitops.CIStatusPending ||
		mr.Source != "merge_request_event" || mr.SHA != strings.Repeat("9", 40) || mr.URL != "https://gitlab.com/grp/pipes/-/pipelines/30" {
		t.Errorf("glab merge request pipeline = %+v", mr)
	}
	if tag := pipelines[1]; tag.RefKind != gitops.PipelineRefTag || tag.Ref != "v2.0.0" {
		t.Errorf("glab tag pipeline = %+v (the tag scope read marks it)", tag)
	}
	if nightly := pipelines[2]; nightly.Source != "schedule" || nightly.Status != gitops.CIStatusSuccess {
		t.Errorf("glab scheduled pipeline = %+v", nightly)
	}
	onMain, err := r.core.ListPipelines(t.Context(), gl, gitops.PipelineListFilter{Ref: "main", Status: gitops.CIStatusSuccess})
	if err != nil || len(onMain) != 1 || onMain[0].ID != "10" {
		t.Fatalf("glab filtered ListPipelines = %+v, %v", onMain, err)
	}
	pipeline, err := r.core.ReadPipeline(t.Context(), gl, "20", nil, nil)
	if err != nil || pipeline.ID != "20" || pipeline.Ref != "v2.0.0" || pipeline.Status != gitops.CIStatusFailed || len(pipeline.Stages) != 2 ||
		pipeline.Stages[0].Name != "build" || pipeline.Stages[1].Status != gitops.CIStatusFailed {
		t.Fatalf("glab ReadPipeline = %+v, %v", pipeline, err)
	}
	if log, err := streamLog(t, r.core, gl, gitops.CIJobLogRequest{JobID: "1101"}); err != nil || log.text != "fine" {
		t.Errorf("glab repository pipeline log = %+v, %v", log, err)
	}
	glCI, err := r.core.ListPRCIJobs(t.Context(), gitops.PRReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp", Repo: "pipes", Number: 9}, nil, nil)
	if err != nil || glCI.ID != "30" || glCI.Ref != "refs/merge-requests/9/head" || glCI.SHA != strings.Repeat("9", 40) {
		t.Errorf("glab PR CI = %+v, %v", glCI, err)
	}
	expectHandled(t, r)
}
