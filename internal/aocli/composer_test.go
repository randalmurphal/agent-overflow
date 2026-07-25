package aocli

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderComposerContextNamesTheSurfaceAndTheLiveState(t *testing.T) {
	block := RenderComposerContext(ComposerContext{
		ProjectName: "Agent Overflow", SessionReady: true,
		SharedDir: "/config/workflows", ProjectDir: "/config/projects/ao/workflows",
		Workflows: []ComposerWorkflow{
			{ID: "release", Name: "Release train", Scope: "project"},
			{ID: "audit", Name: "Dependency audit", Scope: "shared"},
		},
		Runs: []ComposerRun{
			{ItemID: "run-1", WorkflowID: "release", State: "running", PhaseID: "build"},
			{ItemID: "run-2", WorkflowID: "audit", State: "needs-human", Reason: "gate"},
		},
	})
	for _, want := range []string{
		"ao run start", "ao run list", "ao workflow schema", "--json",
		EnvEndpoint, EnvToken, EnvThreadID,
		"/config/workflows", "/config/projects/ao/workflows", "Agent Overflow",
		"audit (shared) — Dependency audit", "release (project) — Release train",
		"run-1 workflow=release state=running phase=build",
		"run-2 workflow=audit state=needs-human reason=gate",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
	// Workflows sort by id so the same project renders the same block twice.
	if strings.Index(block, "audit (shared)") > strings.Index(block, "release (project)") {
		t.Fatalf("workflows were not sorted by id:\n%s", block)
	}
	if strings.Contains(block, "no live session yet") {
		t.Fatalf("a ready session was described as absent:\n%s", block)
	}
}

func TestRenderComposerContextHandlesAnEmptyProject(t *testing.T) {
	block := RenderComposerContext(ComposerContext{SharedDir: "/config/workflows"})
	for _, want := range []string{
		"No workflows are configured here yet", "No runs are active in this project",
		"no live session yet", "(unnamed)", "(not created yet)",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
}

func TestComposerListsAreBounded(t *testing.T) {
	context := ComposerContext{SessionReady: true, SharedDir: "/config/workflows"}
	for i := 0; i < MaxComposerWorkflows+7; i++ {
		context.Workflows = append(context.Workflows, ComposerWorkflow{
			ID: fmt.Sprintf("wf-%02d", i), Name: "W", Scope: "shared",
		})
	}
	for i := 0; i < MaxComposerRuns+4; i++ {
		context.Runs = append(context.Runs, ComposerRun{
			ItemID: fmt.Sprintf("run-%02d", i), WorkflowID: "wf-00", State: "running",
		})
	}
	trimmed := TrimComposerLists(context)
	if len(trimmed.Workflows) != MaxComposerWorkflows || trimmed.WorkflowOverflow != 7 {
		t.Fatalf("workflows = %d, overflow = %d", len(trimmed.Workflows), trimmed.WorkflowOverflow)
	}
	if len(trimmed.Runs) != MaxComposerRuns || trimmed.RunOverflow != 4 {
		t.Fatalf("runs = %d, overflow = %d", len(trimmed.Runs), trimmed.RunOverflow)
	}
	block := RenderComposerContext(context)
	if !strings.Contains(block, "and 7 more") || !strings.Contains(block, "and 4 more") {
		t.Fatalf("the block did not say what it left out:\n%s", block)
	}
	// Truncation must be visible, not silent: the dropped entries are gone and
	// the block says how many.
	if strings.Contains(block, "wf-31") || strings.Contains(block, "run-13") {
		t.Fatalf("the block exceeded its bounds:\n%s", block)
	}
}
