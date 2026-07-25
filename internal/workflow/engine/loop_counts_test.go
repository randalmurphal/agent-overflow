package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// The derivation tests below replay persisted attempt rows the way the engine
// writes them: one row per attempt, in insertion order, each carrying the gate
// trace of the decision that ended it. The row currently in flight has no trace
// yet, which is exactly what a gate evaluating its own attempt sees.

func traceRow(t *testing.T, phaseID string, attempt int, status string, decision def.RouteDecision) store.WorkItemPhaseContext {
	t.Helper()
	payload, err := json.Marshal(def.GateTrace{Decision: decision})
	if err != nil {
		t.Fatal(err)
	}
	return store.WorkItemPhaseContext{PhaseID: phaseID, Attempt: attempt, Status: status, GateTrace: payload}
}

func advanceRow(t *testing.T, phaseID string, attempt int, target string) store.WorkItemPhaseContext {
	t.Helper()
	return traceRow(t, phaseID, attempt, "completed", def.RouteDecision{
		Kind: def.DecisionAdvance, RouteIndex: 0, Target: target,
	})
}

func loopRow(t *testing.T, phaseID string, attempt, routeIndex int, target string) store.WorkItemPhaseContext {
	t.Helper()
	return traceRow(t, phaseID, attempt, "completed", def.RouteDecision{
		Kind: def.DecisionLoop, RouteIndex: routeIndex, Target: target,
		LoopEdge: def.GateEdgeKey(phaseID, routeIndex), Max: 1,
	})
}

// exhaustedRow is the park a spent loop bound produces: no target, so nothing
// explains where the run goes next.
func exhaustedRow(t *testing.T, phaseID string, attempt int) store.WorkItemPhaseContext {
	t.Helper()
	return traceRow(t, phaseID, attempt, "parked", def.RouteDecision{
		Kind: def.DecisionRetriesExhausted, RouteIndex: -1,
	})
}

// pendingRow is an attempt with no decision yet — in flight, or parked on a
// question or an execution failure.
func pendingRow(phaseID string, attempt int, status string) store.WorkItemPhaseContext {
	return store.WorkItemPhaseContext{PhaseID: phaseID, Attempt: attempt, Status: status}
}

func requireLoopCounts(t *testing.T, rows []store.WorkItemPhaseContext, want map[string]int) {
	t.Helper()
	counts, err := loopCounts("item", rows)
	if err != nil {
		t.Fatal(err)
	}
	for edge, wantCount := range want {
		if counts[edge] != wantCount {
			t.Fatalf("loop count %q = %d, want %d (all counts: %v)", edge, counts[edge], wantCount, counts)
		}
	}
	for edge, gotCount := range counts {
		if _, expected := want[edge]; !expected && gotCount != 0 {
			t.Fatalf("unexpected loop count %q = %d (all counts: %v)", edge, gotCount, counts)
		}
	}
}

// A forward entry into the loop's target restarts its budget; the loops taken
// before that entry are a spent epoch, not a running total.
func TestLoopCountsRestartOnForwardEntryIntoTheTarget(t *testing.T) {
	inner, outer := def.GateEdgeKey("check", 0), def.GateEdgeKey("verify", 0)
	rows := []store.WorkItemPhaseContext{
		advanceRow(t, "setup", 1, "build"),
		advanceRow(t, "build", 1, "check"),
		loopRow(t, "check", 1, 0, "build"),
		advanceRow(t, "build", 2, "check"),
		loopRow(t, "check", 2, 0, "build"),
		advanceRow(t, "build", 3, "check"),
	}
	requireLoopCounts(t, rows, map[string]int{inner: 2})

	// The outer loop re-enters the cycle head from outside: `setup` advances
	// into `build`, so the inner edge starts its next entry with a full budget.
	rows = append(rows,
		advanceRow(t, "check", 3, "verify"),
		loopRow(t, "verify", 1, 0, "setup"),
		advanceRow(t, "setup", 2, "build"),
		advanceRow(t, "build", 4, "check"),
		loopRow(t, "check", 4, 0, "build"),
		pendingRow("build", 5, "running"),
	)
	requireLoopCounts(t, rows, map[string]int{inner: 1, outer: 1})
}

// Two loop edges pointing at one phase must not clear each other, or the pair
// would iterate forever with both counters below their bound.
func TestLoopCountsSharedTargetEdgesDoNotResetEachOther(t *testing.T) {
	first, second := def.GateEdgeKey("build", 0), def.GateEdgeKey("check", 0)
	rows := []store.WorkItemPhaseContext{
		advanceRow(t, "plan", 1, "build"),
		loopRow(t, "build", 1, 0, "plan"),
		advanceRow(t, "plan", 2, "build"),
		advanceRow(t, "build", 2, "check"),
		loopRow(t, "check", 1, 0, "plan"),
		advanceRow(t, "plan", 3, "build"),
		loopRow(t, "build", 3, 0, "plan"),
		pendingRow("plan", 4, "running"),
	}
	requireLoopCounts(t, rows, map[string]int{first: 2, second: 1})
}

