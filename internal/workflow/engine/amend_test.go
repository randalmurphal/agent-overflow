package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// seededWorkflow is the fixture the amend tests change a value on: one agent
// phase, and a workflow that declares the inputs an amendment is judged against.
func seededWorkflow(id string) def.Workflow {
	workflow := onePhaseWorkflow(id, nil, []def.Route{{To: "done"}})
	workflow.Inputs = map[string]def.Variable{
		"fix-budget": {Schema: def.JSONSchema{Type: "number"}},
		"label":      {Schema: def.JSONSchema{Type: "string"}, Optional: true},
	}
	return workflow
}

// startSeeded admits a run of `seededWorkflow` carrying `seeds`.
func startSeeded(t *testing.T, h *testHarness, seeds string) store.WorkItem {
	t.Helper()
	item := testItem("item", "project", "flow", 0)
	item.Seeds = json.RawMessage(seeds)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	return item
}

func newSeededHarness(t *testing.T) *testHarness {
	t.Helper()
	return newHarness(t, Config{}, map[string]def.Workflow{"flow": seededWorkflow("flow")},
		[]string{"project"}, nil)
}

// The seeds column is the durable evidence, and the log is the record that a
// human changed it — the same division `--refresh-def` makes, where the
// re-frozen snapshot is the evidence and the log says it happened. Without the
// line, a run that suddenly renders different inputs has no account of why.
func TestAmendSeedsIsLogged(t *testing.T) {
	sink := &recordingLog{}
	h := newHarness(t, Config{Log: sink}, map[string]def.Workflow{"flow": seededWorkflow("flow")},
		[]string{"project"}, nil)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	parkStuck(t, h, item.ID)
	if _, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)}); err != nil {
		t.Fatal(err)
	}

	amends := sink.matching(LogEventSeedAmend)
	if len(amends) != 1 {
		t.Fatalf("seed-amend log events = %+v, want exactly one", amends)
	}
	if amends[0].ItemID != item.ID || amends[0].PhaseID != "work" {
		t.Fatalf("log event = %+v, want the run and the phase it is parked in", amends[0])
	}
	if !strings.Contains(amends[0].Message, "fix-budget") ||
		!strings.Contains(amends[0].Message, string(SeedEffectNextAttempt)) {
		t.Fatalf("log message = %q, want the names changed and when they are read", amends[0].Message)
	}
	// A refused amendment logs nothing: the line means the record changed.
	if _, err := h.engine.AmendSeeds(item.ID, map[string]any{"nope": 1}); err == nil {
		t.Fatal("an undeclared seed was accepted")
	}
	if amends := sink.matching(LogEventSeedAmend); len(amends) != 1 {
		t.Fatalf("a refused amendment logged: %+v", amends)
	}
}

// The amendment is durable on the run row and the next attempt reads it: a
// phase's variable context is rebuilt from the row at every entry, which is the
// whole mechanism the verb rests on.
func TestAmendSeedsIsReadByTheNextAttempt(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2,"label":"first"}`)
	if got := startedVar(t, h, 0, "fix-budget"); got != "2" {
		t.Fatalf("first attempt fix-budget = %s, want 2", got)
	}
	parkStuck(t, h, item.ID)

	amendment, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err != nil {
		t.Fatal(err)
	}
	if len(amendment.Names) != 1 || amendment.Names[0] != "fix-budget" {
		t.Fatalf("amended names = %v, want just the one that changed", amendment.Names)
	}
	if amendment.Effect != SeedEffectNextAttempt {
		t.Fatalf("effect = %q, want %q", amendment.Effect, SeedEffectNextAttempt)
	}
	// Durable before anything resumes: an operator who amends and walks away has
	// changed the run, not one process's memory of it. The seed the amendment did
	// not name survives — the verb changes what it was told to change.
	requireSeeds(t, h, item.ID, `{"fix-budget":4,"label":"first"}`)

	if err := h.engine.Resume(item.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if got := startedVar(t, h, 1, "fix-budget"); got != "4" {
		t.Fatalf("resumed attempt fix-budget = %s, want the amended 4", got)
	}
}

// A run that is doing work is refused: an attempt reads its seeds when it
// starts, so the write would land under an attempt already rendering the old
// ones.
func TestAmendSeedsRefusesARunningRun(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)

	_, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err == nil || !strings.Contains(err.Error(), "the run is running") {
		t.Fatalf("amend error = %v, want a running-run refusal", err)
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

// A terminal run is refused for the opposite reason: nothing is left to read a
// seed, so accepting the write would record an intention nothing acts on.
func TestAmendSeedsRefusesATerminalRun(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")

	_, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err == nil || !strings.Contains(err.Error(), "no attempt left to read a seed") {
		t.Fatalf("amend error = %v, want a terminal-run refusal", err)
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

// An undeclared key is refused naming the ones that exist, because the fix is a
// name the caller has to be able to read off the refusal.
func TestAmendSeedsRefusesAnUndeclaredKeyNamingTheDeclaredOnes(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	parkStuck(t, h, item.ID)

	_, err := h.engine.AmendSeeds(item.ID, map[string]any{"fixbudget": float64(4)})
	if err == nil {
		t.Fatal("an undeclared seed was accepted")
	}
	for _, want := range []string{`"fixbudget" is not an input`, "fix-budget, label"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("amend error = %v, want it to contain %q", err, want)
		}
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

// A declared key with the wrong type is refused by the SAME validator a start
// seed passes through, so a value accepted at start and one accepted later
// cannot be judged by different rules.
func TestAmendSeedsTypechecksAgainstTheInputSchema(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	parkStuck(t, h, item.ID)

	_, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": "four"})
	if err == nil || !strings.Contains(err.Error(), "$.seeds.fix-budget") {
		t.Fatalf("amend error = %v, want the schema validator's own finding", err)
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

// Naming nothing is a refusal rather than a no-op write: an amendment that
// changed nothing and reported success is indistinguishable from one that
// applied.
func TestAmendSeedsRefusesAnEmptyChange(t *testing.T) {
	h := newSeededHarness(t)
	item := startSeeded(t, h, `{"fix-budget":2}`)
	parkStuck(t, h, item.ID)

	if _, err := h.engine.AmendSeeds(item.ID, nil); err == nil {
		t.Fatal("an amendment naming no seed was accepted")
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

// A fan-out park is repaired in place by a bare resume, and that attempt runs on
// the variables it persisted — so the amendment reports the fresh entry that
// will read it instead of claiming an effect it does not have.
func TestAmendSeedsReportsAFreshEntryForAnInPlaceRepair(t *testing.T) {
	workflow := fanOutWorkflow("flow", 2)
	workflow.Inputs = map[string]def.Variable{
		"fix-budget": {Schema: def.JSONSchema{Type: "number"}},
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"flow": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "flow", 0)
	item.Seeds = json.RawMessage(`{"fix-budget":2}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("unit blew up")})
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("v1")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonUnitFailed)

	amendment, err := h.engine.AmendSeeds(item.ID, map[string]any{"fix-budget": float64(4)})
	if err != nil {
		t.Fatal(err)
	}
	if amendment.Effect != SeedEffectFreshEntry {
		t.Fatalf("effect = %q, want %q for an attempt a bare resume repairs in place",
			amendment.Effect, SeedEffectFreshEntry)
	}
	if amendment.PhaseID != "work" {
		t.Fatalf("phase = %q, want the parked phase a fresh entry would name", amendment.PhaseID)
	}
	requireSeeds(t, h, item.ID, `{"fix-budget":4}`)
}

