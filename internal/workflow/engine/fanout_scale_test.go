package engine

import (
	"fmt"
	"reflect"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// A width the rest of the suite never reaches. Three units at a time over
// fourteen means eleven of them genuinely wait for a slot, the failure lands
// with units still queued behind it, and the retry has to relaunch more than
// the unit it repaired — none of which a three-unit fan-out can show.
const (
	scaleWidth    = 14
	scaleCapacity = 3
	scaleBranch   = "ao/run"
)

func scaleUnitID(index int) string { return fmt.Sprintf("work-unit-%d", index) }

// scaleUnitIDs is the launch order of units [from, to).
func scaleUnitIDs(from, to int) []string {
	ids := make([]string, 0, to-from)
	for index := from; index < to; index++ {
		ids = append(ids, scaleUnitID(index))
	}
	return ids
}

func scaleUnitBranch(index int) string { return fmt.Sprintf("%s/unit-%d", scaleBranch, index) }

// TestFanOutAtRealisticWidthQueuesFailsRetriesAndJoins drives one fourteen-unit
// fan-out through the whole §3 unit lifecycle at a width where the scheduling
// actually matters: units queue on provider capacity, one fails with siblings
// still in flight and others still pending, a human retries it, and the join
// finally consolidates all fourteen.
//
// It is deterministic by construction: nothing sleeps and nothing polls. Every
// unit finishes exactly when the test reports its outcome, and `engine.Sync`
// drains the command loop, so each assertion runs against a settled scheduler
// rather than a racing one.
func TestFanOutAtRealisticWidthQueuesFailsRetriesAndJoins(t *testing.T) {
	h := newFanOutHarness(t, scaleWidth)
	h.limitProviderCapacity("project", scaleCapacity)

	item := testItem("item", "project", "fan", 0)
	// A writing run has one branch; the join runs on it, and each unit gets a
	// sub-branch of its own (§9).
	item.Branch = scaleBranch
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}

	// The app runner registers a unit's branch as soon as it provisions the
	// sub-worktree; the fake runner does not, so the rows are stamped here. A
	// real git merge of fourteen branches is out of reach at this level, so what
	// the join is held to is that it receives every unit's branch — the handle a
	// merge would need — not that a merge happened.
	for index := 0; index < scaleWidth; index++ {
		if err := h.store.AttachWorkItemUnitWorkspace(
			item.ID, "work", 1, scaleUnitID(index), scaleUnitBranch(index), "",
		); err != nil {
			t.Fatal(err)
		}
	}

	// peak proves the test actually exercised contention rather than trivially
	// staying under a bound it never approached.
	peak := 0
	settle := func() {
		t.Helper()
		if err := h.engine.Sync(); err != nil {
			t.Fatal(err)
		}
		running := 0
		for id, status := range h.unitStatuses(t, item.ID, "work", 1) {
			if id != "work-join" && status == store.WorkItemUnitRunning {
				running++
			}
		}
		if running > scaleCapacity {
			t.Fatalf("%d units running at once, above the provider capacity of %d", running, scaleCapacity)
		}
		if running > peak {
			peak = running
		}
	}

	settle()
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, scaleUnitIDs(0, scaleCapacity)) {
		t.Fatalf("started units = %v, want only the first %d under capacity", got, scaleCapacity)
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")

	// Four completions, four freed slots, four launches — strictly in order.
	for index := 0; index < 4; index++ {
		h.runner.completeRun(t, unitKey(item.ID, "work", 1, scaleUnitID(index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
		settle()
	}
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, scaleUnitIDs(0, 7)) {
		t.Fatalf("launch order = %v, want each freed slot to take the next pending unit", got)
	}

	// Unit 4 fails with 5 and 6 in flight and 7..13 still pending.
	h.runner.completeRun(t, unitKey(item.ID, "work", 1, scaleUnitID(4)),
		Outcome{Kind: OutcomeExecutionFailure, Envelope: failureEnvelope("unit 4 blew up")})
	settle()
	if got := h.runner.startedUnitIDs(); !reflect.DeepEqual(got, scaleUnitIDs(0, 7)) {
		t.Fatalf("started units = %v, want the failure to stop further launches", got)
	}
	for _, key := range h.runner.stopped() {
		if key.UnitID == scaleUnitID(5) || key.UnitID == scaleUnitID(6) {
			t.Fatalf("in-flight unit was stopped by a sibling failure: %+v", key)
		}
	}
	requireItemState(t, h.store, item.ID, StateRunning, "")
	statuses := h.unitStatuses(t, item.ID, "work", 1)
	for index := 7; index < scaleWidth; index++ {
		if statuses[scaleUnitID(index)] != store.WorkItemUnitPending {
			t.Fatalf("unit %d = %q after the failure, want it left pending", index, statuses[scaleUnitID(index)])
		}
	}

	// The in-flight units finish; only then does the attempt park.
	for _, index := range []int{5, 6} {
		h.runner.completeRun(t, unitKey(item.ID, "work", 1, scaleUnitID(index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
		settle()
	}
	requireItemState(t, h.store, item.ID, StateNeedsHuman, ReasonUnitFailed)
	h.requireNoHeldResources(t)
	requireScaleStatuses(t, h, item.ID, map[string]string{
		scaleUnitID(4): store.WorkItemUnitFailed,
	}, 7)

	// Retrying the one failed unit reopens the attempt: the repaired unit AND
	// the units its failure stopped relaunch together, up to capacity.
	if err := h.engine.RetryUnit(item.ID, scaleUnitID(4), "the target moved"); err != nil {
		t.Fatal(err)
	}
	settle()
	requireItemState(t, h.store, item.ID, StateRunning, "")
	relaunched := h.runner.startedUnitIDs()[7:]
	if !reflect.DeepEqual(relaunched, []string{scaleUnitID(4), scaleUnitID(7), scaleUnitID(8)}) {
		t.Fatalf("relaunched = %v, want the repaired unit and the pending ones it was blocking", relaunched)
	}
	retried := h.runner.startFor(t, unitKey(item.ID, "work", 1, scaleUnitID(4)))
	if retried.UnitAttempt != 2 {
		t.Fatalf("retried unit try = %d, want a second try", retried.UnitAttempt)
	}
	if retried.Feedback == nil || retried.Feedback.Note != "the target moved" {
		t.Fatalf("retry feedback = %+v, want the human's note", retried.Feedback)
	}

	// Drain the rest. Each completion frees the slot the next pending unit takes,
	// so this order always addresses a running unit.
	for _, index := range []int{4, 7, 8, 9, 10, 11, 12, 13} {
		h.runner.completeRun(t, unitKey(item.ID, "work", 1, scaleUnitID(index)),
			Outcome{Kind: OutcomeDone, Envelope: unitDoneEnvelope(fmt.Sprintf("v%d", index))})
		settle()
	}
	if peak != scaleCapacity {
		t.Fatalf("peak concurrency = %d, want the run to have actually saturated capacity %d", peak, scaleCapacity)
	}

	join := h.runner.startFor(t, unitKey(item.ID, "work", 1, "work-join"))
	if join.Item.Branch != scaleBranch {
		t.Fatalf("join branch = %q, want the run's own branch — the join consolidates onto it", join.Item.Branch)
	}
	results, ok := join.Vars[def.UnitsVariable].([]any)
	if !ok || len(results) != scaleWidth {
		t.Fatalf("join units binding = %#v, want all %d unit results", join.Vars[def.UnitsVariable], scaleWidth)
	}
	for index, entry := range results {
		result, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("unit result %d = %#v, want an object", index, entry)
		}
		if result["id"] != scaleUnitID(index) || result["status"] != store.WorkItemUnitDone {
			t.Fatalf("unit result %d = %#v, want %q done", index, result, scaleUnitID(index))
		}
		if result["branch"] != scaleUnitBranch(index) {
			t.Fatalf("unit result %d branch = %#v, want the sub-branch a merge would need", index, result["branch"])
		}
		outputs, ok := result["outputs"].(map[string]any)
		if !ok || outputs["value"] != fmt.Sprintf("v%d", index) {
			t.Fatalf("unit result %d outputs = %#v, want its own outputs", index, result["outputs"])
		}
	}

	h.runner.completeRun(t, unitKey(item.ID, "work", 1, "work-join"),
		Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, item.ID, StateDone, "")
	h.requireNoHeldResources(t)
}

// requireScaleStatuses asserts the whole fourteen-unit status map: units below
// pendingFrom are done except for the explicit overrides, units from there up
// are pending, and the join has not run.
func requireScaleStatuses(t *testing.T, h *testHarness, itemID string, overrides map[string]string, pendingFrom int) {
	t.Helper()
	want := map[string]string{"work-join": store.WorkItemUnitPending}
	for index := 0; index < scaleWidth; index++ {
		status := store.WorkItemUnitDone
		if index >= pendingFrom {
			status = store.WorkItemUnitPending
		}
		if override, ok := overrides[scaleUnitID(index)]; ok {
			status = override
		}
		want[scaleUnitID(index)] = status
	}
	h.requireUnitStatuses(t, itemID, "work", 1, want)
}
