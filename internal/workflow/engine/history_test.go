package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// historyLoopWorkflow is the shape the binding exists for: a fix phase and a
// reviewer that routes back to it. `fix` reads both its own prior rounds and
// the rulings that sent it back.
func historyLoopWorkflow(window int) def.Workflow {
	fix := agentPhase("fix", nil, []def.Route{{To: "review"}})
	fix.Outputs = map[string]def.Variable{"note": {Schema: def.JSONSchema{Type: "string"}}}
	fix.Inputs = map[string]def.Variable{
		"history.fix":    {Schema: def.JSONSchema{Type: "array"}, Window: window},
		"history.review": {Schema: def.JSONSchema{Type: "array"}, Window: window},
	}
	review := agentPhase("review", nil, []def.Route{
		{When: &def.Predicate{Eq: &def.Comparison{Ref: "review.ok", Value: true}}, To: "done"},
		{Loop: "fix", Max: def.LiteralBound(9)},
	})
	return def.Workflow{ID: "review-loop", Phases: []def.Phase{fix, review}}
}

func noteEnvelope(note string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"status":"done","outputs":{"note":%q},"question":null,"reason":null}`, note))
}

// runHistoryRounds drives `rounds` full fix→review laps, each one rejecting, and
// leaves the run at the start of the next `fix` attempt.
func runHistoryRounds(t *testing.T, h *testHarness, itemID string, rounds int) {
	t.Helper()
	for round := 1; round <= rounds; round++ {
		h.runner.complete(t, itemID, Outcome{Kind: OutcomeDone, Envelope: noteEnvelope(fmt.Sprintf("round-%d", round))})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		h.runner.complete(t, itemID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(false)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
}

func historySeries(t *testing.T, vars map[string]any, name string) []map[string]any {
	t.Helper()
	raw, ok := vars[name]
	if !ok {
		t.Fatalf("variable %q is not bound (bound: %v)", name, boundNames(vars))
	}
	series, ok := raw.([]any)
	if !ok {
		t.Fatalf("variable %q = %T, want a series", name, raw)
	}
	entries := make([]map[string]any, 0, len(series))
	for index, value := range series {
		entry, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("variable %q entry %d = %T, want an object", name, index, value)
		}
		entries = append(entries, entry)
	}
	return entries
}

func boundNames(vars map[string]any) []string {
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	return names
}

func requireHistoryEntry(t *testing.T, entry map[string]any, attempt int, status string) {
	t.Helper()
	if entry["attempt"] != attempt || entry["status"] != status {
		t.Fatalf("entry = %v, want attempt %d status %q", entry, attempt, status)
	}
}

func historyOutput(t *testing.T, entry map[string]any, name string) any {
	t.Helper()
	outputs, ok := entry["outputs"].(map[string]any)
	if !ok {
		t.Fatalf("entry %v carries no outputs", entry)
	}
	return outputs[name]
}

// The incident this binding exists for: a loop re-enters a phase and every
// round is blind to the ones before it. The re-entered attempt's context must
// carry the prior rounds' outputs, oldest first, without its own row.
func TestLoopReentryCarriesPriorRoundOutputs(t *testing.T) {
	workflow := historyLoopWorkflow(0)
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	runHistoryRounds(t, h, item.ID, 2)

	vars := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "fix", Attempt: 3}).Vars
	own := historySeries(t, vars, "history.fix")
	if len(own) != 2 {
		t.Fatalf("history.fix = %v, want the two prior attempts", own)
	}
	requireHistoryEntry(t, own[0], 1, "completed")
	requireHistoryEntry(t, own[1], 2, "completed")
	if got := historyOutput(t, own[0], "note"); got != "round-1" {
		t.Fatalf("oldest entry note = %v, want round-1", got)
	}
	if got := historyOutput(t, own[1], "note"); got != "round-2" {
		t.Fatalf("newest entry note = %v, want round-2", got)
	}
	rulings := historySeries(t, vars, "history.review")
	if len(rulings) != 2 {
		t.Fatalf("history.review = %v, want both rulings", rulings)
	}
	if got := historyOutput(t, rulings[0], "ok"); got != false {
		t.Fatalf("first ruling ok = %v, want false", got)
	}
	// The ordinary reference still resolves to the latest completed attempt
	// alone: the carve-out is scoped to the binding.
	if got := vars["fix.note"]; got != "round-2" {
		t.Fatalf("fix.note = %v, want the latest attempt only", got)
	}
	// The whole series renders as JSON, exactly as `{{units}}` does.
	rendered, err := def.Interpolate("{{history.fix}}", workflow.Phases[0].Inputs, vars)
	if err != nil {
		t.Fatalf("Interpolate: %v", err)
	}
	if !strings.Contains(rendered, `"round-1"`) || !strings.Contains(rendered, `"round-2"`) {
		t.Fatalf("rendered = %q", rendered)
	}
}

// The first attempt of a phase has no history, and an empty series is a real
// answer rather than an absent variable.
func TestFirstAttemptBindsAnEmptyHistory(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": historyLoopWorkflow(0)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	vars := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "fix", Attempt: 1}).Vars
	if entries := historySeries(t, vars, "history.fix"); len(entries) != 0 {
		t.Fatalf("history.fix = %v, want empty", entries)
	}
}

// A window trims from the OLD end: the rounds the next attempt is reacting to
// are the ones it keeps.
func TestHistoryWindowKeepsTheMostRecentAttempts(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": historyLoopWorkflow(2)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	runHistoryRounds(t, h, item.ID, 3)

	entries := historySeries(t, h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "fix", Attempt: 4}).Vars, "history.fix")
	if len(entries) != 2 {
		t.Fatalf("history.fix = %v, want a window of 2", entries)
	}
	requireHistoryEntry(t, entries[0], 2, "completed")
	requireHistoryEntry(t, entries[1], 3, "completed")
}

// A round that rested instead of completing is part of why a loop is where it
// is, so it appears — as a stub carrying what the attempt itself said, and no
// outputs, because nothing ratified any.
func TestNonCompletedAttemptAppearsAsAStub(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": historyLoopWorkflow(0)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "fix", 1, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonQuestion)
	if err := h.engine.Answer(item.ID, "use the safe option"); err != nil {
		t.Fatal(err)
	}

	entries := historySeries(t, h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "fix", Attempt: 2}).Vars, "history.fix")
	if len(entries) != 1 {
		t.Fatalf("history.fix = %v, want the parked attempt", entries)
	}
	requireHistoryEntry(t, entries[0], 1, "parked")
	if entries[0]["envelopeStatus"] != "question" || entries[0]["question"] != "Need input" {
		t.Fatalf("stub = %v, want the attempt's own question", entries[0])
	}
	if _, present := entries[0]["outputs"]; present {
		t.Fatalf("stub = %v, want no outputs on a non-completed attempt", entries[0])
	}
}

// The binding is bound last for the reason `units` is: a seed of the same name
// must not be able to replace the history a phase declared.
func TestSeededVariableCannotShadowAHistoryBinding(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": historyLoopWorkflow(0)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	item.Seeds = json.RawMessage(`{"history.fix":"there is no history"}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	runHistoryRounds(t, h, item.ID, 1)

	entries := historySeries(t, h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "fix", Attempt: 2}).Vars, "history.fix")
	if len(entries) != 1 {
		t.Fatalf("history.fix = %v, want the prior attempt", entries)
	}
	requireHistoryEntry(t, entries[0], 1, "completed")
}

