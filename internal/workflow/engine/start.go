package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

// startNewItem persists a newly admitted run and begins its first phase. The
// row is created before the definition is resolved so a workflow that broke
// between validation and admission parks a visible run record rather than
// vanishing.
//
// `resolved` is the definition a call phase already froze for its child; a
// root start passes nil and resolves here. Passing it through means one
// invocation resolves the child exactly once, so the definition the parent
// validated is the definition the child runs.
func (e *Engine) startNewItem(item store.WorkItem, resolved *ResolvedDefinition) error {
	if item.ID == "" || item.ProjectID == "" {
		return fmt.Errorf("start item: id and project id are required")
	}
	if item.ParentItemID != "" && len(item.Budget) > 0 {
		// §12: one ceiling per tree, enforced against the root. A budget on a
		// child would be a second, silently-ignored one.
		return fmt.Errorf("start item %q: only a root run carries a budget; it is enforced across the whole call tree", item.ID)
	}
	if resolved == nil && item.ParentItemID != "" {
		// Child runs exist only because a call phase created them, and they carry
		// a budget-free, workspace-inherited shape that only that path builds.
		// Admitting one from outside would produce a run whose tree accounting is
		// wrong from birth.
		return fmt.Errorf("start item %q: parent linkage is set by the call path, not by intake", item.ID)
	}
	if state := State(item.State); state != "" && state != StateRunning {
		return fmt.Errorf("start item %q: state must be empty or running, got %q", item.ID, item.State)
	}
	if len(item.Seeds) > MaxSeedBytes {
		return fmt.Errorf("start item %q: seeds are %d bytes; maximum is %d", item.ID, len(item.Seeds), MaxSeedBytes)
	}
	if len(item.Seeds) > 0 {
		var seeds map[string]any
		if err := decodeJSON(item.Seeds, &seeds); err != nil || seeds == nil {
			return fmt.Errorf("start item %q: seeds must be one JSON object", item.ID)
		}
	}
	if _, exists := e.items[item.ID]; exists {
		return fmt.Errorf("start item %q: already tracked", item.ID)
	}
	item.State = string(StateRunning)
	item.Reason = ""
	item.EndedAt = 0
	if err := e.store.CreateWorkItem(item); err != nil {
		return fmt.Errorf("start item %q: %w", item.ID, err)
	}
	runtime := &runtimeItem{item: item}
	e.items[item.ID] = runtime
	e.emitItemState(item.ID, item.ProjectID, "", StateRunning, "")
	return e.beginRun(runtime, resolved)
}

// beginRun freezes the resolved workflow into the run record and enters the
// first phase. The item is already persisted running, so every failure here
// parks it through teardown instead of leaving an unstarted row.
func (e *Engine) beginRun(item *runtimeItem, resolved *ResolvedDefinition) error {
	if resolved == nil {
		definition, err := e.definitions.Resolve(e.ctx, item.item)
		if err != nil {
			return e.parkUnstartable(item, fmt.Errorf("resolve item %q workflow: %w", item.item.ID, err))
		}
		resolved = &definition
	}
	workflow := resolved.Workflow
	if len(workflow.Phases) == 0 {
		return e.parkUnstartable(item, fmt.Errorf("resolve item %q workflow: workflow has no phases", item.item.ID))
	}
	// Named before the freeze, not after it: a failure from here on is a run
	// that could not start its FIRST PHASE, and knowing which phase is what lets
	// the park rest on an attempt row carrying its cause instead of on nothing.
	item.phaseID = workflow.Phases[0].ID
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow, WorkspaceNeed: resolved.WorkspaceNeed})
	if err != nil {
		return e.parkUnstartable(item, fmt.Errorf("snapshot item %q workflow: %w", item.item.ID, err))
	}
	if len(snapshot) > MaxSnapshotBytes {
		return e.parkUnstartable(item, fmt.Errorf("snapshot item %q workflow is %d bytes; maximum is %d", item.item.ID, len(snapshot), MaxSnapshotBytes))
	}
	startedAt := e.timestamp()
	if err := e.store.UpdateWorkItemRunStart(
		item.item.ID, snapshot, item.item.WorktreePath, item.item.Branch,
		item.item.BaseBranch, startedAt,
	); err != nil {
		return e.parkUnstartable(item, fmt.Errorf("freeze item %q run start: %w", item.item.ID, err))
	}
	item.item.Snapshot = snapshot
	item.item.StartedAt = startedAt
	item.workflow = workflow
	item.workspaceNeed = resolved.WorkspaceNeed
	return e.enterPhase(item, entryFresh)
}

