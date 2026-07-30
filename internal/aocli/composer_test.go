package aocli

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderComposerContextNamesTheSurfaceAndTheLiveState(t *testing.T) {
	block := RenderComposerContext(ComposerContext{
		ProjectName: "Agent Overflow", ProjectSlug: "agent-overflow",
		SessionReady: true, CommandOnPath: true,
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
		"agent-overflow run start", "agent-overflow run list", "agent-overflow workflow list|validate", "--json",
		EnvEndpoint, EnvToken, EnvThreadID,
		"/config/workflows", "Agent Overflow",
		// The project scope line names the slug --project takes: the offline
		// commands cannot infer it, so a reader who has only this block must
		// still be able to write the flag.
		"/config/projects/ao/workflows   (this project only — --project agent-overflow; shadows a shared id)",
		"audit (shared) — Dependency audit", "release (project) — Release train",
		"run-1 workflow=release state=running phase=build",
		"run-2 workflow=audit state=needs-human reason=gate",
		// The repair map: a reason an agent can act on names its verb, and the
		// ones only a human can settle say that rather than being omitted — an
		// omission reads as "there must be a verb I haven't found".
		"When a run needs a human, the reason picks the fix:",
		"paused|interrupted|checkpoint → run resume",
		"unit-failed → run retry-failed-units (retry-unit for one)",
		"state failed → run rerun",
		"gate|question → a human decides in the app; surface it, don't answer it",
		"`run status` names a run's failed units and its parent; any other reason names its own cause",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
	// The block names the app binary, never the retired `ao` one: an agent that
	// copies a line out of it has to get a command that exists.
	for _, gone := range []string{"  ao ", "`ao`", "`ao "} {
		if strings.Contains(block, gone) {
			t.Fatalf("block still names the retired `ao` command (%q):\n%s", gone, block)
		}
	}
	// Both tables pad their left column at render time, so an edit to one row
	// cannot leave the block ragged in an agent's context window.
	for _, table := range [][]composerRow{composerCommands, composerRepair} {
		width := 0
		for _, row := range table {
			width = max(width, utf8.RuneCountInString(row.left))
		}
		for _, row := range table {
			if !strings.Contains(block, "  "+row.left+strings.Repeat(" ", width-utf8.RuneCountInString(row.left))+"   "+row.right) {
				t.Fatalf("row %q was not padded into one column:\n%s", row.left, block)
			}
		}
	}
	// Workflows sort by id so the same project renders the same block twice.
	if strings.Index(block, "audit (shared)") > strings.Index(block, "release (project)") {
		t.Fatalf("workflows were not sorted by id:\n%s", block)
	}
	if strings.Contains(block, "no live session yet") {
		t.Fatalf("a ready session was described as absent:\n%s", block)
	}
	if strings.Contains(block, "could not publish") {
		t.Fatalf("a reachable command was described as unpublished:\n%s", block)
	}
}

// A boot that could not publish the command says so in the one place an agent
// reads before running it, rather than leaving "command not found" as the
// first news of it.
func TestRenderComposerContextSaysWhenTheCommandIsNotOnPath(t *testing.T) {
	block := RenderComposerContext(ComposerContext{
		ProjectName: "Agent Overflow", SessionReady: true, CommandOnPath: false,
		SharedDir: "/config/workflows",
	})
	if !strings.Contains(block, "could not publish `agent-overflow` on this session's PATH") {
		t.Fatalf("block did not report the unpublished command:\n%s", block)
	}
}

func TestRenderComposerContextHandlesAnEmptyProject(t *testing.T) {
	block := RenderComposerContext(ComposerContext{SharedDir: "/config/workflows"})
	for _, want := range []string{
		"No workflows are configured here yet", "No runs are active in this project",
		"no live session yet", "(unnamed)", "(not created yet)",
		// With no slug resolved the flag is still named, as a placeholder: the
		// alternative is a reader who does not know the flag exists.
		"--project <slug>",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
}

func TestComposerListsAreBounded(t *testing.T) {
	context := ComposerContext{SessionReady: true, CommandOnPath: true, SharedDir: "/config/workflows"}
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
