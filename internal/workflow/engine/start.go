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
func (e *Engine) startNewItem(item store.WorkItem) error {
	if item.ID == "" || item.ProjectID == "" {
		return fmt.Errorf("start item: id and project id are required")
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
	return e.beginRun(runtime)
}

// beginRun freezes the resolved workflow into the run record and enters the
// first phase. The item is already persisted running, so every failure here
// parks it through teardown instead of leaving an unstarted row.
func (e *Engine) beginRun(item *runtimeItem) error {
	workflow, err := e.definitions.Resolve(e.ctx, item.item)
	if err != nil {
		return e.parkUnstartable(item, fmt.Errorf("resolve item %q workflow: %w", item.item.ID, err))
	}
	if len(workflow.Phases) == 0 {
		return e.parkUnstartable(item, fmt.Errorf("resolve item %q workflow: workflow has no phases", item.item.ID))
	}
	snapshot, err := json.Marshal(Snapshot{Workflow: workflow})
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
	item.phaseID = workflow.Phases[0].ID
	return e.enterPhase(item)
}

func (e *Engine) parkUnstartable(item *runtimeItem, cause error) error {
	return errors.Join(
		e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
		cause,
	)
}

// enterPhase records the next phase attempt and starts it as soon as the
// global pause is clear and its resources are free. A held phase leaves the
// item running and waiting, never parked.
func (e *Engine) enterPhase(item *runtimeItem) error {
	phase, ok := findPhase(item.workflow, item.phaseID)
	if !ok {
		return errors.Join(
			e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}),
			fmt.Errorf("phase %q is absent from item %q snapshot", item.phaseID, item.item.ID),
		)
	}
	if halted, err := e.enforceBudget(item); halted {
		return err
	}
	vars, priorPhases, err := e.variableContext(item, nil)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}), err)
	}
	attempt := nextAttempt(priorPhases, phase.ID)
	input, err := json.Marshal(PhaseInput{Vars: vars, Feedback: item.feedback})
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonWiringError}), err)
	}
	if err := e.store.CreateWorkItemPhase(store.WorkItemPhase{
		ItemID: item.item.ID, PhaseID: phase.ID, Attempt: attempt,
		InputEnvelope: input, Status: "running", StartedAt: e.timestamp(),
	}); err != nil {
		createErr := fmt.Errorf("create phase attempt %s/%s/%d: %w", item.item.ID, phase.ID, attempt, err)
		return errors.Join(e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}), createErr)
	}
	item.attempt = attempt
	e.emitter.Emit("workflow:phase-state", PhaseEvent{
		ItemID: item.item.ID, PhaseID: phase.ID, Attempt: attempt, Status: "running",
	})

	if e.paused {
		e.addWaiting(item)
		return nil
	}
	acquired, ok, err := e.acquirePhaseResources(item.item.ProjectID, phase)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}), err)
	}
	if !ok {
		e.addWaiting(item)
		return nil
	}
	item.acquired = acquired
	return e.startRunner(item, phase, vars)
}

func (e *Engine) startRunner(item *runtimeItem, phase def.Phase, vars map[string]any) error {
	request := RunRequest{
		Key:  RunKey{ItemID: item.item.ID, PhaseID: item.phaseID, Attempt: item.attempt},
		Item: item.item, Workflow: item.workflow, Phase: phase, Vars: vars,
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
		State(item.item.State) != StateRunning || !item.runnerStarting {
		return nil
	}
	if item.runnerStartCancel != nil {
		item.runnerStartCancel()
	}
	item.runnerStarting = false
	item.runnerStartCancel = nil
	if command.err == nil {
		item.runnerActive = true
		return nil
	}
	reason := ReasonAgentError
	switch {
	case errors.Is(command.err, ErrSetupFailed):
		reason = ReasonSetupFailed
	case errors.Is(command.err, ErrWiringFailed):
		reason = ReasonWiringError
	}
	return errors.Join(
		e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: reason}),
		command.err,
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
