package engine

import (
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
)

// RetryUnit re-runs one failed unit of a parked fan-out attempt. The attempt
// itself continues: units that already finished keep their results, and the
// join still runs once everything rests.
func (e *Engine) RetryUnit(itemID, unitID, note string) error {
	return e.request(retryUnitCommand{itemID: itemID, unitID: unitID, note: note})
}

// RetryFailedUnits re-runs every currently failed unit of a parked fan-out
// attempt at once. It is exactly RetryUnit applied to all of them — the same
// reopened attempt, the same per-unit attempt bookkeeping, the same admission
// through the project's semaphores — and it exists because the failure it
// repairs is usually one cause hitting many units at the same time (a provider
// usage limit stopping most of a wide fan-out). The human clears that cause
// once, so they repair it once.
//
// Units a human took over are left alone, exactly as retrying each failed unit
// by hand would leave them: the human is driving those, and the attempt stays
// parked until they decide.
func (e *Engine) RetryFailedUnits(itemID, note string) error {
	return e.request(retryFailedUnitsCommand{itemID: itemID, note: note})
}

// DropUnit accepts a failed unit's absence. The unit is recorded `dropped`, its
// result reaches the join as such, and the attempt resumes.
func (e *Engine) DropUnit(itemID, unitID, note string) error {
	return e.request(dropUnitCommand{itemID: itemID, unitID: unitID, note: note})
}

// TakeOverUnit detaches one running unit from engine control for human
// steering. Launching stops, in-flight units run to completion, and the item
// parks `needs-human(taken-over)` once they rest — from where the human either
// retries the unit under engine control or drops it.
func (e *Engine) TakeOverUnit(itemID, unitID string) error {
	return e.request(takeoverUnitCommand{itemID: itemID, unitID: unitID})
}

func (e *Engine) retryUnit(itemID, unitID, note string) error {
	item, unit, err := e.parkedUnit(itemID, unitID, "retry unit")
	if err != nil {
		return err
	}
	if err := repairable(itemID, unitID, "retry unit", unit); err != nil {
		return err
	}
	if err := e.store.RetryWorkItemUnit(itemID, item.phaseID, item.attempt, unitID); err != nil {
		return fmt.Errorf("retry unit %q of item %q: %w", unitID, itemID, err)
	}
	unit.status = store.WorkItemUnitPending
	unit.attempt++
	unit.envelope = nil
	unit.feedback = retryFeedback(note)
	e.emitUnitState(item, unit)
	return e.resumeRepairedFanOut(item)
}

// retryFailedUnits is deliberately ONE command rather than N RetryUnit calls.
// The loop serializes commands but not the gaps between them, so a fan-out
// repaired by N submitted retries could interleave with a drop or a single
// retry and reach the second half of the set against an attempt the first half
// already returned to running. Here the failed set is read, reopened, and
// resumed inside one turn of the loop, so no other command observes the
// half-repaired attempt.
func (e *Engine) retryFailedUnits(itemID, note string) error {
	const action = "retry failed units"
	item, err := e.parkedFanOut(itemID, action)
	if err != nil {
		return err
	}
	// Collected before anything is written, so "nothing was failed" is
	// structurally a no-op refusal rather than a repair that happened to write
	// nothing. The join is not in this list: `fan.units` excludes it, and it is
	// not repairable in any case (see repairable).
	failed := make([]*unitRun, 0, len(item.fan.units))
	for _, unit := range item.fan.units {
		if unit.status == store.WorkItemUnitFailed {
			failed = append(failed, unit)
		}
	}
	if len(failed) == 0 {
		return fmt.Errorf(
			"%s of item %q: no unit of attempt %s/%d is failed",
			action, itemID, item.phaseID, item.attempt,
		)
	}
	for _, unit := range failed {
		if err := e.store.RetryWorkItemUnit(itemID, item.phaseID, item.attempt, unit.id); err != nil {
			// A write that fails here leaves the units before it reopened and the
			// run still parked, which is a state the same action recovers from:
			// reopened units rest `pending`, and the next repair relaunches them
			// alongside whatever is still failed.
			return fmt.Errorf("%s of item %q: reopen unit %q: %w", action, itemID, unit.id, err)
		}
		unit.status = store.WorkItemUnitPending
		unit.attempt++
		unit.envelope = nil
		// One Feedback per unit rather than one shared pointer: the note is the
		// same text, but the value rides each unit's next start independently.
		unit.feedback = retryFeedback(note)
		e.emitUnitState(item, unit)
	}
	// The repaired units re-enter through the same resume a single retry uses, so
	// they are admitted one by one through acquireUnitResources and queue in the
	// shared waiting FIFO when the project is at capacity — a retry-all wider
	// than the provider bound starts what fits and holds the rest, never a burst.
	// A unit still resting `taken-over` keeps the attempt parked.
	return e.resumeRepairedFanOut(item)
}

