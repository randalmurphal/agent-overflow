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
	if e.beginWork != nil {
		release, err := e.beginWork(e.ctx)
		if err != nil {
			return err
		}
		defer release()
	}
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

// phaseEntry says what the attempt about to be created IS: a fresh phase entry,
// a continuation of a parked round, or reconstruction of that round after its
// provider context disappeared.
//
// It is a parameter rather than something inferred from the item, because
// nothing on the item distinguishes the two — a warm loop round and an `Answer`
// continuation both carry a prior thread id, and both create a new attempt row.
// What separates them is which caller asked, so the caller says. It decides
// prompt shape, route/guidance preservation, and whether pending operator
// guidance is delivered here. A
// continuation continues a turn the operator was already steering; guidance
// delivered into it would arrive as a second instruction to a round that has
// already read the first.
type phaseEntry int

const (
	// entryFresh starts the phase over: a gate advance or loop, a resume aimed at
	// a phase, a rerun, and the run's first phase.
	entryFresh phaseEntry = iota
	// entryContinuation continues the round the parked attempt was in, on the
	// session it parked on: an answered question, a finalized takeover, a bare
	// resume of a continuable park.
	entryContinuation
	// entryRestart rebuilds the parked round after its provider context became
	// unavailable. It sends a full prompt to a new session, but unlike an
	// operator-requested fresh entry it preserves the round's prompt route and
	// delivered guidance. Those are part of the task being reconstructed, not
	// state belonging to the dead provider process.
	entryRestart
)

