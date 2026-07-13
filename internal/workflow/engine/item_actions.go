package engine

import (
	"fmt"
	"strings"
)

// RemoveQueued cancels a not-yet-started item without provisioning a
// worktree. A start that wins the command-loop race makes this fail with the
// item's current state instead of interrupting live work.
func (e *Engine) RemoveQueued(itemID string) error {
	return e.request(removeQueuedCommand{itemID: itemID})
}

// ParkDisposition moves a completed item into the typed human-attention state
// after a disposition refusal. The engine remains the lifecycle owner even
// though done items are no longer held in its in-memory scheduler map.
func (e *Engine) ParkDisposition(itemID string) error {
	return e.request(parkDispositionCommand{itemID: itemID})
}

// ReenqueueFailed rebuilds a failed item from its frozen snapshot and queues
// the failed phase for another attempt.
func (e *Engine) ReenqueueFailed(itemID string) error {
	return e.request(reenqueueFailedCommand{itemID: itemID})
}

// ResolveDisposition returns a disposition-parked item to done after its
// external disposition and receipt persistence succeed.
func (e *Engine) ResolveDisposition(itemID string) error {
	return e.request(resolveDispositionCommand{itemID: itemID})
}

func (e *Engine) removeQueued(itemID string) error {
	item, ok := e.items[itemID]
	if !ok {
		stored, err := e.store.GetWorkItem(itemID)
		if err != nil {
			return fmt.Errorf("remove queued item %q: %w", itemID, err)
		}
		return fmt.Errorf("remove queued item %q: invalid state %s, want %s", itemID, stored.State, StateQueued)
	}
	if State(item.item.State) != StateQueued {
		return fmt.Errorf("remove queued item %q: invalid state %s, want %s", itemID, item.item.State, StateQueued)
	}
	e.removeQueuedRuntime(itemID)
	endedAt := e.timestamp()
	if err := e.store.UpdateWorkItemState(itemID, string(StateCancelled), string(ReasonInterrupted), endedAt); err != nil {
		e.insertQueued(item)
		return fmt.Errorf("remove queued item %q: %w", itemID, err)
	}
	item.item.State = string(StateCancelled)
	item.item.Reason = string(ReasonInterrupted)
	item.item.EndedAt = endedAt
	delete(e.items, itemID)
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: item.item.ProjectID,
		From: StateQueued, To: StateCancelled, Reason: ReasonInterrupted,
	})
	return nil
}

func (e *Engine) parkDisposition(itemID string) error {
	item, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("park disposition item %q: %w", itemID, err)
	}
	if State(item.State) != StateDone {
		return fmt.Errorf("park disposition item %q: invalid state %s, want %s", itemID, item.State, StateDone)
	}
	endedAt := e.timestamp()
	if err := e.store.UpdateWorkItemState(itemID, string(StateNeedsHuman), string(ReasonDisposition), endedAt); err != nil {
		return fmt.Errorf("park disposition item %q: %w", itemID, err)
	}
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: item.ProjectID,
		From: StateDone, To: StateNeedsHuman, Reason: ReasonDisposition,
	})
	return nil
}

func (e *Engine) reenqueueFailed(itemID string) error {
	stored, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("re-enqueue failed item %q: %w", itemID, err)
	}
	if State(stored.State) != StateFailed {
		return fmt.Errorf("re-enqueue failed item %q: invalid state %s, want %s", itemID, stored.State, StateFailed)
	}
	if len(stored.Disposition) > 0 {
		return fmt.Errorf("re-enqueue failed item %q: item was discarded — record kept; enqueue a new run instead", itemID)
	}
	runtime := &runtimeItem{item: stored}
	if err := decodeSnapshot(stored.Snapshot, &runtime.workflow); err != nil {
		return fmt.Errorf("re-enqueue failed item %q snapshot: %w", itemID, err)
	}
	phases, err := e.store.ListWorkItemPhases(itemID)
	if err != nil {
		return fmt.Errorf("re-enqueue failed item %q phases: %w", itemID, err)
	}
	if len(phases) == 0 {
		return fmt.Errorf("re-enqueue failed item %q: no phase attempts", itemID)
	}
	latest := phases[len(phases)-1]
	if _, ok := findPhase(runtime.workflow, latest.PhaseID); !ok {
		return fmt.Errorf("re-enqueue failed item %q: phase %q is not in the frozen workflow", itemID, latest.PhaseID)
	}
	runtime.phaseID = latest.PhaseID
	runtime.attempt = 0
	runtime.reenqueued = true
	// This guidance seed intentionally has the same process-local durability as
	// the transient process-N queue bound. The diagnosis remains durable in the
	// phase history if the app restarts before the queued item starts.
	runtime.feedback, err = failedAttemptFeedback(Reason(stored.Reason), latest.PhaseID, latest.OutputEnvelope)
	if err != nil {
		return fmt.Errorf("re-enqueue failed item %q output envelope: %w", itemID, err)
	}
	position, err := e.store.NextWorkItemSortPosition(stored.ProjectID)
	if err != nil {
		return fmt.Errorf("re-enqueue failed item %q queue position: %w", itemID, err)
	}
	if err := e.store.ReenqueueFailedWorkItem(itemID, position); err != nil {
		return fmt.Errorf("re-enqueue failed item %q: %w", itemID, err)
	}
	runtime.item.State = string(StateQueued)
	runtime.item.Reason = ""
	runtime.item.EndedAt = 0
	runtime.item.SortPosition = position
	e.items[itemID] = runtime
	e.insertQueued(runtime)
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: stored.ProjectID, From: StateFailed, To: StateQueued,
	})
	return e.schedule()
}

func (e *Engine) resolveDisposition(itemID string) error {
	item, err := e.store.GetWorkItem(itemID)
	if err != nil {
		return fmt.Errorf("resolve disposition item %q: %w", itemID, err)
	}
	if State(item.State) != StateNeedsHuman || Reason(item.Reason) != ReasonDisposition {
		return fmt.Errorf(
			"resolve disposition item %q: invalid state %s(%s), want %s(%s)",
			itemID, item.State, item.Reason, StateNeedsHuman, ReasonDisposition,
		)
	}
	if err := e.store.UpdateWorkItemState(itemID, string(StateDone), "", item.EndedAt); err != nil {
		return fmt.Errorf("resolve disposition item %q: %w", itemID, err)
	}
	e.emitter.Emit("workflow:item-state", StateEvent{
		ItemID: itemID, ProjectID: item.ProjectID, From: StateNeedsHuman, To: StateDone,
	})
	return nil
}

func failedAttemptFeedback(reason Reason, phaseID string, payload []byte) (*Feedback, error) {
	note := string(reason)
	var envelope struct {
		Outputs map[string]any `json:"outputs"`
		Reason  *string        `json:"reason"`
	}
	if len(payload) > 0 {
		if err := decodeJSON(payload, &envelope); err != nil {
			return nil, err
		}
	}
	feedback := &Feedback{Values: make(map[string]any)}
	for _, name := range []string{"diagnosis", "summary"} {
		value, ok := envelope.Outputs[name]
		if !ok {
			continue
		}
		feedback.Values[phaseID+"."+name] = value
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" && note == string(reason) {
			note += ": " + strings.TrimSpace(text)
		}
	}
	if envelope.Reason != nil && note == string(reason) {
		if diagnosis := strings.TrimSpace(*envelope.Reason); diagnosis != "" {
			note += ": " + diagnosis
		}
	}
	feedback.Note = note
	if len(feedback.Values) == 0 {
		feedback.Values = nil
	}
	return feedback, nil
}
