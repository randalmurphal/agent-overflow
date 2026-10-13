package workflowtools

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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
		MCPServerName, "/config/workflows", "Agent Overflow",
		"/config/projects/ao/workflows   (this project only; shadows a shared id)",
		"audit (shared): Dependency audit", "release (project): Release train",
		"run-1 workflow=release state=running phase=build",
		"run-2 workflow=audit state=needs-human reason=gate",
		"When a run needs a human, the reason picks the fix:",
		"gate (decision=human) → run_resolve approve|reject",
		"question → run_resolve answer",
		"loop-limit-exhausted → run_control resume with phase_id",
		"resume continues and preserves finished work; resume with phase_id starts that phase over",
		"Deciding a park needs the `resolve` grant in a workflow phase; an interactive session like this one holds it already.",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q:\n%s", want, block)
		}
	}
	// Every tool the block names exists, and every tool exists in the block:
	// an agent copies a call out of it.
	for _, name := range ToolNames() {
		if !strings.Contains(block, name) {
			t.Errorf("block does not name %s", name)
		}
	}
	// Every call a tool row spells names a tool and only parameters its schema
	// declares.
	for _, row := range composerTools {
		for _, spelled := range strings.Split(row.left, " / ") {
			name, params, _ := strings.Cut(spelled, "(")
			spec, ok := lookupTool(strings.TrimSpace(name))
			if !ok {
				t.Errorf("row %q names %q, which is not a tool", row.left, name)
				continue
			}
			properties := spec.schema()["inputSchema"].(map[string]any)["properties"].(map[string]any)
			for _, param := range strings.Split(strings.TrimSuffix(params, ")"), ",") {
				param = strings.TrimSuffix(strings.TrimSpace(param), "?")
				if param == "" {
					continue
				}
				if _, ok := properties[param]; !ok {
					t.Errorf("row %q passes %q, which %s does not take", row.left, param, spec.name)
				}
			}
		}
	}
	for _, row := range composerRepair {
		_, call, _ := strings.Cut(row.left, "→ ")
		if name := strings.Fields(call)[0]; !slices.Contains(ToolNames(), name) {
			t.Errorf("repair row %q names %q, which is not a tool", row.left, name)
		}
	}
	// Every run_control action a repair row names is one the tool takes.
	for _, row := range composerRepair {
		if _, after, ok := strings.Cut(row.left, "run_control "); ok {
			action := strings.Fields(after)[0]
			if !slices.Contains(ControlActions, action) {
				t.Errorf("row %q names action %q, which run_control does not take", row.left, action)
			}
		}
	}
	// The block names no CLI verb: there is none.
	for _, gone := range []string{"agent-overflow run", "agent-overflow workflow", "agent-overflow memory", "--json"} {
		if strings.Contains(block, gone) {
			t.Fatalf("block still names the retired CLI (%q):\n%s", gone, block)
		}
	}
	// Both tables pad their left column at render time, so an edit to one row
	// cannot leave the block ragged in an agent's context window.
	for _, table := range [][]composerRow{composerTools, composerRepair} {
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