// parkUnstartable parks a run that never reached its first phase. A run whose
// workflow would not resolve at all has no phase to rest an attempt on, so its
// cause reaches the caller and the engine log and stops there — which is the
// honest record, since a run with zero attempt rows is exactly a run that never
// entered a phase.
func (e *Engine) parkUnstartable(item *runtimeItem, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonSetupFailed,
		}),
		cause,
	)
}

// phaseEntry says what the attempt about to be created IS: a fresh entry into
// the phase, or the continuation of a round that already ran.
//
// It is a parameter rather than something inferred from the item, because
// nothing on the item distinguishes the two — a warm loop round and an `Answer`
// continuation both carry a prior thread id, and both create a new attempt row.
// What separates them is which caller asked, so the caller says. It decides one
// thing today: whether pending operator guidance is delivered here. A
// continuation continues a turn the operator was already steering; guidance
// delivered into it would arrive as a second instruction to a round that has
// already read the first.
type phaseEntry int

const (
	// entryFresh starts the phase over: a gate advance or loop, a resume aimed at
	// a phase, a rerun, the run's first phase, and the resume whose parked
	// attempt had no session left to continue.
	entryFresh phaseEntry = iota
	// entryContinuation continues the round the parked attempt was in, on the
	// session it parked on: an answered question, a finalized takeover, a bare
	// resume of a continuable park.
	entryContinuation
)

// entryPromptRoute resolves which loop route's `prompt:` override the attempt
// being created renders.
//
// A FRESH entry renders what the decision that produced it armed — and only
// that: `consumePromptRoute` refuses an arming whose route does not loop to
// THIS phase, so a coordinate that outlived its entry is inert rather than a
// body borrowed by the next phase. A CONTINUATION renders what the round it
// continues rendered, restored from that round's own persisted input, because a
// continuation is the same round asked the same narrower question.
func entryPromptRoute(item *runtimeItem, entry phaseEntry, phaseID string) *PromptRoute {
	if entry == entryContinuation {
		return item.promptRoute
	}
	return consumePromptRoute(item.workflow, item.nextPromptRoute, phaseID)
}

// consumesPriorSession reports whether entering this phase can hand a prior
// provider session to something that starts a turn on it. Only a call phase
// cannot: it starts a child run and rests.
func consumesPriorSession(phase def.Phase) bool { return !phase.IsCall() }

