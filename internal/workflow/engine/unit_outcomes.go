package engine

import (
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// completeUnit records one unit's outcome and re-evaluates the attempt. The
// join's outcome is the phase's outcome; a work unit's non-done outcome is a
// unit failure, which stops further launches and eventually parks.
func (e *Engine) completeUnit(item *runtimeItem, key RunKey, outcome Outcome) error {
	fan := item.fan
	if fan == nil {
		return nil // The attempt was torn down; this completion is stale.
	}
	unit := fan.find(key.UnitID)
	if unit == nil || unit.status != store.WorkItemUnitRunning {
		return nil
	}
	clearUnitStart(unit)
	unit.runnerActive = false
	if unit.kind == UnitJoin {
		// The join reported for itself, so its row settles first; then its
		// envelope is evaluated as the phase's, gate and all.
		if err := e.teardownUnit(item, unit, unitStatusFor(outcome), outcome.Envelope, unitOutcomeNote(outcome)); err != nil {
			return err
		}
		return e.completePhaseOutcome(item, key, outcome)
	}
	if outcome.Kind == OutcomeDone {
		if err := e.teardownUnit(item, unit, store.WorkItemUnitDone, outcome.Envelope, ""); err != nil {
			return err
		}
		return e.advanceFanOut(item)
	}
	// Recording the failure is enough to stop further launches: advanceFanOut
	// derives that from the unit statuses. In-flight units keep running — the
	// park happens once they rest, and every failure recorded before then is in
	// the record, so a run with three failures parks once and lists three.
	if err := e.teardownUnit(item, unit, store.WorkItemUnitFailed, outcome.Envelope, unitOutcomeNote(outcome)); err != nil {
		return err
	}
	return e.advanceFanOut(item)
}

// dequeuePendingUnits drops queued units from the wait FIFO once the attempt
// has stopped launching. They stay `pending` — they never started, so a
// recovery action can still launch them.
func (e *Engine) dequeuePendingUnits(item *runtimeItem) {
	for _, unit := range item.fan.all() {
		if unit.status == store.WorkItemUnitPending && unit.waiting {
			e.removeWaiting(item, unit)
		}
	}
}

// teardownUnit is the only path that releases a unit's resources, and the only
// caller of Runner.Stop for a unit key — the per-unit half of the teardown
// contract, including the runnerStarting window where a unit that has not
// reported yet is still stopped by key.
func (e *Engine) teardownUnit(item *runtimeItem, unit *unitRun, status string, envelope json.RawMessage, note string) error {
	var errs []error
	key := RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt, UnitID: unit.id}
	if unit.runnerStarting {
		clearUnitStart(unit)
		if _, err := e.runner.Stop(e.ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("stop starting unit %s: %w", unit.id, err))
		}
	}
	if unit.runnerActive {
		partial, err := e.runner.Stop(e.ctx, key)
		if err != nil {
			errs = append(errs, fmt.Errorf("stop unit %s: %w", unit.id, err))
		}
		if len(envelope) == 0 && len(partial) > 0 {
			envelope = partial
		}
		unit.runnerActive = false
	}
	if err := e.releaseUnitResources(item, unit); err != nil {
		errs = append(errs, err)
	}
	unit.status = status
	unit.envelope = envelope
	if err := e.store.CompleteWorkItemUnit(
		item.item.ID, item.phaseID, item.attempt, unit.id, status, envelope, note, e.timestamp(),
	); err != nil {
		errs = append(errs, fmt.Errorf(
			"persist unit %s/%s/%d/%s: %w", item.item.ID, item.phaseID, item.attempt, unit.id, err,
		))
	} else {
		e.emitUnitState(item, unit)
	}
	return errors.Join(errs...)
}

// teardownUnits brings an attempt's in-flight units down with the attempt. A
// unit that never started stays pending — it holds nothing and did nothing, so
// a resumed or retried attempt can still launch it.
//
// retainCallChildren is pause's one difference, and it reaches units for the
// same reason it reaches a call phase: pause does not abandon the attempt, so a
// call unit whose child is being paused alongside it stays `running` and resume
// re-links to that child. Every other exit leaves the attempt for good, and its
// call units come down failed with everything else.
func (e *Engine) teardownUnits(item *runtimeItem, phaseStatus string, retainCallChildren bool) error {
	if item.fan == nil {
		return nil
	}
	note := interruptedUnitNote(phaseStatus)
	var errs []error
	for _, unit := range item.fan.all() {
		if unit.status != store.WorkItemUnitRunning {
			e.removeWaiting(item, unit)
			continue
		}
		if retainCallChildren && unit.definition.IsCall() {
			e.removeWaiting(item, unit)
			errs = append(errs, e.releaseUnitResources(item, unit))
			continue
		}
		errs = append(errs, e.teardownUnit(item, unit, store.WorkItemUnitFailed, unit.envelope, note))
	}
	return errors.Join(errs...)
}

