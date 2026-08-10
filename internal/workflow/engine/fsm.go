package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

type teardownRequest struct {
	output    json.RawMessage
	gateTrace json.RawMessage
	// cause is the ENGINE's diagnosis of why this teardown is happening, set by
	// every site whose trigger is a Go error rather than an envelope an agent
	// authored. It is persisted on the attempt row (`park_cause`) and written to
	// the engine log, and it is the difference between a run that rests with a
	// typed reason and one a human can act on. Sites whose reason names its own
	// cause — `interrupted`, `paused`, `taken-over`, a gate a human is deciding
	// — deliberately leave it nil; a restatement there would be noise.
	cause       error
	phaseStatus string
	nextState   State
	reason      Reason
	// retainCallChildren keeps a waiting call phase's child subtree alive
	// instead of cancelling it. Pause is the only trigger that sets it, and it
	// is correct for exactly one reason: pause does not abandon the phase. The
	// attempt is preserved so resume re-enters it holding the same child, which
	// is also being paused alongside. Every other trigger leaves the call phase
	// for good, and a descendant nothing can consume must come down with it.
	retainCallChildren bool
}

func transitionAllowed(from, to State) bool {
	switch from {
	case StateRunning:
		return to == StateNeedsHuman || to == StateDone || to == StateFailed || to == StateCancelled
	case StateNeedsHuman:
		// Running is the resume. Cancelled is the other way out, and it has to
		// exist: a run resting at a gate a human decides never to approve would
		// otherwise be immortal short of resuming it into work nobody wants.
		return to == StateRunning || to == StateCancelled
	case StateFailed:
		// Rerun. `done` and `cancelled` stay terminal: a finished run is
		// re-entered by starting a new one, and a cancelled run was stopped on
		// purpose.
		return to == StateRunning
	default:
		return false
	}
}

func reasonAllowed(reason Reason) bool {
	switch reason {
	case ReasonGate, ReasonQuestion, ReasonStuck, ReasonStalled,
		ReasonBudgetExhausted, ReasonRetriesExhausted,
		ReasonCheckFailedGenuine, ReasonAgentError, ReasonWiringError,
		ReasonDisposition, ReasonSetupFailed, ReasonInterrupted, ReasonTakenOver,
		ReasonUnitFailed, ReasonChildFailed, ReasonPaused, ReasonCheckpoint:
		return true
	default:
		return false
	}
}

func (e *Engine) transition(item *runtimeItem, to State, reason Reason) error {
	from := State(item.item.State)
	if !transitionAllowed(from, to) {
		return fmt.Errorf("transition item %q: invalid transition %s -> %s", item.item.ID, from, to)
	}
	if (to == StateNeedsHuman || to == StateFailed || to == StateCancelled) && reason == "" {
		return fmt.Errorf("transition item %q: %s requires a typed reason", item.item.ID, to)
	}
	if reason != "" && !reasonAllowed(reason) {
		return fmt.Errorf("transition item %q: unknown typed reason %q", item.item.ID, reason)
	}
	if (to == StateRunning || to == StateDone) && reason != "" {
		return fmt.Errorf("transition item %q: %s does not accept reason %q", item.item.ID, to, reason)
	}
	endedAt := int64(0)
	if to != StateRunning {
		endedAt = e.timestamp()
	}
	if err := e.store.UpdateWorkItemState(item.item.ID, string(to), string(reason), endedAt); err != nil {
		return fmt.Errorf("transition item %q %s -> %s: %w", item.item.ID, from, to, err)
	}
	item.item.State = string(to)
	item.item.Reason = string(reason)
	item.item.EndedAt = endedAt
	e.emitItemStateAt(item, from, to, reason)
	if to != StateRunning {
		delete(e.items, item.item.ID)
	}
	// A child run reaching a terminal state is what re-enters the call phase
	// waiting on it. The parent is driven from the command loop's deferred queue
	// rather than from inside this transition, so the child's own teardown
	// finishes first and the parent's phase completion is an ordinary serialized
	// step instead of a re-entrant one.
	if item.item.ParentItemID != "" && isTerminal(to) {
		settled := item.item
		e.deferred = append(e.deferred, deferredWork{
			itemID: settled.ParentItemID,
			run:    func() error { return e.settleCallChild(settled) },
		})
	}
	return nil
}

func isTerminal(state State) bool {
	return state == StateDone || state == StateFailed || state == StateCancelled
}

