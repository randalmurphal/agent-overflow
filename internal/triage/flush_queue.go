package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"agent-overflow/internal/store"
)

// flush_queue.go owns the per-thread "queued user message awaiting a
// provider boundary" state. The Composer surfaces these items as
// retractable rows under the working indicator; the queue drains when
// triage observes that the provider has no running top-level tool or
// live background task left for the thread.
//
// Why a backend-owned queue rather than a frontend-owned one:
//
//   1. Core Principle 1: triage classifies wire events and the
//      completion-boundary trigger IS a wire-event-driven decision. Putting
//      it in the frontend would re-derive provider state in JS, which
//      is exactly the in-memory read model we forbid.
//   2. Race window correctness: AO writes the user message to the
//      provider's stdin (Claude) or via a JSON-RPC call (Codex) at
//      the moment the trigger fires. Doing this from the frontend
//      adds an event-emission roundtrip during a window where the
//      provider is already moving between tool_use blocks; the in-process
//      backend can fire on the same goroutine that observed the
//      lifecycle boundary, eliminating the JS-bus latency from the seam.
//   3. Remote-client survivability: an `agent-overflow --connect`
//      client that attaches mid-round still observes the same flush
//      behaviour because the queue lives in the backend, not the
//      attached UI.
//
// All state lives on Router (router.go). The methods here use r.mu —
// the existing Router mutex — and never introduce a separate lock; the
// flush-queue maps share the same critical section as every other
// per-thread correlation map so a CleanupThread sweep is consistent
// across all of them.

// QueuedFlushItem is one user message awaiting provider-boundary flush.
// Lives only in router memory — never persisted to SQLite — and is
// drained when the boundary trigger fires or cleared on session teardown.
//
// The Payload is opaque to triage: the app layer (app_flush_queue.go)
// owns the wire shape (attachments, source-plan refs, revision
// metadata) and decodes it when the dispatcher fires. Keeping the
// payload opaque preserves the responsibility boundary — triage
// classifies events and dispatches a batch; everything else stays in
// the app layer where attachment / plan types already live.
type QueuedFlushItem struct {
	// ID is the frontend-allocated identifier (e.g. "queue:<uuid>")
	// used by the UP-arrow retract handler to drop a specific entry.
	ID string
	// Message is the user-typed text. Held at the top level so a
	// remote-client snapshot RPC can render the queue overlay without
	// having to deserialise Payload first.
	Message string
	// Payload is the app-layer opaque payload that the dispatcher
	// decodes when firing — typically a JSON encoding of attachments,
	// source-plan refs, and revision metadata.
	Payload json.RawMessage
	// EnqueuedAt is the wall clock at register time. Diagnostic only:
	// the dispatcher does not key off it.
	EnqueuedAt int64
}

// FlushDispatcher is the app-layer callback invoked when triage drains
// queued user messages, either at a safe provider boundary or because
// the user explicitly forced delivery with Send Now.
// Triage hands ownership of the consumed batch over; the dispatcher is
// responsible for allocating AO item ids, persisting user rows,
// registering pending-send markers, and writing to the provider.
//
// Invoked after r.mu is released so the dispatcher can call back into the
// router (RegisterPendingSend, PersistItem, etc) without re-entrancy. The
// callback must return quickly; provider writes belong behind the app-layer
// async/FIFO dispatcher. FlushDispatchMode carries the distinction
// between same-round boundary drains and fresh-turn immediate drains.
type FlushDispatchMode string

const (
	FlushDispatchModeBoundary  FlushDispatchMode = "boundary"
	FlushDispatchModeImmediate FlushDispatchMode = "immediate"
)

type FlushDispatcher func(threadID string, items []QueuedFlushItem, mode FlushDispatchMode)

// SetFlushDispatcher wires the app-layer callback. Nil disables
// dispatch — useful for tests that exercise registration-only paths
// or for the brief window between Router construction and app-layer
// wiring at startup.
func (r *Router) SetFlushDispatcher(fn FlushDispatcher) {
	r.mu.Lock()
	r.dispatchFlush = fn
	r.mu.Unlock()
}

// SetDeferredUserTextConfirmedHook wires the app-layer callback that runs
// after a deferred queued user_text row is persisted from provider echo.
// Nil disables the callback. The hook runs outside r.mu.
func (r *Router) SetDeferredUserTextConfirmedHook(fn func(threadID string, item store.Item)) {
	r.mu.Lock()
	r.deferredUserTextConfirmed = fn
	r.mu.Unlock()
}

