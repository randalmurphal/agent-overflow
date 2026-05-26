package triage

import (
	"log"
	"time"

	"agent-overflow/internal/store"
)

// pending_send.go owns the pending-send correlation state used by the
// triage router. The send path registers an entry when an AO-initiated
// user message is dispatched to a provider; the matching wire
// EventUserText pops the FIFO head once Phase E lands the consumer
// path. handleTurnStart also peeks the FIFO head (non-destructively)
// to recover the dispatcher-stamped TurnIndex when the wire init
// carries no turn index (Claude system.init). wireOnlyUserTextSeen
// dedupes replay-or-self-prompt envelopes that don't correspond to
// any pending AO send.
//
// All state lives on Router (router.go). The methods here use r.mu —
// the existing Router mutex — and never introduce a separate lock; the
// pending-send maps share the same critical section as every other
// per-thread correlation map so a CleanupThread sweep is consistent
// across all of them.

// pendingSend records an AO-initiated user message awaiting wire
// confirmation. FIFO per thread; consumed when the matching wire
// EventUserText arrives. Bounded by user attention (typically 0-1
// entries per thread); swept at CleanupThread as a safety net.
//
// Position contract for queued sends (DeferredItem != nil):
// `item_index` is NOT captured here — persistDeferredUserText calls
// the standard MAX+1 path at echo time so the queued message lands
// AFTER any rows the model emitted between dispatch and echo.
// Capturing the index at dispatch was the queued-message ordering
// bug: streaming rows landed at MAX+1 in the captured slot, then
// InsertItemAtIndex shifted them down and placed the queued message
// ABOVE content that arrived first. See handle_user_text_test.go's
// TestHandleUserText_DeferredFlush_LandsAfterContentThatArrivedFirst.
//
// TurnIndex mirrors DeferredItem.TurnIndex when DeferredItem is set
// (the dispatcher writes the same value to both). It is also
// populated for direct sends (DeferredItem == nil) so the FIFO
// carries the dispatch-decided turn without forcing every consumer
// to crack open the item.
//
// Load-bearing for handleTurnStart: when the wire init carries no
// turn index (Claude system.init), resolveTurnIndexOnStart peeks the
// FIFO head and reads this field as the authoritative answer.
// Producers MUST stamp it before registering. The fallback in
// persistDeferredUserText (`item.TurnIndex == 0 && pending.TurnIndex
// != 0`) remains defensive against an item-level zero — it is not
// the contract that keeps queue-dispatched turn rows from colliding
// with the previous turn's id-allocating counters.
type pendingSend struct {
	AOItemID     string // "user:<turnIndex>"
	QueueItemID  string
	TurnIndex    int
	EnqueuedAt   int64
	DeferredItem *store.Item
}

type PendingFlushItemSnapshot struct {
	QueueItemID string
	UserItemID  string
	Message     string
}

// PendingSendSnapshot is the test-visible projection of a pendingSend entry.
type PendingSendSnapshot struct {
	AOItemID    string
	TurnIndex   int
	HasDeferred bool
}

// PeekPendingSendHeadForTest returns a snapshot of the FIFO head for threadID.
// Exported for integration tests in the main package that verify the turn-index
// split between persist and response turns.
func (r *Router) PeekPendingSendHeadForTest(threadID string) (PendingSendSnapshot, bool) {
	head, ok := r.peekPendingSendHead(threadID)
	if !ok {
		return PendingSendSnapshot{}, false
	}
	return PendingSendSnapshot{
		AOItemID:    head.AOItemID,
		TurnIndex:   head.TurnIndex,
		HasDeferred: head.DeferredItem != nil,
	}, true
}

// RegisterPendingSend appends a new entry to the per-thread FIFO. The
// send path calls this synchronously so the queue mirrors user-typed
// order. EnqueuedAt is stamped from time.Now here — Phase E uses it
// for diagnostics on stranded entries and the wall clock at the
// register call is the natural reference.
func (r *Router) RegisterPendingSend(threadID, aoItemID string, turnIndex int) {
	r.registerPendingSend(threadID, aoItemID, turnIndex, "", nil)
}

// RegisterPendingFlushSend registers a deferred user_text row whose
// persistence is gated on the wire echo. The row's item_index is
// recomputed at echo time (persistDeferredUserText) so the queued
// message lands after content the model emitted between dispatch and
// echo — see the pendingSend doc comment.
func (r *Router) RegisterPendingFlushSend(threadID, queueItemID string, item store.Item) {
	r.registerPendingSend(threadID, item.ID, item.TurnIndex, queueItemID, &item)
}

