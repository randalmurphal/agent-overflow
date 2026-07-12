package engine

import (
	"encoding/json"
	"errors"
	"fmt"

	"agent-overflow/internal/workflow/def"
)

type teardownRequest struct {
	output      json.RawMessage
	gateTrace   json.RawMessage
	phaseStatus string
	nextState   State
	reason      Reason
}

func transitionAllowed(from, to State) bool {
	switch from {
	case StateQueued:
		return to == StateRunning
	case StateRunning:
		return to == StateNeedsHuman || to == StateDone || to == StateFailed || to == StateCancelled
	case StateNeedsHuman:
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
		ReasonDisposition, ReasonSetupFailed, ReasonInterrupted:
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
	if (to == StateQueued || to == StateRunning || to == StateDone) && reason != "" {
		return fmt.Errorf("transition item %q: %s does not accept reason %q", item.item.ID, to, reason)
	}
	endedAt := int64(0)
	if to != StateRunning && to != StateQueued {
		endedAt = e.timestamp()
	}
	if err := e.store.UpdateWorkItemState(item.item.ID, string(to), string(reason), endedAt); err != nil {
		return fmt.Errorf("transition item %q %s -> %s: %w", item.item.ID, from, to, err)
	}
	if to == StateRunning && !item.slot {
		item.slot = true
		e.activeSlots++
	}
	if from == StateRunning && to != StateRunning && item.slot {
		item.slot = false
		e.activeSlots--
	}
	item.item.State = string(to)
	item.item.Reason = string(reason)
	item.item.EndedAt = endedAt
	e.emitter.Emit("workflow:item-state", StateEvent{ItemID: item.item.ID, From: from, To: to, Reason: reason})
	if to != StateRunning && to != StateQueued {
		delete(e.items, item.item.ID)
	}
	return nil
}

// teardown is the only function allowed to release resource holders. It is
// shared by normal phase exit, park, failure, cancellation, and crash sweep.
func (e *Engine) teardown(item *runtimeItem, request teardownRequest) error {
	var errs []error
	output := request.output
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

	phasePersisted := true
	if item.phaseID != "" && item.attempt > 0 {
		if err := e.store.CompleteWorkItemPhase(
			item.item.ID, item.phaseID, item.attempt, output, request.gateTrace,
			request.phaseStatus, e.timestamp(),
		); err != nil {
			errs = append(errs, fmt.Errorf("persist phase teardown: %w", err))
			phasePersisted = false
		} else {
			e.emitter.Emit("workflow:phase-state", PhaseEvent{
				ItemID: item.item.ID, PhaseID: item.phaseID,
				Attempt: item.attempt, Status: request.phaseStatus,
			})
		}
	}
	if request.nextState != "" && phasePersisted {
		if err := e.transition(item, request.nextState, request.reason); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) complete(key RunKey, outcome Outcome) error {
	item, ok := e.items[key.ItemID]
	if !ok || item.phaseID != key.PhaseID || item.attempt != key.Attempt || State(item.item.State) != StateRunning {
		return nil // A completion racing a persisted teardown is stale, not an error.
	}
	item.runnerActive = false
	switch outcome.Kind {
	case OutcomeDone:
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
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonAgentError})
	case OutcomeStopped:
		return e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonInterrupted})
	default:
		return errors.Join(
			e.teardown(item, teardownRequest{output: outcome.Envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonAgentError}),
			fmt.Errorf("phase %s/%s/%d returned unknown outcome %q", key.ItemID, key.PhaseID, key.Attempt, outcome.Kind),
		)
	}
}

func (e *Engine) completeDone(item *runtimeItem, envelope json.RawMessage) error {
	vars, phases, err := e.variableContext(item, envelope)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{output: envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonAgentError}), err)
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return errors.Join(
			e.teardown(item, teardownRequest{output: envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID),
		)
	}
	counts, countErr := loopCounts(phases)
	if countErr != nil {
		return errors.Join(e.teardown(item, teardownRequest{output: envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}), countErr)
	}
	decision, trace, evaluateErr := def.EvaluateGate(phase, vars, counts)
	if evaluateErr != nil {
		decision = def.RouteDecision{Kind: def.DecisionNoMatch, RouteIndex: -1}
		trace.Decision = decision
	}
	encodedTrace, marshalErr := json.Marshal(trace)
	if marshalErr != nil {
		return errors.Join(e.teardown(item, teardownRequest{output: envelope, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}), marshalErr)
	}
	if evaluateErr != nil {
		return errors.Join(
			e.teardown(item, teardownRequest{output: envelope, gateTrace: encodedTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			evaluateErr,
		)
	}
	return e.finishDecision(item, decision, envelope, encodedTrace, vars)
}

func (e *Engine) finishDecision(item *runtimeItem, decision def.RouteDecision, envelope, gateTrace json.RawMessage, vars map[string]any) error {
	switch decision.Kind {
	case def.DecisionAdvance, def.DecisionLoop:
		if err := e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "completed"}); err != nil {
			return err
		}
		item.feedback = feedbackFor(vars, decision.Feedback, "")
		item.phaseID = decision.Target
		item.attempt = 0
		waitingErr := e.startWaiting()
		return errors.Join(waitingErr, e.enterPhase(item))
	case def.DecisionDone:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "completed", nextState: StateDone})
	case def.DecisionFailed:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "failed", nextState: StateFailed, reason: ReasonCheckFailedGenuine})
	case def.DecisionHuman, def.DecisionPark:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonGate})
	case def.DecisionRetriesExhausted:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonRetriesExhausted})
	case def.DecisionNoMatch:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError})
	default:
		return e.teardown(item, teardownRequest{output: envelope, gateTrace: gateTrace, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError})
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
