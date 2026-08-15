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

// workflowTakeoverRestore preserves what `detachForTakeover` removed beyond the
// registry entries, so a refused takeover restores the attempt exactly as it
// found it: the reliability deadline it was running against, and whether the
// thread's usage attribution belonged to this attempt at detach time — restoring
// one that did not would silently reassign another item's thread.
type workflowTakeoverRestore struct {
	mode       workflowTimerMode
	deadline   time.Time
	attributed bool
}

func (r *workflowAppRunner) StopForTakeover(ctx context.Context, key engine.RunKey) (json.RawMessage, error) {
	runKey := workflowRunKey(key)
	r.mu.Lock()
	_, isTool := r.tools[runKey]
	r.mu.Unlock()
	if isTool {
		return nil, fmt.Errorf("workflow runner: attempt %s runs a deterministic command and has no session to take over", runKey)
	}
	attempt, restore, ok := r.detachForTakeover(runKey)
	if !ok {
		return nil, fmt.Errorf("workflow runner: live takeover attempt %s is not registered", runKey)
	}
	// Bounded for the same reason `Stop` is: this runs on the engine's
	// command-loop goroutine, and a send wedged on provider IO would freeze every
	// run in the process rather than only refusing this takeover. Refusing is the
	// honest answer anyway — the human cannot steer a thread whose previous turn
	// is still mid-dispatch — so the attempt goes back exactly as it was and the
	// verb fails with a cause the operator can act on.
	if !r.runBoundedBySendWait(func() { r.awaitInFlightSend(attempt) }) {
		r.restoreTakeoverAttempt(runKey, attempt, restore)
		return nil, fmt.Errorf(
			"workflow runner: attempt %s has a send on thread %s that has not reached the wire in %s; "+
				"the attempt is still running and the takeover was refused",
			runKey, attempt.threadID, r.stopSendWait,
		)
	}
	partial, err := r.interruptAndWaitForYield(ctx, runKey, attempt.threadID)
	if err == nil {
		return partial, nil
	}
	r.restoreTakeoverAttempt(runKey, attempt, restore)
	return nil, err
}

// restoreTakeoverAttempt puts a detached attempt back the way
// `detachForTakeover` found it, for a takeover that was refused: observer,
// registry, schema, work-item attribution, and the reliability deadline it was
// already running against.
func (r *workflowAppRunner) restoreTakeoverAttempt(
	runKey string, attempt *workflowAttempt, restore workflowTakeoverRestore,
) {
	attempt.unsubscribe = r.app.subscribeThreadTurnObserver(attempt.threadID, func(_ string, event provider.ProviderEvent) {
		r.observe(runKey, event)
	})
	r.mu.Lock()
	r.runs[runKey] = attempt
	r.schemas[attempt.threadID] = append(json.RawMessage(nil), attempt.schema...)
	if restore.attributed {
		r.workItems[attempt.threadID] = attempt.key.ItemID
	}
	r.restoreTakeoverTimerLocked(runKey, attempt, restore)
	r.mu.Unlock()
}

func (r *workflowAppRunner) detachForTakeover(runKey string) (*workflowAttempt, workflowTakeoverRestore, bool) {
	r.mu.Lock()
	restore := workflowTakeoverRestore{}
	// The timer state and the attribution are captured before `detachLocked`
	// disarms the one and conditionally deletes the other.
	if attempt, ok := r.runs[runKey]; ok {
		restore.mode = attempt.timerMode
		restore.deadline = attempt.timerDeadline
		restore.attributed = r.workItems[attempt.threadID] == attempt.key.ItemID
	}
	attempt, ok := r.detachLocked(runKey)
	r.mu.Unlock()
	if ok && attempt.unsubscribe != nil {
		attempt.unsubscribe()
	}
	return attempt, restore, ok
}