func (r *Router) registerPendingSend(threadID, aoItemID string, turnIndex int, queueItemID string, deferredItem *store.Item) {
	if threadID == "" || aoItemID == "" {
		return
	}
	r.mu.Lock()
	r.pendingByThread[threadID] = append(r.pendingByThread[threadID], pendingSend{
		AOItemID:     aoItemID,
		QueueItemID:  queueItemID,
		TurnIndex:    turnIndex,
		EnqueuedAt:   time.Now().UnixMilli(),
		DeferredItem: deferredItem,
	})
	r.mu.Unlock()
}

// HasPendingSendForThread reports whether the FIFO holds at least one
// entry for threadID. Used by handleInit's Phase F gate to decide
// whether an incoming wire init corresponds to an AO send still
// awaiting confirmation. Read-only — does not mutate state. Exported
// so the send-failure cleanup path in app_send.go (and tests in
// package main) can verify the marker is in the expected state.
func (r *Router) HasPendingSendForThread(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pendingByThread[threadID]) > 0
}

// peekPendingSendHead returns the FIFO head for threadID without
// popping it. Used by resolveTurnIndexOnStart to recover the
// dispatcher-stamped TurnIndex when the wire init carries no turn
// index (Claude system.init). The pop stays owned by handleUserText
// so this read-only peek can't strand the marker before the matching
// wire user_text echo arrives. Returns (zero, false) when no entry
// exists.
func (r *Router) peekPendingSendHead(threadID string) (pendingSend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return pendingSend{}, false
	}
	return queue[0], true
}

// consumePendingSendHead pops and returns the FIFO head for threadID.
// Returns (zero, false) when no entry exists. Pure FIFO — Phase E
// pairs this with the wire EventUserText that triggers it.
func (r *Router) consumePendingSendHead(threadID string) (pendingSend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return pendingSend{}, false
	}
	head := queue[0]
	rest := queue[1:]
	if len(rest) == 0 {
		delete(r.pendingByThread, threadID)
	} else {
		// Copy into a fresh slice so we don't pin the previous
		// backing array (which still references the popped head)
		// in the map. Pending-send queues are tiny, so the
		// allocation is cheap and the GC behavior is obvious.
		next := make([]pendingSend, len(rest))
		copy(next, rest)
		r.pendingByThread[threadID] = next
	}
	return head, true
}

// ClearPendingSendForFailure removes a single matching entry from the
// FIFO. Distinct from consumePendingSendHead because failure recovery
// is not necessarily head-of-queue: a later send may be the one that
// failed (e.g. a provider write returned an error after queueing a
// follow-up). Preserves relative order of surviving entries. Exported
// because the send path in `app_send.go` (package main) calls this
// when sendToProvider fails — without it, the marker stays live and a
// later wire init would mis-route the next AO send onto a dead turn.
func (r *Router) ClearPendingSendForFailure(threadID, aoItemID string) {
	if threadID == "" || aoItemID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return
	}
	for i, entry := range queue {
		if entry.AOItemID != aoItemID {
			continue
		}
		nextLen := len(queue) - 1
		if nextLen == 0 {
			delete(r.pendingByThread, threadID)
		} else {
			next := make([]pendingSend, 0, nextLen)
			next = append(next, queue[:i]...)
			next = append(next, queue[i+1:]...)
			r.pendingByThread[threadID] = next
		}
		return
	}
}

// clearPendingSendsForThread sweeps every entry for threadID. Called
// by CleanupThread on session teardown. Acquires r.mu — for the
// CleanupThread codepath that already holds r.mu, see
// clearPendingSendsLocked.
func (r *Router) clearPendingSendsForThread(threadID string) {
	r.mu.Lock()
	r.clearPendingSendsLocked(threadID)
	r.mu.Unlock()
}

// clearPendingSendsLocked is the no-lock variant of
// clearPendingSendsForThread. Caller MUST hold r.mu. Used by
// CleanupThread to keep the entire teardown inside one critical
// section so a concurrent Handle observes a consistent snapshot.
func (r *Router) clearPendingSendsLocked(threadID string) {
	delete(r.pendingByThread, threadID)
}