// teardown is the only function allowed to release resource holders. It is
// shared by normal phase exit, park, failure, cancellation, and crash sweep.
func (e *Engine) teardown(item *runtimeItem, request teardownRequest) error {
	var errs []error
	output := request.output
	// Teardown is tree-aware (spec §12): an attempt's in-flight units come down
	// with it, through the one per-unit release path, and a call phase's whole
	// live child subtree comes down before the phase releases anything of its
	// own. Any exit from a waiting call phase — cancel, rerun, takeover, crash
	// park — therefore leaves no descendant running with nothing to consume it.
	if err := e.teardownUnits(item, request.phaseStatus, request.retainCallChildren); err != nil {
		errs = append(errs, err)
	}
	if !request.retainCallChildren {
		if err := e.cancelCallChildren(item); err != nil {
			errs = append(errs, err)
		}
	}
	if item.runnerStarting {
		if item.runnerStartCancel != nil {
			item.runnerStartCancel()
		}
		item.runnerStarting = false
		item.runnerStartCancel = nil
		if _, err := e.runner.Stop(e.ctx, RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt}); err != nil {
			errs = append(errs, fmt.Errorf("stop starting runner: %w", err))
		}
	}
	if item.runnerActive {
		partial, err := e.runner.Stop(e.ctx, RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt})
		if err != nil {
			errs = append(errs, fmt.Errorf("stop runner: %w", err))
		}
		if len(output) == 0 && len(partial) > 0 {
			output = partial
		}
		item.runnerActive = false
	}

	if err := e.releaseResources(item); err != nil {
		errs = append(errs, err)
	}
	// Sweep the persisted rows last: after a crash no in-memory unit state
	// existed to tear down, and a row left claiming `running` would make a
	// rebuilt attempt look live forever.
	if err := e.sweepPersistedUnits(item, request.phaseStatus, request.retainCallChildren); err != nil {
		errs = append(errs, err)
	}
	// The attempt's own state dies with the attempt, here rather than in each
	// caller: the fan-out it expanded, the guidance block its elements rendered
	// (and the acknowledgement it still owed the pending slot, so entries no turn
	// read stay pending), and both halves of the prompt-route override — what this
	// attempt rendered, and any arming for an entry this teardown means will never
	// happen. Leaving them for the next `enterPhase` to reassign put the safety in
	// caller discipline, and one caller that entered a phase without going through
	// that reassignment leaked a loop route's body into an unrelated round.
	item.fan = nil
	item.guidance = nil
	item.guidanceUnacked = nil
	item.promptRoute = nil
	item.nextPromptRoute = nil

	cause := parkCauseText(request.cause)
	phasePersisted := true
	switch {
	case item.phaseID != "" && item.attempt > 0:
		if err := e.store.CompleteWorkItemPhase(
			item.item.ID, item.phaseID, item.attempt, output, request.gateTrace,
			request.phaseStatus, cause, e.timestamp(),
		); err != nil {
			errs = append(errs, fmt.Errorf("persist phase teardown: %w", err))
			phasePersisted = false
		} else {
			e.emitter.Emit("workflow:phase-state", PhaseEvent{
				ItemID: item.item.ID, PhaseID: item.phaseID,
				Attempt: item.attempt, Status: request.phaseStatus,
			})
		}
	case cause != "" && item.phaseID != "":
		if err := e.parkOnNewAttempt(item, request.phaseStatus, cause); err != nil {
			errs = append(errs, err)
			phasePersisted = false
		}
	}
	if request.nextState != "" && phasePersisted {
		if err := e.transition(item, request.nextState, request.reason); err != nil {
			errs = append(errs, err)
		} else {
			e.logTeardown(item, request, cause)
		}
	}
	return errors.Join(errs...)
}

// parkOnNewAttempt persists the attempt row an engine-diagnosed park rests on
// when the failure landed BEFORE the attempt existed — a phase the frozen
// snapshot does not declare, a budget the tree had already spent, a variable
// context that would not build. Those parks used to leave a typed reason and
// nothing else: no row, no cause, and no way to answer "entering what, and why
// did it stop" short of reading the process's stderr.
//
// The row is an honest record — the run did attempt to enter that phase and did
// not get past it — and it costs one thing worth stating: the loop-budget
// derivation now sees a parked attempt of that phase where it previously saw the
// advance that led there, so a resume through it under-refills rather than
// refills. That is the direction the derivation is already required to err in.
func (e *Engine) parkOnNewAttempt(item *runtimeItem, status, cause string) error {
	phases, err := e.store.ListWorkItemPhaseContexts(item.item.ID)
	if err != nil {
		return fmt.Errorf("park item %q on phase %q: %w", item.item.ID, item.phaseID, err)
	}
	attempt := nextAttempt(phases, item.phaseID)
	now := e.timestamp()
	if err := e.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: attempt,
		ParkCause: cause, Status: status, StartedAt: now, EndedAt: now,
	}); err != nil {
		return fmt.Errorf("park item %q on phase %q: %w", item.item.ID, item.phaseID, err)
	}
	item.attempt = attempt
	e.emitter.Emit("workflow:phase-state", PhaseEvent{
		ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: attempt, Status: status,
	})
	return nil
}