// A CALLED run owns its own seeds. They were evaluated from the caller's
// arguments at invocation, but the row is this run's and its remaining phases
// read it — so amending one is legal and takes effect on the child's own next
// attempt. What the caller's `args:` decide is the seeds of the NEXT invocation,
// which is why the app names the parent rather than the engine refusing here.
func TestAmendSeedsReachesACalledRunsRemainingPhases(t *testing.T) {
	child := def.Workflow{
		ID: "child",
		Inputs: map[string]def.Variable{
			"fix-budget": {Schema: def.JSONSchema{Type: "number"}},
		},
		Phases:  []def.Phase{agentPhase("work", nil, []def.Route{{To: "done"}})},
		Outputs: map[string]def.WorkflowOutput{"verdict": {From: "work.ok"}},
	}
	parent := def.Workflow{
		ID: "parent",
		Inputs: map[string]def.Variable{
			"fix-budget": {Schema: def.JSONSchema{Type: "number"}},
		},
		Phases: []def.Phase{
			callPhaseDef("wave", "child", map[string]string{"fix-budget": "fix-budget"}, 0,
				[]def.Route{{To: "done"}}),
		},
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"parent": parent, "child": child},
		[]string{"project"}, nil)
	item := testItem("item", "project", "parent", 0)
	item.Seeds = json.RawMessage(`{"fix-budget":2}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	childRun := h.callChild(t, item.ID, "wave", 1)
	// The call edge seeded the child from the caller's value, which is the state
	// an amendment then changes.
	requireSeeds(t, h, childRun.ID, `{"fix-budget":2}`)
	parkStuck(t, h, childRun.ID)

	amendment, err := h.engine.AmendSeeds(childRun.ID, map[string]any{"fix-budget": float64(9)})
	if err != nil {
		t.Fatal(err)
	}
	if amendment.Effect != SeedEffectNextAttempt {
		t.Fatalf("effect = %q, want %q", amendment.Effect, SeedEffectNextAttempt)
	}
	if err := h.engine.Resume(childRun.ID, "", false); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	last := starts[len(starts)-1]
	if last.Key.ItemID != childRun.ID {
		t.Fatalf("last runner start was %s, want the resumed child", last.Key.ItemID)
	}
	if rendered := renderVar(last.Vars["fix-budget"]); rendered != "9" {
		t.Fatalf("resumed child fix-budget = %s, want the amended 9", rendered)
	}
	// The caller is untouched: amending a child is not a way to edit the run that
	// invoked it, and the next wave still evaluates the parent's own value.
	requireSeeds(t, h, item.ID, `{"fix-budget":2}`)
}

func requireSeeds(t *testing.T, h *testHarness, itemID, want string) {
	t.Helper()
	stored, err := h.store.GetWorkItem(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonicalJSON(t, stored.Seeds); got != canonicalJSON(t, json.RawMessage(want)) {
		t.Fatalf("seeds of %q = %s, want %s", itemID, got, want)
	}
}

// canonicalJSON re-encodes through a map so an assertion compares values rather
// than key order or spacing.
func canonicalJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// startedVar reads one variable out of the Nth runner start, which is what that
// attempt actually rendered with.
func startedVar(t *testing.T, h *testHarness, index int, name string) string {
	t.Helper()
	starts := h.runner.started()
	if index >= len(starts) {
		t.Fatalf("only %d runner starts, want at least %d", len(starts), index+1)
	}
	return renderVar(starts[index].Vars[name])
}

func renderVar(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unencodable>"
	}
	return string(encoded)
}