// MaxPendingSendTurnIndex returns the highest TurnIndex across all
// pending send entries for threadID. Returns (0, false) when no entries
// exist. Used by the flush dispatch turn allocator to avoid assigning
// the same turn index to two messages queued during the same active
// turn — deferred items don't land in the items/turns tables until
// echo, so store.LastTurnIndex alone can't see them.
func (r *Router) MaxPendingSendTurnIndex(threadID string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pendingByThread[threadID]
	if !ok || len(pending) == 0 {
		return 0, false
	}
	maxTurn := pending[0].TurnIndex
	for _, p := range pending[1:] {
		if p.TurnIndex > maxTurn {
			maxTurn = p.TurnIndex
		}
	}
	return maxTurn, true
}

// markWireOnlyUserTextSeen records a wire EventUserText whose
// providerItemID has no pending AO send to match. Returns true on the
// first sighting (caller may persist a "wire-only" timeline row),
// false on duplicates so a session-resume replay doesn't double-write.
func (r *Router) markWireOnlyUserTextSeen(threadID, providerItemID string) bool {
	if threadID == "" || providerItemID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	seen, ok := r.wireOnlyUserTextSeen[threadID]
	if !ok {
		seen = make(map[string]struct{})
		r.wireOnlyUserTextSeen[threadID] = seen
	}
	if _, dup := seen[providerItemID]; dup {
		return false
	}
	seen[providerItemID] = struct{}{}
	return true
}

// clearWireOnlyUserTextForThread sweeps the dedup set for threadID.
// Called by CleanupThread on session teardown. Acquires r.mu — for
// the CleanupThread codepath that already holds r.mu, see
// clearWireOnlyUserTextLocked.
func (r *Router) clearWireOnlyUserTextForThread(threadID string) {
	r.mu.Lock()
	r.clearWireOnlyUserTextLocked(threadID)
	r.mu.Unlock()
}

// clearWireOnlyUserTextLocked is the no-lock variant of
// clearWireOnlyUserTextForThread. Caller MUST hold r.mu.
func (r *Router) clearWireOnlyUserTextLocked(threadID string) {
	delete(r.wireOnlyUserTextSeen, threadID)
}

// EagerPersistedFlush describes a deferred flush send that was eagerly
// persisted during interrupt. The app layer uses it for checkpoint
// capture and (for Codex) re-send.
type EagerPersistedFlush struct {
	UserItemID string
	TurnIndex  int
	Content    string
	Meta       string
}

// EagerPersistDeferredFlushSends immediately persists all deferred
// pending flush sends for threadID into the timeline. Called on user
// interrupt so queued messages become visible without waiting for the
// provider echo.
//
// Under lock: snapshots deferred items and nils DeferredItem on each
// pending send entry. Nilling ensures the echo path routes to
// attachProviderItemIDToUserRow (stamp-only) instead of
// persistDeferredUserText (re-persist).
//
// Each item is persisted via persistItem (UpsertItem + emit) so the
// frontend receives a provider:item_event upsert immediately.
func (r *Router) EagerPersistDeferredFlushSends(threadID string) []EagerPersistedFlush {
	if threadID == "" || r.store == nil {
		return nil
	}

	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	var snapshots []store.Item
	for i := range pending {
		if pending[i].DeferredItem != nil && pending[i].QueueItemID != "" {
			snapshots = append(snapshots, *pending[i].DeferredItem)
			pending[i].DeferredItem = nil
		}
	}
	r.mu.Unlock()

	if len(snapshots) == 0 {
		return nil
	}

	result := make([]EagerPersistedFlush, 0, len(snapshots))
	for _, item := range snapshots {
		if err := r.persistItem(item, nil); err != nil {
			log.Printf("triage: eager persist deferred flush %s/%s: %v", item.ThreadID, item.ID, err)
			continue
		}
		result = append(result, EagerPersistedFlush{
			UserItemID: item.ID,
			TurnIndex:  item.TurnIndex,
			Content:    item.Summary,
			Meta:       item.Meta,
		})
	}
	return result
}

// ClearPendingSendsByItemIDs removes pending send entries whose
// AOItemID matches one of the provided ids. Used by the Codex
// interrupt path to remove stranded entries whose echo will never
// arrive (Codex discards steered pending_input on turn/interrupt).
func (r *Router) ClearPendingSendsByItemIDs(threadID string, ids []string) {
	if threadID == "" || len(ids) == 0 {
		return
	}
	remove := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return
	}
	filtered := make([]pendingSend, 0, len(queue))
	for _, entry := range queue {
		if _, drop := remove[entry.AOItemID]; !drop {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		delete(r.pendingByThread, threadID)
	} else {
		r.pendingByThread[threadID] = filtered
	}
}
