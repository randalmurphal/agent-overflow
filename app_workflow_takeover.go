package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/def"
	"agent-overflow/internal/workflow/engine"
)

// Human takeover of a live phase thread. The engine owns the FSM edges
// (`TakeOver` / `CompleteTakeover`); this file owns the runner-side bookkeeping
// they depend on: detaching a live attempt without losing its reliability
// timer, tracking which threads are under takeover, and the schema swap Claude
// needs because it binds structured output at session start.

// workflowTakeoverYieldTimeout bounds the wait for an interrupted turn to yield
// before the takeover is refused and the attempt is restored.
const workflowTakeoverYieldTimeout = 15 * time.Second

type workflowTakeover struct {
	itemID         string
	schemaAttached bool
	transitioning  bool
}

// workflowTakeoverTimer preserves a detached attempt's reliability timer so a
// refused takeover restores the deadline it was already running against.
type workflowTakeoverTimer struct {
	mode     workflowTimerMode
	deadline time.Time
}

func (r *workflowAppRunner) StopForTakeover(ctx context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	r.mu.Lock()
	_, isTool := r.tools[runKey]
	r.mu.Unlock()
	if isTool {
		return nil, fmt.Errorf("workflow runner: attempt %s runs a deterministic command and has no session to take over", runKey)
	}
	attempt, timerState, ok := r.detachForTakeover(runKey)
	if !ok {
		return nil, fmt.Errorf("workflow runner: live takeover attempt %s is not registered", runKey)
	}
	attempt.sendMu.Lock()
	attempt.sendMu.Unlock()
	partial, err := r.interruptAndWaitForYield(ctx, runKey, attempt.threadID)
	if err == nil {
		return partial, nil
	}
	attempt.unsubscribe = r.app.subscribeThreadTurnObserver(attempt.threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(runKey, event)
	})
	r.mu.Lock()
	r.runs[runKey] = attempt
	r.schemas[attempt.threadID] = append(json.RawMessage(nil), attempt.schema...)
	r.workItems[attempt.threadID] = attempt.key.ItemID
	r.restoreTakeoverTimerLocked(runKey, attempt, timerState)
	r.mu.Unlock()
	return nil, err
}

func (r *workflowAppRunner) detachForTakeover(runKey string) (*workflowAttempt, workflowTakeoverTimer, bool) {
	r.mu.Lock()
	attempt, ok := r.runs[runKey]
	state := workflowTakeoverTimer{}
	if ok {
		state.mode = attempt.timerMode
		state.deadline = attempt.timerDeadline
		delete(r.runs, runKey)
		delete(r.schemas, attempt.threadID)
		if r.workItems[attempt.threadID] == attempt.key.ItemID {
			delete(r.workItems, attempt.threadID)
		}
		r.disarmTimerLocked(attempt)
	}
	r.mu.Unlock()
	if ok && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	return attempt, state, ok
}

func (r *workflowAppRunner) restoreTakeoverTimerLocked(runKey string, attempt *workflowAttempt, state workflowTakeoverTimer) {
	if state.mode == workflowTimerNone {
		return
	}
	delay := state.deadline.Sub(r.now())
	if delay < 0 {
		delay = 0
	}
	attempt.timerMode = state.mode
	attempt.timerDeadline = state.deadline
	attempt.timer = r.newTimer(delay, func() { r.timerFired(runKey) })
}

func (r *workflowAppRunner) interruptAndWaitForYield(ctx context.Context, runKey, threadID string) (json.RawMessage, error) {
	yielded := make(chan struct{}, 1)
	unsubscribe := r.app.subscribeThreadTurnObserver(threadID, func(_ string, event provider.ProviderEvent) {
		if event.Kind == provider.EventTurnComplete {
			select {
			case yielded <- struct{}{}:
			default:
			}
		}
	})
	defer unsubscribe()
	if err := r.app.InterruptTurn(threadID); err != nil {
		return nil, fmt.Errorf("workflow runner: interrupt %s: %w", runKey, err)
	}
	if _, active, err := r.app.store.GetActiveTurn(threadID); err != nil {
		return nil, fmt.Errorf("workflow runner: inspect interrupted turn %s: %w", runKey, err)
	} else if !active {
		return nil, nil
	}
	timer := time.NewTimer(workflowTakeoverYieldTimeout)
	defer timer.Stop()
	select {
	case <-yielded:
		return nil, nil
	case <-timer.C:
		return nil, fmt.Errorf("workflow runner: interrupted turn %s did not yield within %s", runKey, workflowTakeoverYieldTimeout)
	case <-ctx.Done():
		return nil, fmt.Errorf("workflow runner: wait for interrupted turn %s: %w", runKey, ctx.Err())
	}
}