// FlushQueuedItemsNow drains the queue immediately if a dispatcher is wired.
// This is used by explicit user interrupts: once the user has asked to stop
// the current turn and send their queued message, retractability is over and
// AO should write the batch without waiting for the next provider boundary.
func (r *Router) FlushQueuedItemsNow(threadID string) bool {
	return r.tryFlushQueueWithMode(threadID, FlushDispatchModeImmediate)
}

// RegisterQueueItem appends a queued user message to the per-thread
// flush queue. Returns the resolved EnqueuedAt timestamp so callers
// can surface it on the wire (provider:queue_added). No-op when
// threadID or item.ID is empty — both are required for the retract
// path and the trigger fire to dispatch correctly.
//
// EnqueuedAt is stamped from time.Now here when zero so callers don't
// have to keep a clock around; passing a non-zero value preserves it
// (used by tests that need deterministic timestamps).
func (r *Router) RegisterQueueItem(threadID string, item QueuedFlushItem) int64 {
	if threadID == "" || item.ID == "" {
		return 0
	}
	if item.EnqueuedAt == 0 {
		item.EnqueuedAt = time.Now().UnixMilli()
	}
	r.mu.Lock()
	r.queuedFlushItems[threadID] = append(r.queuedFlushItems[threadID], item)
	r.mu.Unlock()
	return item.EnqueuedAt
}

// DropQueueItem removes a single item from the per-thread flush queue
// by id. Returns the dropped item and true on success, or zero/false
// when no entry matches. Used by the per-row × button on the queue
// overlay (when retained for individual drops; today the overlay
// surfaces UP-arrow retract-all, but the primitive stays for
// completeness and remote-client edge cases).
//
// Preserves relative order of the surviving entries — drop is a
// targeted removal, not a head-pop.
func (r *Router) DropQueueItem(threadID, itemID string) (QueuedFlushItem, bool) {
	if threadID == "" || itemID == "" {
		return QueuedFlushItem{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.queuedFlushItems[threadID]
	for i, entry := range queue {
		if entry.ID != itemID {
			continue
		}
		dropped := entry
		queue = append(queue[:i], queue[i+1:]...)
		if len(queue) == 0 {
			delete(r.queuedFlushItems, threadID)
		} else {
			r.queuedFlushItems[threadID] = queue
		}
		return dropped, true
	}
	return QueuedFlushItem{}, false
}

// DropAllQueuedItems removes and returns every item in the per-thread
// flush queue. Used by the UP-arrow "retract all" path — the frontend
// combines the returned items into one composer draft, matching the
// Claude TUI's popAllEditable behaviour.
//
// Returns nil (not an empty slice) when no items are queued so the
// caller can short-circuit the combine-into-draft path with a single
// nil check.
func (r *Router) DropAllQueuedItems(threadID string) []QueuedFlushItem {
	if threadID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items, ok := r.queuedFlushItems[threadID]
	if !ok || len(items) == 0 {
		return nil
	}
	delete(r.queuedFlushItems, threadID)
	out := make([]QueuedFlushItem, len(items))
	copy(out, items)
	return out
}

// HasQueuedFlushItems reports whether the per-thread flush queue
// contains at least one entry. Read-only — callers may use it as a
// short-circuit before the more expensive snapshot path.
func (r *Router) HasQueuedFlushItems(threadID string) bool {
	if threadID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queuedFlushItems[threadID]) > 0
}

// QueuedFlushItemCount returns the number of pending queue entries
// for threadID. Used by the App-level RegisterQueueItem RPC to
// enforce a per-thread length cap before appending — bounded
// router-memory consumption is a security concern (DoS resistance)
// and the cap lives at the App boundary, not in triage.
func (r *Router) QueuedFlushItemCount(threadID string) int {
	if threadID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queuedFlushItems[threadID])
}

func (r *Router) DeferredPendingFlushItemCount(threadID string) int {
	if threadID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, pending := range r.pendingByThread[threadID] {
		if pending.DeferredItem != nil && strings.Contains(pending.AOItemID, ":flush:") {
			count++
		}
	}
	return count
}

