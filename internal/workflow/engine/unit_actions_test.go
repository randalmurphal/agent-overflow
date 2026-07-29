package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// parkOneFailedUnit drives a two-unit fan-out to the unit-failed park every
// recovery action starts from: unit 0 failed, unit 1 done.
func parkOneFailedUnit(t *testing.T, h *testHarness) string {
	t.Helper()
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("unit blew up")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("b")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	return item
}

func TestRetryUnitResumesTheAttemptWithoutRerunningFinishedUnits(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := parkOneFailedUnit(t, h)

	if err := h.engine.RetryUnit(item, "work-unit-0", "the file moved"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-unit-0",
	}) {
		t.Fatalf("started units = %v, want only the retried unit restarted", got)
	}
	retried := h.runner.startFor(t, unitKey(item, "work", 1, "work-unit-0"))
	if retried.Feedback == nil || retried.Feedback.Note != "the file moved" {
		t.Fatalf("retry feedback = %+v, want the human's note", retried.Feedback)
	}
	rows, err := h.store.ListWorkItemPhaseUnits(item, "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		switch row.UnitID {
		case "work-unit-0":
			if row.Status != store.WorkItemUnitRunning || row.UnitAttempt != 2 || row.Feedback != "the file moved" {
				t.Fatalf("retried unit row = %+v, want a second running attempt", row)
			}
		case "work-unit-1":
			if row.Status != store.WorkItemUnitDone || row.UnitAttempt != 1 {
				t.Fatalf("finished unit row = %+v, want its first attempt preserved", row)
			}
		}
	}
	phases, err := h.store.ListWorkItemPhases(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "running" || phases[0].EndedAt != 0 ||
		len(phases[0].OutputEnvelope) != 0 {
		t.Fatalf("phase attempts = %+v, want the same attempt reopened", phases)
	}

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("a")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if got := h.runner.startedUnitIDs(); got[len(got)-1] != "work-join" {
		t.Fatalf("started units = %v, want the join after the retry completed", got)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

func TestRetryUnitLeavesTheRunParkedWhileAnotherUnitIsStillFailed(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 3)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("first")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("second")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-2"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("c")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	if err := h.engine.RetryUnit(item, "work-unit-0", ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitPending,
		"work-unit-1": store.WorkItemUnitFailed,
		"work-unit-2": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	if got := h.runner.startedUnitIDs(); len(got) != 3 {
		t.Fatalf("started units = %v, want nothing restarted while a unit is still failed", got)
	}

	if err := h.engine.RetryUnit(item, "work-unit-1", ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs()[3:]; !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("restarted units = %v, want both repaired units launched together", got)
	}
}

// The wave's whole reason: a provider usage limit fails most of a wide fan-out,
// the human waits it out, and one action puts every failed unit back on the
// same attempt — finished units untouched, each repaired unit on a fresh try
// carrying the same note.
func TestRetryFailedUnitsRepairsEveryFailedUnitAtOnce(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 3)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("usage limit reached")})
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-2"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("c")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	if err := h.engine.RetryFailedUnits(item, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs()[3:]; !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("restarted units = %v, want both failed units and only them", got)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitRunning,
		"work-unit-1": store.WorkItemUnitRunning,
		"work-unit-2": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	for _, unitID := range []string{"work-unit-0", "work-unit-1"} {
		retried := h.runner.startFor(t, unitKey(item, "work", 1, unitID))
		if retried.Feedback == nil || retried.Feedback.Note != "the limit reset" {
			t.Fatalf("unit %q retry feedback = %+v, want the human's note", unitID, retried.Feedback)
		}
	}
	rows, err := h.store.ListWorkItemPhaseUnits(item, "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		switch row.UnitID {
		case "work-unit-0", "work-unit-1":
			if row.UnitAttempt != 2 || row.Feedback != "the limit reset" {
				t.Fatalf("repaired unit row = %+v, want a second try carrying the note", row)
			}
		case "work-unit-2":
			if row.UnitAttempt != 1 || row.Status != store.WorkItemUnitDone {
				t.Fatalf("finished unit row = %+v, want its first attempt preserved", row)
			}
		}
	}
	phases, err := h.store.ListWorkItemPhases(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "running" || phases[0].EndedAt != 0 {
		t.Fatalf("phase attempts = %+v, want the same attempt reopened", phases)
	}

	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("v")})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

