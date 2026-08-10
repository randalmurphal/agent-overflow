package engine

import (
	"testing"

	"agent-overflow/internal/workflow/def"
)

// The reserved `call-depth` read is bound from the run row the engine already
// keeps, which is the whole point: a recursive campaign that threaded its own
// wave ordinal through call arguments and incremented it with model arithmetic
// desynced from the tree it was describing. The engine knows the number.

// callDepthOf reads the reserved binding out of one started element's variables.
func callDepthOf(t *testing.T, request RunRequest) int {
	t.Helper()
	value, present := request.Vars[def.CallDepthVariable]
	if !present {
		t.Fatalf("%+v rendered no %q binding", request.Key, def.CallDepthVariable)
	}
	depth, ok := value.(int)
	if !ok {
		t.Fatalf("%q = %T(%v), want an int", def.CallDepthVariable, value, value)
	}
	return depth
}

// TestCallDepthIsZeroAtTheRootAndCountsCallEdges — a directly started run is 0,
// and each call edge below it is one deeper. Nothing is authored, seeded, or
// incremented by a model.
func TestCallDepthIsZeroAtTheRootAndCountsCallEdges(t *testing.T) {
	h := newCallHarness(t, defaultCallWorkflows(), nil)
	parent := startCaller(t, h)

	rootStart := h.runner.startFor(t, RunKey{ItemID: parent, PhaseID: "prepare", Attempt: 1})
	if depth := callDepthOf(t, rootStart); depth != 0 {
		t.Fatalf("root call-depth = %d, want 0", depth)
	}

	child := h.callChild(t, parent, "audit", 1)
	if child.CallDepth != 1 {
		t.Fatalf("child row depth = %d, want 1", child.CallDepth)
	}
	childStart := h.runner.startFor(t, RunKey{ItemID: child.ID, PhaseID: "work", Attempt: 1})
	if depth := callDepthOf(t, childStart); depth != 1 {
		t.Fatalf("called run call-depth = %d, want 1", depth)
	}
}

// TestCallDepthReachesFanOutUnitsAndTheirJoin — a wave's lanes and the join that
// consolidates them read the same ordinal their phase does, which is what makes
// it usable as the wave number a campaign renders into every lane's prompt.
func TestCallDepthReachesFanOutUnitsAndTheirJoin(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": fanOutWorkflow("flow", 2),
	}, []string{"project"}, nil)
	item := testItem("wave", "project", "flow", 0)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	for _, unitID := range []string{"work-unit-0", "work-unit-1"} {
		h.runner.completeRun(t, unitKey(item.ID, "work", 1, unitID), Outcome{Kind: OutcomeDone, Envelope: doneEnvelope(true)})
	}
	if err := h.engine.Sync(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []RunKey{
		unitKey(item.ID, "work", 1, "work-unit-0"),
		unitKey(item.ID, "work", 1, "work-unit-1"),
		unitKey(item.ID, "work", 1, "work-join"),
	} {
		if depth := callDepthOf(t, h.runner.startFor(t, key)); depth != 0 {
			t.Fatalf("%s call-depth = %d, want the phase's 0", key.UnitID, depth)
		}
	}
}

// TestCallDepthIsNotSeededAndCannotBeOverridden — the read comes off the run
// row, so a seed of the same name (which validation refuses to author, but a
// caller can still write into a child's seeds column) never displaces it.
func TestCallDepthIsNotSeededAndCannotBeOverridden(t *testing.T) {
	h := newHarness(t, Config{}, map[string]def.Workflow{
		"flow": onePhaseWorkflow("flow", nil, []def.Route{{To: "done"}}),
	}, []string{"project"}, nil)
	item := testItem("seeded", "project", "flow", 0)
	item.Seeds = []byte(`{"call-depth":42}`)
	if err := h.engine.StartItem(item); err != nil {
		t.Fatal(err)
	}
	starts := h.runner.started()
	if len(starts) != 1 {
		t.Fatalf("runner starts = %d, want one", len(starts))
	}
	if depth := callDepthOf(t, starts[0]); depth != 0 {
		t.Fatalf("call-depth = %d, want the engine's 0 rather than the seed", depth)
	}
}
