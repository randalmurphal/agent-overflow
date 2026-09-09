package triage

import (
	"sort"
	"strings"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// LiveStateSnapshot is triage's in-memory live projection for one thread.
// It is not persisted history; App converts it to transport DTOs for
// refresh/reconnect hydration.
//
// Everything here dies with the provider session. The todo list deliberately
// does NOT live here: it is durable thread state (threads.live_todo,
// migration v65) that GetThreadLiveState reads straight from the store.
type LiveStateSnapshot struct {
	ActiveTurn   *ActiveTurnSnapshot
	QueueItems   []QueuedFlushItem
	FlushedItems []PendingFlushItemSnapshot
	// DeferredItems are the timeline rows of every pending send whose
	// row is NOT in SQLite yet (a pending send persists on its wire
	// echo — see AGENTS.md § Pending sends). A frontend reconciling
	// against a SQLite slice merges these in, because the slice is
	// structurally blind to them: without this, a transport-gap refresh
	// mid-send drops the user's own message from the timeline until the
	// echo lands (incident 2026-08-29). FIFO order, all send shapes.
	DeferredItems          []store.Item
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
		if pending.DeferredItem == nil {
			continue
		}
		snapshot.DeferredItems = append(snapshot.DeferredItems, *pending.DeferredItem)
		if pending.Shape != sendShapeFlush {
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
			UserMeta:    pending.DeferredItem.Meta,
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

// ThreadLiveActivity is the sidebar-grade live state of one thread: what a
// client connected for the whole session would have learned from
// provider:turn_started, provider:approval, provider:user_input and
// provider:compacting. Ids and timestamps only; the requests' prose stays
// on LiveStateSnapshotForThread, whose RPC carries the higher scope.
type ThreadLiveActivity struct {
	ThreadID              string
	ActiveTurn            *ActiveTurnSnapshot
	ApprovalRequestIDs    []string
	UserInputRequestIDs   []string
	CompactingSinceUnixMs int64
}

// LiveActivitySnapshot lists every thread with live activity, copied under
// one router lock and sorted by thread id. Threads with nothing open are
// omitted: a reader treats the list as authoritative for this backend and
// clears what it does not name.
func (r *Router) LiveActivitySnapshot() []ThreadLiveActivity {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ThreadLiveActivity
	for threadID, st := range r.threads {
		if st == nil {
			continue
		}
		entry := ThreadLiveActivity{ThreadID: threadID, CompactingSinceUnixMs: st.compactingSince}
		if st.currentRoundOpen {
			round := st.currentRound
			entry.ActiveTurn = &round
		}
		for _, requestID := range st.pendingApprovalOrder {
			if _, ok := st.pendingApprovals[requestID]; ok {
				entry.ApprovalRequestIDs = append(entry.ApprovalRequestIDs, requestID)
			}
		}
		for _, requestID := range st.pendingUserInputOrder {
			if _, ok := st.pendingUserInputs[requestID]; ok {
				entry.UserInputRequestIDs = append(entry.UserInputRequestIDs, requestID)
			}
		}
		if entry.ActiveTurn == nil && len(entry.ApprovalRequestIDs) == 0 &&
			len(entry.UserInputRequestIDs) == 0 && entry.CompactingSinceUnixMs == 0 {
			continue
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ThreadID < out[j].ThreadID })
	return out
}
