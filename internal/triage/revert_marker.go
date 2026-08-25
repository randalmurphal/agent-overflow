package triage

// revert_marker.go owns the per-thread "next turn-completed should be
// flagged as a revert" marker used by the App-layer revert-on-interrupt
// path. The marker is set BEFORE the session is torn down so that the
// CleanupThread-synthesized truncated turn-complete carries the flag
// through to the wire payload — letting the frontend distinguish
// "user pressed Stop and we kept the message" (paint the Interrupted
// pill) from "user pressed Stop and the message is gone" (no pill).
//
// State lives on the thread's threadState (thread_state.go). All
// accesses share r.mu with every other per-thread correlation field so
// CleanupThread sees a consistent snapshot. clearOpenTurn clears the
// flag as a safety net and CleanupThread drops the whole state entry;
// the normal flow is one set + one read-and-clear per revert.

// MarkTurnReverted flags the thread so the next provider:turn_completed
// emission for any in-flight round carries RevertedUserMessage=true.
// Read-and-clear via consumeRevertedTurn happens inside
// buildRoundCompletedEvent. The marker is one-shot per set.
func (r *Router) MarkTurnReverted(threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	r.state(threadID).revertedTurn = true
	r.mu.Unlock()
}

// ClearTurnReverted drops a pending revert marker when the App aborts a
// revert after marking but before any turn-completed emission consumes it.
func (r *Router) ClearTurnReverted(threadID string) {
	if threadID == "" {
		return
	}
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil {
		st.revertedTurn = false
	}
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
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.revertedTurn {
		return false
	}
	st.revertedTurn = false
	return true
}