func (r *workflowAppRunner) registerTakeover(itemID, threadID string) error {
	r.mu.Lock()
	if existing, ok := r.takeovers[threadID]; ok && existing.itemID == itemID {
		if existing.transitioning {
			r.mu.Unlock()
			return fmt.Errorf("workflow runner: takeover item %s is completing or resuming", itemID)
		}
		r.workItems[threadID] = itemID
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	item, err := r.app.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("workflow runner: load takeover item: %w", err)
	}
	if item.State != string(engine.StateNeedsHuman) || item.Reason != string(engine.ReasonTakenOver) {
		return fmt.Errorf("workflow runner: item %s is %s(%s), want needs-human(taken-over)", itemID, item.State, item.Reason)
	}
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return fmt.Errorf("workflow runner: decode takeover snapshot: %w", err)
	}
	phases, err := r.app.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("workflow runner: list takeover phases: %w", err)
	}
	var current store.WorkItemPhase
	for index := len(phases) - 1; index >= 0; index-- {
		if phases[index].ThreadID == threadID {
			current = phases[index]
			break
		}
	}
	if current.ThreadID == "" {
		return fmt.Errorf("workflow runner: thread %s is not attached to item %s", threadID, itemID)
	}
	var phase def.Phase
	for _, candidate := range snapshot.Workflow.Phases {
		if candidate.ID == current.PhaseID {
			phase = candidate
			break
		}
	}
	if phase.ID == "" {
		return fmt.Errorf("workflow runner: phase %s is absent from item %s snapshot", current.PhaseID, itemID)
	}
	if _, err := def.EnvelopeSchema(phase); err != nil {
		return fmt.Errorf("workflow runner: takeover phase schema: %w", err)
	}
	_, sessionAlive := r.app.sessionManager().get(threadID)
	r.mu.Lock()
	if existing, ok := r.takeovers[threadID]; ok && existing.itemID == itemID {
		sessionAlive = existing.schemaAttached
	}
	r.takeovers[threadID] = workflowTakeover{
		itemID: itemID, schemaAttached: sessionAlive,
	}
	r.workItems[threadID] = itemID
	delete(r.schemas, threadID)
	r.mu.Unlock()
	if phase.Provider == string(provider.Claude) && sessionAlive {
		if err := r.app.stopSession(threadID); err != nil {
			return fmt.Errorf("workflow runner: stop schema-attached takeover session: %w", err)
		}
		if err := r.app.startSession(threadID); err != nil {
			return fmt.Errorf("workflow runner: restart takeover session without schema: %w", err)
		}
	}
	return nil
}

func (r *workflowAppRunner) beginTakeoverTransition(itemID, threadID string) error {
	if err := r.registerTakeover(itemID, threadID); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	takeover, ok := r.takeovers[threadID]
	if !ok || takeover.itemID != itemID {
		return fmt.Errorf("workflow runner: takeover item %s is not registered on thread %s", itemID, threadID)
	}
	takeover.transitioning = true
	r.takeovers[threadID] = takeover
	return nil
}

func (r *workflowAppRunner) cancelTakeoverTransition(itemID, threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	takeover, ok := r.takeovers[threadID]
	if ok && takeover.itemID == itemID {
		takeover.transitioning = false
		r.takeovers[threadID] = takeover
	}
}

func (r *workflowAppRunner) clearTakeover(itemID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for threadID, takeover := range r.takeovers {
		if takeover.itemID != itemID {
			continue
		}
		delete(r.takeovers, threadID)
		if r.workItems[threadID] == itemID {
			delete(r.workItems, threadID)
		}
	}
}

// removeTemporarySchema drops the schema a finalize-takeover start attached for
// a Claude session restart that then failed, so a later takeover does not
// inherit it.
func (r *workflowAppRunner) removeTemporarySchema(threadID string) {
	r.mu.Lock()
	delete(r.schemas, threadID)
	r.mu.Unlock()
}
