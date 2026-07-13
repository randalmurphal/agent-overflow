package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

const maxHumanNoteBytes = 16 * 1024

func (e *Engine) takeOver(itemID string) error {
	item, tracked := e.items[itemID]
	if tracked {
		if State(item.item.State) != StateRunning {
			return fmt.Errorf("take over item %q: invalid state %s", itemID, item.item.State)
		}
	} else {
		var err error
		item, err = e.loadParked(itemID)
		if err != nil {
			return err
		}
	}
	if item.phaseID == "" || item.attempt < 1 {
		return fmt.Errorf("take over item %q: current phase attempt is missing", itemID)
	}
	var partial json.RawMessage
	if tracked {
		if item.runnerStarting {
			return fmt.Errorf("take over item %q: phase runner is still starting", itemID)
		}
		if item.runnerActive {
			var stopErr error
			partial, stopErr = e.runner.StopForTakeover(e.ctx, RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt})
			if stopErr != nil {
				return fmt.Errorf("take over item %q: stop live attempt: %w", itemID, stopErr)
			}
			item.runnerActive = false
		}
	}
	intervention, err := json.Marshal(TakeoverIntervention{Kind: "taken-over", At: e.timestamp()})
	if err != nil {
		return fmt.Errorf("take over item %q: encode intervention: %w", itemID, err)
	}
	interventionErr := e.store.UpdateWorkItemPhaseIntervention(itemID, item.phaseID, item.attempt, intervention)
	if tracked {
		teardownErr := e.teardown(item, teardownRequest{
			output: partial, phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonTakenOver,
		})
		if interventionErr != nil {
			interventionErr = fmt.Errorf("take over item %q: persist intervention: %w", itemID, interventionErr)
		}
		return errors.Join(interventionErr, teardownErr)
	}
	if interventionErr != nil {
		return fmt.Errorf("take over item %q: persist intervention: %w", itemID, interventionErr)
	}
	endedAt := item.item.EndedAt
	if endedAt == 0 {
		endedAt = e.timestamp()
	}
	if err := e.store.UpdateWorkItemState(itemID, string(StateNeedsHuman), string(ReasonTakenOver), endedAt); err != nil {
		return fmt.Errorf("take over item %q: persist parked state: %w", itemID, err)
	}
	item.item.Reason = string(ReasonTakenOver)
	item.item.EndedAt = endedAt
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: item.item.ProjectID,
		From: StateNeedsHuman, To: StateNeedsHuman, Reason: ReasonTakenOver,
	})
	return nil
}