func (e *Engine) dropUnit(itemID, unitID, note string) error {
	item, unit, err := e.parkedUnit(itemID, unitID, "drop unit")
	if err != nil {
		return err
	}
	if err := repairable(itemID, unitID, "drop unit", unit); err != nil {
		return err
	}
	if err := e.store.CompleteWorkItemUnit(
		itemID, item.phaseID, item.attempt, unitID, store.WorkItemUnitDropped,
		unit.envelope, dropNote(note), e.timestamp(),
	); err != nil {
		return fmt.Errorf("drop unit %q of item %q: %w", unitID, itemID, err)
	}
	unit.status = store.WorkItemUnitDropped
	e.emitUnitState(item, unit)
	return e.resumeRepairedFanOut(item)
}

func (e *Engine) takeOverUnit(itemID, unitID string) error {
	item, tracked := e.items[itemID]
	if !tracked {
		return fmt.Errorf("take over unit %q of item %q: item is not running", unitID, itemID)
	}
	if State(item.item.State) != StateRunning {
		return fmt.Errorf("take over unit %q of item %q: invalid state %s", unitID, itemID, item.item.State)
	}
	if item.fan == nil {
		return fmt.Errorf("take over unit %q of item %q: phase %q is not running a fan-out", unitID, itemID, item.phaseID)
	}
	unit := item.fan.find(unitID)
	if unit == nil {
		return fmt.Errorf("take over unit %q of item %q: unit is not part of attempt %s/%d", unitID, itemID, item.phaseID, item.attempt)
	}
	if unit.runnerStarting {
		return fmt.Errorf("take over unit %q of item %q: unit runner is still starting", unitID, itemID)
	}
	if unit.status != store.WorkItemUnitRunning || !unit.runnerActive {
		return fmt.Errorf("take over unit %q of item %q: unit is %s, want a live %s unit", unitID, itemID, unit.status, store.WorkItemUnitRunning)
	}
	partial, err := e.runner.StopForTakeover(e.ctx, RunKey{
		ItemID: itemID, PhaseID: item.phaseID, Attempt: item.attempt, UnitID: unitID,
	})
	if err != nil {
		return fmt.Errorf("take over unit %q of item %q: stop live unit: %w", unitID, itemID, err)
	}
	// The unit is now the human's. StopForTakeover left its session alive, so
	// teardownUnit must not stop it again.
	unit.runnerActive = false
	if err := e.teardownUnit(item, unit, store.WorkItemUnitTakenOver, partial, "taken over for human steering"); err != nil {
		return err
	}
	return e.advanceFanOut(item)
}

// parkedFanOut loads a parked run and rebuilds the fan-out attempt a recovery
// action addresses. Recovery actions address an attempt that is no longer
// resident, so the state they act on is reconstructed from the record every
// time rather than kept alive in memory.
//
// `action` is the whole subject of every refusal it raises — "retry unit
// \"beta\"", "retry failed units" — so a message names the verb the human used
// and, where there is one, the unit they named.
func (e *Engine) parkedFanOut(itemID, action string) (*runtimeItem, error) {
	if _, tracked := e.items[itemID]; tracked {
		return nil, fmt.Errorf("%s of item %q: the run is still active", action, itemID)
	}
	item, err := e.loadParked(itemID)
	if err != nil {
		return nil, err
	}
	if !recoverableUnitPark(Reason(item.item.Reason)) {
		return nil, fmt.Errorf(
			"%s of item %q: item is parked %q; unit recovery applies to runs parked %s, %s, or %s",
			action, itemID, item.item.Reason,
			ReasonUnitFailed, ReasonInterrupted, ReasonTakenOver,
		)
	}
	if item.phaseID == "" || item.attempt < 1 {
		return nil, fmt.Errorf("%s of item %q: current phase attempt is missing", action, itemID)
	}
	if err := e.restoreFanOut(item); err != nil {
		return nil, fmt.Errorf("%s of item %q: %w", action, itemID, err)
	}
	return item, nil
}

