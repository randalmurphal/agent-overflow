package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// fanOutRun is the live state of one fan-out phase attempt: the units it
// expanded and the join that follows them. It exists only while the attempt is
// resident; every field is reconstructible from persistence (restoreFanOut),
// because a park evicts the item from memory and a recovery action has to pick
// the attempt back up.
type fanOutRun struct {
	vars        map[string]any
	units       []*unitRun
	join        *unitRun
	joinStarted bool
}

// unitRun is one unit's live state. `attempt` is the per-unit retry counter
// (persisted as unit_attempt); the phase attempt it belongs to is the item's.
type unitRun struct {
	id         string
	index      int
	kind       UnitKind
	definition def.Unit
	bindings   map[string]any

	status   string
	attempt  int
	envelope json.RawMessage
	// feedback is the note a recovery action attached to the unit's next start.
	// Like the phase-level seed it is process-local until the start persists it,
	// so a crash in between loses only the convenience copy of a reason the run
	// record already carries.
	feedback *Feedback

	runnerActive      bool
	runnerStarting    bool
	runnerStartCancel context.CancelFunc
	acquired          []string
	waiting           bool
}

// all returns every unit of the attempt, join last. The slice is fresh on each
// call so a caller can never append into the fan-out's own storage.
func (f *fanOutRun) all() []*unitRun {
	all := make([]*unitRun, 0, len(f.units)+1)
	all = append(all, f.units...)
	if f.join != nil {
		all = append(all, f.join)
	}
	return all
}

func (f *fanOutRun) find(unitID string) *unitRun {
	for _, unit := range f.all() {
		if unit.id == unitID {
			return unit
		}
	}
	return nil
}

// blocked reports why the attempt can no longer reach its join, and the partial
// envelope that explains it. It is derived from the unit statuses rather than
// latched into a flag, so an attempt rebuilt from its rows after a crash or a
// park blocks for exactly the same reason the live one did.
//
// An empty reason means the attempt may still launch units. A non-empty one
// stops new launches (spec §3: a failed unit stops the launch of not-yet-started
// units while in-flight units run to completion — their work is durable and
// interrupting it wastes it) and parks the attempt once everything rests.
//
// A unit a human took over outranks a failure: the human is now driving, and
// that is the reason worth parking under.
func (f *fanOutRun) blocked() (Reason, json.RawMessage) {
	var reason Reason
	var output json.RawMessage
	for _, unit := range f.units {
		switch unit.status {
		case store.WorkItemUnitTakenOver:
			return ReasonTakenOver, unit.envelope
		case store.WorkItemUnitFailed:
			if reason == "" {
				reason, output = ReasonUnitFailed, unit.envelope
			}
		}
	}
	return reason, output
}

// resting reports that no unit is running, starting, or queued for capacity —
// the condition under which the attempt either parks or runs its join.
func (f *fanOutRun) resting() bool {
	for _, unit := range f.all() {
		if unit.runnerActive || unit.runnerStarting || unit.waiting {
			return false
		}
	}
	return true
}

func unitRunFrom(expanded def.ExpandedUnit, kind UnitKind) *unitRun {
	return &unitRun{
		id: expanded.ID, index: expanded.Index, kind: kind,
		definition: expanded.Unit, bindings: expanded.Bindings,
		status: store.WorkItemUnitPending, attempt: 1,
	}
}

// startPhaseWork begins the work a phase attempt does once it holds its
// resources: one runner for a single-shape phase, or the fan-out's units. It is
// shared by phase entry, by every path that resumes a held start, and by unit
// recovery, so a resumed fan-out continues its existing attempt instead of
// expanding a second set of units.
func (e *Engine) startPhaseWork(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	switch phase.EffectiveShape() {
	case def.ShapeCall:
		// A call phase starts a child run instead of a runner, then rests with no
		// runner and no capacity until that run reaches a terminal state.
		return e.startCall(item, phase, vars)
	case def.ShapeSingle:
		return e.startRunner(item, phase, vars)
	}
	if item.fan == nil {
		if err := e.expandFanOut(item, phase, vars); err != nil {
			return err
		}
	}
	return e.advanceFanOut(item)
}

