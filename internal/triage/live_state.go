package triage

import (
	"strings"
	"time"

	"agent-overflow/internal/provider"
)

// LiveStateSnapshot is triage's in-memory live projection for one thread.
// It is not persisted history; App converts it to transport DTOs for
// refresh/reconnect hydration.
type LiveStateSnapshot struct {
	ActiveTurn             *ActiveTurnSnapshot
	QueueItems             []QueuedFlushItem
	FlushedItems           []PendingFlushItemSnapshot
	Interactive            provider.PendingInteractiveRequests
	Todo                   *LiveTodoSnapshot
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

	if active, ok := r.currentRoundByThread[threadID]; ok {
		activeCopy := active
		snapshot.ActiveTurn = &activeCopy
	}

	snapshot.EffectiveModel = r.effectiveModelByThread[threadID]
	snapshot.EffectiveModelRevision = r.effectiveModelRevisions[threadID]
	snapshot.CompactingSinceUnixMs = r.compactingSinceByThread[threadID]

	if queue := r.queuedFlushItems[threadID]; len(queue) > 0 {
		snapshot.QueueItems = make([]QueuedFlushItem, len(queue))
		copy(snapshot.QueueItems, queue)
	}

	for _, pending := range r.pendingByThread[threadID] {
		if pending.DeferredItem == nil || !strings.Contains(pending.AOItemID, ":flush:") {
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

	for _, requestID := range r.pendingApprovalOrder[threadID] {
		key := approvalStateKey(threadID, requestID)
		pending, ok := r.pendingApprovals[key]
		if !ok {
			continue
		}
		snapshot.Interactive.Approvals = append(snapshot.Interactive.Approvals, pending.Request)
	}

	for _, requestID := range r.pendingUserInputOrder[threadID] {
		key := approvalStateKey(threadID, requestID)
		request, ok := r.pendingUserInputs[key]
		if !ok {
			continue
		}
		snapshot.Interactive.UserInputs = append(snapshot.Interactive.UserInputs, request)
	}

	snapshot.Todo = r.liveTodoSnapshotLocked(threadID, time.Now().UnixMilli())

	return snapshot
}
