package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func (e *Engine) rebuild() error {
	projects, err := e.store.ListProjects()
	if err != nil {
		return fmt.Errorf("rebuild workflow engine: list projects: %w", err)
	}
	projectIDs := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		projectIDs[project.ID] = struct{}{}
	}
	activeItems, err := e.store.ListWorkItems(store.WorkItemListFilter{
		States: []string{string(StateRunning), string(StateNeedsHuman)},
	})
	if err != nil {
		return fmt.Errorf("rebuild workflow engine: list active items: %w", err)
	}
	itemsByProject := make(map[string][]store.WorkItem, len(projects))
	orphanItems := make([]store.WorkItem, 0)
	for _, item := range activeItems {
		if _, exists := projectIDs[item.ProjectID]; !exists {
			orphanItems = append(orphanItems, item)
			continue
		}
		itemsByProject[item.ProjectID] = append(itemsByProject[item.ProjectID], item)
	}
	// Human gate decisions that were persisted but not yet executed are
	// replayed after every item is rebuilt, so their downstream phases see a
	// fully reconstructed run.
	var pendingHuman []string
	for _, project := range projects {
		items := itemsByProject[project.ID]
		for _, storedItem := range items {
			e.observeItemTimestamps(storedItem)
			if State(storedItem.State) == StateNeedsHuman {
				if Reason(storedItem.Reason) == ReasonGate && len(storedItem.Snapshot) > 0 {
					pendingHuman = append(pendingHuman, storedItem.ID)
				}
				continue
			}
			item := &runtimeItem{item: storedItem}
			e.items[storedItem.ID] = item
			snapshot, err := decodeSnapshot(storedItem.Snapshot)
			if err != nil {
				parkErr := e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError})
				rebuildErr := fmt.Errorf("rebuild item %q snapshot: %w", storedItem.ID, err)
				e.emitError(storedItem.ID, errors.Join(rebuildErr, parkErr))
				continue
			}
			item.adoptSnapshot(snapshot)
			phases, err := e.store.ListWorkItemPhases(storedItem.ID)
			if err != nil {
				return fmt.Errorf("rebuild item %q phases: %w", storedItem.ID, err)
			}
			current, hasCurrent := currentPhaseAttempt(phases)
			if hasCurrent {
				item.phaseID = current.PhaseID
				item.attempt = current.Attempt
				e.observePhaseTimestamps(current)
			} else if len(item.workflow.Phases) > 0 {
				item.phaseID = item.workflow.Phases[0].ID
			}
			// A call phase resting on a live child is a legitimate `running`
			// state with no envelope and no runner, so it is re-linked rather
			// than swept: the child is the thing that reports, and it rebuilt (or
			// will rebuild) as an ordinary item of its own.
			if hasCurrent && current.Status == "running" && callPhase(item.workflow, current.PhaseID) {
				if err := e.recoverCall(item, current); err != nil {
					recoverErr := fmt.Errorf("recover call phase for item %q: %w", storedItem.ID, err)
					if State(item.item.State) == StateRunning {
						recoverErr = errors.Join(recoverErr, e.teardown(item, teardownRequest{
							phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
						}))
					}
					e.emitError(storedItem.ID, recoverErr)
				}
				continue
			}
			// A fan-out attempt whose call units are still linked to live child runs
			// is recoverable in place for the same reason a call phase is: those
			// units hold no process state, and the children rebuild as ordinary runs
			// of their own. Anything the recovery cannot adopt falls through to the
			// ordinary crash park below, which is what a fan-out of agent units gets.
			if hasCurrent && current.Status == "running" {
				recovered, err := e.recoverFanOutCalls(item, current)
				if err != nil {
					recoverErr := fmt.Errorf("recover fan-out call units for item %q: %w", storedItem.ID, err)
					if State(item.item.State) == StateRunning {
						recoverErr = errors.Join(recoverErr, e.teardown(item, teardownRequest{
							phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError,
						}))
					}
					e.emitError(storedItem.ID, recoverErr)
					continue
				}
				if recovered {
					continue
				}
			}
			if !hasCurrent || !terminalEnvelope(current.OutputEnvelope) {
				item.runnerActive = hasCurrent && current.Status == "running"
				if err := e.teardown(item, teardownRequest{
					output: current.OutputEnvelope, phaseStatus: "parked",
					nextState: StateNeedsHuman, reason: ReasonInterrupted,
				}); err != nil {
					return fmt.Errorf("crash sweep item %q: %w", storedItem.ID, err)
				}
				continue
			}
			if err := e.recoverTerminal(item, current); err != nil {
				recoverErr := fmt.Errorf("recover terminal phase for item %q: %w", storedItem.ID, err)
				if State(item.item.State) == StateRunning {
					recoverErr = errors.Join(recoverErr, e.teardown(item, teardownRequest{
						output: current.OutputEnvelope, phaseStatus: "parked",
						nextState: StateNeedsHuman, reason: ReasonWiringError,
					}))
				}
				e.emitError(storedItem.ID, recoverErr)
				continue
			}
		}
	}
	for _, item := range orphanItems {
		endedAt := e.timestamp()
		if err := e.store.UpdateWorkItemState(item.ID, string(StateCancelled), string(ReasonInterrupted), endedAt); err != nil {
			return fmt.Errorf("rebuild workflow engine: cancel orphan item %q: %w", item.ID, err)
		}
		e.emitItemState(item.ID, item.ProjectID, State(item.State), StateCancelled, ReasonInterrupted)
		log.Printf("workflow rebuild: cancelled orphan item %s for missing project %s", item.ID, item.ProjectID)
	}
	e.recoverPendingHumanDecisions(pendingHuman)
	e.emitEngineState()
	return nil
}