// expandFanOut resolves the attempt's units and persists every one of them
// before any of them starts. Rows are written at creation, not at completion: a
// unit's branch and sub-worktree have to be registrable the moment they exist,
// and an attempt whose width lived only in memory could not be recovered after
// a crash.
//
// It is also the one seam every unit that will ever exist passes through, so it
// is where the project's fan-out ceiling is enforced — before the first row,
// worktree, or provider session. The dry-run refuses a static list over the
// ceiling at authoring time; this refuses everything, static and dynamic alike,
// because a frozen snapshot is decoded and never re-validated (a run whose
// definition predates the rule, or whose project lowered its ceiling since,
// reaches here with no finding behind it).
func (e *Engine) expandFanOut(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	if phase.Join == nil {
		return e.parkFanOutSetup(item, ReasonWiringError,
			fmt.Errorf("fan-out phase %q of item %q declares no join", phase.ID, item.item.ID))
	}
	expanded, err := def.ExpandUnits(phase, vars)
	if err != nil {
		// A missing or non-array `over` variable at runtime is the frozen
		// definition and the live context failing to produce runnable work.
		return e.parkFanOutSetup(item, ReasonWiringError,
			fmt.Errorf("expand fan-out %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	maxWidth, err := e.maxFanOutWidth(item.item.ProjectID)
	if err != nil {
		return e.parkFanOutSetup(item, ReasonSetupFailed,
			fmt.Errorf("expand fan-out %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	if len(expanded) > maxWidth {
		// A wiring error, for the same reason the non-array `over` above is one:
		// the frozen definition and the live project together could not produce
		// runnable work. Nothing has started and nothing can be retried unit by
		// unit — the answer is to raise the ceiling or narrow the data.
		return e.parkFanOutSetup(item, ReasonWiringError, fmt.Errorf(
			"fan-out phase %q of item %q expands to %d units, above the project maximum fan-out width of %d; raise max_fan_out_width in the project profile or fan out over fewer units",
			phase.ID, item.item.ID, len(expanded), maxWidth,
		))
	}
	fan := &fanOutRun{vars: vars, units: make([]*unitRun, 0, len(expanded))}
	for _, unit := range expanded {
		fan.units = append(fan.units, unitRunFrom(unit, UnitWork))
	}
	fan.join = unitRunFrom(def.ExpandedUnit{
		ID: phase.Join.ID, Index: len(expanded), Unit: *phase.Join,
	}, UnitJoin)

	rows := make([]store.WorkItemUnit, 0, len(fan.units)+1)
	for _, unit := range fan.all() {
		rows = append(rows, store.WorkItemUnit{
			ItemID: item.item.ID, PhaseID: phase.ID, Attempt: item.attempt,
			UnitID: unit.id, UnitIndex: unit.index, Kind: string(unit.kind),
			Provider: unit.definition.Provider, Model: unit.definition.Model,
			Status: unit.status, UnitAttempt: unit.attempt,
		})
	}
	if err := e.store.CreateWorkItemUnits(rows); err != nil {
		return e.parkFanOutSetup(item, ReasonSetupFailed,
			fmt.Errorf("persist fan-out units %s/%s/%d: %w", item.item.ID, phase.ID, item.attempt, err))
	}
	item.fan = fan
	for _, unit := range fan.all() {
		e.emitUnitState(item, unit)
	}
	return nil
}

// maxFanOutWidth is the ceiling this project's next expansion gets, read from
// the live profile like every other bound (§6). It is deliberately not frozen
// into the run snapshot: lowering the ceiling has to take effect on the next
// expansion, including the next attempt of a run that is already going.
func (e *Engine) maxFanOutWidth(projectID string) (int, error) {
	projectProfile, err := e.liveProfile(projectID)
	if err != nil {
		return 0, err
	}
	return def.EffectiveMaxFanOutWidth(projectProfile), nil
}

// parkFanOutSetup parks an attempt that could not be expanded at all. Nothing
// has started, so this is a phase-level park with the phase-level reason rather
// than the unit-failure policy. The cause is written onto the attempt as its
// envelope: no unit ran to author one, and the cause is the only place the
// resolved width or the unusable `over:` value is stated.
func (e *Engine) parkFanOutSetup(item *runtimeItem, reason Reason, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			output:      parkCauseEnvelope(cause),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}

// advanceFanOut is the fan-out's whole scheduling decision in one place: launch
// what may still launch, then — once nothing is in flight and nothing is queued
// — either park on the recorded reason or run the join. Every event that can
// change an attempt's resting state calls it.
func (e *Engine) advanceFanOut(item *runtimeItem) error {
	fan := item.fan
	if fan == nil {
		return nil
	}
	var errs []error
	blockReason, blockOutput := fan.blocked()
	if blockReason == "" {
		for _, unit := range fan.units {
			if unit.status != store.WorkItemUnitPending || unit.waiting {
				continue
			}
			if err := e.startUnitOrWait(item, unit); err != nil {
				errs = append(errs, err)
			}
			// A failed start or acquisition tears the attempt down; anything left
			// in the list belongs to an attempt that no longer exists.
			if item.fan != fan || State(item.item.State) != StateRunning {
				return errors.Join(errs...)
			}
		}
	} else {
		// Queued units are not launching any more, and must not hold the capacity
		// the resting units are about to release.
		e.dequeuePendingUnits(item)
	}
	if !fan.resting() {
		return errors.Join(errs...)
	}
	if blockReason != "" {
		errs = append(errs, e.teardown(item, teardownRequest{
			output: blockOutput, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: blockReason,
		}))
		return errors.Join(errs...)
	}
	if !fan.joinStarted {
		errs = append(errs, e.startJoin(item))
	}
	return errors.Join(errs...)
}

// startUnitOrWait starts one unit, or holds it in the same FIFO a held phase
// start uses. Global pause holds unit starts exactly like phase starts: while
// paused nothing new starts anywhere, and unpause replays both through the one
// startWaiting release path.
func (e *Engine) startUnitOrWait(item *runtimeItem, unit *unitRun) error {
	if e.paused {
		e.addWaiting(item, unit)
		return nil
	}
	acquired, ok, err := e.acquireUnitResources(item.item.ProjectID, unit.definition)
	if err != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
			fmt.Errorf("acquire unit %s/%s/%d/%s resources: %w", item.item.ID, item.phaseID, item.attempt, unit.id, err),
		)
	}
	if !ok {
		e.addWaiting(item, unit)
		return nil
	}
	unit.acquired = acquired
	return e.startUnitRunner(item, unit)
}

// startUnitRunner persists the unit's running state and dispatches it on a
// worker goroutine, mirroring startRunner. The row moves to running before the
// worker is dispatched, so a crash inside the startup window still leaves a row
// the teardown sweep can find and fail.
func (e *Engine) startUnitRunner(item *runtimeItem, unit *unitRun) error {
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID),
		)
	}
	vars, err := e.unitVars(item, unit)
	if err != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			err,
		)
	}
	note := ""
	if unit.feedback != nil {
		note = unit.feedback.Note
	}
	if err := e.store.StartWorkItemUnit(
		item.item.ID, item.phaseID, item.attempt, unit.id, unit.attempt, note, e.timestamp(),
	); err != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
			fmt.Errorf("persist unit start %s/%s/%d/%s: %w", item.item.ID, item.phaseID, item.attempt, unit.id, err),
		)
	}
	unit.status = store.WorkItemUnitRunning
	unit.envelope = nil
	e.emitUnitState(item, unit)

	definition := unit.definition
	request := RunRequest{
		Key: RunKey{
			ItemID: item.item.ID, PhaseID: item.phaseID,
			Attempt: item.attempt, UnitID: unit.id,
		},
		Item: item.item, Workflow: item.workflow, WorkspaceNeed: item.workspaceNeed,
		Phase: phase,
		Unit:  &definition, UnitIndex: unit.index, UnitKind: unit.kind,
		UnitAttempt: unit.attempt,
		Vars:        vars, Feedback: cloneFeedback(unit.feedback),
	}
	if unit.kind == UnitJoin {
		// A fan-out's phase-level continuation is always the join's: it is the
		// only unit whose envelope is the phase's. Consumed here exactly as
		// startRunner consumes it, so a continuation can never leak into the
		// phase that follows.
		request.PriorThreadID = item.priorThreadID
		request.FinalizeTakeover = item.takeoverFinalize
		item.priorThreadID = ""
	}
	startCtx, cancel := context.WithCancel(e.ctx)
	future := &runnerStartFuture{key: request.Key, done: make(chan response, 1)}
	unit.runnerStarting = true
	unit.runnerStartCancel = cancel
	e.commandStarts = append(e.commandStarts, future)
	e.inflightStarts[future] = struct{}{}
	unit.feedback = nil
	entered := make(chan struct{})
	go func() {
		err := e.runner.Start(startCtx, request, func() { close(entered) }, func(outcome Outcome) {
			select {
			case e.commands <- completionCommand{key: request.Key, outcome: outcome}:
			case <-e.done:
			}
		})
		result := runnerStartCommand{key: request.Key, future: future, err: err}
		select {
		case e.commands <- result:
		case <-e.done:
			settleRunnerStart(future, response{err: fmt.Errorf(
				"start unit %s/%s/%d/%s: engine closed before startup settled",
				request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt, request.Key.UnitID,
			)})
		}
	}()
	// Wait only until the worker enters Runner.Start, exactly as a phase start
	// does; provisioning happens after the acknowledgement, on the worker.
	<-entered
	return nil
}