// Repairing everything is still admitted one unit at a time: the retry-all
// re-enters through the same acquisition the single retry uses, so a set wider
// than the project's provider bound starts what fits and queues the rest in the
// shared FIFO. A burst here would be the one way this action could outrun the
// bound a usage limit is the reason to respect.
func TestRetryFailedUnitsQueuesBeyondProviderCapacity(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 3)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 3; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("usage limit reached")})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	// The account the human switched to admits one session at a time. Capacity is
	// read live at every acquisition, so the repair sees this bound, not the one
	// the attempt originally started under.
	h.limitProviderCapacity("project", 1)
	if err := h.engine.RetryFailedUnits(item, ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs()[3:]; !reflect.DeepEqual(got, []string{"work-unit-0"}) {
		t.Fatalf("restarted units = %v, want only the one capacity admits", got)
	}
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitRunning,
		"work-unit-1": store.WorkItemUnitPending,
		"work-unit-2": store.WorkItemUnitPending,
		"work-join":   store.WorkItemUnitPending,
	})

	for index := 0; index < 3; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("v")})
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.runner.startedUnitIDs()[3:]; !reflect.DeepEqual(got, []string{
		"work-unit-0", "work-unit-1", "work-unit-2", "work-join",
	}) {
		t.Fatalf("launch order = %v, want each queued unit released in turn", got)
	}
}

// The repairs compose through the command loop rather than around it: a single
// retry that left one unit pending and a drop that resolved another are both
// facts the retry-all reads off the record, and it repairs exactly what is
// still failed while resuming everything the attempt still owes.
func TestRetryFailedUnitsInterleavesWithSingleRetryAndDrop(t *testing.T) {
	h := newFanOutHarness(t, 4)
	h.limitProviderCapacity("project", 4)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 3; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("boom")})
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-3"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("d")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	if err := h.engine.RetryUnit(item, "work-unit-0", "by hand"); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.DropUnit(item, "work-unit-1", "not worth another try"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	if err := h.engine.RetryFailedUnits(item, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitRunning,
		"work-unit-1": store.WorkItemUnitDropped,
		"work-unit-2": store.WorkItemUnitRunning,
		"work-unit-3": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	// Only unit 2 was still failed, so only unit 2 is the retry-all's repair, and
	// it is the one carrying the retry-all's note on a second try.
	started := h.runner.startFor(t, unitKey(item, "work", 1, "work-unit-2"))
	if started.Feedback == nil || started.Feedback.Note != "the limit reset" {
		t.Fatalf("repaired unit feedback = %+v, want the retry-all's note", started.Feedback)
	}
	// Unit 0 is relaunched, not re-repaired, and it carries what its ROW carries
	// — which is now the single retry's own note and try count. The repair that
	// reopened it left the run parked, so the item was evicted and rebuilt from
	// persistence before this launch: everything the retry decided had to be on
	// the row to survive that round trip.
	byHand := h.runner.startFor(t, unitKey(item, "work", 1, "work-unit-0"))
	if byHand.Feedback == nil || byHand.Feedback.Note != "by hand" {
		t.Fatalf("already-reopened unit feedback = %+v, want the single retry's note", byHand.Feedback)
	}
	if byHand.UnitAttempt != 2 {
		t.Fatalf("already-reopened unit attempt = %d, want the single retry's bumped try", byHand.UnitAttempt)
	}
}