// parkedUnit is parkedFanOut plus the one unit inside it a per-unit action
// names.
func (e *Engine) parkedUnit(itemID, unitID, action string) (*runtimeItem, *unitRun, error) {
	if strings.TrimSpace(unitID) == "" {
		return nil, nil, fmt.Errorf("%s of item %q: unit id is required", action, itemID)
	}
	item, err := e.parkedFanOut(itemID, fmt.Sprintf("%s %q", action, unitID))
	if err != nil {
		return nil, nil, err
	}
	unit := item.fan.find(unitID)
	if unit == nil {
		return nil, nil, fmt.Errorf(
			"%s %q of item %q: unit is not part of attempt %s/%d",
			action, unitID, itemID, item.phaseID, item.attempt,
		)
	}
	return item, unit, nil
}

// repairable is the one rule both retry and drop apply: they act on a work
// unit resting in a state that blocks its attempt. The join is not repairable —
// it settles with the attempt, and re-running it is re-running the phase.
func repairable(itemID, unitID, action string, unit *unitRun) error {
	if unit.kind == UnitJoin {
		return fmt.Errorf("%s %q of item %q: the join settles with its attempt; repair a unit instead", action, unitID, itemID)
	}
	if unit.status != store.WorkItemUnitFailed && unit.status != store.WorkItemUnitTakenOver {
		return fmt.Errorf(
			"%s %q of item %q: unit is %s; retry and drop apply to units resting %s or %s",
			action, unitID, itemID, unit.status, store.WorkItemUnitFailed, store.WorkItemUnitTakenOver,
		)
	}
	return nil
}

// recoverableUnitPark reports whether a parked run is one whose units are the
// state worth repairing: a unit failed, the run was stopped mid-attempt (by a
// pause or by the process dying), or a human took a unit over.
func recoverableUnitPark(reason Reason) bool {
	switch reason {
	case ReasonUnitFailed, ReasonTakenOver:
		return true
	default:
		return ResumableReason(reason)
	}
}

// resumeRepairedFanOut returns an attempt to running once nothing blocks it any
// more. While another unit still rests failed or taken over the run stays
// parked: the record is updated, but resuming would only re-park at the next
// rest, and the units the human has not decided about yet would run against a
// join that can never receive them.
//
// The attempt row is reopened rather than superseded: its finished units keep
// their results, so re-entering the phase with a fresh attempt would throw away
// exactly the work the recovery action is preserving.
func (e *Engine) resumeRepairedFanOut(item *runtimeItem) error {
	if reason, _ := item.fan.blocked(); reason != "" {
		return nil
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return fmt.Errorf("resume fan-out %s/%s: phase is absent from the frozen workflow", item.item.ID, item.phaseID)
	}
	if err := e.store.ReopenWorkItemPhase(item.item.ID, item.phaseID, item.attempt); err != nil {
		return fmt.Errorf("resume fan-out %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	e.items[item.item.ID] = item
	e.emitter.Emit("workflow:phase-state", PhaseEvent{
		ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt, Status: "running",
	})
	if halted, err := e.enforceBudget(item); halted {
		return err
	}
	if e.paused {
		e.addWaiting(item, nil)
		return nil
	}
	acquired, ok, err := e.acquirePhaseResources(item.item.ProjectID, phase)
	if err != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
			err,
		)
	}
	if !ok {
		e.addWaiting(item, nil)
		return nil
	}
	item.acquired = acquired
	return e.advanceFanOut(item)
}

func retryFeedback(note string) *Feedback {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return &Feedback{Note: "retried by a human after the previous unit attempt did not complete"}
	}
	return &Feedback{Note: trimmed}
}

func dropNote(note string) string {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "dropped by a human"
	}
	return "dropped by a human: " + trimmed
}