// sweepPersistedUnits fails any unit row still marked running for the attempt
// being torn down. In-memory teardown already handled every unit this process
// knows about; this covers the attempt nobody was tracking — the crash-restart
// sweep, where the rows are the only surviving state. Those rows are left
// `failed` with an interrupted note, which is exactly what RetryUnit recovers.
//
// A retaining teardown skips it entirely. Pause is its only trigger and it
// refuses a persisted-running run the scheduler does not hold, so the attempt is
// always resident with live unit state there — the sweep would find nothing
// except the call units retained on purpose, and fail exactly the rows resume
// needs to re-link.
func (e *Engine) sweepPersistedUnits(item *runtimeItem, phaseStatus string, retainCallChildren bool) error {
	if retainCallChildren || item.phaseID == "" || item.attempt < 1 {
		return nil
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok || phase.EffectiveShape() != def.ShapeFanOut {
		return nil
	}
	if _, err := e.store.FailRunningWorkItemUnits(
		item.item.ID, item.phaseID, item.attempt, interruptedUnitNote(phaseStatus), e.timestamp(),
	); err != nil {
		return fmt.Errorf("sweep running units %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	return nil
}

// restoreFanOut rebuilds a parked attempt's fan-out state from persistence. The
// expansion is re-derived from the attempt's frozen input variables, so ids and
// element bindings come out identical to the ones the rows were written with; a
// row the expansion cannot account for is corruption, not something to guess
// around.
func (e *Engine) restoreFanOut(item *runtimeItem) error {
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return fmt.Errorf("restore fan-out %s/%s: phase is absent from the frozen workflow", item.item.ID, item.phaseID)
	}
	if phase.EffectiveShape() != def.ShapeFanOut || phase.Join == nil {
		return fmt.Errorf("restore fan-out %s/%s: phase is not a fan-out with a join", item.item.ID, item.phaseID)
	}
	phases, err := e.store.ListWorkItemPhases(item.item.ID)
	if err != nil {
		return fmt.Errorf("restore fan-out %s/%s: %w", item.item.ID, item.phaseID, err)
	}
	attempt, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok {
		return fmt.Errorf("restore fan-out %s/%s/%d: attempt row is missing", item.item.ID, item.phaseID, item.attempt)
	}
	// Recovery stamps new unit timestamps onto an attempt this process may never
	// have run. Seed the engine clock from the attempt first, so a restarted
	// process cannot record a unit as starting before the attempt it belongs to.
	e.observePhaseTimestamps(attempt)
	var input PhaseInput
	if len(attempt.InputEnvelope) > 0 {
		if err := decodeJSON(attempt.InputEnvelope, &input); err != nil {
			return fmt.Errorf("restore fan-out %s/%s/%d input: %w", item.item.ID, item.phaseID, item.attempt, err)
		}
	}
	expanded, err := def.ExpandUnits(phase, input.Vars)
	if err != nil {
		return fmt.Errorf("restore fan-out %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	rows, err := e.store.ListWorkItemPhaseUnits(item.item.ID, item.phaseID, item.attempt)
	if err != nil {
		return fmt.Errorf("restore fan-out %s/%s/%d units: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	byID := make(map[string]store.WorkItemUnit, len(rows))
	for _, row := range rows {
		byID[row.UnitID] = row
	}
	fan := &fanOutRun{vars: input.Vars, units: make([]*unitRun, 0, len(expanded))}
	for _, unit := range expanded {
		restored, err := restoreUnit(unitRunFrom(unit, UnitWork), byID)
		if err != nil {
			return fmt.Errorf("restore fan-out %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
		}
		fan.units = append(fan.units, restored)
	}
	join, err := restoreUnit(unitRunFrom(def.ExpandedUnit{
		ID: phase.Join.ID, Index: len(expanded), Unit: *phase.Join,
	}, UnitJoin), byID)
	if err != nil {
		return fmt.Errorf("restore fan-out %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	fan.join = join
	fan.joinStarted = join.status != store.WorkItemUnitPending
	item.fan = fan
	return nil
}

func restoreUnit(unit *unitRun, rows map[string]store.WorkItemUnit) (*unitRun, error) {
	row, ok := rows[unit.id]
	if !ok {
		return nil, fmt.Errorf("unit %q has no persisted row", unit.id)
	}
	if row.Status == store.WorkItemUnitRunning && !unit.definition.IsCall() {
		// Teardown marks running rows failed on every exit path, so a row that
		// still claims to be running outlived the process that wrote it. Adopting
		// it would make a dead unit look live forever.
		//
		// A call unit is the exception, and it is the same exception a call phase
		// makes: it holds no runner, so `running` means "its child run is still
		// the thing that reports", and the child is re-linked rather than swept.
		return nil, fmt.Errorf("unit %q is persisted running with no live runner", unit.id)
	}
	unit.status = row.Status
	unit.attempt = row.UnitAttempt
	unit.envelope = row.Envelope
	if row.Feedback != "" {
		unit.feedback = &Feedback{Note: row.Feedback}
	}
	return unit, nil
}

// clearUnitStart releases the startup cancel a unit is holding. Every path that
// stops treating a unit as starting goes through it, so no unit can leave a
// context uncancelled.
func clearUnitStart(unit *unitRun) {
	if unit.runnerStartCancel != nil {
		unit.runnerStartCancel()
	}
	unit.runnerStarting = false
	unit.runnerStartCancel = nil
}

func unitStatusFor(outcome Outcome) string {
	if outcome.Kind == OutcomeDone {
		return store.WorkItemUnitDone
	}
	return store.WorkItemUnitFailed
}

// unitOutcomeNote is the unit-level half of `outcomeDetailCause`: a unit whose
// outcome carries no envelope recorded only its outcome KIND, which says
// nothing a human could act on. The runner's detail is appended under the same
// rule the phase cause follows — only where the envelope is empty, so a unit
// that authored one keeps it as the sole account.
func unitOutcomeNote(outcome Outcome) string {
	if outcome.Kind == OutcomeDone {
		return ""
	}
	note := "unit outcome " + string(outcome.Kind)
	if cause := outcomeDetailCause(outcome); cause != nil {
		note += ": " + cause.Error()
	}
	return note
}

func interruptedUnitNote(phaseStatus string) string {
	if phaseStatus == "" {
		return "interrupted with its phase attempt"
	}
	return "interrupted with its phase attempt (" + phaseStatus + ")"
}
