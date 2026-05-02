package triage

import (
	"encoding/json"
	"time"
)

// flush_queue.go owns the per-thread "queued user message awaiting
// first-tool-use flush" state. The Composer surfaces these items as
// retractable rows under the working indicator; the trigger fires from
// handleToolStart on the first non-subagent tool_use of a wire round
// and consumes the entire batch.
//
// Why a backend-owned queue rather than a frontend-owned one:
//
//   1. Core Principle 1: triage classifies wire events and the
//      first-tool-use trigger IS a wire-event-driven decision. Putting
//      it in the frontend would re-derive provider state in JS, which
//      is exactly the in-memory read model we forbid.
//   2. Race window correctness: AO writes the user message to the
//      provider's stdin (Claude) or via a JSON-RPC call (Codex) at
//      the moment the trigger fires. Doing this from the frontend
//      adds an event-emission roundtrip during a window where the
//      provider is already executing tool_use blocks; the in-process
//      backend can fire on the same goroutine that observed the
//      tool_start, eliminating the JS-bus latency from the seam.
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

// QueuedFlushItem is one user message awaiting first-tool-use flush.
// Lives only in router memory — never persisted to SQLite — and is
// drained when the trigger fires (first non-subagent tool_use of the
// round) or cleared on session teardown.
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

// FlushDispatcher is the app-layer callback invoked when the first
// non-subagent tool_use of a wire round fires the flush trigger.
// Triage hands ownership of the consumed batch over; the dispatcher is
// responsible for allocating AO item ids, persisting user rows,
// registering pending-send markers, and writing to the provider.
//
// Invoked synchronously from handleToolStart AFTER r.mu is released so
// the dispatcher can call back into the router (RegisterPendingSend,
// PersistItem, etc) without re-entrancy.
type FlushDispatcher func(threadID string, items []QueuedFlushItem)

// SetFlushDispatcher wires the app-layer callback. Nil disables
// dispatch — useful for tests that exercise registration-only paths
// or for the brief window between Router construction and app-layer
// wiring at startup.
func (r *Router) SetFlushDispatcher(fn FlushDispatcher) {
	r.mu.Lock()
	r.dispatchFlush = fn
	r.mu.Unlock()
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
// no-op until new items are registered. The wire-round id is no
// longer load-bearing for suppression (a per-round marker used to
// short-circuit subsequent calls within the same round, but that
// blocked any user message queued AFTER the first drain from ever
// flushing in the same round); the maybeFireFlushTrigger gate above
// still requires a non-empty roundID so we don't drain into a closed
// round.
//
// Returns true when the dispatcher was invoked; false otherwise (no
// items, no dispatcher, empty threadID). The boolean is
// informational — handleToolStart doesn't branch on it.
//
// Dispatcher invocation happens AFTER r.mu is released so the
// dispatcher can call back into the router (RegisterPendingSend,
// PersistItem) without re-entrancy. The batch is copied out under
// r.mu so a concurrent CleanupThread between unlock and dispatch
// doesn't observe a partially-cleared queue.
func (r *Router) tryFlushQueue(threadID string) bool {
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

	dispatcher(threadID, batch)
	return true
}

// clearFlushQueueLocked drops every queued item for threadID. Caller
// MUST hold r.mu. Called by CleanupThread — the queue is
// session-scoped, so a torn-down session must not leak entries into
// a fresh one.
func (r *Router) clearFlushQueueLocked(threadID string) {
	delete(r.queuedFlushItems, threadID)
}
