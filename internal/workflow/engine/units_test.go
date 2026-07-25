package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func unitDoneEnvelope(value string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"status":"done","outputs":{"ok":true,"value":%q},"question":null,"reason":null}`, value,
	))
}

func failureEnvelope(reason string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"status":"stuck","outputs":null,"question":null,"reason":%q}`, reason,
	))
}

// startFanOut admits a fan-out run and returns its item id.
func startFanOut(t *testing.T, h *testHarness, workflowID string) string {
	t.Helper()
	if err := h.engine.StartItem(testItem("item", "project", workflowID, 0)); err != nil {
		t.Fatal(err)
	}
	return "item"
}

func newFanOutHarness(t *testing.T, width int) *testHarness {
	t.Helper()
	workflow := fanOutWorkflow("fan", width)
	return newHarness(t, Config{}, map[string]def.Workflow{"fan": workflow}, []string{"project"}, nil)
}

func TestFanOutLaunchesUnitsInOrderUnderProviderCapacity(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 1)
	item := startFanOut(t, h, "fan")

	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0"}) {
		t.Fatalf("started units = %v, want only the first under capacity 1", got)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitRunning,
		"work-unit-1": store.WorkItemUnitPending,
		"work-unit-2": store.WorkItemUnitPending,
		"work-join":   store.WorkItemUnitPending,
	})
	requireItemState(t, h.store, item, StateRunning, "")

	for index := 0; index < 3; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-unit-2", "work-join",
	}) {
		t.Fatalf("launch order = %v, want each unit in turn then the join", got)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-unit-1": store.WorkItemUnitDone,
		"work-unit-2": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitRunning,
	})

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

func TestFanOutHoldsQueuedUnitWhilePausedAndReplaysOnUnpause(t *testing.T) {
	h := newFanOutHarness(t, 2)
	h.limitProviderCapacity("project", 1)
	item := startFanOut(t, h, "fan")

	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0"}) {
		t.Fatalf("started units = %v, want no new start while paused", got)
	}
	requireItemState(t, h.store, item, StateRunning, "")

	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("started units = %v, want the held unit replayed on unpause", got)
	}
}

func TestFanOutHoldsJoinWhilePaused(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	if err := h.engine.Pause(true); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("v")})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	requireItemState(t, h.store, item, StateRunning, "")

	if err := h.engine.Pause(false); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); got[len(got)-1] != "work-join" {
		t.Fatalf("started units = %v, want the join released on unpause", got)
	}
}

func TestFanOutFailureStopsPendingLaunchesAndParksAfterInFlightUnits(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 2)
	item := startFanOut(t, h, "fan")

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("unit blew up")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	// The third unit must not take the failed unit's freed capacity, and the
	// in-flight second unit must be left alone.
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("started units = %v, want no launch after the failure", got)
	}
	for _, key := range h.runner.stopped() {
		if key.UnitID == "work-unit-1" {
			t.Fatalf("in-flight unit was stopped by a sibling failure: %+v", key)
		}
	}
	requireItemState(t, h.store, item, StateRunning, "")

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("b")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitFailed,
		"work-unit-1": store.WorkItemUnitDone,
		"work-unit-2": store.WorkItemUnitPending,
		"work-join":   store.WorkItemUnitPending,
	})
	phases, err := h.store.ListWorkItemPhases(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "parked" {
		t.Fatalf("phase attempt = %+v, want one parked attempt", phases)
	}
	if string(phases[0].OutputEnvelope) != string(failureEnvelope("unit blew up")) {
		t.Fatalf("park envelope = %s, want the failed unit's partial envelope", phases[0].OutputEnvelope)
	}
	h.requireNoHeldResources(t)
}

func TestFanOutRecordsEveryFailureAndParksOnce(t *testing.T) {
	h := newFanOutHarness(t, 3)
	item := startFanOut(t, h, "fan")
	h.limitProviderCapacity("project", 3)

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("first")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeStalled, Envelope: failureEnvelope("second")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	states := h.emitter.stateEvents(item)
	parks := 0
	for _, state := range states {
		if state.To == StateNeedsHuman {
			parks++
		}
	}
	if parks != 1 {
		t.Fatalf("park transitions = %d, want exactly one for two failed units", parks)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitFailed,
		"work-unit-1": store.WorkItemUnitFailed,
		"work-unit-2": store.WorkItemUnitPending,
		"work-join":   store.WorkItemUnitPending,
	})
}