// A phase that declares no binding pays nothing: the series is never built, so
// an unrelated phase's context does not grow with the run's length.
func TestPhaseWithoutABindingGetsNoHistory(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"review-loop": historyLoopWorkflow(0)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	runHistoryRounds(t, h, item.ID, 2)

	vars := h.runner.startFor(t, RunKey{ItemID: item.ID, PhaseID: "review", Attempt: 2}).Vars
	for name := range vars {
		if strings.HasPrefix(name, def.HistoryPrefix) {
			t.Fatalf("review bound %q; it declares no history binding", name)
		}
	}
}

func historyRow(attempt int, status string, envelope string) store.WorkItemPhaseContext {
	return store.WorkItemPhaseContext{
		PhaseID: "fix", Attempt: attempt, Status: status, OutputEnvelope: json.RawMessage(envelope),
	}
}

func TestHistoryEntriesExcludeTheCurrentAttempt(t *testing.T) {
	rows := []store.WorkItemPhaseContext{
		historyRow(1, "completed", `{"status":"done","outputs":{"note":"a"}}`),
		historyRow(2, "running", ""),
		{PhaseID: "review", Attempt: 1, Status: "completed", OutputEnvelope: json.RawMessage(`{"status":"done"}`)},
	}
	entries, err := historyEntries("item", "fix", rows, attemptRef{phaseID: "fix", attempt: 2}, def.DefaultHistoryWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want the one prior attempt of this phase", entries)
	}
	if entries[0].(map[string]any)["attempt"] != 1 {
		t.Fatalf("entry = %v, want attempt 1", entries[0])
	}
}

