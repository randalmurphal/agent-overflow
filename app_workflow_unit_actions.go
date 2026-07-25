package main

import (
	"fmt"

	"agent-overflow/internal/store"
)

// Human recovery of one fan-out unit. A parked fan-out attempt is repaired unit
// by unit rather than replaced: its finished units keep their results, and the
// run returns to `running` once nothing is left blocking it. The engine owns
// those FSM edges; this file owns the app-side surface and the runner
// bookkeeping a taken-over unit thread needs.

// WorkflowRetryUnit re-runs one failed or taken-over unit of a parked fan-out
// attempt. The note explains the retry in the run record and reaches the unit's
// next try as feedback.
func (a *App) WorkflowRetryUnit(itemID, unitID, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	// Read the row before the engine reopens it: a retry supersedes any steering
	// registration on the previous try's thread, and after the call the row has
	// already moved on.
	abandoned := a.workflowUnitThreadUnderTakeover(itemID, unitID)
	if err := workflowEngine.RetryUnit(itemID, unitID, note); err != nil {
		return err
	}
	a.releaseWorkflowUnitTakeover(abandoned)
	return nil
}

// WorkflowDropUnit accepts a failed or taken-over unit's absence. The unit is
// recorded `dropped`, its join sees it as such, and the attempt resumes.
func (a *App) WorkflowDropUnit(itemID, unitID, note string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	abandoned := a.workflowUnitThreadUnderTakeover(itemID, unitID)
	if err := workflowEngine.DropUnit(itemID, unitID, note); err != nil {
		return err
	}
	a.releaseWorkflowUnitTakeover(abandoned)
	return nil
}

// WorkflowTakeOverUnit detaches one live fan-out unit from engine control so a
// human can steer its thread directly. The unit's session stays alive and is
// re-registered schema-less, exactly as a taken-over phase thread is; its
// siblings keep running and the attempt parks once they rest.
func (a *App) WorkflowTakeOverUnit(itemID, unitID string) error {
	workflowEngine, err := a.requireWorkflowEngine()
	if err != nil {
		return err
	}
	if a.workflowRunner == nil {
		return fmt.Errorf("take over workflow unit %q of item %s: runner unavailable", unitID, itemID)
	}
	unit, err := a.currentWorkflowUnit(itemID, unitID)
	if err != nil {
		return fmt.Errorf("take over workflow unit: %w", err)
	}
	if unit.ThreadID == "" {
		return fmt.Errorf(
			"take over workflow unit %q of item %s: the unit runs a deterministic command and has no session to steer",
			unitID, itemID,
		)
	}
	if err := workflowEngine.TakeOverUnit(itemID, unitID); err != nil {
		return err
	}
	if err := a.workflowRunner.registerTakeover(itemID, unit.ThreadID); err != nil {
		return fmt.Errorf(
			"take over workflow unit %q of item %s: register schema-less steering: %w",
			unitID, itemID, err,
		)
	}
	return nil
}

// currentWorkflowUnit resolves one unit of a run's current phase attempt. Unit
// ids are unique inside an attempt, not inside a run, so the attempt is what
// makes the lookup unambiguous.
func (a *App) currentWorkflowUnit(itemID, unitID string) (store.WorkItemUnit, error) {
	if a.store == nil {
		return store.WorkItemUnit{}, fmt.Errorf("workflow store unavailable")
	}
	phases, err := a.store.ListWorkItemPhases(itemID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	current, ok := currentWorkflowPhaseAttempt(phases)
	if !ok {
		return store.WorkItemUnit{}, fmt.Errorf("item %s has no phase attempt", itemID)
	}
	unit, found, err := a.store.GetWorkItemUnit(itemID, current.PhaseID, current.Attempt, unitID)
	if err != nil {
		return store.WorkItemUnit{}, err
	}
	if !found {
		return store.WorkItemUnit{}, fmt.Errorf(
			"unit %q is not part of attempt %s/%d of item %s", unitID, current.PhaseID, current.Attempt, itemID,
		)
	}
	return unit, nil
}

// workflowUnitThreadUnderTakeover returns the thread a unit is currently being
// steered on, or "" when it is not under human control. Failing to resolve it is
// deliberately not an error: the recovery action is what the human asked for,
// and the engine validates the unit itself.
func (a *App) workflowUnitThreadUnderTakeover(itemID, unitID string) string {
	unit, err := a.currentWorkflowUnit(itemID, unitID)
	if err != nil || unit.Status != store.WorkItemUnitTakenOver {
		return ""
	}
	return unit.ThreadID
}

// releaseWorkflowUnitTakeover drops the steering registration of a unit thread
// the human has just retried or dropped. A retry starts a fresh try on a fresh
// thread and a drop ends the unit outright, so leaving the old thread registered
// would keep an abandoned session claiming to be a live takeover — and a send
// into it would be accepted as steering work nothing consumes.
func (a *App) releaseWorkflowUnitTakeover(threadID string) {
	if threadID == "" || a.workflowRunner == nil {
		return
	}
	a.workflowRunner.clearTakeoverThread(threadID)
}
