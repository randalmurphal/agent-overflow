package triage

import "strings"

// SendsPending reports whether threadID holds a user message triage has
// accepted but the provider has not yet echoed: a queued or claimed flush
// item, or a flush-shaped pending send whose row is still the composer's
// marker (the FlushedItems rule in LiveStateSnapshotForThread). Direct
// sends are not counted; their sender shows its own optimistic state and
// every other client learns of them from the round they open.
//
// The App combines this with the messages its flush dispatcher holds and
// publishes the answer on provider:sends_pending, so a client with no pane
// on the thread knows the same thing a pane's send queue shows.
func (r *Router) SendsPending(threadID string) bool {
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendsPendingLocked(threadID)
}

func (r *Router) sendsPendingLocked(threadID string) bool {
	if id := r.identityIfPresent(threadID); id != nil && len(id.claimedFlushItems) > 0 {
		return true
	}
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	if len(st.queuedFlushItems) > 0 {
		return true
	}
	for _, pending := range st.pendingSends {
		if pending.Shape == sendShapeFlush && !pending.AnchoredAtInterrupt &&
			(pending.DeferredItem != nil || pending.QuietItem != nil) {
			return true
		}
	}
	return false
}

// SetSendsPendingObserver wires the callback told that threadID's
// SendsPending answer may have changed. It runs under r.mu, after the
// change, so it must not block or call back into the router; reading
// SendsPending once the lock is free observes the change.
func (r *Router) SetSendsPendingObserver(fn func(threadID string)) {
	r.mu.Lock()
	r.sendsPendingChanged = fn
	r.mu.Unlock()
}

// noteSendsPendingLocked reports a mutation of an input SendsPending
// reads: the flush queue, the claimed batch, or the pending-send list.
// Every writer of those fields calls it while still holding r.mu.
func (r *Router) noteSendsPendingLocked(threadID string) {
	if r.sendsPendingChanged != nil {
		r.sendsPendingChanged(threadID)
	}
}
