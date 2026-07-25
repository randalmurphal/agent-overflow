package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/workflow/def"
)

// requireRefusedBeforeAnyUnitStarted asserts the whole of "it refuses, it never
// truncates": the run parked, and the attempt left behind no runner start, no
// unit row, and no held capacity. Truncation would show up here as unit rows.
func requireRefusedBeforeAnyUnitStarted(t *testing.T, h *testHarness, itemID string, wantInReason []string) {
	t.Helper()
	requireItemState(t, h.store, itemID, StateNeedsHuman, ReasonWiringError)
	if started := h.runner.startedKeys(); len(started) != 0 {
		t.Fatalf("started runs = %+v, want nothing started before the refusal", started)
	}
	rows, err := h.store.ListWorkItemPhaseUnits(itemID, "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted %d unit rows before the refusal, want none", len(rows))
	}
	h.requireNoHeldResources(t)

	// The park has to explain itself: nothing ran a turn, so the engine's own
	// envelope is the only place the width and the ceiling are stated.
	phases, err := h.store.ListWorkItemPhases(itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].Status != "parked" {
		t.Fatalf("phase attempts = %+v, want one parked attempt", phases)
	}
	var envelope controlEnvelope
	if err := json.Unmarshal(phases[0].OutputEnvelope, &envelope); err != nil {
		t.Fatalf("park envelope %s: %v", phases[0].OutputEnvelope, err)
	}
	for _, want := range wantInReason {
		if !strings.Contains(envelope.Reason, want) {
			t.Fatalf("park reason %q does not state %q", envelope.Reason, want)
		}
	}
}

// A dynamic width only exists once the attempt's variables are resolved, so the
// dry-run cannot check it. The engine does, at expansion — the one seam every
// unit passes through — and refuses rather than truncating: dropping units
// silently would leave the join consolidating a set nobody chose.
func TestDynamicFanOutOverTheProjectCeilingRefusesBeforeAnythingStarts(t *testing.T) {
	workflow := def.Workflow{ID: "dyn", Phases: []def.Phase{
		dynamicFanOutPhase("work", "targets", "target", []def.Route{{To: "done"}}),
	}}
	newDynamicHarness := func(t *testing.T) *testHarness {
		t.Helper()
		h := newHarness(t, Config{}, map[string]def.Workflow{"dyn": workflow}, []string{"project"}, nil)
		h.limitFanOutWidth("project", 3)
		h.limitProviderCapacity("project", 4)
		return h
	}

	t.Run("exactly at the ceiling runs", func(t *testing.T) {
		h := newDynamicHarness(t)
		item := testItem("item", "project", "dyn", 0)
		item.Seeds = json.RawMessage(`{"targets":["a","b","c"]}`)
		if err := h.engine.StartItem(item); err != nil {
			t.Fatal(err)
		}
		requireItemState(t, h.store, item.ID, StateRunning, "")
		if got := h.runner.startedUnitIDs(); len(got) != 3 {
			t.Fatalf("started units = %v, want all three at the ceiling", got)
		}
	})

	t.Run("one over is refused", func(t *testing.T) {
		h := newDynamicHarness(t)
		item := testItem("item", "project", "dyn", 0)
		item.Seeds = json.RawMessage(`{"targets":["a","b","c","d"]}`)
		if err := h.engine.StartItem(item); err == nil {
			t.Fatal("a fan-out over the project ceiling started silently")
		}
		requireRefusedBeforeAnyUnitStarted(t, h, item.ID, []string{
			`phase "work"`, "expands to 4 units", "maximum fan-out width of 3", "max_fan_out_width",
		})
	})
}

// A frozen snapshot is decoded and never re-validated, so the dry-run's static
// finding cannot be the enforcement: a run whose definition predates the rule,
// or whose project lowered its ceiling after the run started, still reaches the
// engine with a static list. Expansion refuses that too.
func TestStaticFanOutOverTheProjectCeilingIsRefusedAtExpansion(t *testing.T) {
	h := newFanOutHarness(t, 4)
	h.limitFanOutWidth("project", 3)
	h.limitProviderCapacity("project", 4)
	if err := h.engine.StartItem(testItem("item", "project", "fan", 0)); err == nil {
		t.Fatal("a frozen static fan-out over the ceiling started")
	}
	requireRefusedBeforeAnyUnitStarted(t, h, "item", []string{
		`phase "work"`, "expands to 4 units", "maximum fan-out width of 3",
	})
}

// A profile the engine cannot read is a setup failure, never an unbounded
// start. A fan-out phase acquires no resources of its own, so this expansion is
// the first thing that reads the profile at all — without the check it would be
// the last place an unknown ceiling could be silently treated as no ceiling.
func TestFanOutWithAnUnreadableProfileParksSetupFailed(t *testing.T) {
	h := newFanOutHarness(t, 2)
	h.profiles.remove("project")
	if err := h.engine.StartItem(testItem("item", "project", "fan", 0)); err == nil {
		t.Fatal("a fan-out expanded with no readable project profile")
	}
	requireItemState(t, h.store, "item", StateNeedsHuman, ReasonSetupFailed)
	if started := h.runner.startedKeys(); len(started) != 0 {
		t.Fatalf("started runs = %+v, want none", started)
	}
	rows, err := h.store.ListWorkItemPhaseUnits("item", "work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted %d unit rows with no profile, want none", len(rows))
	}
}

// The ceiling is read live, like every other bound (§6): raising it in
// profile.yaml takes effect on the next expansion, with no restart and no
// re-resolution of the frozen definition.
func TestFanOutCeilingIsReadLiveAtEachExpansion(t *testing.T) {
	h := newFanOutHarness(t, 3)
	h.limitFanOutWidth("project", 2)
	h.limitProviderCapacity("project", 3)
	if err := h.engine.StartItem(testItem("first", "project", "fan", 0)); err == nil {
		t.Fatal("a fan-out over the ceiling started")
	}
	requireItemState(t, h.store, "first", StateNeedsHuman, ReasonWiringError)

	h.limitFanOutWidth("project", 3)
	if err := h.engine.StartItem(testItem("second", "project", "fan", 1)); err != nil {
		t.Fatal(err)
	}
	requireItemState(t, h.store, "second", StateRunning, "")
	if got := h.runner.startedUnitIDs(); len(got) != 3 {
		t.Fatalf("started units = %v, want the raised ceiling to admit all three", got)
	}
}