func TestFanOutJoinReceivesUnitResultsAndDrivesTheGate(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	join := h.runner.startFor(t, unitKey(item, "work", 1, "work-join"))
	results, ok := join.Vars[def.UnitsVariable].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("join units variable = %#v, want two unit results", join.Vars[def.UnitsVariable])
	}
	for index, entry := range results {
		result, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("unit result %d = %#v, want an object", index, entry)
		}
		wantID := fmt.Sprintf("work-unit-%d", index)
		if result["id"] != wantID || result["status"] != store.WorkItemUnitDone {
			t.Fatalf("unit result %d = %#v, want %q done", index, result, wantID)
		}
		outputs, ok := result["outputs"].(map[string]any)
		if !ok || outputs["value"] != fmt.Sprintf("v%d", index) {
			t.Fatalf("unit result %d outputs = %#v, want the unit's own outputs", index, result["outputs"])
		}
	}
	if join.UnitKind != UnitJoin || join.Unit == nil || join.Unit.ID != "work-join" {
		t.Fatalf("join request = %+v, want the join unit definition", join)
	}

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
}

func TestFanOutJoinFailureIsAPhaseFailureNotAUnitFailure(t *testing.T) {
	h := newFanOutHarness(t, 1)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("join blew up")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonAgentError)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitFailed,
	})
	h.requireNoHeldResources(t)
}

func TestFanOutTeardownReleasesUnitCapacityOnCancel(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	if err := h.engine.Cancel(item); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateCancelled, ReasonInterrupted)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitFailed,
		"work-unit-1": store.WorkItemUnitFailed,
		"work-join":   store.WorkItemUnitPending,
	})
	stopped := map[string]bool{}
	for _, key := range h.runner.stopped() {
		stopped[key.UnitID] = true
	}
	if !stopped["work-unit-0"] || !stopped["work-unit-1"] {
		t.Fatalf("stopped keys = %+v, want both in-flight units stopped by teardown", h.runner.stopped())
	}
	h.requireNoHeldResources(t)
}

// TestFanOutUnitStartFailureParksBySentinel pins that a unit that cannot start
// is a phase-level setup failure, not the unit-failure policy: the frozen
// definition never produced runnable work, so there is nothing to retry unit by
// unit.
func TestFanOutUnitStartFailureParksBySentinel(t *testing.T) {
	h := newFanOutHarness(t, 2)
	h.runner.startErrs["item/work/1/work-unit-1"] = fmt.Errorf("provision unit: %w", ErrSetupFailed)
	if err := h.engine.StartItem(testItem("item", "project", "fan", 0)); err == nil {
		t.Fatal("a unit that could not start reported success")
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonSetupFailed)
	h.requireNoHeldResources(t)
}