// recoverPendingHumanDecisions replays gate decisions that were recorded
// before the app died. Each failure is item-scoped and reported; one broken
// run never blocks the rest of the rebuild.
func (e *Engine) recoverPendingHumanDecisions(itemIDs []string) {
	for _, itemID := range itemIDs {
		if _, err := e.recoverPersistedHumanDecision(itemID); err != nil {
			e.emitError(itemID, err)
		}
	}
}

func decodeSnapshot(payload json.RawMessage) (Snapshot, error) {
	if len(payload) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot is empty")
	}
	if len(payload) > MaxSnapshotBytes {
		return Snapshot{}, fmt.Errorf("snapshot is %d bytes; maximum is %d", len(payload), MaxSnapshotBytes)
	}
	var snapshot Snapshot
	if err := decodeJSON(payload, &snapshot); err != nil {
		return Snapshot{}, err
	}
	if len(snapshot.Workflow.Phases) == 0 {
		return Snapshot{}, fmt.Errorf("snapshot workflow has no phases")
	}
	return snapshot, nil
}

// adoptSnapshot installs a frozen definition on a resident run. A snapshot
// written before workspace need was frozen carries none; deriving it from the
// frozen graph alone is exactly what the runner did then, so an in-flight run
// keeps the workspace it already has instead of changing it mid-run.
func (item *runtimeItem) adoptSnapshot(snapshot Snapshot) {
	item.workflow = snapshot.Workflow
	item.workspaceNeed = snapshot.WorkspaceNeed
	if item.workspaceNeed == "" {
		item.workspaceNeed = def.DeriveWorkspaceNeed(snapshot.Workflow)
	}
}

func currentPhaseAttempt(phases []store.WorkItemPhase) (store.WorkItemPhase, bool) {
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i].Status == "running" {
			return phases[i], true
		}
	}
	if len(phases) == 0 {
		return store.WorkItemPhase{}, false
	}
	return phases[len(phases)-1], true
}

func terminalEnvelope(payload json.RawMessage) bool {
	if len(payload) == 0 {
		return false
	}
	var envelope controlEnvelope
	if err := decodeJSON(payload, &envelope); err != nil {
		return false
	}
	return envelope.Status == "done" || envelope.Status == "question" || envelope.Status == "stuck"
}

