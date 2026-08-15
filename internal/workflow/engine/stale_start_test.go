package engine

import (
	"errors"
	"testing"

	"agent-overflow/internal/store"
)

// A repaired unit reuses its whole key (item/phase/attempt/unitID), so a wedged
// predecessor's late Start return arrives with a key that matches the repaired
// start exactly. Only the future's identity tells them apart: the stale return
// must be dropped without touching the repaired start's state, and the future
// that armed the flag must still settle it. A bool guard passed the stale one
// and parked the repaired run (incident 2026-08-15).
func TestLateReturnOfASupersededStartCannotSettleTheRepairedUnit(t *testing.T) {
	stale := &runnerStartFuture{}
	current := &runnerStartFuture{}
	cancelled := false
	unit := &unitRun{
		id:                "lane",
		kind:              UnitWork,
		status:            store.WorkItemUnitRunning,
		runnerStarting:    true,
		runnerStart:       current,
		runnerStartCancel: func() { cancelled = true },
	}
	item := &runtimeItem{fan: &fanOutRun{units: []*unitRun{unit}}}
	engine := &Engine{}

	staleCommand := runnerStartCommand{
		key:    RunKey{ItemID: "item", PhaseID: "phase", Attempt: 1, UnitID: "lane"},
		future: stale,
		err:    errors.New("the wedged start finally unwound"),
	}
	if err := engine.finishUnitStart(item, staleCommand); err != nil {
		t.Fatalf("stale start return: %v", err)
	}
	if !unit.runnerStarting || unit.runnerStart != current {
		t.Fatalf("stale start return settled the repaired unit's in-flight start")
	}
	if unit.runnerActive || cancelled {
		t.Fatalf("stale start return mutated the repaired unit (active=%v cancelled=%v)",
			unit.runnerActive, cancelled)
	}

	liveCommand := staleCommand
	liveCommand.future, liveCommand.err = current, nil
	if err := engine.finishUnitStart(item, liveCommand); err != nil {
		t.Fatalf("live start return: %v", err)
	}
	if unit.runnerStarting || unit.runnerStart != nil || !cancelled {
		t.Fatalf("live start return did not clear the start it armed (starting=%v future=%p cancelled=%v)",
			unit.runnerStarting, unit.runnerStart, cancelled)
	}
	if !unit.runnerActive {
		t.Fatalf("live start return did not activate the unit")
	}
}

// The phase-level copy of the same guard: `finishRunnerStart` acts only for the
// future that armed `runnerStarting`, so the two guards cannot drift.
func TestLateReturnOfASupersededStartCannotSettleTheRepairedPhase(t *testing.T) {
	stale := &runnerStartFuture{}
	current := &runnerStartFuture{}
	cancelled := false
	item := &runtimeItem{
		item:              store.WorkItem{ID: "item", State: string(StateRunning)},
		phaseID:           "phase",
		attempt:           2,
		runnerStarting:    true,
		runnerStart:       current,
		runnerStartCancel: func() { cancelled = true },
	}
	engine := &Engine{items: map[string]*runtimeItem{"item": item}}

	staleCommand := runnerStartCommand{
		key:    RunKey{ItemID: "item", PhaseID: "phase", Attempt: 2},
		future: stale,
		err:    errors.New("the wedged start finally unwound"),
	}
	if err := engine.finishRunnerStart(staleCommand); err != nil {
		t.Fatalf("stale start return: %v", err)
	}
	if !item.runnerStarting || item.runnerStart != current {
		t.Fatalf("stale start return settled the repaired attempt's in-flight start")
	}
	if item.runnerActive || cancelled {
		t.Fatalf("stale start return mutated the repaired attempt (active=%v cancelled=%v)",
			item.runnerActive, cancelled)
	}

	liveCommand := staleCommand
	liveCommand.future, liveCommand.err = current, nil
	if err := engine.finishRunnerStart(liveCommand); err != nil {
		t.Fatalf("live start return: %v", err)
	}
	if item.runnerStarting || item.runnerStart != nil || !cancelled {
		t.Fatalf("live start return did not clear the start it armed (starting=%v future=%p cancelled=%v)",
			item.runnerStarting, item.runnerStart, cancelled)
	}
	if !item.runnerActive {
		t.Fatalf("live start return did not activate the attempt")
	}
}