// enterPhase records the next phase attempt and starts it as soon as the
// global pause is clear and its resources are free. A held phase leaves the
// item running and waiting, never parked.
func (e *Engine) enterPhase(item *runtimeItem, entry phaseEntry) error {
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		cause := fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID)
		return errors.Join(
			e.teardown(item, teardownRequest{
				cause: cause, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: ReasonWiringError,
			}),
			cause,
		)
	}
	if halted, err := e.enforceBudget(item); halted {
		return err
	}
	// A new attempt never inherits the previous one's fan-out. Teardown already
	// clears it on every exit path; this keeps that true by construction rather
	// than by the caller having come through teardown.
	item.fan = nil
	vars, priorPhases, err := e.variableContext(item, attemptRef{})
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	delivered, err := e.deliverGuidance(item, phase, entry)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	if len(delivered) > 0 {
		item.feedback = appendFeedbackNote(item.feedback, guidanceNote(delivered))
	}
	// Assigned on every entry, delivered or not: they belong to the attempt being
	// created, so leaving a previous attempt's values in place would render them
	// again for a round nobody asked for either on. The entries stay owed an
	// acknowledgement until a session that renders them starts (`ackGuidance`).
	item.guidance, item.guidanceUnacked = delivered, delivered
	// The arming is consumed here and nowhere else, and a fresh entry consumes it
	// only when the armed route's loop target is the phase being entered.
	item.promptRoute = entryPromptRoute(item, entry, phase.ID)
	item.nextPromptRoute = nil
	if !consumesPriorSession(phase) {
		// A call phase runs no turn of its own: it starts a child run and rests.
		// Nothing in this attempt will ever hand a prior session to a runner, so
		// an id still set here would be consumed by whatever phase starts NEXT.
		// Every other shape consumes it at its own start — `startRunner` for a
		// single-shape phase, the join for a fan-out.
		item.priorThreadID = ""
	}
	attempt := nextAttempt(priorPhases, phase.ID)
	phaseInput := PhaseInput{
		Vars: vars, Feedback: item.feedback,
		PromptRoute: item.promptRoute, Guidance: delivered,
	}
	if phase.IsCall() {
		// A call phase's input *is* its argument map: it runs no turn, so the args
		// are the only thing it hands anywhere. Persisting them on the attempt row
		// makes the invocation auditable and lets a rebuild re-invoke the same
		// call after a crash between the attempt and the child.
		//
		// An argument whose reference does not resolve is simply absent from the
		// record. Whether that is legal is the child's declared inputs' answer,
		// and only the call edge has resolved the child to ask — by which point
		// this row exists to carry the refusal, which is the difference between a
		// diagnosable park and a run that stopped with no phase history at all.
		phaseInput.Args, _ = resolveCallArgs(phase.Args, vars)
	}
	input, err := json.Marshal(phaseInput)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause:       fmt.Errorf("encode phase attempt %s/%s/%d input: %w", item.item.ID, phase.ID, attempt, err),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	startedAt := e.timestamp()
	if err := e.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.item.ID, PhaseID: phase.ID, Attempt: attempt,
		InputEnvelope: input, Status: "running", StartedAt: startedAt,
	}); err != nil {
		createErr := fmt.Errorf("create phase attempt %s/%s/%d: %w", item.item.ID, phase.ID, attempt, err)
		return errors.Join(e.teardown(item, teardownRequest{
			cause: createErr, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonSetupFailed,
		}), createErr)
	}
	item.attempt = attempt
	// The pending slot is deliberately NOT cleared here. The row carrying the
	// entries now exists, but nothing has rendered them yet: this attempt may
	// still be held by a pause, parked by a failed acquisition, or lost to a
	// crash, and a slot cleared against a turn that never ran is the silent loss
	// the whole ordering rule exists to prevent. `ackGuidance` clears it once a
	// provider session that renders the block has started.
	e.emitPhaseState(PhaseEvent{
		ItemID: item.item.ID, PhaseID: phase.ID, Attempt: attempt, Status: "running",
		OccurredAt: startedAt,
	})

	if e.paused {
		e.addWaitingPhase(item, vars)
		return nil
	}
	acquired, ok, err := e.acquirePhaseResources(item.item.ProjectID, phase)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: acquisitionParkReason(err),
		}), err)
	}
	if !ok {
		e.addWaitingPhase(item, vars)
		return nil
	}
	item.acquired = acquired
	return e.startPhaseWork(item, phase, vars)
}

