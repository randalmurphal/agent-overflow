package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// promptWorkflow is the fixture every refresh test edits: one agent phase whose
// prompt is the inlined text a run freezes at start.
func promptWorkflow(id, prompt string) def.Workflow {
	workflow := onePhaseWorkflow(id, nil, []def.Route{{To: "done"}})
	workflow.Phases[0].Prompt = prompt
	return workflow
}

func writingPromptWorkflow(id, prompt string) def.Workflow {
	workflow := promptWorkflow(id, prompt)
	workflow.Phases[0].Access = def.AccessWrite
	return workflow
}

// frozenPrompt reads the prompt the run's persisted snapshot carries, which is
// what a crash rebuild — and every attempt after one — would render.
func frozenPrompt(t *testing.T, h *testHarness, itemID, phaseID string) string {
	t.Helper()
	stored, err := h.store.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(stored.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	phase, ok := findPhase(snapshot.Workflow, phaseID)
	if !ok {
		t.Fatalf("snapshot of %q has no phase %q", itemID, phaseID)
	}
	return phase.Prompt
}

// parkStuck runs the item's current attempt into the park an operator repairs:
// the phase reported it could not proceed, which is not a continuable reason, so
// a bare resume re-enters the phase.
func parkStuck(t *testing.T, h *testHarness, itemID string) {
	t.Helper()
	h.runner.complete(t, itemID, Outcome{Kind: OutcomeStuck, Envelope: stuckEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonStuck)
}

// The freeze is the default and the refresh is the exception: an edit made while
// a run is parked reaches it only when the resume asks for it, and once it does,
// the run record carries the edited definition so every later attempt — and a
// crash rebuild — renders it too.
func TestResumeRefreshRendersTheEditedPromptAndRefreezesIt(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": promptWorkflow("flow", "original prompt"),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkStuck(t, h, item.ID)
	h.definitions.edit("flow", promptWorkflow("flow", "edited prompt"))

	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 2 || starts[1].Phase.Prompt != "original prompt" {
		t.Fatalf("resume without a refresh rendered %q, want the frozen prompt", starts[1].Phase.Prompt)
	}
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the run's frozen one", frozen)
	}

	parkStuck(t, h, item.ID)
	if err := h.engine.Resume(item.ID, "", true); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	if len(starts) != 3 || starts[2].Phase.Prompt != "edited prompt" {
		t.Fatalf("refreshed resume rendered %q, want the edited prompt", starts[2].Phase.Prompt)
	}
	// The next turn is told its instructions changed; nothing else says so.
	if starts[2].Feedback == nil || !strings.Contains(starts[2].Feedback.Note, definitionRefreshNote) {
		t.Fatalf("refreshed attempt feedback = %+v, want the refresh note", starts[2].Feedback)
	}
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "edited prompt" {
		t.Fatalf("snapshot prompt = %q, want the re-frozen edited prompt", frozen)
	}
	// The re-freeze is not a new run: a wall-clock budget is measured against the
	// start it keeps.
	stored, err := h.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StartedAt != starts[0].Item.StartedAt {
		t.Fatalf("run start = %d, want the original %d", stored.StartedAt, starts[0].Item.StartedAt)
	}
}

// A refresh whose definition cannot carry the entry refuses before it writes:
// the run keeps the definition it froze, so resuming without one still works.
func TestResumeRefreshRefusesAPhaseTheEditedWorkflowDropped(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": promptWorkflow("flow", "original prompt"),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkStuck(t, h, item.ID)
	renamed := promptWorkflow("flow", "edited prompt")
	renamed.Phases[0].ID = "rebuild"
	h.definitions.edit("flow", renamed)

	err := h.engine.Resume(item.ID, "", true)
	if err == nil || !strings.Contains(err.Error(), `phase "work" is not in the workflow on disk`) {
		t.Fatalf("refresh error = %v, want the missing phase named", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonStuck)
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the refused refresh to have written nothing", frozen)
	}
	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
}

// A bare resume of a continuable park continues an attempt whose work was
// launched under the frozen definition, so the refresh is refused there and the
// refusal names the fresh entry that would accept it.
func TestResumeRefreshRefusesAContinuablePark(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": promptWorkflow("flow", "original prompt"),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.PauseItem(item.ID); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonPaused)
	h.definitions.edit("flow", promptWorkflow("flow", "edited prompt"))

	err := h.engine.Resume(item.ID, "", true)
	if err == nil || !strings.Contains(err.Error(), "--phase work") {
		t.Fatalf("refresh error = %v, want the fresh entry named", err)
	}
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the refused refresh to have written nothing", frozen)
	}
	// Naming the parked phase is the deliberate discard, and it takes the edit.
	if err := h.engine.Resume(item.ID, "work", true); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if starts[len(starts)-1].Phase.Prompt != "edited prompt" {
		t.Fatalf("targeted refresh rendered %q, want the edited prompt", starts[len(starts)-1].Phase.Prompt)
	}
}

// A run's work lives in the worktree it provisioned. A definition that no longer
// needs one would run every remaining phase in the project root, so the refresh
// is refused rather than silently relocating the run.
func TestResumeRefreshRefusesGivingUpTheRunsWorktree(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": writingPromptWorkflow("flow", "original prompt"),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// The runner provisions and records the workspace; this is that record.
	if err := h.store.UpdateWorkItemWorkspace(item.ID, "/tmp/wt-item", "ao/item", "main"); err != nil {
		t.Fatal(err)
	}
	parkStuck(t, h, item.ID)
	h.definitions.edit("flow", promptWorkflow("flow", "edited prompt"))

	err := h.engine.Resume(item.ID, "", true)
	if err == nil || !strings.Contains(err.Error(), "/tmp/wt-item") ||
		!strings.Contains(err.Error(), string(def.WorkspaceProjectRoot)) {
		t.Fatalf("refresh error = %v, want the workspace mismatch named", err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonStuck)
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the refused refresh to have written nothing", frozen)
	}
}