// A rerun re-enters the phase whose gate failed the run, which is a new run
// epoch: the edges aiming at that phase get their budget back.
func TestLoopCountsRerunAfterFailureRefreshesEdgesIntoTheRerunPhase(t *testing.T) {
	edge := def.GateEdgeKey("check", 0)
	failedByStatus := traceRow(t, "build", 2, "failed", def.RouteDecision{Kind: def.DecisionFailed, RouteIndex: 0})
	// A human gate whose approve target is `failed` records the same decision
	// on a completed row, so the decision — not the row status — is the marker.
	failedByDecision := traceRow(t, "build", 2, "completed", def.RouteDecision{Kind: def.DecisionFailed, RouteIndex: 0})
	for _, failed := range []store.WorkItemPhaseContext{failedByStatus, failedByDecision} {
		rows := []store.WorkItemPhaseContext{
			advanceRow(t, "build", 1, "check"),
			loopRow(t, "check", 1, 0, "build"),
			failed,
		}
		requireLoopCounts(t, rows, map[string]int{edge: 1})
		requireLoopCounts(t, append(rows, pendingRow("build", 3, "running")), nil)
	}
}

// Answering a question continues the same phase execution on the same provider
// thread. It is not a re-entry, so it must not hand back loop budget.
func TestLoopCountsAnswerContinuationKeepsSpentBudget(t *testing.T) {
	edge := def.GateEdgeKey("check", 0)
	requireLoopCounts(t, []store.WorkItemPhaseContext{
		advanceRow(t, "build", 1, "check"),
		loopRow(t, "check", 1, 0, "build"),
		pendingRow("build", 2, "parked"), // Question park: no decision.
		pendingRow("build", 3, "running"),
	}, map[string]int{edge: 1})
}

// Resuming a parked run at a named phase enters that phase from outside the
// cycle, so the edges aiming at it start over; resuming in place does not.
func TestLoopCountsHumanResumeRefreshesOnlyTheTargetedPhase(t *testing.T) {
	edge := def.GateEdgeKey("check", 0)
	parked := []store.WorkItemPhaseContext{
		advanceRow(t, "build", 1, "check"),
		loopRow(t, "check", 1, 0, "build"),
		advanceRow(t, "build", 2, "check"),
		exhaustedRow(t, "check", 2),
	}
	requireLoopCounts(t, append(parked, pendingRow("check", 3, "running")), map[string]int{edge: 1})
	requireLoopCounts(t, append(parked, pendingRow("build", 3, "running")), nil)
}

func TestLoopCountsIgnoreGateAttemptAbandonedByTakeover(t *testing.T) {
	edge := def.GateEdgeKey("review", 0)
	intervention, err := json.Marshal(TakeoverIntervention{Kind: TakeoverInterventionKind, At: 10})
	if err != nil {
		t.Fatal(err)
	}
	abandoned := loopRow(t, "review", 2, 0, "build")
	abandoned.Status, abandoned.Intervention = "parked", intervention
	requireLoopCounts(t, []store.WorkItemPhaseContext{
		loopRow(t, "review", 1, 0, "build"),
		abandoned,
	}, map[string]int{edge: 1})
}

// A takeover parks an attempt and its finalize turn resumes the same phase, so
// the pair must leave the budget of the entry they interrupt untouched.
func TestLoopCountsTakeoverFinalizeContinuesTheInterruptedEntry(t *testing.T) {
	edge := def.GateEdgeKey("check", 0)
	intervention, err := json.Marshal(TakeoverIntervention{Kind: TakeoverInterventionKind, At: 10})
	if err != nil {
		t.Fatal(err)
	}
	takenOver := pendingRow("build", 2, "parked")
	takenOver.Intervention = intervention
	requireLoopCounts(t, []store.WorkItemPhaseContext{
		advanceRow(t, "build", 1, "check"),
		loopRow(t, "check", 1, 0, "build"),
		takenOver,
		pendingRow("build", 3, "running"),
	}, map[string]int{edge: 1})
}

// A human decision recorded on a phase shares the intervention column with the
// takeover marker; only the takeover kind drops an attempt from the count.
func TestLoopCountsCountHumanInterventionAttempts(t *testing.T) {
	edge := def.GateEdgeKey("review", 0)
	rejected := loopRow(t, "review", 1, 0, "build")
	rejected.Intervention = json.RawMessage(`{"decision":"reject","note":"again"}`)
	requireLoopCounts(t, []store.WorkItemPhaseContext{rejected}, map[string]int{edge: 1})
}

