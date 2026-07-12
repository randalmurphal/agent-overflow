package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
)

func (e *Engine) schedule() error {
	var errs []error
	errs = append(errs, e.startWaiting())
	errs = append(errs, e.resumePendingHuman())
	if !e.queueActive {
		return errors.Join(errs...)
	}
	for e.activeSlots < e.config.GlobalConcurrency && e.queueActive {
		item := e.popQueued()
		if item == nil {
			break
		}
		if err := e.startItem(item); err != nil {
			errs = append(errs, err)
			e.emitError(item.item.ID, err)
			if State(item.item.State) == StateQueued {
				e.insertQueued(item)
				break
			}
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) resumePendingHuman() error {
	var errs []error
	for len(e.pendingHuman) > 0 && e.activeSlots < e.config.GlobalConcurrency {
		itemID := e.pendingHuman[0]
		e.pendingHuman[0] = ""
		e.pendingHuman = e.pendingHuman[1:]
		_, err := e.recoverPersistedHumanDecision(itemID)
		if err != nil {
			e.emitError(itemID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) removePendingHuman(itemID string) {
	for index, pending := range e.pendingHuman {
		if pending == itemID {
			copy(e.pendingHuman[index:], e.pendingHuman[index+1:])
			e.pendingHuman[len(e.pendingHuman)-1] = ""
			e.pendingHuman = e.pendingHuman[:len(e.pendingHuman)-1]
			return
		}
	}
}

func queueLess(left, right store.WorkItem) bool {
	if left.SortPosition != right.SortPosition {
		return left.SortPosition < right.SortPosition
	}
	if left.CreatedAt != right.CreatedAt {
		return left.CreatedAt < right.CreatedAt
	}
	return left.ID < right.ID
}

func (e *Engine) insertQueued(item *runtimeItem) {
	e.compactQueued()
	index := sort.Search(len(e.queued), func(index int) bool {
		return !queueLess(e.queued[index].item, item.item)
	})
	e.queued = append(e.queued, nil)
	copy(e.queued[index+1:], e.queued[index:])
	e.queued[index] = item
}

func (e *Engine) sortQueued() {
	e.compactQueued()
	sort.Slice(e.queued, func(i, j int) bool {
		return queueLess(e.queued[i].item, e.queued[j].item)
	})
}

func (e *Engine) popQueued() *runtimeItem {
	if e.queueHead >= len(e.queued) {
		e.queued = nil
		e.queueHead = 0
		return nil
	}
	item := e.queued[e.queueHead]
	e.queued[e.queueHead] = nil
	e.queueHead++
	return item
}

func (e *Engine) compactQueued() {
	if e.queueHead == 0 {
		return
	}
	copy(e.queued, e.queued[e.queueHead:])
	remaining := len(e.queued) - e.queueHead
	for index := remaining; index < len(e.queued); index++ {
		e.queued[index] = nil
	}
	e.queued = e.queued[:remaining]
	e.queueHead = 0
}

func (e *Engine) startItem(item *runtimeItem) error {
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
		return err
	}
	item.item.Snapshot = snapshot
	item.item.StartedAt = startedAt
	item.workflow = workflow
	item.phaseID = workflow.Phases[0].ID
	if err := e.transition(item, StateRunning, ""); err != nil {
		return err
	}
	e.recordStart()
	return e.enterPhase(item)
}

func (e *Engine) parkUnstartable(item *runtimeItem, cause error) error {
	if err := e.transition(item, StateRunning, ""); err != nil {
		return errors.Join(cause, err)
	}
	e.recordStart()
	return errors.Join(
		e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}),
		cause,
	)
}

func (e *Engine) recordStart() {
	if e.startsRemaining < 0 {
		return
	}
	e.startsRemaining--
	if e.startsRemaining == 0 {
		e.queueActive = false
		e.emitQueue()
	}
}

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

	acquired, err := e.acquireResources(item.item.ProjectID, phase.Resources)
	if err != nil {
		return errors.Join(e.teardown(item, teardownRequest{phaseStatus: "parked", nextState: StateNeedsHuman, reason: ReasonSetupFailed}), err)
	}
	if !acquired {
		item.waiting = true
		return nil
	}
	item.acquired = canonicalResources(phase.Resources)
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
	if errors.Is(command.err, ErrSetupFailed) {
		reason = ReasonSetupFailed
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
