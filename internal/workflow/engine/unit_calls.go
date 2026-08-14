package engine

import (
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// A call-bound fan-out unit (§3a at unit scope). The unit runs no turn of its
// own: it starts a child run linked to this attempt and this unit, then rests
// until that run reaches a terminal state. Its envelope is the child's declared
// outputs, exactly as a call phase's is, so the join consolidates a called
// workflow's deliverables the same way it consolidates an agent unit's.
//
// The difference from a phase call is where the child executes. Isolation is
// introduced by fan-out (§9), so a unit's child runs in the *unit's*
// sub-worktree rather than the caller's — which is what makes "each unit runs a
// whole sub-workflow on its own branch, then the join merges" expressible at
// all. The engine stamps no workspace on such a child; the app resolves it
// through the child's parent linkage, cutting the sub-worktree on first use.

// startUnitCall begins one call unit: it persists the unit running, evaluates
// the unit's arguments against its own variable context, and starts the child.
//
// The unit row moves to running *before* the child exists, mirroring the phase
// call's ordering for the same reason: the only gap a crash can open is a
// running unit with no child, which recovery re-invokes. The reverse order would
// leave a child run no unit row claims.
func (e *Engine) startUnitCall(item *runtimeItem, unit *unitRun) error {
	vars, err := e.unitVars(item, unit)
	if err != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{
				cause: err, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: ReasonWiringError,
			}),
			err,
		)
	}
	invocation := callInvocation{
		edge: callEdge{
			phaseID: item.phaseID, unitID: unit.id, maxDepth: unit.definition.MaxDepth,
		},
		target: unit.definition.CallTarget(), declared: unit.definition.Args, vars: vars,
	}
	// Planned before the unit row moves, so a call that cannot be made is not a
	// unit failure: nothing runnable was ever produced, the row is untouched, and
	// the attempt parks under the phase-level reason a single-shape phase takes.
	plan, reason, err := e.planCall(item, invocation)
	if err != nil {
		return e.parkUnitCallSetup(item, reason, err)
	}
	note := ""
	if unit.feedback != nil {
		note = unit.feedback.Note
	}
	startedAt := e.timestamp()
	if err := e.store.StartWorkItemUnit(
		item.item.ID, item.phaseID, item.attempt, unit.id, unit.attempt, note, startedAt,
	); err != nil {
		return e.parkUnitCallSetup(item, ReasonSetupFailed, fmt.Errorf(
			"persist unit call start %s/%s/%d/%s: %w", item.item.ID, item.phaseID, item.attempt, unit.id, err,
		))
	}
	unit.status = store.WorkItemUnitRunning
	unit.envelope = nil
	unit.feedback = nil
	e.emitUnitStateAt(item, unit, startedAt)

	reason, err = e.invokeCall(item, invocation, plan)
	if reason != "" {
		return e.parkUnitCallSetup(item, reason, err)
	}
	return err
}