// The other direction is allowed: a read-only run has written nothing, and
// provisioning is lazy, so the phase this entry starts cuts the worktree the
// edited definition needs exactly as the run's first phase would have.
func TestResumeRefreshAdoptsANewlyWritingDefinition(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": promptWorkflow("flow", "original prompt"),
	}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if need := h.runner.started()[0].WorkspaceNeed; need != def.WorkspaceProjectRoot {
		t.Fatalf("initial workspace need = %q", need)
	}
	parkStuck(t, h, item.ID)
	h.definitions.edit("flow", writingPromptWorkflow("flow", "edited prompt"))

	if err := h.engine.Resume(item.ID, "", true); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if got := starts[len(starts)-1].WorkspaceNeed; got != def.WorkspaceWorktree {
		t.Fatalf("refreshed workspace need = %q, want %q", got, def.WorkspaceWorktree)
	}
}

// A rerun is a fresh phase entry, so it offers the same re-read — and the same
// default, which is the definition the run failed under.
func TestRerunFailedRefreshesTheDefinition(t *testing.T) {
	failedWhenFalse := def.Predicate{Eq: &def.Comparison{Ref: "work.ok", Value: false}}
	workflow := promptWorkflow("flow", "original prompt")
	workflow.Phases[0].Gate = def.Gate{Routes: []def.Route{
		{When: &failedWhenFalse, To: "failed"},
		{To: "done"},
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"flow": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	failRun := func() {
		t.Helper()
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		requireItemState(t, h.store, item.ID, StateFailed, ReasonCheckFailedGenuine)
	}
	failRun()
	edited := promptWorkflow("flow", "edited prompt")
	edited.Phases[0].Gate = workflow.Phases[0].Gate
	h.definitions.edit("flow", edited)

	if err := h.engine.RerunFailed(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if starts[len(starts)-1].Phase.Prompt != "original prompt" {
		t.Fatalf("rerun rendered %q, want the frozen prompt", starts[len(starts)-1].Phase.Prompt)
	}

	failRun()
	if err := h.engine.RerunFailed(item.ID, "try again", true); err != nil {
		t.Fatal(err)
	}
	starts = h.runner.started()
	last := starts[len(starts)-1]
	if last.Phase.Prompt != "edited prompt" {
		t.Fatalf("refreshed rerun rendered %q, want the edited prompt", last.Phase.Prompt)
	}
	// The rerun's own guidance and diagnosis survive the refresh note.
	if last.Feedback == nil || !strings.Contains(last.Feedback.Note, "try again") ||
		!strings.Contains(last.Feedback.Note, definitionRefreshNote) {
		t.Fatalf("refreshed rerun feedback = %+v", last.Feedback)
	}
	if frozen := frozenPrompt(t, h, item.ID, "work"); frozen != "edited prompt" {
		t.Fatalf("snapshot prompt = %q, want the re-frozen edited prompt", frozen)
	}
}

// The human-gate guard runs before the dispatch, so asking for a refresh is not
// a way past it: the gate's decision is still ResolveHumanGate's, and the run
// keeps the definition it froze.
func TestResumeRefreshDoesNotBypassTheHumanGateGuard(t *testing.T) {
	workflow := humanWorkflow()
	workflow.Phases[1].Prompt = "original prompt"
	h := newHarness(t, Config{}, map[string]def.Workflow{"human": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "human", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	parkAtHumanGate(t, h, item.ID)
	edited := humanWorkflow()
	edited.Phases[1].Prompt = "edited prompt"
	h.definitions.edit("human", edited)

	err := h.engine.Resume(item.ID, "", true)
	if err == nil || !strings.Contains(err.Error(), "ResolveHumanGate") {
		t.Fatalf("refresh error = %v, want the human-gate refusal", err)
	}
	if frozen := frozenPrompt(t, h, item.ID, "review"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the refused resume to have written nothing", frozen)
	}
}

// A called run provisions no workspace of its own (§9): it executes in the one
// its tree's ROOT froze. A re-read that turns it into a writing workflow would
// therefore run a writing phase in whatever the root provisioned, so it is
// refused and names the caller.
func TestResumeRefreshRefusesAWritingDefinitionForACalledRun(t *testing.T) {
	workflows := defaultCallWorkflows()
	workflows["child"] = childWorkflow("child", promptWorkflow("child", "original prompt").Phases[0])
	h := newCallHarness(t, workflows, nil)
	parent := startCaller(t, h)
	child := h.callChild(t, parent, "audit", 1)
	parkStuck(t, h, child.ID)
	h.definitions.edit("child", childWorkflow("child", writingPromptWorkflow("child", "edited prompt").Phases[0]))

	err := h.engine.Resume(child.ID, "", true)
	if err == nil || !strings.Contains(err.Error(), parent) ||
		!strings.Contains(err.Error(), string(def.WorkspaceWorktree)) {
		t.Fatalf("refresh error = %v, want the caller's workspace named", err)
	}
	if frozen := frozenPrompt(t, h, child.ID, "work"); frozen != "original prompt" {
		t.Fatalf("snapshot prompt = %q, want the refused refresh to have written nothing", frozen)
	}
}