func (e *Engine) startRunner(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	// The one place a route's prompt override becomes the body that runs. The
	// phase handed to the runner is otherwise the snapshot's, and a request built
	// without going through here cannot exist: this is the only constructor of a
	// phase-level RunRequest.
	phase.Prompt = promptBody(item.workflow, phase, item.promptRoute)
	request := RunRequest{
		Key:  RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt},
		Item: item.item, Workflow: item.workflow, WorkspaceNeed: item.workspaceNeed,
		Phase: phase, Vars: vars, Guidance: item.guidance,
		Feedback: cloneFeedback(item.feedback), PriorThreadID: item.priorThreadID,
		FinalizeTakeover: item.takeoverFinalize,
	}
	startCtx, cancel := context.WithCancel(e.ctx)
	future := &runnerStartFuture{key: request.Key, done: make(chan response, 1)}
	item.runnerStarting = true
	item.runnerStartCancel = cancel
	e.commandStarts = append(e.commandStarts, future)
	e.inflightStarts[future] = struct{}{}
	item.feedback = nil
	item.priorThreadID = ""
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
				"start runner %s/%s/%d: engine closed before startup settled",
				request.Key.ItemID, request.Key.PhaseID, request.Key.Attempt,
			)})
		}
	}()
	// Wait only until the worker enters Runner.Start. Provisioning and every
	// other blocking operation happen after this acknowledgement on the worker.
	<-entered
	return nil
}

func (e *Engine) finishRunnerStart(command runnerStartCommand) error {
	item, ok := e.items[command.key.ItemID]
	if !ok || item.phaseID != command.key.PhaseID || item.attempt != command.key.Attempt ||
		State(item.item.State) != StateRunning {
		return nil
	}
	if command.key.UnitID != "" {
		return e.finishUnitStart(item, command)
	}
	if !item.runnerStarting {
		return nil
	}
	if item.runnerStartCancel != nil {
		item.runnerStartCancel()
	}
	item.runnerStarting = false
	item.runnerStartCancel = nil
	if command.err == nil {
		item.runnerActive = true
		// The turn that renders this attempt's guidance block now exists, which is
		// the event the pending slot is cleared against. Only an agent phase is a
		// delivery boundary, so only one ever has entries owed here.
		e.ackGuidance(item)
		return nil
	}
	return e.parkStartFailure(item, command.err)
}

// finishUnitStart settles one unit's provisioning. A unit that could not start
// is not a unit failure: the frozen definition, the live profile, or the
// workspace could not produce runnable work at all, so the attempt parks under
// the same sentinel-mapped reason a single-shape phase would.
func (e *Engine) finishUnitStart(item *runtimeItem, command runnerStartCommand) error {
	if item.fan == nil {
		return nil
	}
	unit := item.fan.find(command.key.UnitID)
	if unit == nil || !unit.runnerStarting {
		return nil
	}
	clearUnitStart(unit)
	if command.err == nil {
		unit.runnerActive = true
		if driver, runsWork := unit.definition.EffectiveDriver(); runsWork && driver == def.DriverAgent {
			// The first agent element of the wave — a unit or the join — is the
			// first turn that renders the phase entry's guidance block. A command
			// unit renders no prompt, so its start acknowledges nothing.
			e.ackGuidance(item)
		}
		return nil
	}
	return e.parkStartFailure(item, command.err)
}

// parkStartFailure maps a runner startup failure onto a typed park reason by
// sentinel, never by string matching.
func (e *Engine) parkStartFailure(item *runtimeItem, cause error) error {
	reason := ReasonAgentError
	switch {
	case errors.Is(cause, ErrSetupFailed):
		reason = ReasonSetupFailed
	case errors.Is(cause, ErrWiringFailed):
		reason = ReasonWiringError
	}
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: reason,
		}),
		cause,
	)
}

func cloneFeedback(feedback *Feedback) *Feedback {
	if feedback == nil {
		return nil
	}
	copy := &Feedback{Note: feedback.Note, Values: make(map[string]any, len(feedback.Values))}
	for name, value := range feedback.Values {
		copy.Values[name] = value
	}
	return copy
}
