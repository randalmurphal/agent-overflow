package triage

import (
	"strings"

	"agent-overflow/internal/provider"
)

// LiveStateSnapshot is triage's in-memory live projection for one thread.
// It is not persisted history; App converts it to transport DTOs for
// refresh/reconnect hydration.
//
// Everything here dies with the provider session. The todo list deliberately
// does NOT live here: it is durable thread state (threads.live_todo,
// migration v65) that GetThreadLiveState reads straight from the store.
type LiveStateSnapshot struct {
	ActiveTurn             *ActiveTurnSnapshot
	QueueItems             []QueuedFlushItem
	FlushedItems           []PendingFlushItemSnapshot
	Interactive            provider.PendingInteractiveRequests
	EffectiveModel         string
	EffectiveModelRevision uint64
	// CompactingSinceUnixMs is the open compacting window's start (epoch
	// ms), or 0 when the provider is not compacting this thread's
	// context. Snapshot-carried because the window can span minutes of
	// total wire silence — a reconnect inside it has no upcoming frame
	// to re-learn the state from. See compaction_status.go.
	CompactingSinceUnixMs int64
}

// LiveStateSnapshotForThread copies all frontend-visible live state for one
// thread under a single router lock so callers cannot observe a mix of
// pre- and post-cleanup state.
func (r *Router) LiveStateSnapshotForThread(threadID string) LiveStateSnapshot {
	snapshot := LiveStateSnapshot{
		Interactive: provider.PendingInteractiveRequests{
			Approvals:  []provider.ApprovalRequest{},
			UserInputs: []provider.UserInputRequest{},
		},
	}
	threadID = strings.TrimSpace(threadID)
	if r == nil || threadID == "" {
		return snapshot
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if id := r.identityIfPresent(threadID); id != nil {
		snapshot.EffectiveModelRevision = id.effectiveModelRevision
	}
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return snapshot
	}

	if st.currentRoundOpen {
		activeCopy := st.currentRound
		snapshot.ActiveTurn = &activeCopy
	}

	snapshot.EffectiveModel = st.effectiveModel
	snapshot.CompactingSinceUnixMs = st.compactingSince

	if queue := st.queuedFlushItems; len(queue) > 0 {
		snapshot.QueueItems = make([]QueuedFlushItem, len(queue))
		copy(snapshot.QueueItems, queue)
	}

	for _, pending := range st.pendingSends {
		if pending.DeferredItem == nil || !r.sniffFlushShape(threadID, &pending, sendShapeSiteLiveSnapshot) {
			continue
		}
		queueItemID := pending.QueueItemID
		if queueItemID == "" {
			queueItemID = pending.AOItemID
		}
		snapshot.FlushedItems = append(snapshot.FlushedItems, PendingFlushItemSnapshot{
			QueueItemID: queueItemID,
			UserItemID:  pending.AOItemID,
			Message:     pending.DeferredItem.Summary,
		})
	}

	for _, requestID := range st.pendingApprovalOrder {
		pending, ok := st.pendingApprovals[requestID]
		if !ok {
			continue
		}
		snapshot.Interactive.Approvals = append(snapshot.Interactive.Approvals, pending.Request)
	}

	for _, requestID := range st.pendingUserInputOrder {
		request, ok := st.pendingUserInputs[requestID]
		if !ok {
			continue
		}
		snapshot.Interactive.UserInputs = append(snapshot.Interactive.UserInputs, request)
	}

	return snapshot
}
