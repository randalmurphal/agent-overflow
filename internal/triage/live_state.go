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
	// DeferredItems are the timeline rows of NON-FLUSH pending sends whose
	// row is NOT in SQLite yet (a pending send persists on its wire
	// echo — see AGENTS.md § Pending sends). A frontend reconciling
	// against a SQLite slice merges these in, because the slice is
	// structurally blind to them: without this, a transport-gap refresh
	// mid-send drops the user's own message from the timeline until the
	// echo lands (incident 2026-08-29). FIFO order.
	//
	// A flush-shaped send is never here: it is published in FlushedItems
	// and drawn above the composer instead, and one pending send appears
	// in exactly one of the two lists. Merging it into the timeline as
	// well put the same message on screen twice for every refresh taken
	// while a queued send awaited its echo.
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

	// One pending send, one list. The split is the row's user-visible
	// home, not its storage:
	//
	//   - Deferred (DeferredItem): no SQLite row until the echo, so the
	//     message exists only as the composer's pending marker.
	//   - Quiet (QuietItem): the row IS in SQLite, reserving its timeline
	//     position, but it was persisted without a provider:item_event and
	//     is revealed on consumption — so it is still the composer's
	//     marker, not a timeline row, until the echo lands.
	//   - Anchored at an interrupt: the row is persisted AND emitted, so
	//     the user is already reading it in the timeline. No marker, and
	//     no deferred merge either. The claim is only ever set after the
	//     store write succeeds — EagerPersistDeferredFlushSends persists
	//     with an emit and drops DeferredItem in the same locked step,
	//     PromoteQuietFlushSends bumps an already-persisted quiet row and
	//     emits it, and both restore paths clear the claim when their
	//     write fails — so it outranks a retained copy on the same entry,
	//     which a same-id re-registration marked alongside it can leave
	//     behind. Checked FIRST for that reason.
	//   - A flush resend carries neither copy: its row was persisted and
	//     emitted by the interrupt that produced it. No marker.
	for _, pending := range st.pendingSends {
		if pending.Shape == sendShapeFlush {
			if pending.AnchoredAtInterrupt {
				continue
			}
			row := pending.DeferredItem
			if row == nil {
				row = pending.QuietItem
			}
			if row == nil {
				continue
			}
			queueItemID := pending.QueueItemID
			if queueItemID == "" {
				queueItemID = pending.AOItemID
			}
			snapshot.FlushedItems = append(snapshot.FlushedItems, PendingFlushItemSnapshot{
				QueueItemID: queueItemID,
				UserItemID:  pending.AOItemID,
				Message:     row.Summary,
				UserMeta:    row.Meta,
			})
			continue
		}
		if pending.DeferredItem == nil {
			continue
		}
		snapshot.DeferredItems = append(snapshot.DeferredItems, *pending.DeferredItem)
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
