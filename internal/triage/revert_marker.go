package triage

// revert_marker.go owns the per-thread "next turn-completed should be
// flagged as a revert" marker used by the App-layer revert-on-interrupt
// path. The marker is set BEFORE the session is torn down so that the
// CleanupThread-synthesized truncated turn-complete carries the flag
// through to the wire payload — letting the frontend distinguish
// "user pressed Stop and we kept the message" (paint the Interrupted
// pill) from "user pressed Stop and the message is gone" (no pill).
//
// State lives on Router (router.go). All accesses share r.mu with
// every other per-thread correlation map so CleanupThread sees a
// consistent snapshot. Cleanup paths in clearOpenTurn and
// CleanupThread sweep stale flags as a safety net; the normal flow
// is one set + one read-and-clear per revert.

// MarkTurnReverted flags the thread so the next provider:turn_completed
// emission for any in-flight round carries RevertedUserMessage=true.
// Read-and-clear via consumeRevertedTurn happens inside
// buildRoundCompletedEvent. The marker is one-shot per set.
func (r *Router) MarkTurnReverted(threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	r.revertedTurns[threadID] = struct{}{}
	r.mu.Unlock()
}

// ClearTurnReverted drops a pending revert marker when the App aborts a
// revert after marking but before any turn-completed emission consumes it.
func (r *Router) ClearTurnReverted(threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	delete(r.revertedTurns, threadID)
	r.mu.Unlock()
}

// consumeRevertedTurn returns true and clears the marker when the
// thread is flagged for revert. Safe to call when no marker is set
// (returns false). Called by buildRoundCompletedEvent so the wire
// payload reflects the revert; subsequent rounds on the same thread
// emit cleanly without the flag.
func (r *Router) consumeRevertedTurn(threadID string) bool {
	if threadID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.revertedTurns[threadID]; !ok {
		return false
	}
	delete(r.revertedTurns, threadID)
	return true
}
