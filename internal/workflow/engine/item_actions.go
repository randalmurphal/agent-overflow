package engine

import "fmt"

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
		ItemID: itemID, From: StateQueued, To: StateCancelled, Reason: ReasonInterrupted,
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
		ItemID: itemID, From: StateDone, To: StateNeedsHuman, Reason: ReasonDisposition,
	})
	return nil
}