// unitVars is the variable context one unit runs with: the attempt's context
// plus, for a dynamic expansion, its bound element. The join instead receives
// the results of the units it consolidates.
func (e *Engine) unitVars(item *runtimeItem, unit *unitRun) (map[string]any, error) {
	vars := make(map[string]any, len(item.fan.vars)+1)
	for name, value := range item.fan.vars {
		vars[name] = value
	}
	if unit.kind != UnitJoin {
		for name, value := range unit.bindings {
			vars[name] = value
		}
		return vars, nil
	}
	results, err := e.unitResults(item)
	if err != nil {
		return nil, err
	}
	// Bound last, and under the name def declares to the prompt validator, so a
	// workflow variable of the same name can never shadow the results the join
	// exists to consolidate.
	vars[def.UnitsVariable] = results
	return vars, nil
}

// unitResults reads the persisted unit rows so the join sees what actually
// happened, including the branch and thread the runner registered for each
// unit. Dropped units stay in the list: a join consolidating survivors has to
// be able to say what it did not receive.
func (e *Engine) unitResults(item *runtimeItem) ([]any, error) {
	rows, err := e.store.ListWorkItemPhaseUnits(item.item.ID, item.phaseID, item.attempt)
	if err != nil {
		return nil, fmt.Errorf("read fan-out units %s/%s/%d: %w", item.item.ID, item.phaseID, item.attempt, err)
	}
	results := make([]any, 0, len(rows))
	for _, row := range rows {
		if row.Kind == store.WorkItemUnitKindJoin {
			continue
		}
		result := map[string]any{
			"id":     row.UnitID,
			"index":  row.UnitIndex,
			"status": row.Status,
		}
		for name, value := range map[string]string{
			"branch": row.Branch, "worktree": row.WorktreePath, "thread": row.ThreadID,
		} {
			if value != "" {
				result[name] = value
			}
		}
		if len(row.Envelope) > 0 {
			var envelope controlEnvelope
			if err := decodeJSON(row.Envelope, &envelope); err != nil {
				return nil, fmt.Errorf(
					"decode unit envelope %s/%s/%d/%s: %w",
					item.item.ID, item.phaseID, item.attempt, row.UnitID, err,
				)
			}
			if envelope.Outputs != nil {
				result["outputs"] = envelope.Outputs
			}
		}
		results = append(results, result)
	}
	return results, nil
}

// startJoin runs the join as the attempt's final unit. Its envelope becomes the
// phase's envelope, so the existing gate path evaluates it unchanged.
func (e *Engine) startJoin(item *runtimeItem) error {
	fan := item.fan
	if fan.join == nil {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			fmt.Errorf("fan-out %s/%s/%d has no join", item.item.ID, item.phaseID, item.attempt),
		)
	}
	fan.joinStarted = true
	return e.startUnitOrWait(item, fan.join)
}

func (e *Engine) emitUnitState(item *runtimeItem, unit *unitRun) {
	e.emitter.Emit("workflow:phase-state", PhaseEvent{
		ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt,
		Status: unit.status, UnitID: unit.id, UnitIndex: unit.index, UnitKind: unit.kind,
	})
}