// entryPromptRoute resolves which loop route's `prompt:` override the attempt
// being created renders.
//
// A FRESH entry renders what the decision that produced it armed — and only
// that: `consumePromptRoute` refuses an arming whose route does not loop to
// THIS phase, so a coordinate that outlived its entry is inert rather than a
// body borrowed by the next phase. A CONTINUATION or RESTART renders what the
// round it continues rendered, restored from that round's persisted input,
// because both represent the same round even though only one retains context.
func entryPromptRoute(item *runtimeItem, entry phaseEntry, phaseID string) *PromptRoute {
	if entry != entryFresh {
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
	if entry != entryFresh && entry != entryContinuation && entry != entryRestart {
		return fmt.Errorf("enter workflow phase: unknown entry kind %d", entry)
	}
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
	item.entry = entry
	vars, priorPhases, err := e.variableContext(item, attemptRef{})
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	if entry != entryFresh {
		item.feedback = continuationFeedback(item.workflow.Inputs, vars, item.parkedVars, item.feedback)
	}
	promptGuidance, unacked, err := e.entryGuidance(item, phase, entry)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	if entry == entryFresh && len(promptGuidance) > 0 {
		item.feedback = appendFeedbackNote(item.feedback, guidanceNote(promptGuidance))
	}
	// Fresh entries establish a new round's guidance. Continuations preserve that
	// round state without rendering it; restarts render the preserved block into
	// replacement context. The entries stay owed an acknowledgement until the
	// send door reports a prompt that renders them dispatched
	// (`AckFeedbackRendered` → `ackGuidance`).
	if entry != entryContinuation {
		item.guidance = promptGuidance
	}
	item.guidanceUnacked = unacked
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
	// A provider-context restart hands this entry one source's note directly, and
	// names it here. The arming is consumed whether or not the entry succeeds: a
	// failed entry leaves the source owed, which is the phase's next entry
	// redelivering it through the ordinary read.
	carriedFrom := item.feedbackCarriedFrom
	item.feedbackCarriedFrom = 0
	// Anything a PRIOR attempt of this phase persisted as feedback and no provider
	// session ever rendered is folded in here, before the input envelope is built,
	// so the row this entry writes is the record of what the turn actually reads.
	// Every entry kind participates: an answer stays relevant however the phase is
	// re-entered. See feedback.go for why the window is what it is.
	owedFeedback, err := e.redeliverFeedback(item, phase, attempt, carriedFrom)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	phaseInput := PhaseInput{
		Vars: vars, Feedback: item.feedback,
		PromptRoute: item.promptRoute, Guidance: item.guidance,
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
		FeedbackDeliveredAt: phaseFeedbackCreateStamp(phase, item.feedback, startedAt),
	}); err != nil {
		createErr := fmt.Errorf("create phase attempt %s/%s/%d: %w", item.item.ID, phase.ID, attempt, err)
		return errors.Join(e.teardown(item, teardownRequest{
			cause: createErr, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonSetupFailed,
		}), createErr)
	}
	item.attempt = attempt
	item.feedbackOwed = phaseOwesFeedback(phase, item.feedback)
	// The row that now carries the redelivered notes exists, so the attempts they
	// came from are settled — and not one statement earlier: a source stamped
	// before its carrier landed would be a note nothing holds, and the create
	// above can still fail. The restart's own carry-forward settles here for
	// exactly the same reason, under the provenance describing what happened to it
	// rather than a redelivery sentence.
	e.dischargeCarriedFeedback(item, phase.ID, owedFeedback, attempt)
	if carriedFrom > 0 {
		e.dischargeRestartedFeedback(item, phase.ID, carriedFrom)
	}
	// The pending slot is deliberately NOT cleared here, and neither is this
	// attempt's own feedback settled. The row carrying them now exists, but
	// nothing has rendered them yet: this attempt may still be held by a pause,
	// parked by a failed acquisition, or lost to a crash, and a slot cleared —
	// or a stamp written — against a turn that never ran is the silent loss the
	// whole ordering rule exists to prevent. `ackGuidance` and
	// `dischargeRenderedFeedback` both wait for the send door to report a prompt
	// that renders the blocks dispatched (`AckFeedbackRendered`).
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
	launch, err := phaseTurnLaunch(item.entry, item.priorThreadID, item.takeoverFinalize)
	if err != nil {
		return e.parkStartFailure(item, errors.Join(ErrWiringFailed, err))
	}
	request := RunRequest{
		Key:  RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt},
		Item: item.item, Workflow: item.workflow, WorkspaceNeed: item.workspaceNeed,
		Phase: phase, Vars: vars, Guidance: promptGuidanceForEntry(item),
		Feedback: cloneFeedback(item.feedback),
		Launch:   launch,
	}
	startCtx, cancel := context.WithCancel(e.ctx)
	future := &runnerStartFuture{key: request.Key, done: make(chan response, 1)}
	item.runnerStarting = true
	item.runnerStart = future
	item.runnerStartCancel = cancel
	e.commandStarts = append(e.commandStarts, future)
	e.inflightStarts[future] = struct{}{}
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
	// Identity, not just presence: only the start that armed the flag may settle
	// it. See `runtimeItem.runnerStart`.
	if !item.runnerStarting || item.runnerStart != command.future {
		return nil
	}
	if item.runnerStartCancel != nil {
		item.runnerStartCancel()
	}
	item.runnerStarting = false
	item.runnerStart = nil
	item.runnerStartCancel = nil
	if command.err == nil {
		item.runnerActive = true
		// The one line that says a dispatch became a running turn. Every FAILURE
		// here parks through teardown, which logs its cause; success said nothing,
		// so a start that never reported and a start that succeeded produced the
		// same log — silence — and telling them apart meant reading the database.
		// It is emitted before priorThreadID is cleared, because the session a
		// continuation landed on is exactly what correlates this line with the
		// `answer` / `resume` / `takeover-complete` line that dispatched it.
		e.logEvent(LogEvent{
			Event: LogEventRunnerStart, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
			PhaseID: command.key.PhaseID, Attempt: command.key.Attempt,
			ThreadID: item.priorThreadID, Message: runnerStartNote(item.priorThreadID),
		})
		// Neither the attempt's feedback nor its guidance is settled here. A start
		// can return nil having dropped its opening send, and a stamp or a slot
		// clear written against that would record blocks no turn ever rendered.
		// The send door reports the dispatch itself through `AckFeedbackRendered`,
		// the earliest event that proves either block reached a model, and the
		// command loop settles both from it (`ackFeedbackRendered`).
		item.feedback = nil
		item.priorThreadID = ""
		item.entry = entryFresh
		return nil
	}
	if errors.Is(command.err, ErrProviderContextUnavailable) {
		if item.takeoverFinalize {
			return e.parkUnavailableTakeover(item, command.err)
		}
		if item.entry == entryContinuation || (item.entry == entryFresh && item.priorThreadID != "") {
			return e.restartPhaseWithoutProviderContext(item, command.err)
		}
	}
	return e.parkStartFailure(item, command.err)
}

// runnerStartNote distinguishes the two starts a reader cares about telling
// apart: a phase that began a turn of its own, and one that took its turn on a
// session an earlier attempt started.
func runnerStartNote(priorThreadID string) string {
	if priorThreadID != "" {
		return "the runner started the attempt on the session it continued"
	}
	return "the runner started the attempt"
}