func TestDynamicFanOutStampsOneUnitPerElement(t *testing.T) {
	workflow := def.Workflow{ID: "dyn", Phases: []def.Phase{
		dynamicFanOutPhase("work", "targets", "target", []def.Route{{To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"dyn": workflow}, []string{"project"}, nil)
	h.limitProviderCapacity("project", 3)
	item := testItem("item", "project", "dyn", 0)
	item.Seeds = json.RawMessage(`{"targets":["alpha","beta"]}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("started units = %v, want one per array element", got)
	}
	for index, want := range []string{"alpha", "beta"} {
		request := h.runner.startFor(t, unitKey(item.ID, "work", 1, fmt.Sprintf("work-unit-%d", index)))
		if request.Vars["target"] != want {
			t.Fatalf("unit %d element binding = %#v, want %q", index, request.Vars["target"], want)
		}
		if request.UnitIndex != index || request.UnitKind != UnitWork {
			t.Fatalf("unit %d request = %+v, want index %d work unit", index, request, index)
		}
	}
}

func TestDynamicFanOutOverEmptyArrayRunsTheJoinImmediately(t *testing.T) {
	workflow := def.Workflow{ID: "dyn", Phases: []def.Phase{
		dynamicFanOutPhase("work", "targets", "target", []def.Route{{To: "done"}}),
	}}
	h := newHarness(t, Config{}, map[string]def.Workflow{"dyn": workflow}, []string{"project"}, nil)
	item := testItem("item", "project", "dyn", 0)
	item.Seeds = json.RawMessage(`{"targets":[]}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-join"}) {
		t.Fatalf("started units = %v, want only the join for a zero-width fan-out", got)
	}
	join := h.runner.startFor(t, unitKey(item.ID, "work", 1, "work-join"))
	if results, ok := join.Vars[def.UnitsVariable].([]any); !ok || len(results) != 0 {
		t.Fatalf("join units variable = %#v, want an empty list", join.Vars[def.UnitsVariable])
	}
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
}

func TestDynamicFanOutOverNonArrayParksWiringError(t *testing.T) {
	workflow := def.Workflow{ID: "dyn", Phases: []def.Phase{
		dynamicFanOutPhase("work", "targets", "target", []def.Route{{To: "done"}}),
	}}
	for name, seeds := range map[string]string{
		"not an array": `{"targets":"alpha"}`,
		"absent":       `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{}, map[string]def.Workflow{"dyn": workflow}, []string{"project"}, nil)
			item := testItem("item", "project", "dyn", 0)
			item.Seeds = json.RawMessage(seeds)
			if err := h.engine.StartItem(item); err == nil {
				t.Fatal("unusable fan-out variable started silently")
			}
			requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonWiringError)
			if got := h.runner.startedUnitIDs(); len(got) != 0 {
				t.Fatalf("started units = %v, want none", got)
			}
			h.requireNoHeldResources(t)
		})
	}
}

// A fan-out attempt's phase-level continuations belong to its join: the join's
// envelope is the phase's, so the join's thread is what the phase attempt row
// carries and what Answer/CompleteTakeover resume on. The attempt is repaired
// rather than replaced, so the work units keep the results the join consolidates
// instead of every one of them re-running.
func TestAnswerOnFanOutRerunsOnlyTheJoinOnItsThread(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	// The app runner stamps the join's thread onto the phase attempt row the
	// moment it creates it. Without that, a fan-out that parks on a question has
	// no thread to answer on and Answer refuses outright.
	if err := h.store.AttachWorkItemPhaseRun(item, "work", 1, "join-thread", ""); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"),
		Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonQuestion)

	if err := h.engine.Answer(item, "use the second one"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-join", "work-join",
	}) {
		t.Fatalf("started units = %v, want only the join re-run", got)
	}
	// The attempt was reopened, not replaced: the same attempt number, the same
	// unit rows, and the finished work units still carrying their results.
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDone,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitRunning,
	})
	rerun := h.runner.startFor(t, unitKey(item, "work", 1, "work-join"))
	if rerun.PriorThreadID != "join-thread" {
		t.Fatalf("join prior thread = %q, want the thread the attempt parked on", rerun.PriorThreadID)
	}
	if rerun.Feedback == nil || rerun.Feedback.Note != "use the second one" {
		t.Fatalf("join feedback = %+v, want the human's answer", rerun.Feedback)
	}
	if rerun.UnitAttempt != 2 {
		t.Fatalf("join try = %d, want a fresh try so its narrative is not overwritten", rerun.UnitAttempt)
	}
	if rerun.FinalizeTakeover {
		t.Fatal("an answered question is not a takeover finalize")
	}
	units, ok := rerun.Vars[def.UnitsVariable].([]any)
	if !ok || len(units) != 2 {
		t.Fatalf("join units binding = %#v, want the two consolidated results", rerun.Vars[def.UnitsVariable])
	}

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

// A continuation is refused while the attempt still has units a human has not
// decided about: there is nothing coherent for a join to consolidate, and
// resuming would run it against results that are about to change.
func TestAnswerOnFanOutRefusesWhileUnitsRestFailed(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("v0")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeStuck, Envelope: failureEnvelope("no")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	if err := h.store.AttachWorkItemPhaseRun(item, "work", 1, "join-thread", ""); err != nil {
		t.Fatal(err)
	}
	// The park reason is unit-failed, not question, so Answer refuses on the
	// reason check before it ever reaches the join.
	if err := h.engine.Answer(item, "carry on"); err == nil {
		t.Fatal("Answer accepted a run parked on a failed unit")
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
}