// The reopen is durable on its own, without a second repair to carry it: a
// retry that leaves the run parked is evicted, and the unit that eventually
// starts must be on the try the retry put it on, carrying the note the retry
// gave it rather than the failure note it replaced.
func TestRetryUnitPersistsItsNoteAndTryAcrossEviction(t *testing.T) {
	h := newFanOutHarness(t, 2)
	h.limitProviderCapacity("project", 2)
	item := startFanOut(t, h, "fan")
	for index := 0; index < 2; index++ {
		h.runner.completeRun(t, unitKey(item, "work", 1, fmt.Sprintf("work-unit-%d", index)),
			Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("boom")})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	// Repairing one of two failed units leaves the run parked on the other, so
	// the item is evicted with the reopen holding nothing but the row.
	if err := h.engine.RetryUnit(item, "work-unit-0", "the limit reset"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonUnitFailed)

	reopened, found, err := h.store.GetWorkItemUnit(item, "work", 1, "work-unit-0")
	if err != nil || !found {
		t.Fatalf("reopened unit row: found=%v err=%v", found, err)
	}
	if reopened.Status != store.WorkItemUnitPending {
		t.Fatalf("reopened unit status = %q, want %q", reopened.Status, store.WorkItemUnitPending)
	}
	if reopened.UnitAttempt != 2 {
		t.Fatalf("reopened unit attempt = %d, want the bumped try persisted", reopened.UnitAttempt)
	}
	if reopened.Feedback != "the limit reset" {
		t.Fatalf("reopened unit feedback = %q, want the retry note persisted", reopened.Feedback)
	}

	// Clearing the other unit relaunches the repaired one from the restored
	// record alone.
	if err := h.engine.DropUnit(item, "work-unit-1", ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	started := h.runner.startFor(t, unitKey(item, "work", 1, "work-unit-0"))
	if started.Feedback == nil || started.Feedback.Note != "the limit reset" {
		t.Fatalf("relaunched unit feedback = %+v, want the retry note", started.Feedback)
	}
	if started.UnitAttempt != 2 {
		t.Fatalf("relaunched unit attempt = %d, want 2", started.UnitAttempt)
	}
	// The start persists the same try the reopen wrote — one bump, two writes.
	running, found, err := h.store.GetWorkItemUnit(item, "work", 1, "work-unit-0")
	if err != nil || !found {
		t.Fatalf("running unit row: found=%v err=%v", found, err)
	}
	if running.UnitAttempt != 2 {
		t.Fatalf("started unit attempt = %d, want the reopen's try rather than a second bump", running.UnitAttempt)
	}
}

// Nothing failed is a refusal, and it is a refusal that wrote nothing: the
// failed set is collected before the first store write, so an action with no
// work to do cannot half-repair an attempt on its way to saying so.
func TestRetryFailedUnitsWithNoFailedUnitIsARefusalThatChangesNothing(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	if err := h.engine.TakeOverUnit(item, "work-unit-0"); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("b")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)

	before := h.runner.startedUnitIDs()
	if err := h.engine.RetryFailedUnits(item, ""); err == nil ||
		!strings.Contains(err.Error(), "no unit of attempt") {
		t.Fatalf("retry-all with nothing failed = %v, want a refusal", err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitTakenOver,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, before) {
		t.Fatalf("started units = %v, want nothing started by a refusal (was %v)", got, before)
	}
}

// A unit under human steering is the human's, so the retry-all leaves it and
// the attempt stays parked on it — exactly what retrying each failed unit by
// hand would have produced.
func TestRetryFailedUnitsLeavesATakenOverUnitAndItsPark(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 3)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("boom")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOverUnit(item, "work-unit-1"); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-2"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("c")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)

	if err := h.engine.RetryFailedUnits(item, "the limit reset"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitPending,
		"work-unit-1": store.WorkItemUnitTakenOver,
		"work-unit-2": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	if got := h.runner.startedUnitIDs(); len(got) != 3 {
		t.Fatalf("started units = %v, want nothing relaunched while a unit is steered", got)
	}
}

func TestRetryFailedUnitsRejectsRunsItCannotRepair(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	if err := h.engine.RetryFailedUnits(item, ""); err == nil ||
		!strings.Contains(err.Error(), "still active") {
		t.Fatalf("retry-all on a live run = %v, want a refusal", err)
	}

	workflow := onePhaseWorkflow("single", nil, []def.Route{{To: "done"}})
	single := newHarness(t, Config{}, map[string]def.Workflow{"single": workflow}, []string{"project"}, nil)
	if err := single.engine.StartItem(testItem("item", "project", "single", 0)); err != nil {
		t.Fatal(err)
	}
	single.runner.complete(t, "item", Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := single.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := single.engine.RetryFailedUnits("item", ""); err == nil ||
		!strings.Contains(err.Error(), "unit recovery applies to") {
		t.Fatalf("retry-all on a question park = %v, want a refusal", err)
	}
}

func TestDropUnitLetsTheJoinProceedWithTheDroppedResult(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := parkOneFailedUnit(t, h)

	if err := h.engine.DropUnit(item, "work-unit-0", "not worth another try"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitDropped,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitRunning,
	})
	join := h.runner.startFor(t, unitKey(item, "work", 1, "work-join"))
	results, ok := join.Vars[def.UnitsVariable].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("join units variable = %#v, want both units including the dropped one", join.Vars[def.UnitsVariable])
	}
	dropped, ok := results[0].(map[string]any)
	if !ok || dropped["status"] != store.WorkItemUnitDropped {
		t.Fatalf("dropped unit result = %#v, want it visible as dropped", results[0])
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-join"), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateDone, "")
	h.requireNoHeldResources(t)
}

func TestTakeOverUnitParksTheAttemptAndIsRecoverable(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")

	if err := h.engine.TakeOverUnit(item, "work-unit-0"); err != nil {
		t.Fatal(err)
	}
	takeovers := h.runner.takeovers()
	if len(takeovers) != 1 || takeovers[0].UnitID != "work-unit-0" {
		t.Fatalf("takeover stops = %+v, want the unit detached by key", takeovers)
	}
	// A takeover leaves the session alive: teardown must not also stop it.
	for _, key := range h.runner.stopped() {
		if key.UnitID == "work-unit-0" {
			t.Fatalf("taken-over unit was also stopped: %+v", key)
		}
	}
	requireItemState(t, h.store, item, StateRunning, "")

	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("b")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)
	h.requireUnitStatuses(t, item, "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitTakenOver,
		"work-unit-1": store.WorkItemUnitDone,
		"work-join":   store.WorkItemUnitPending,
	})
	h.requireNoHeldResources(t)

	if err := h.engine.DropUnit(item, "work-unit-0", "finished it by hand"); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateRunning, "")
	if got := h.runner.startedUnitIDs(); got[len(got)-1] != "work-join" {
		t.Fatalf("started units = %v, want the join once the taken-over unit was resolved", got)
	}
}