// restartPhaseWithoutProviderContext settles an attempt whose selected provider
// context disappeared before any prompt was sent, then reconstructs the same
// round on a new thread. This covers both a recovery continuation and a warm
// loop's new logical round. Superseded is the honest status: the attempt existed
// and recorded its intended thread, but ran no turn and produced no output.
func (e *Engine) restartPhaseWithoutProviderContext(item *runtimeItem, cause error) error {
	promptRoute := item.promptRoute
	guidance := append([]GuidanceEntry(nil), item.guidance...)
	feedback := item.feedback
	// Captured before the teardown, which clears the in-memory half of both.
	superseded, supersededOwed := item.attempt, item.feedbackOwed
	message := "reconstructing the parked round because its provider context is unavailable"
	if item.entry == entryContinuation {
		feedback = continuationUnavailableFeedback(feedback)
	} else {
		feedback = appendFeedbackNote(feedback, loopContinuationNote)
		message = "reconstructing the warm loop round because its provider context is unavailable"
	}
	if err := e.teardown(item, teardownRequest{cause: cause, phaseStatus: "superseded"}); err != nil {
		return errors.Join(err, cause)
	}
	// This restart is a carry-forward of its own: the superseded attempt's
	// feedback rides `feedback` into the reconstruction verbatim (with the
	// context-loss sentence edited into it). Naming the source rather than
	// settling it here is the whole ordering: `enterPhase` excludes it from the
	// redelivery read (so the two mechanisms cannot both deliver it) and settles
	// it only once the reconstruction's row EXISTS. Settling it here would open a
	// window — a crash, or an `enterPhase` that parks before its create — in which
	// the source reads as delivered and the only surviving copy of the note is
	// this in-memory value.
	if supersededOwed {
		item.feedbackCarriedFrom = superseded
	}
	item.promptRoute = promptRoute
	item.guidance = guidance
	item.feedback = feedback
	item.priorThreadID = ""
	item.entry = entryRestart
	item.attempt = 0
	e.noteResume(item, message)
	return errors.Join(e.startWaiting(), e.enterPhase(item, entryRestart))
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
	// Identity, not just presence: a repaired unit reuses its whole key, so only
	// the future that armed `runnerStarting` may settle it. See
	// `unitRun.runnerStart`.
	if unit == nil || !unit.runnerStarting || unit.runnerStart != command.future {
		return nil
	}
	clearUnitStart(unit)
	if command.err == nil {
		unit.runnerActive = true
		unit.feedback = nil
		if unit.kind == UnitJoin {
			item.priorThreadID = ""
			item.entry = entryFresh
		}
		// The phase entry's guidance and feedback blocks are NOT settled here, for
		// the reason the phase-level start does not settle them: a unit start can
		// succeed having dropped its opening send. The first agent element whose
		// send actually dispatches settles both through `AckFeedbackRendered`.
		return nil
	}
	if errors.Is(command.err, ErrProviderContextUnavailable) && unit.kind == UnitJoin &&
		(item.entry == entryContinuation || (item.entry == entryFresh && item.priorThreadID != "")) {
		if item.takeoverFinalize {
			return e.parkUnavailableTakeover(item, command.err)
		}
		return e.restartJoinWithoutProviderContext(item, unit, command.err)
	}
	return e.parkStartFailure(item, command.err)
}

func (e *Engine) restartJoinWithoutProviderContext(item *runtimeItem, join *unitRun, cause error) error {
	if err := e.releaseUnitResources(item, join); err != nil {
		return errors.Join(e.parkStartFailure(item, errors.Join(cause, err)), cause, err)
	}
	feedback := join.feedback
	message := "reconstructing the fan-out join because its provider context is unavailable"
	if item.entry == entryContinuation {
		feedback = continuationUnavailableFeedback(feedback)
	} else {
		feedback = appendFeedbackNote(feedback, loopContinuationNote)
		message = "reconstructing the warm fan-out join because its provider context is unavailable"
	}
	if err := e.reopenUnit(item, join, feedback); err != nil {
		return errors.Join(e.parkStartFailure(item, errors.Join(cause, err)), cause, err)
	}
	item.priorThreadID = ""
	item.entry = entryRestart
	e.noteResume(item, message)
	return errors.Join(e.startWaiting(), e.advanceFanOut(item))
}

// A takeover finalize cannot be reconstructed in a blank provider context: the
// human's steering exists only in the session being finalized. Keep the typed
// takeover park and surface the missing context instead of either inventing a
// full restart or misclassifying the human-owned attempt as an agent error.
func (e *Engine) parkUnavailableTakeover(item *runtimeItem, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{
			cause: cause, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonTakenOver,
		}),
		cause,
	)
}