// parkCauseText renders a cause for persistence, bounded. A cause that would
// exceed the bound is truncated with the fact stated, because a reader who
// cannot tell a short cause from a cut-off one will trust the wrong half.
func parkCauseText(cause error) string {
	if cause == nil {
		return ""
	}
	text := cause.Error()
	if len(text) <= MaxParkCauseBytes {
		return text
	}
	// Cut on a rune boundary. The column is TEXT and the cause can carry a path,
	// a branch name, or a model's own words; half a rune would be invalid UTF-8
	// in the store before any reader got the chance to quote it.
	cut := MaxParkCauseBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " …(cause truncated)"
}

// logTeardown records the lifecycle transition a teardown just made. It runs
// after the transition rather than before it, so the log never claims a park
// that a failed write did not make.
func (e *Engine) logTeardown(item *runtimeItem, request teardownRequest, cause string) {
	event := LogEventPark
	if request.nextState == StateCancelled {
		event = LogEventCancel
	}
	e.logEvent(LogEvent{
		Event: event, ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: item.phaseID, Attempt: item.attempt,
		State: request.nextState, Reason: request.reason, Message: cause,
	})
}

func (e *Engine) complete(key RunKey, outcome Outcome) error {
	item, ok := e.items[key.ItemID]
	if !ok || item.phaseID != key.PhaseID || item.attempt != key.Attempt || State(item.item.State) != StateRunning {
		return nil // A completion racing a persisted teardown is stale, not an error.
	}
	if key.UnitID != "" {
		return e.completeUnit(item, key, outcome)
	}
	if item.runnerStartCancel != nil {
		item.runnerStartCancel()
	}
	item.runnerStarting = false
	item.runnerStartCancel = nil
	item.runnerActive = false
	return e.completePhaseOutcome(item, key, outcome)
}

// completePhaseOutcome maps a phase-level outcome onto the FSM. A fan-out's
// join reaches it too, under its own unit key: the join's envelope is the
// phase's envelope, so its outcome is the phase's outcome — but a join that
// FAILED is still a unit of the attempt failing, and it parks as one
// (phaseFailureReason).
func (e *Engine) completePhaseOutcome(item *runtimeItem, key RunKey, outcome Outcome) error {
	switch outcome.Kind {
	case OutcomeDone:
		item.takeoverFinalize = false
		return e.completeDone(item, outcome.Envelope)
	case OutcomeQuestion:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonQuestion})
	case OutcomeStuck:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonStuck})
	case OutcomeStalled:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonStalled})
	case OutcomeTransientExhausted:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonRetriesExhausted})
	case OutcomeExecutionFailure:
		if item.takeoverFinalize {
			return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonTakenOver})
		}
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: phaseFailureReason(key)})
	case OutcomeStopped:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonInterrupted})
	default:
		cause := fmt.Errorf("phase %s/%s/%d returned unknown outcome %q", key.ItemID, key.PhaseID, key.Attempt, outcome.Kind)
		return errors.Join(
			e.teardown(item, teardownRequest{
				output: outcome.Envelope, cause: cause, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: phaseFailureReason(key),
			}),
			cause,
		)
	}
}

// phaseFailureReason classifies an attempt that produced no usable result. Only
// a fan-out's join reaches completePhaseOutcome under a unit key, and its
// failure is the failure of one unit of the attempt: the wave's finished units
// still hold their results, and the call children among them are entire runs
// that must never be re-executed to repair the step that consolidates them. So
// it parks `unit-failed`, where the retry verbs reach it and a bare resume
// continues the attempt. A single-shape phase has nothing of the kind behind it
// and stays an agent error.
func phaseFailureReason(key RunKey) Reason {
	if key.UnitID != "" {
		return ReasonUnitFailed
	}
	return ReasonAgentError
}