func TestTakeOverUnitOutranksAnEarlierUnitFailure(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitProviderCapacity("project", 3)
	item := startFanOut(t, h, "fan")
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("first")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.TakeOverUnit(item, "work-unit-1"); err != nil {
		t.Fatal(err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-2"), Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("c")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item, StateNeedsHuman, ReasonTakenOver)
}

func TestUnitRecoveryRejectsRunsAndUnitsItCannotRepair(t *testing.T) {
	h := newFanOutHarness(t, 2)
	item := startFanOut(t, h, "fan")
	if err := h.engine.RetryUnit(item, "work-unit-0", ""); err == nil ||
		!strings.Contains(err.Error(), "still active") {
		t.Fatalf("retry on a live run = %v, want a refusal", err)
	}
	if err := h.engine.TakeOverUnit(item, "nope"); err == nil ||
		!strings.Contains(err.Error(), "not part of attempt") {
		t.Fatalf("takeover of an unknown unit = %v, want a refusal", err)
	}
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-0"),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("boom")})
	h.runner.completeRun(t, unitKey(item, "work", 1, "work-unit-1"),
		Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope("b")})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.RetryUnit(item, "work-unit-1", ""); err == nil ||
		!strings.Contains(err.Error(), "failed or taken-over") {
		t.Fatalf("retry of a finished unit = %v, want a refusal", err)
	}
	if err := h.engine.DropUnit(item, "work-join", ""); err == nil ||
		!strings.Contains(err.Error(), "join") {
		t.Fatalf("dropping the join = %v, want a refusal", err)
	}
	if err := h.engine.RetryUnit(item, "", ""); err == nil {
		t.Fatal("retry without a unit id was accepted")
	}
}

func TestUnitRecoveryRejectsParksItDoesNotOwn(t *testing.T) {
	workflow := onePhaseWorkflow("single", nil, []def.Route{{To: "done"}})
	h := newHarness(t, Config{}, map[string]def.Workflow{"single": workflow}, []string{"project"}, nil)
	if err := h.engine.StartItem(testItem("item", "project", "single", 0)); err != nil {
		t.Fatal(err)
	}
	h.runner.complete(t, "item", Outcome{Kind: OutcomeQuestion, Envelope: questionEnvelope()})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonQuestion)
	if err := h.engine.RetryUnit("item", "work-unit-0", ""); err == nil ||
		!strings.Contains(err.Error(), "unit recovery applies to") {
		t.Fatalf("retry on a question park = %v, want a refusal", err)
	}
}

