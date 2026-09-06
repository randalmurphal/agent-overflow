package triage

import "time"

// AnyUnfinishedWork is the host-wide admission check. Queue, echo, turn and
// streaming-settlement ownership are read together so a handoff cannot look
// idle between two individually accurate queries. Wakeups use the same grace
// as the session reaper; a stale scheduled timestamp cannot block forever.
func (r *Router) AnyUnfinishedWork(wakeupGrace time.Duration) bool {
	if r == nil {
		return false
	}
	now := time.Now().UnixMilli()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.threads {
		if st == nil {
			continue
		}
		if st.openTurnSet || st.currentRoundOpen || st.streamingItemCount > 0 ||
			len(st.pendingSends) > 0 || len(st.queuedFlushItems) > 0 ||
			len(st.pendingApprovalOrder) > 0 || len(st.pendingUserInputOrder) > 0 ||
			(st.pendingWakeupSet && now < st.pendingWakeupAt+wakeupGrace.Milliseconds()) {
			return true
		}
	}
	// A dispatcher may hold a batch after its thread state was swept.
	r.identitiesMu.Lock()
	defer r.identitiesMu.Unlock()
	for _, id := range r.identities {
		if id.claimedFlushItems > 0 || id.pendingEchoes > 0 {
			return true
		}
	}
	return false
}