func (r *Router) MaxPendingFlushSequence(threadID string, turnIndex int) int {
	if threadID == "" {
		return 0
	}
	prefix := fmt.Sprintf("user:%d:flush:", turnIndex)
	r.mu.Lock()
	defer r.mu.Unlock()
	maxSeq := 0
	for _, pending := range r.pendingByThread[threadID] {
		if pending.DeferredItem == nil || !strings.HasPrefix(pending.AOItemID, prefix) {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimPrefix(pending.AOItemID, prefix))
		if err == nil && seq > maxSeq {
			maxSeq = seq
		}
	}
	return maxSeq
}

// QueuedFlushItems returns a copy of the per-thread flush queue.
// Callers receive a fresh slice they may mutate without affecting
// router state; the underlying QueuedFlushItem values share their
// json.RawMessage backing with the originals (Payload bytes are
// treated as immutable). Returns nil when no items are queued.
//
// Used by the App layer to surface initial queue state on bootstrap
// (a remote `--connect` client that attaches mid-session) and to
// replay queue state to the active webview after a reload.
func (r *Router) QueuedFlushItems(threadID string) []QueuedFlushItem {
	if threadID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	src, ok := r.queuedFlushItems[threadID]
	if !ok || len(src) == 0 {
		return nil
	}
	out := make([]QueuedFlushItem, len(src))
	copy(out, src)
	return out
}

// tryFlushQueue drains the per-thread flush queue when it has items
// and a dispatcher is wired. Idempotent across repeated calls — once
// the batch is consumed the queue is empty and subsequent calls
// no-op until new items are registered.
//
// Returns true when the dispatcher was invoked; false otherwise (no
// items, no dispatcher, empty threadID). The boolean is
// informational — lifecycle handlers don't branch on it.
//
// Dispatcher invocation happens AFTER r.mu is released so callbacks can call
// back into the router without re-entrancy. The callback must return quickly;
// the App-layer dispatcher owns asynchronous provider writes and per-thread
// ordering. The batch is copied out under r.mu so a concurrent CleanupThread
// between unlock and dispatch doesn't observe a partially-cleared queue.
func (r *Router) tryFlushQueue(threadID string) bool {
	return r.tryFlushQueueWithMode(threadID, FlushDispatchModeBoundary)
}

func (r *Router) tryFlushQueueWithMode(threadID string, mode FlushDispatchMode) bool {
	if threadID == "" {
		return false
	}
	r.mu.Lock()
	queue, ok := r.queuedFlushItems[threadID]
	if !ok || len(queue) == 0 {
		r.mu.Unlock()
		return false
	}
	dispatcher := r.dispatchFlush
	if dispatcher == nil {
		r.mu.Unlock()
		return false
	}
	batch := make([]QueuedFlushItem, len(queue))
	copy(batch, queue)
	delete(r.queuedFlushItems, threadID)
	r.mu.Unlock()

	dispatcher(threadID, batch, mode)
	return true
}

func (r *Router) maybeFlushQueueAtBoundary(threadID string) bool {
	if threadID == "" || !r.HasQueuedFlushItems(threadID) {
		return false
	}
	active, err := r.hasQueueBlockingWork(threadID)
	if err != nil {
		// Failing closed keeps the queue retractable instead of racing a
		// message into provider context on incomplete lifecycle knowledge.
		log.Printf("triage: queue boundary check failed for thread %s: %v", threadID, err)
		return false
	}
	if active {
		return false
	}
	return r.tryFlushQueue(threadID)
}

func (r *Router) hasQueueBlockingWork(threadID string) (bool, error) {
	if r.store == nil {
		return false, nil
	}
	if r.hasActiveCodexUnifiedExec(threadID) {
		return true, nil
	}
	active, err := r.store.HasRunningTopLevelForegroundToolCall(threadID)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	active, err = r.store.HasLiveBackgroundToolCall(threadID)
	if err != nil {
		return false, err
	}
	return active, nil
}

func (r *Router) hasActiveCodexUnifiedExec(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.codexBackground[threadID]
	if state == nil {
		return false
	}
	for _, tracker := range state.unifiedExec {
		if tracker != nil && !tracker.completed {
			return true
		}
	}
	return false
}

// clearFlushQueueLocked drops every queued item for threadID. Caller
// MUST hold r.mu. Called by CleanupThread — the queue is
// session-scoped, so a torn-down session must not leak entries into
// a fresh one.
func (r *Router) clearFlushQueueLocked(threadID string) {
	delete(r.queuedFlushItems, threadID)
}