// parkUnitCallSetup parks an attempt whose call unit could not be invoked. The
// cause rides the attempt's `park_cause`: no unit ran to author an envelope, and
// the cause carries the only statement of what went wrong (the call chain of a
// depth refusal, the argument that would not resolve).
func (e *Engine) parkUnitCallSetup(item *runtimeItem, reason Reason, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked", nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}

// unitCallChildOf returns the child run one call unit of the current attempt
// created. A retried unit makes a new child on the same key, so the newest row
// is the invocation the unit is actually waiting on.
func (e *Engine) unitCallChildOf(item *runtimeItem, unitID string) (store.WorkItem, bool, error) {
	children, err := e.store.ListWorkItemUnitCallChildren(item.item.ID, item.phaseID, item.attempt, unitID)
	if err != nil {
		return store.WorkItem{}, false, fmt.Errorf(
			"load call children of %s/%s/%d/%s: %w", item.item.ID, item.phaseID, item.attempt, unitID, err,
		)
	}
	if len(children) == 0 {
		return store.WorkItem{}, false, nil
	}
	return children[len(children)-1], true, nil
}

// settleUnitCallChild maps a finished child run onto the call unit that started
// it. A done child is the unit's success, carrying the child workflow's declared
// outputs as the unit's envelope; anything else is a unit failure, which stops
// further launches and parks the attempt `unit-failed` once its siblings rest —
// the same policy an agent unit's failure takes, because to the fan-out this is
// one unit that did not produce a result.
func (e *Engine) settleUnitCallChild(parent *runtimeItem, child store.WorkItem) error {
	if parent.fan == nil {
		return nil // The attempt was torn down; this completion is stale.
	}
	unit := parent.fan.find(child.ParentUnitID)
	if unit == nil || unit.status != store.WorkItemUnitRunning || !unit.definition.IsCall() {
		return nil
	}
	current, found, err := e.unitCallChildOf(parent, unit.id)
	if err != nil {
		return err
	}
	if !found || current.ID != child.ID {
		// A retry already replaced this invocation; the unit is waiting on the
		// newer child, so this completion is stale rather than an error.
		return nil
	}
	switch State(child.State) {
	case StateDone:
		envelope, outputErr := e.childOutputEnvelope(child)
		if outputErr != nil {
			// The child finished but its declared outputs cannot be read, so there
			// is no envelope to hand the join. Recorded as this unit's failure with
			// the cause, rather than failing the whole attempt outright — the other
			// units' work is still durable and the human repairs this one.
			//
			// The cause goes in the unit's NOTE, which is the row's channel for
			// engine text, and the envelope stays empty: a synthesized envelope
			// here would be read downstream as something the unit's run produced.
			note := unitChildNote(child, "completed without its declared outputs") + ": " + outputErr.Error()
			return errors.Join(
				e.teardownUnit(parent, unit, store.WorkItemUnitFailed, nil, note, 0),
				e.advanceFanOut(parent),
				outputErr,
			)
		}
		if err := e.teardownUnit(parent, unit, store.WorkItemUnitDone, envelope, "", 0); err != nil {
			return err
		}
		return e.advanceFanOut(parent)
	case StateFailed, StateCancelled:
		outcome := "failed"
		if State(child.State) == StateCancelled {
			outcome = "cancelled"
		}
		if err := e.teardownUnit(
			parent, unit, store.WorkItemUnitFailed,
			childOutcomeEnvelope(child, outcome), unitChildNote(child, outcome), 0,
		); err != nil {
			return err
		}
		return e.advanceFanOut(parent)
	default:
		return nil // Still running or parked: the unit keeps waiting.
	}
}

// unitChildNote is what the unit row records about a child that did not deliver.
// It names the run so the tree is navigable from the failed unit.
func unitChildNote(child store.WorkItem, outcome string) string {
	note := fmt.Sprintf("child run %s (workflow %q) %s", child.ID, child.WorkflowID, outcome)
	if child.Reason != "" {
		note += ": " + child.Reason
	}
	return note
}

// recoverUnitCall re-links one call unit to the child its invocation created. It
// is the unit-scoped twin of recoverCall and is used by both recovery paths that
// pick a fan-out attempt back up — the startup rebuild and a resume — so a
// campaign's in-flight sub-workflows survive a restart instead of being
// relaunched from scratch.
//
// The missing-child case is safe for the same reason it is at phase scope: the
// unit row is persisted running before the child is created, so the only gap a
// crash can open is a unit with no child, which re-invokes cleanly.
func (e *Engine) recoverUnitCall(item *runtimeItem, unit *unitRun) error {
	child, found, err := e.unitCallChildOf(item, unit.id)
	if err != nil {
		return err
	}
	if !found {
		return e.startUnitCall(item, unit)
	}
	if State(child.State) == StateRunning || State(child.State) == StateNeedsHuman {
		return nil // The unit rests; the child's own completion re-enters it.
	}
	return e.settleUnitCallChild(item, child)
}

// restingCallUnits reports the call-bound units of an attempt that are persisted
// running — the ones a recovery path has to re-link rather than sweep. A unit
// resting `running` with no runner is a legitimate state for a call unit and
// only for a call unit, which is what makes this the safe discriminator for the
// rebuild's "is this attempt recoverable in place" question.
func restingCallUnits(fan *fanOutRun) []*unitRun {
	var resting []*unitRun
	for _, unit := range fan.units {
		if unit.definition.IsCall() && unit.status == store.WorkItemUnitRunning {
			resting = append(resting, unit)
		}
	}
	return resting
}

// recoverFanOutCalls adopts a fan-out attempt that a crash left holding live
// child runs, instead of parking it and abandoning them. It reports whether the
// attempt was adopted; false means the caller should take the ordinary
// interrupted park, which is the right answer for every fan-out with no call
// unit still linked to a child.
//
// The runner-backed units of the attempt are dead whatever happens — the process
// that ran them is gone — so they are failed first, exactly as the crash sweep
// would leave them, and a resume repairs them alongside anything else that
// failed. Only the call units survive the restart, and they survive because
// their work is a child run this same rebuild is putting back on its feet.
func (e *Engine) recoverFanOutCalls(item *runtimeItem, attempt store.WorkItemPhase) (bool, error) {
	phase, ok := findPhase(item.workflow, attempt.PhaseID)
	if !ok || phase.EffectiveShape() != def.ShapeFanOut || !phaseDeclaresCallUnit(phase) {
		return false, nil
	}
	if err := e.failInterruptedUnitRows(item, phase, attempt); err != nil {
		return false, err
	}
	if err := e.restoreFanOut(item); err != nil {
		return false, err
	}
	if len(restingCallUnits(item.fan)) == 0 {
		// Nothing is linked to a live child, so there is nothing this path can
		// preserve that the ordinary park would lose.
		item.fan = nil
		return false, nil
	}
	acquired, available, err := e.acquirePhaseResources(item.item.ProjectID, phase)
	if err != nil {
		return false, err
	}
	if !available {
		// The attempt's declared resources are held by other work this rebuild has
		// already adopted. Adopting it anyway would hold a lock the semaphores did
		// not grant, so the run takes the ordinary interrupted park and its live
		// children come down with it through the teardown contract.
		item.fan = nil
		return false, nil
	}
	item.acquired = acquired
	var errs []error
	for _, unit := range restingCallUnits(item.fan) {
		if item.fan == nil || State(item.item.State) != StateRunning {
			break
		}
		errs = append(errs, e.recoverUnitCall(item, unit))
	}
	if item.fan != nil && State(item.item.State) == StateRunning {
		errs = append(errs, e.advanceFanOut(item))
	}
	return true, errors.Join(errs...)
}

// failInterruptedUnitRows fails every runner-backed unit row of one attempt that
// is still persisted running. It is the targeted form of the crash sweep: a call
// unit's `running` row is a live relationship rather than a dead runner, so it
// is deliberately left alone for recoverUnitCall to re-link.
func (e *Engine) failInterruptedUnitRows(item *runtimeItem, phase def.Phase, attempt store.WorkItemPhase) error {
	rows, err := e.store.ListWorkItemPhaseUnits(item.item.ID, attempt.PhaseID, attempt.Attempt)
	if err != nil {
		return fmt.Errorf("read fan-out units %s/%s/%d: %w", item.item.ID, attempt.PhaseID, attempt.Attempt, err)
	}
	note := interruptedUnitNote("parked")
	var errs []error
	for _, row := range rows {
		if row.Status != store.WorkItemUnitRunning {
			continue
		}
		definition, found := def.UnitDefinition(phase, row.UnitID, row.Kind == store.WorkItemUnitKindJoin)
		if found && definition.IsCall() {
			continue
		}
		if err := e.store.CompleteWorkItemUnit(
			item.item.ID, attempt.PhaseID, attempt.Attempt, row.UnitID,
			store.WorkItemUnitFailed, row.Envelope, note, 0, e.timestamp(),
		); err != nil {
			errs = append(errs, fmt.Errorf(
				"fail interrupted unit %s/%s/%d/%s: %w",
				item.item.ID, attempt.PhaseID, attempt.Attempt, row.UnitID, err,
			))
		}
	}
	return errors.Join(errs...)
}

// phaseDeclaresCallUnit reports whether any unit the phase can stamp invokes a
// workflow. The join is excluded because validation refuses a call join.
func phaseDeclaresCallUnit(phase def.Phase) bool {
	for _, unit := range phase.UnitDefinitions() {
		if unit.IsCall() {
			return true
		}
	}
	return false
}