func (r *workflowAppRunner) restoreTakeoverTimerLocked(runKey string, attempt *workflowAttempt, restore workflowTakeoverRestore) {
	if restore.mode == workflowTimerNone {
		return
	}
	delay := restore.deadline.Sub(r.now())
	if delay < 0 {
		delay = 0
	}
	attempt.timerMode = restore.mode
	attempt.timerDeadline = restore.deadline
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
	// The interrupt's thread-action-lock acquisition is the one wait here with
	// no bound of its own — the yield select below has the timer and ctx — and
	// it is exactly where the incident wedged (a session restart holding the
	// lock mid-start). This still runs on the engine's command loop, so the
	// acquisition gets the same bound the send barrier above got; expiring it
	// abandons nothing but the wait (`LockCtx` admits no one after refusal),
	// and the caller's error path restores the attempt and refuses the
	// takeover.
	lockCtx, cancel := context.WithTimeout(ctx, r.stopSendWait)
	defer cancel()
	if err := r.interrupt(lockCtx, threadID); err != nil {
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

// workflowTakeoverElement is the piece of a run one thread belongs to: a phase
// attempt, or one fan-out unit of one. Registration needs exactly two facts
// about it — the provider whose session must be restarted schema-less, and the
// contract that session will have to answer again if the takeover is finalized.
type workflowTakeoverElement struct {
	description string
	provider    string
	contract    def.EnvelopeContract
}

// registerTakeover marks a thread as human-steered and, for Claude, restarts its
// session without the phase schema so the human's turns are not held to it.
//
// ctx bounds that restart's start half — the join on somebody else's in-flight
// start — which is why it is threaded from the send that triggered the takeover
// rather than invented here. The bound methods that also reach this have no
// context of their own to give and pass `context.Background()`, which is what
// they mean: a user action with no deadline behind it.
func (r *workflowAppRunner) registerTakeover(ctx context.Context, itemID, threadID string) error {
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
	var snapshot engine.Snapshot
	if err := json.Unmarshal(item.Snapshot, &snapshot); err != nil {
		return fmt.Errorf("workflow runner: decode takeover snapshot: %w", err)
	}
	element, err := r.takeoverElement(item, snapshot.Workflow, threadID)
	if err != nil {
		return err
	}
	if _, err := element.contract.Schema(); err != nil {
		return fmt.Errorf("workflow runner: takeover %s schema: %w", element.description, err)
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
	if element.provider == string(provider.Claude) && sessionAlive {
		if err := r.app.stopSession(threadID); err != nil {
			return fmt.Errorf("workflow runner: stop schema-attached takeover session: %w", err)
		}
		if err := r.app.startSession(ctx, threadID); err != nil {
			return fmt.Errorf("workflow runner: restart takeover session without schema: %w", err)
		}
	}
	return nil
}

// takeoverElement resolves the thread to the phase attempt or fan-out unit it
// belongs to, and refuses one that is not actually under human control.
//
// The two shapes are checked against different state because they *are*
// different state: taking over a phase parks the whole item, while taking over
// one unit leaves the item running until its siblings rest. Checking the item's
// state for a unit would refuse exactly the case unit takeover exists for.
func (r *workflowAppRunner) takeoverElement(
	item store.WorkItem, workflow def.Workflow, threadID string,
) (workflowTakeoverElement, error) {
	phases, err := r.app.store.ListWorkItemPhases(item.ID)
	if err != nil {
		return workflowTakeoverElement{}, fmt.Errorf("workflow runner: list takeover phases: %w", err)
	}
	for index := len(phases) - 1; index >= 0; index-- {
		if phases[index].ThreadID != threadID {
			continue
		}
		if item.State != string(engine.StateNeedsHuman) || item.Reason != string(engine.ReasonTakenOver) {
			return workflowTakeoverElement{}, fmt.Errorf(
				"workflow runner: item %s is %s(%s), want needs-human(taken-over)",
				item.ID, item.State, item.Reason,
			)
		}
		phase, ok := findWorkflowPhase(workflow, phases[index].PhaseID)
		if !ok {
			return workflowTakeoverElement{}, fmt.Errorf(
				"workflow runner: phase %s is absent from item %s snapshot", phases[index].PhaseID, item.ID,
			)
		}
		return workflowTakeoverElement{
			description: fmt.Sprintf("phase %q", phase.ID),
			provider:    phase.Provider, contract: def.PhaseEnvelope(phase),
		}, nil
	}
	row, found, err := r.app.store.GetWorkItemUnitByThread(threadID)
	if err != nil {
		return workflowTakeoverElement{}, fmt.Errorf("workflow runner: resolve takeover unit: %w", err)
	}
	if !found || row.ItemID != item.ID {
		return workflowTakeoverElement{}, fmt.Errorf("workflow runner: thread %s is not attached to item %s", threadID, item.ID)
	}
	if row.Status != store.WorkItemUnitTakenOver {
		return workflowTakeoverElement{}, fmt.Errorf(
			"workflow runner: unit %q of item %s is %s, want %s",
			row.UnitID, item.ID, row.Status, store.WorkItemUnitTakenOver,
		)
	}
	phase, ok := findWorkflowPhase(workflow, row.PhaseID)
	if !ok {
		return workflowTakeoverElement{}, fmt.Errorf(
			"workflow runner: phase %s is absent from item %s snapshot", row.PhaseID, item.ID,
		)
	}
	unit, ok := def.UnitDefinition(phase, row.UnitID, row.Kind == store.WorkItemUnitKindJoin)
	if !ok {
		return workflowTakeoverElement{}, fmt.Errorf(
			"workflow runner: unit %q is absent from phase %q of item %s snapshot", row.UnitID, row.PhaseID, item.ID,
		)
	}
	element := workflowTakeoverElement{
		description: fmt.Sprintf("unit %q of phase %q", unit.ID, phase.ID),
		provider:    unit.Provider, contract: def.UnitEnvelope(unit),
	}
	if row.Kind == store.WorkItemUnitKindJoin {
		// A join answers the phase's contract, so a finalize turn on its thread
		// has to be held to the phase's schema, not to an empty unit one.
		element.contract = def.PhaseEnvelope(phase)
	}
	return element, nil
}

func findWorkflowPhase(workflow def.Workflow, phaseID string) (def.Phase, bool) {
	for _, candidate := range workflow.Phases {
		if candidate.ID == phaseID {
			return candidate, true
		}
	}
	return def.Phase{}, false
}

func (r *workflowAppRunner) beginTakeoverTransition(ctx context.Context, itemID, threadID string) error {
	if err := r.registerTakeover(ctx, itemID, threadID); err != nil {
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

// clearTakeoverThread drops one thread's steering registration. Unit recovery
// uses it: retrying or dropping a taken-over unit ends that thread's role in the
// run, while the item's other takeovers (a sibling unit, the phase) stay live.
func (r *workflowAppRunner) clearTakeoverThread(threadID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	takeover, ok := r.takeovers[threadID]
	if !ok {
		return
	}
	delete(r.takeovers, threadID)
	if r.workItems[threadID] == takeover.itemID {
		delete(r.workItems, threadID)
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