// The budget drops whole entries from the OLD end and each one says so. The
// newest prior attempt is carried whole regardless: one envelope can fill the
// budget by itself, and eliding the round the next attempt is reacting to would
// be worse than carrying no history at all.
func TestHistoryByteBudgetElidesOldestEntriesLoudly(t *testing.T) {
	bulk := strings.Repeat("x", def.MaxHistoryBytes/2)
	rows := []store.WorkItemPhaseContext{
		historyRow(1, "completed", fmt.Sprintf(`{"status":"done","outputs":{"note":%q}}`, bulk)),
		historyRow(2, "completed", fmt.Sprintf(`{"status":"done","outputs":{"note":%q}}`, bulk)),
		historyRow(3, "completed", fmt.Sprintf(`{"status":"done","outputs":{"note":%q}}`, bulk)),
	}
	entries, err := historyEntries("item", "fix", rows, attemptRef{phaseID: "fix", attempt: 4}, def.DefaultHistoryWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want every attempt to still appear", len(entries))
	}
	newest := entries[2].(map[string]any)
	if _, ok := newest["outputs"]; !ok {
		t.Fatalf("newest entry = %v, want its outputs carried whole", newest)
	}
	for index, entry := range entries[:2] {
		fields := entry.(map[string]any)
		if _, elided := fields["elided"]; !elided {
			t.Fatalf("entry %d = %v, want an elision that says so", index, fields)
		}
		if _, present := fields["outputs"]; present {
			t.Fatalf("entry %d = %v, want its content dropped", index, fields)
		}
		if fields["attempt"] != index+1 || fields["status"] != "completed" {
			t.Fatalf("entry %d = %v, want the attempt still identified", index, fields)
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	// One envelope over the budget is the documented worst case; two are not.
	if len(encoded) > def.MaxHistoryBytes+def.DefaultEnvelopeSizeCap {
		t.Fatalf("rendered %d bytes, want at most one envelope past the budget", len(encoded))
	}
}

// A prior attempt's envelope the engine cannot read back is corruption in a
// CHECK-constrained column it wrote itself, so it parks the run rather than
// quietly dropping a round from the phase's own account of itself.
func TestUnreadableHistoryEnvelopeIsAnError(t *testing.T) {
	rows := []store.WorkItemPhaseContext{historyRow(1, "completed", `"not an envelope"`)}
	if _, err := historyEntries("item", "fix", rows, attemptRef{}, def.DefaultHistoryWindow); err == nil {
		t.Fatal("an undecodable prior envelope must not pass silently")
	}
}

// An envelope this engine wrote and cannot read back is a WIRING error — the
// same reason a phase entry parks the identical failure under. It used to land
// in `agent-error` when the decode happened at gate evaluation instead of at
// entry: a shared bucket no repair verb reaches, for a failure no agent had
// anything to do with.
func TestHistoryDecodeFailureAtGateTimeParksWiringError(t *testing.T) {
	h := newHarness(t, Config{},
		map[string]def.Workflow{"review-loop": historyLoopWorkflow(6)}, []string{"project"}, nil)
	item := testItem("item", "project", "review-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// One full lap, so `fix` has a prior attempt of its own to bind.
	runHistoryRounds(t, h, item.ID, 1)

	// The prior round's envelope becomes unreadable AFTER this attempt entered,
	// so the failure lands on the gate's context build rather than the entry's.
	phases, err := h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := phaseAttempt(phases, "fix", 1)
	if !ok {
		t.Fatal("fix/1 has no row")
	}
	corrupt := json.RawMessage(`{"status":"done","outputs":{"note":"round-1"},"question":5}`)
	if err := h.store.CompleteWorkItemPhase(
		item.ID, "fix", 1, corrupt, first.GateTrace, "completed", "", first.EndedAt,
	); err != nil {
		t.Fatal(err)
	}

	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: noteEnvelope("round-2")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}

	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonWiringError)
	phases, err = h.store.ListWorkItemPhases(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	parked, ok := phaseAttempt(phases, "fix", 2)
	if !ok {
		t.Fatal("fix/2 has no row")
	}
	if !strings.Contains(parked.ParkCause, "decode phase history") {
		t.Fatalf("park cause = %q, want the undecodable history entry", parked.ParkCause)
	}
}