func (e *Engine) completeTakeover(itemID string) error {
	if e.activeSlots >= e.config.GlobalConcurrency {
		return fmt.Errorf("complete takeover %q: global concurrency is full", itemID)
	}
	item, err := e.loadParked(itemID)
	if err != nil {
		return err
	}
	if Reason(item.item.Reason) != ReasonTakenOver {
		return fmt.Errorf("complete takeover %q: item reason is %q, want %q", itemID, item.item.Reason, ReasonTakenOver)
	}
	if !e.projectHasCapacity(item.item.ProjectID) {
		return fmt.Errorf("complete takeover %q: project concurrency is full", itemID)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("complete takeover %q phases: %w", itemID, err)
	}
	current, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok || current.ThreadID == "" {
		return fmt.Errorf("complete takeover %q: parked attempt thread is missing", itemID)
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	item.priorThreadID = current.ThreadID
	item.takeoverFinalize = true
	item.attempt = 0
	e.items[itemID] = item
	if err := e.enterPhase(item); err != nil {
		return err
	}
	return e.schedule()
}

func (e *Engine) answer(itemID, answer string) error {
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("answer question %q: answer cannot be empty", itemID)
	}
	if len(answer) > maxHumanNoteBytes {
		return fmt.Errorf("answer question %q: answer is %d bytes; maximum is %d", itemID, len(answer), maxHumanNoteBytes)
	}
	if e.activeSlots >= e.config.GlobalConcurrency {
		return fmt.Errorf("answer question %q: global concurrency is full", itemID)
	}
	item, err := e.loadParked(itemID)
	if err != nil {
		return err
	}
	if Reason(item.item.Reason) != ReasonQuestion {
		return fmt.Errorf("answer question %q: item reason is %q, want %q", itemID, item.item.Reason, ReasonQuestion)
	}
	if !e.projectHasCapacity(item.item.ProjectID) {
		return fmt.Errorf("answer question %q: project concurrency is full", itemID)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("answer question %q phases: %w", itemID, err)
	}
	current, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok || current.ThreadID == "" {
		return fmt.Errorf("answer question %q: parked attempt thread is missing", itemID)
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	item.feedback = &Feedback{Note: answer}
	item.priorThreadID = current.ThreadID
	item.attempt = 0
	e.items[itemID] = item
	if err := e.enterPhase(item); err != nil {
		return err
	}
	return e.schedule()
}

func (e *Engine) resolveHumanGate(itemID string, choice HumanDecision, note string) error {
	if choice != HumanApprove && choice != HumanReject {
		return fmt.Errorf("resolve human gate %q: decision must be approve or reject", itemID)
	}
	if len(note) > maxHumanNoteBytes {
		return fmt.Errorf("resolve human gate %q: note is %d bytes; maximum is %d", itemID, len(note), maxHumanNoteBytes)
	}
	if e.activeSlots >= e.config.GlobalConcurrency {
		return fmt.Errorf("resolve human gate %q: global concurrency is full", itemID)
	}
	item, err := e.loadParked(itemID)
	if err != nil {
		return err
	}
	if Reason(item.item.Reason) != ReasonGate {
		return fmt.Errorf("resolve human gate %q: item reason is %q, want %q", itemID, item.item.Reason, ReasonGate)
	}
	if !e.projectHasCapacity(item.item.ProjectID) {
		return fmt.Errorf("resolve human gate %q: project concurrency is full", itemID)
	}
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return fmt.Errorf("resolve human gate %q: phase %q is absent from snapshot", itemID, item.phaseID)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return err
	}
	current, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok || len(current.GateTrace) == 0 {
		return fmt.Errorf("resolve human gate %q: parked phase trace is missing", itemID)
	}
	var trace def.GateTrace
	if err := decodeJSON(current.GateTrace, &trace); err != nil {
		return fmt.Errorf("resolve human gate %q trace: %w", itemID, err)
	}
	vars, _, err := e.variableContext(item, current.OutputEnvelope)
	if err != nil {
		return err
	}
	if trace.Decision.Kind != def.DecisionHuman {
		if item.item.StepMode && stepModeAutomaticDecision(trace.Decision.Kind) && len(current.Intervention) == 0 {
			if choice == HumanReject {
				return fmt.Errorf("resolve human gate %q: step gates support approve; use cancel, resume, or take over", itemID)
			}
			intervention, err := json.Marshal(HumanIntervention{Decision: choice, Note: note})
			if err != nil {
				return err
			}
			phaseStatus := "completed"
			if trace.Decision.Kind == def.DecisionFailed {
				phaseStatus = "failed"
			}
			if err := e.store.CompleteWorkItemPhase(
				itemID, item.phaseID, item.attempt, current.OutputEnvelope,
				current.GateTrace, phaseStatus, e.timestamp(),
			); err != nil {
				return err
			}
			// Complete the phase before recording approval. If the first write
			// lands alone, approval can be retried. Once intervention exists,
			// startup recovery may safely execute the persisted decision because
			// downstream variable reconstruction can see the completed phase.
			if err := e.store.UpdateWorkItemPhaseIntervention(itemID, item.phaseID, item.attempt, intervention); err != nil {
				return err
			}
			e.emitter.Emit("workflow:phase-state", PhaseEvent{
				ItemID: itemID, PhaseID: item.phaseID, Attempt: item.attempt, Status: phaseStatus,
			})
			if err := e.transition(item, StateRunning, ""); err != nil {
				return err
			}
			e.items[itemID] = item
			return e.continueHumanDecision(item, trace.Decision, feedbackFor(vars, trace.Decision.Feedback, note))
		}
		if len(current.Intervention) == 0 {
			return fmt.Errorf("resolve human gate %q: persisted decision is %q without a human intervention", itemID, trace.Decision.Kind)
		}
		return e.continuePersistedHumanDecision(item, current, trace.Decision, vars)
	}
	if trace.Decision.RouteIndex < 0 || trace.Decision.RouteIndex >= len(phase.Gate.Routes) {
		return fmt.Errorf("resolve human gate %q: persisted decision is not a valid human route", itemID)
	}
	route := phase.Gate.Routes[trace.Decision.RouteIndex]
	if route.Human == nil || route.Human.Reject == nil {
		return fmt.Errorf("resolve human gate %q: route %d has no complete human decision", itemID, trace.Decision.RouteIndex)
	}

	decision := decisionForTarget(route.Human.Approve, trace.Decision.RouteIndex)
	if choice == HumanReject {
		reject := route.Human.Reject
		edge := def.GateEdgeKey(phase.ID, trace.Decision.RouteIndex)
		phaseContexts, err := e.store.ListWorkItemPhaseContexts(itemID)
		if err != nil {
			return err
		}
		counts, err := loopCounts(itemID, phaseContexts)
		if err != nil {
			return err
		}
		if counts[edge] >= reject.Max {
			decision = def.RouteDecision{Kind: def.DecisionRetriesExhausted, RouteIndex: -1}
		} else {
			decision = def.RouteDecision{
				Kind: def.DecisionLoop, RouteIndex: trace.Decision.RouteIndex,
				Target: reject.Loop, Feedback: append([]string(nil), reject.Feedback...),
				LoopEdge: edge, Max: reject.Max,
			}
		}
	}
	intervention, err := json.Marshal(HumanIntervention{Decision: choice, Note: note})
	if err != nil {
		return err
	}
	if err := e.store.UpdateWorkItemPhaseIntervention(itemID, item.phaseID, item.attempt, intervention); err != nil {
		return err
	}
	trace.Decision = decision
	encodedTrace, err := json.Marshal(trace)
	if err != nil {
		return err
	}
	phaseStatus := "completed"
	if decision.Kind == def.DecisionRetriesExhausted {
		phaseStatus = "parked"
	}
	if err := e.store.CompleteWorkItemPhase(
		itemID, item.phaseID, item.attempt, current.OutputEnvelope,
		encodedTrace, phaseStatus, e.timestamp(),
	); err != nil {
		return err
	}
	e.emitter.Emit("workflow:phase-state", PhaseEvent{ItemID: itemID, PhaseID: item.phaseID, Attempt: item.attempt, Status: phaseStatus})

	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	e.items[itemID] = item
	return e.continueHumanDecision(item, decision, feedbackFor(vars, decision.Feedback, note))
}