func TestLoopCountsRejectUndecodableRows(t *testing.T) {
	broken := pendingRow("build", 1, "parked")
	broken.Intervention = json.RawMessage(`{"kind":`)
	if _, err := loopCounts("item", []store.WorkItemPhaseContext{broken}); err == nil ||
		!strings.Contains(err.Error(), "decode intervention for item/build/1") {
		t.Fatalf("intervention decode error = %v", err)
	}
	broken = pendingRow("build", 1, "completed")
	broken.GateTrace = json.RawMessage(`{"decision":`)
	if _, err := loopCounts("item", []store.WorkItemPhaseContext{broken}); err == nil ||
		!strings.Contains(err.Error(), "decode gate trace for item/build/1") {
		t.Fatalf("gate trace decode error = %v", err)
	}
}

// The engine-level tests below drive the real FSM: the derivation only matters
// if the rows the engine persists classify the way the walk expects.

func nestedLoopWorkflow() def.Workflow {
	return def.Workflow{ID: "nested", Phases: []def.Phase{
		agentPhase("setup", nil, []def.Route{{To: "build"}}),
		agentPhase("build", nil, []def.Route{{To: "check"}}),
		agentPhase("check", nil, []def.Route{{Loop: "build", Max: 1}, {To: "verify"}}),
		agentPhase("verify", nil, []def.Route{{Loop: "setup", Max: 1}, {To: "done"}}),
	}}
}

func TestNestedLoopGetsFreshInnerBudgetOnEveryOuterLap(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{"nested": nestedLoopWorkflow()}, []string{"project"}, nil)
	item := testItem("item", "project", "nested", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// Each lap spends the inner bound, falls through to `verify`, and loops
	// back to `setup`, which re-enters `build` from outside the inner cycle.
	wantPhases := []string{
		"setup", "build", "check", "build", "check", "verify",
		"setup", "build", "check", "build", "check", "verify",
	}
	for range wantPhases {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	starts := h.runner.started()
	if len(starts) != len(wantPhases) {
		t.Fatalf("starts = %+v, want %d", startedPhases(starts), len(wantPhases))
	}
	for index, phaseID := range wantPhases {
		if starts[index].Key.PhaseID != phaseID {
			t.Fatalf("start %d = %q, want %q (all: %v)", index, starts[index].Key.PhaseID, phaseID, startedPhases(starts))
		}
	}
}

func TestRerunAfterFailureRestoresLoopBudget(t *testing.T) {
	workflow := def.Workflow{ID: "rerun-loop", Phases: []def.Phase{
		agentPhase("build", nil, []def.Route{
			{When: &def.Predicate{Eq: &def.Comparison{Ref: "build.ok", Value: false}}, To: "failed"},
			{To: "check"},
		}),
		agentPhase("check", nil, []def.Route{{Loop: "build", Max: 1}, {To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"rerun-loop": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "rerun-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	// build -> check -> loop back to build, which then fails the run.
	for _, ok := range []bool{true, true, false} {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(ok)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	requireItemState(t, h.store, item.ID, StateFailed, ReasonCheckFailedGenuine)
	if err := h.engine.RerunFailed(item.ID); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []bool{true, true} {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(ok)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	// The rerun re-entered `build` from outside, so the check gate loops again
	// instead of falling through to `done` on a lifetime-spent bound.
	requireItemState(t, h.store, item.ID, StateRunning, "")
	want := []string{"build", "check", "build", "build", "check", "build"}
	if got := startedPhases(h.runner.started()); !equalStrings(got, want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
}

func TestAnsweredQuestionDoesNotRestoreLoopBudget(t *testing.T) {
	workflow := def.Workflow{ID: "answer-loop", Phases: []def.Phase{
		agentPhase("build", nil, []def.Route{{To: "check"}}),
		agentPhase("check", nil, []def.Route{{Loop: "build", Max: 1}, {To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"answer-loop": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "answer-loop", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 2; step++ {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	// The looped-back build attempt asks a question and is answered, which
	// continues the same attempt rather than re-entering the phase.
	if err := h.store.AttachWorkItemPhaseRun(item.ID, "build", 2, "thread-one", "/tmp/narrative.md"); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, item.ID, Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonQuestion)
	if err := h.engine.Answer(item.ID, "keep going"); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 2; step++ {
		h.runner.complete(t, item.ID, Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	want := []string{"build", "check", "build", "build", "check"}
	if got := startedPhases(h.runner.started()); !equalStrings(got, want) {
		t.Fatalf("starts = %v, want %v", got, want)
	}
}

func startedPhases(starts []RunRequest) []string {
	phases := make([]string, 0, len(starts))
	for _, start := range starts {
		phases = append(phases, start.Key.PhaseID)
	}
	return phases
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