func (e *Engine) completeDone(item *runtimeItem, envelope json.RawMessage) error {
	vars, phases, err := e.variableContext(item, item.currentAttempt(envelope))
	if err != nil {
		// A context this engine cannot build is a WIRING error, the same reason
		// `enterPhase` parks the identical failure under — an envelope this engine
		// wrote and cannot read back, a seeds column that will not decode. It used
		// to land in `agent-error`, the one bucket no repair verb reaches, for a
		// failure no agent had anything to do with.
		return errors.Join(e.teardown(item, teardownRequest{
			output: envelope, cause: err, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), err)
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		cause := fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID)
		return errors.Join(
			e.teardown(item, teardownRequest{
				output: envelope, cause: cause, phaseStatus: "parked",
				nextState: StateNeedsHuman, reason: ReasonWiringError,
			}),
			cause,
		)
	}
	counts, countErr := loopCounts(item.item.ID, phases)
	if countErr != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			output: envelope, cause: countErr, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), countErr)
	}
	decision, trace, evaluateErr := def.EvaluateGate(phase, vars, counts)
	if evaluateErr != nil {
		decision = def.RouteDecision{Kind: def.DecisionNoMatch, RouteIndex: -1}
		trace.Decision = decision
	}
	encodedTrace, marshalErr := json.Marshal(trace)
	if marshalErr != nil {
		return errors.Join(e.teardown(item, teardownRequest{
			output: envelope, cause: marshalErr, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonWiringError,
		}), marshalErr)
	}
	if evaluateErr != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{
				output: envelope, gateTrace: encodedTrace, cause: evaluateErr,
				phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
			}),
			evaluateErr,
		)
	}
	return e.finishDecision(item, decision, envelope, encodedTrace, vars)
}

func (e *Engine) finishDecision(item *runtimeItem, decision def.RouteDecision, envelope, gateTrace json.RawMessage, vars map[string]any) error {
	if item.item.StepMode && stepModeAutomaticDecision(decision.Kind) {
		return e.teardown(item, teardownRequest{
			output: envelope, gateTrace: gateTrace, phaseStatus: "parked",
			nextState: StateNeedsHuman, reason: ReasonGate,
		})
	}
	switch decision.Kind {
	case def.DecisionAdvance, def.DecisionLoop:
		gatePhase, gateAttempt := item.phaseID, item.attempt
		if err := e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "completed"}); err != nil {
			return err
		}
		e.noteGateNotify(item, decision, gatePhase, gateAttempt)
		item.feedback = feedbackFor(vars, decision.Feedback, "")
		// The loop route's per-round knobs are applied after the feedback is
		// composed, because a continuation the engine could not honour states so
		// in it, and after the teardown, because `session: continue` resolves the
		// session off the attempt rows that teardown just completed.
		e.applyLoopRoute(item, decision, gatePhase)
		item.phaseID = decision.Target
		item.attempt = 0
		// The phase just released its locks; hand them to the longest-waiting
		// phase before this item competes for them again.
		waitingErr := e.startWaiting()
		return errors.Join(waitingErr, e.enterPhase(item, entryFresh))
	case def.DecisionDone:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "completed", nextState: StateDone})
	case def.DecisionFailed:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "failed", nextState: StateFailed, reason: ReasonCheckFailedGenuine})
	case def.DecisionHuman, def.DecisionPark:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonGate})
	case def.DecisionRetriesExhausted:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonRetriesExhausted})
	case def.DecisionNoMatch:
		return e.teardown(item, teardownRequest{
			output: envelope, gateTrace: gateTrace,
			cause: fmt.Errorf(
				"phase %q of item %q completed and its gate matched no route; the trace on this attempt shows which predicates were evaluated",
				item.phaseID, item.item.ID),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
		})
	default:
		return e.teardown(item, teardownRequest{
			output: envelope, gateTrace: gateTrace,
			cause: fmt.Errorf("phase %q of item %q produced unknown gate decision %q",
				item.phaseID, item.item.ID, decision.Kind),
			phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
		})
	}
}

// noteGateNotify announces a `notify:`-decorated route the run just continued
// through. It runs AFTER the teardown that persisted the attempt and its gate
// trace, so the app resolving the message reads a record that already says what
// the gate decided; a teardown that failed returns before this and announces
// nothing.
//
// It is fire-and-forget by construction: `Emit` is the same seam every other
// engine event crosses, the app hands the work to its own queue, and no result
// comes back. A progress wake can therefore never park, fail, or delay the run
// it is reporting on — the run has already moved.
func (e *Engine) noteGateNotify(item *runtimeItem, decision def.RouteDecision, phaseID string, attempt int) {
	if !decision.Notify {
		return
	}
	e.emitter.Emit("workflow:gate-notify", NotifyEvent{
		ItemID: item.item.ID, ProjectID: item.item.ProjectID,
		PhaseID: phaseID, Attempt: attempt,
		Decision: string(decision.Kind), Target: decision.Target,
		RouteIndex: decision.RouteIndex,
	})
}

func stepModeAutomaticDecision(kind def.DecisionKind) bool {
	switch kind {
	case def.DecisionAdvance, def.DecisionLoop, def.DecisionDone, def.DecisionFailed:
		return true
	default:
		return false
	}
}

func findPhase(workflow def.Workflow, phaseID string) (def.Phase, bool) {
	for _, phase := range workflow.Phases {
		if phase.ID == phaseID {
			return phase, true
		}
	}
	return def.Phase{}, false
}
