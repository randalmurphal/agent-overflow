package main

import (
	"testing"

	gitops "agent-overflow/internal/git"
	"agent-overflow/internal/harness/forgefake"
)

// A GitLab pipeline's rows reach the app's read through the engine's
// PipelineJobs: an external pipeline's commit status and a bridge as
// link-only rows, a build with its job id and log, pages past 100 rows.
func TestGitLabPipelineRowsThroughTheAppsForgeCode(t *testing.T) {
	r := newRig(t, forgefake.Fixture{Repos: []forgefake.Repo{{
		Forge: "gitlab", Project: "grp/sub/tool",
		Pipelines: []forgefake.Pipeline{
			{ID: 880, Source: "external", Jobs: []forgefake.Job{
				{ID: 8801, Name: "CodeRabbit", Kind: forgefake.JobKindExternal, Status: "success", TargetURL: "https://coderabbit.example/r/1"},
			}},
			{ID: 890, Jobs: []forgefake.Job{
				{ID: 8901, Name: "build", Stage: "build", Status: "success", Log: "built"},
				{ID: 8902, Name: "child", Stage: "deploy", Kind: forgefake.JobKindBridge, Status: "running", Downstream: "/grp/child/-/pipelines/891"},
			}},
			{ID: 900, Jobs: manyJobs(130)},
		},
	}}})
	repo := gitops.RepoReference{Forge: "gitlab", Host: "gitlab.com", Namespace: "grp/sub", Repo: "tool"}

	external, err := r.core.ReadPipeline(t.Context(), repo, "880", nil, nil)
	if err != nil {
		t.Fatalf("external ReadPipeline: %v", err)
	}
	if len(external.Stages) != 1 || external.Stages[0].Name != "external" || external.Stages[0].Status != gitops.CIStatusSuccess {
		t.Fatalf("external stages = %+v", external.Stages)
	}
	if status := external.Stages[0].Jobs[0]; status.ID != "" || status.LogsAvailable || status.URL != "https://coderabbit.example/r/1" || status.Name != "CodeRabbit" {
		t.Fatalf("external status = %+v", status)
	}

	mixed, err := r.core.ReadPipeline(t.Context(), repo, "890", nil, nil)
	if err != nil {
		t.Fatalf("bridge ReadPipeline: %v", err)
	}
	if len(mixed.Stages) != 2 || mixed.Stages[0].Name != "build" || mixed.Stages[1].Name != "deploy" {
		t.Fatalf("bridge pipeline stages = %+v", mixed.Stages)
	}
	if build := mixed.Stages[0].Jobs[0]; build.ID != "8901" || !build.LogsAvailable || build.URL != "https://gitlab.com/grp/sub/tool/-/jobs/8901" {
		t.Fatalf("build = %+v", build)
	}
	if bridge := mixed.Stages[1].Jobs[0]; bridge.ID != "" || bridge.LogsAvailable || bridge.URL != "https://gitlab.com/grp/child/-/pipelines/891" || bridge.Status != gitops.CIStatusRunning {
		t.Fatalf("bridge = %+v", bridge)
	}
	if log, err := streamLog(t, r.core, repo, gitops.CIJobLogRequest{JobID: "8901"}); err != nil || log.text != "built" {
		t.Fatalf("build trace = %+v, %v", log, err)
	}

	before := len(r.engine.Invocations(0).Invocations)
	long, err := r.core.ReadPipeline(t.Context(), repo, "900", nil, nil)
	if err != nil || len(long.Stages) != 1 || len(long.Stages[0].Jobs) != 130 {
		t.Fatalf("long ReadPipeline = %+v, %v", long.Stages, err)
	}
	pages := 0
	for _, inv := range r.engine.Invocations(0).Invocations[before:] {
		if inv.Unhandled {
			t.Fatalf("unhandled: %+v", inv)
		}
		if inv.Route == "glab graphql PipelineJobs" {
			pages++
		}
	}
	if pages != 2 {
		t.Fatalf("PipelineJobs pages = %d, want 2", pages)
	}
}