func (e *Engine) recoverTerminal(item *runtimeItem, phase store.WorkItemPhase) error {
	var envelope controlEnvelope
	if err := decodeJSON(phase.OutputEnvelope, &envelope); err != nil {
		return err
	}
	if phase.Status == "running" {
		kind := OutcomeDone
		if envelope.Status == "question" {
			kind = OutcomeQuestion
		} else if envelope.Status == "stuck" {
			kind = OutcomeStuck
		}
		return e.complete(RunKey{ItemID: item.item.ID, PhaseID: phase.PhaseID, Attempt: phase.Attempt}, Outcome{Kind: kind, Envelope: phase.OutputEnvelope})
	}
	if phase.Status == "parked" {
		switch envelope.Status {
		case "question":
			return e.transition(item, StateNeedsHuman, ReasonQuestion)
		case "stuck":
			return e.transition(item, StateNeedsHuman, ReasonStuck)
		case "done":
			if len(phase.GateTrace) == 0 {
				return fmt.Errorf("parked done envelope has no gate trace")
			}
			var trace def.GateTrace
			if err := decodeJSON(phase.GateTrace, &trace); err != nil {
				return err
			}
			return e.recoverDecision(item, trace.Decision)
		default:
			return fmt.Errorf("parked phase has unsupported envelope status %q", envelope.Status)
		}
	}
	if phase.Status == "cancelled" {
		return e.transition(item, StateCancelled, ReasonInterrupted)
	}
	if phase.Status == "failed" {
		return e.transition(item, StateFailed, ReasonCheckFailedGenuine)
	}
	if len(phase.GateTrace) == 0 {
		return fmt.Errorf("completed terminal envelope has no gate trace")
	}
	var trace def.GateTrace
	if err := decodeJSON(phase.GateTrace, &trace); err != nil {
		return err
	}
	if trace.Decision.Kind == def.DecisionAdvance || trace.Decision.Kind == def.DecisionLoop {
		vars, _, err := e.variableContext(item, phase.OutputEnvelope)
		if err != nil {
			return err
		}
		note := ""
		if len(phase.Intervention) > 0 {
			var intervention HumanIntervention
			if err := decodeJSON(phase.Intervention, &intervention); err != nil {
				return fmt.Errorf("decode recovered intervention: %w", err)
			}
			note = intervention.Note
		}
		item.feedback = feedbackFor(vars, trace.Decision.Feedback, note)
	}
	return e.recoverDecision(item, trace.Decision)
}

func (e *Engine) recoverDecision(item *runtimeItem, decision def.RouteDecision) error {
	switch decision.Kind {
	case def.DecisionAdvance, def.DecisionLoop:
		item.phaseID = decision.Target
		item.attempt = 0
		waitingErr := e.startWaiting()
		return errors.Join(waitingErr, e.enterPhase(item))
	case def.DecisionDone:
		return e.transition(item, StateDone, "")
	case def.DecisionFailed:
		return e.transition(item, StateFailed, ReasonCheckFailedGenuine)
	case def.DecisionHuman, def.DecisionPark:
		return e.transition(item, StateNeedsHuman, ReasonGate)
	case def.DecisionRetriesExhausted:
		return e.transition(item, StateNeedsHuman, ReasonRetriesExhausted)
	case def.DecisionNoMatch:
		return e.transition(item, StateNeedsHuman, ReasonWiringError)
	default:
		return fmt.Errorf("unknown recovered gate decision %q", decision.Kind)
	}
}

func (e *Engine) observeItemTimestamps(item store.WorkItem) {
	for _, timestamp := range []int64{item.CreatedAt, item.StartedAt, item.EndedAt} {
		if timestamp > e.lastTimestamp {
			e.lastTimestamp = timestamp
		}
	}
}

func (e *Engine) observePhaseTimestamps(phase store.WorkItemPhase) {
	for _, timestamp := range []int64{phase.StartedAt, phase.EndedAt} {
		if timestamp > e.lastTimestamp {
			e.lastTimestamp = timestamp
		}
	}
}