func TestStartupCrashSweepFailsRunningUnitsAndRetryUnitRecoversThem(t *testing.T) {
	workflow := fanOutWorkflow("fan", 2)
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(PhaseInput{Vars: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{}, map[string]def.Workflow{"fan": workflow}, []string{"project"}, func(database *store.Store) {
		item := testItem("crashed", "project", "fan", 0)
		if err := database.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemRunStart(item.ID, snapshot, "", "", "", 20); err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateWorkItemState(item.ID, string(StateRunning), "", 0); err != nil {
			t.Fatal(err)
		}
		if err := database.CreateWorkItemPhase(store.WorkItemPhase{
			ItemID: item.ID, PhaseID: "work", Attempt: 1,
			InputEnvelope: input, Status: "running", StartedAt: 21,
		}); err != nil {
			t.Fatal(err)
		}
		units := []store.WorkItemUnit{
			{
				ItemID: item.ID, PhaseID: "work", Attempt: 1, UnitID: "work-unit-0", UnitIndex: 0,
				Kind: store.WorkItemUnitKindUnit, Provider: testProvider, Model: "test-model",
				Status: store.WorkItemUnitRunning, UnitAttempt: 1, StartedAt: 22,
			},
			{
				ItemID: item.ID, PhaseID: "work", Attempt: 1, UnitID: "work-unit-1", UnitIndex: 1,
				Kind: store.WorkItemUnitKindUnit, Provider: testProvider, Model: "test-model",
				Status: store.WorkItemUnitPending, UnitAttempt: 1,
			},
			{
				ItemID: item.ID, PhaseID: "work", Attempt: 1, UnitID: "work-join", UnitIndex: 2,
				Kind: store.WorkItemUnitKindJoin, Provider: testProvider, Model: "test-model",
				Status: store.WorkItemUnitPending, UnitAttempt: 1,
			},
		}
		if err := database.CreateWorkItemUnits(units); err != nil {
			t.Fatal(err)
		}
	})

	requireItemState(t, h.store, "crashed", StateNeedsHuman, ReasonInterrupted)
	h.requireUnitStatuses(t, "crashed", "work", 1, map[string]string{
		"work-unit-0": store.WorkItemUnitFailed,
		"work-unit-1": store.WorkItemUnitPending,
		"work-join":   store.WorkItemUnitPending,
	})
	rows, err := h.store.ListWorkItemPhaseUnits("crashed", "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rows[0].Feedback, "interrupted") {
		t.Fatalf("swept unit feedback = %q, want it to say the attempt was interrupted", rows[0].Feedback)
	}
	if len(h.runner.startedUnitIDs()) != 0 {
		t.Fatalf("crash sweep auto-reran units: %v", h.runner.startedUnitIDs())
	}

	if err := h.engine.RetryUnit("crashed", "work-unit-0", ""); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "crashed", StateRunning, "")
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, []string{"work-unit-0", "work-unit-1"}) {
		t.Fatalf("started units = %v, want the retried unit and the never-started one", got)
	}
}

// TestRestoreUnitRefusesRowsItCannotTrust pins the two ways a rebuilt attempt
// can disagree with its record. Both are corruption: adopting either would put
// the scheduler in a state no live run could have produced.
func TestRestoreUnitRefusesRowsItCannotTrust(t *testing.T) {
	if _, err := restoreUnit(&unitRun{id: "absent"}, map[string]store.WorkItemUnit{}); err == nil ||
		!strings.Contains(err.Error(), "no persisted row") {
		t.Fatalf("restore of an unrecorded unit = %v, want a refusal", err)
	}
	rows := map[string]store.WorkItemUnit{"live": {Status: store.WorkItemUnitRunning, UnitAttempt: 1}}
	if _, err := restoreUnit(&unitRun{id: "live"}, rows); err == nil ||
		!strings.Contains(err.Error(), "no live runner") {
		t.Fatalf("restore of a still-running row = %v, want a refusal", err)
	}
}