func (e *Engine) recoverPersistedHumanDecision(itemID string) (bool, error) {
	item, err := e.loadParked(itemID)
	if err != nil {
		return false, err
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return false, err
	}
	current, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok || len(current.GateTrace) == 0 {
		return false, nil
	}
	var trace def.GateTrace
	if err := decodeJSON(current.GateTrace, &trace); err != nil {
		return false, err
	}
	if trace.Decision.Kind == def.DecisionHuman {
		return false, nil
	}
	if len(current.Intervention) == 0 {
		return false, nil
	}
	vars, _, err := e.variableContext(item, current.OutputEnvelope)
	if err != nil {
		return false, err
	}
	return true, e.continuePersistedHumanDecision(item, current, trace.Decision, vars)
}

func (e *Engine) isHumanGate(item *runtimeItem) (bool, error) {
	phases, err := e.store.ListWorkItemPhases(item.item.ID)
	if err != nil {
		return false, err
	}
	current, ok := phaseAttempt(phases, item.phaseID, item.attempt)
	if !ok || len(current.GateTrace) == 0 {
		return false, nil
	}
	var trace def.GateTrace
	if err := decodeJSON(current.GateTrace, &trace); err != nil {
		return false, err
	}
	return trace.Decision.Kind == def.DecisionHuman ||
		item.item.StepMode && stepModeAutomaticDecision(trace.Decision.Kind) ||
		len(current.Intervention) > 0, nil
}

func (e *Engine) continuePersistedHumanDecision(item *runtimeItem, current store.WorkItemPhase, decision def.RouteDecision, vars map[string]any) error {
	var intervention HumanIntervention
	if len(current.Intervention) > 0 {
		if err := decodeJSON(current.Intervention, &intervention); err != nil {
			return fmt.Errorf("continue human gate %q intervention: %w", item.item.ID, err)
		}
	}
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	e.items[item.item.ID] = item
	return e.continueHumanDecision(item, decision, feedbackFor(vars, decision.Feedback, intervention.Note))
}

func (e *Engine) continueHumanDecision(item *runtimeItem, decision def.RouteDecision, feedback *Feedback) error {
	switch decision.Kind {
	case def.DecisionAdvance, def.DecisionLoop:
		item.phaseID = decision.Target
		item.attempt = 0
		item.feedback = feedback
		waitingErr := e.startWaiting()
		return errors.Join(waitingErr, e.enterPhase(item))
	case def.DecisionDone:
		return e.transition(item, StateDone, "")
	case def.DecisionFailed:
		return e.transition(item, StateFailed, ReasonCheckFailedGenuine)
	case def.DecisionRetriesExhausted:
		return e.transition(item, StateNeedsHuman, ReasonRetriesExhausted)
	default:
		return fmt.Errorf("continue human gate %q: unsupported decision %q", item.item.ID, decision.Kind)
	}
}

func decisionForTarget(target string, routeIndex int) def.RouteDecision {
	switch target {
	case "done":
		return def.RouteDecision{Kind: def.DecisionDone, RouteIndex: routeIndex}
	case "failed":
		return def.RouteDecision{Kind: def.DecisionFailed, RouteIndex: routeIndex}
	default:
		return def.RouteDecision{Kind: def.DecisionAdvance, RouteIndex: routeIndex, Target: target}
	}
}

func feedbackFor(vars map[string]any, refs []string, note string) *Feedback {
	if len(refs) == 0 && note == "" {
		return nil
	}
	feedback := &Feedback{Note: note, Values: make(map[string]any, len(refs))}
	for _, ref := range refs {
		if value, ok := def.LookupVariable(vars, ref); ok {
			feedback.Values[ref] = value
		}
	}
	return feedback
}

func phaseAttempt(phases []store.WorkItemPhase, phaseID string, attempt int) (store.WorkItemPhase, bool) {
	for _, phase := range phases {
		if phase.PhaseID == phaseID && phase.Attempt == attempt {
			return phase, true
		}
	}
	return store.WorkItemPhase{}, false
}
