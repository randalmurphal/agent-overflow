package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// The seed write behind `agent-overflow run amend`. The engine owns the rules
// (see internal/workflow/engine/amend_test.go); what these drive is the app half
// — that the verb reaches them through a real engine, that its refusals arrive
// whole, and that the read verbs afterwards report the change.

// amendWorkflowInputs declares what an amendment of this fixture's run may name.
const amendWorkflowInputs = `
inputs:
  fix-budget:
    schema:
      type: number
  label:
    optional: true
    schema:
      type: string`

// newAmendFixture boots a run that PARKS: the check is bound to a binary that
// does not exist, so the phase cannot start and the run rests `setup-failed` —
// a park with nothing to continue, which is the ordinary shape an operator
// amends and resumes.
func newAmendFixture(t *testing.T) (*toolWorkflowFixture, store.WorkItem) {
	t.Helper()
	fixture := newToolWorkflowFixture(t, cliToolPhase+amendWorkflowInputs)
	fixture.writeProfile(t, map[string][]string{
		"verify": {filepath.Join(t.TempDir(), "does-not-exist")},
	}, nil, "")
	item, err := fixture.app.WorkflowStartRun(
		fixture.project.ID, "tool-flow", "shared", "amend me",
		json.RawMessage(`{"fix-budget":2,"label":"first"}`), nil, "", false,
	)
	if err != nil {
		t.Fatal(err)
	}
	waitForWorkflowItem(t, fixture.app, item.ID, engine.StateNeedsHuman, engine.ReasonSetupFailed)
	return fixture, item
}

// `run amend`'s app half. The engine's rules are its own (see
// internal/workflow/engine/amend_test.go); what this asserts is that the verb
// reaches them through a real engine, and that the amendment is what the read
// verbs afterwards report — a change no surface showed would be a change an
// operator has no way to confirm.
func TestWorkflowAgentAmendSeedsIsVisibleToTheReadVerbs(t *testing.T) {
	fixture, item := newAmendFixture(t)
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, "thread-1"))

	result, err := fixture.app.WorkflowAgentAmendSeeds(ctx,
		WorkflowAgentAmendSeedsInput{ItemID: item.ID, Seeds: json.RawMessage(`{"fix-budget":4}`)})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Names) != 1 || result.Names[0] != "fix-budget" {
		t.Fatalf("names = %v", result.Names)
	}
	if result.Effect != string(engine.SeedEffectNextAttempt) {
		t.Fatalf("effect = %q", result.Effect)
	}
	if result.AppliesNote == "" {
		t.Fatal("the amendment did not say when the run will read it")
	}
	if result.CallerNote != "" {
		t.Fatalf("a root run's amendment named a caller: %q", result.CallerNote)
	}

	status, err := fixture.app.WorkflowAgentRunStatus(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seeds map[string]any
	if err := json.Unmarshal(status.Seeds, &seeds); err != nil {
		t.Fatal(err)
	}
	if seeds["fix-budget"] != float64(4) || seeds["label"] != "first" {
		t.Fatalf("run status seeds = %v, want the amended value beside the untouched one", seeds)
	}
	inspected, err := fixture.app.WorkflowAgentInspectRun(ctx,
		WorkflowAgentInspectInput{ItemID: item.ID})
	if err != nil {
		t.Fatal(err)
	}
	var inspectedSeeds map[string]any
	if err := json.Unmarshal(inspected.Run.Seeds, &inspectedSeeds); err != nil {
		t.Fatal(err)
	}
	if inspectedSeeds["fix-budget"] != float64(4) {
		t.Fatalf("run inspect seeds = %s", inspected.Run.Seeds)
	}
}

// The app's refusals are the engine's, forwarded whole: an undeclared key names
// the declared ones, and nothing is written.
func TestWorkflowAgentAmendSeedsForwardsTheEnginesRefusal(t *testing.T) {
	fixture, item := newAmendFixture(t)
	ctx := transport.WithCallerScope(context.Background(), interactiveScope(fixture, "thread-1"))

	_, err := fixture.app.WorkflowAgentAmendSeeds(ctx,
		WorkflowAgentAmendSeedsInput{ItemID: item.ID, Seeds: json.RawMessage(`{"fixbudget":4}`)})
	if err == nil {
		t.Fatal("an undeclared seed was accepted")
	}
	if !strings.Contains(err.Error(), "fix-budget, label") {
		t.Fatalf("error = %v, want the declared inputs named", err)
	}
	stored, err := fixture.app.store.GetWorkItem(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.Seeds), "fixbudget") {
		t.Fatalf("a refused amendment still wrote: %s", stored.Seeds)
	}
}

// A phase may amend only the runs it started — the same rule every other
// control verb takes, and deliberately not the wider read rule `introspect`
// grants: changing a run's inputs is acting on it.
func TestWorkflowAgentAmendSeedsIsConfinedToWhatAPhaseStarted(t *testing.T) {
	fixture, item := newAmendFixture(t)
	ctx := transport.WithCallerScope(context.Background(),
		phaseScope(fixture, "supervisor", def.GrantIntrospect, def.GrantStartRun))

	_, err := fixture.app.WorkflowAgentAmendSeeds(ctx,
		WorkflowAgentAmendSeedsInput{ItemID: item.ID, Seeds: json.RawMessage(`{"fix-budget":4}`)})
	if err == nil {
		t.Fatal("a phase amended a run it did not start")
	}
	if !strings.Contains(err.Error(), "may only act on the runs it started") {
		t.Fatalf("error = %v", err)
	}
}
