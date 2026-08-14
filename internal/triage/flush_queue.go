package triage

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/store"
	"agent-overflow/internal/usermessage"
)

// flush_queue.go owns the per-thread "queued user message awaiting provider
// dispatch" state. The Composer surfaces these items above the chat box until
// provider echo confirms the message is visible to the agent.
//
// Why a backend-owned queue rather than a frontend-owned one:
//
//   1. Core Principle 1: provider-facing dispatch decisions stay in the
//      backend. Putting them in the frontend would re-derive provider state in
//      JS, which is exactly the in-memory read model we forbid.
//   2. Race window correctness: AO writes the user message to the provider's
//      stdin (Claude) or via JSON-RPC (Codex) from the same process that owns
//      provider/session state, so the above-composer pending row and the
//      eventual provider echo share one correlation path.
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

// QueuedFlushItem is one user message awaiting provider dispatch. Lives only
// in router memory — never persisted to SQLite — and is drained when the app
// dispatch worker accepts it or cleared on session teardown. That is why it
// carries a FlushSettlement: "queued" is not "durable", and an app-layer
// record that outlives the process has to wait for either a provider write or
// a successful recovery into the durable composer draft.
//
// The Payload is opaque to triage: the app layer (app_flush_queue.go)
// owns the wire shape (attachments, source-plan refs, revision
// metadata) and decodes it when the dispatcher fires. Keeping the
// payload opaque preserves the responsibility boundary — triage
// classifies events and dispatches a batch; everything else stays in
// the app layer where attachment / plan types already live.
type QueuedFlushItem struct {
	// ID is the frontend-allocated identifier (e.g. "queue:<uuid>")
	// used to correlate the pending preview row with its dispatched
	// user_text item.
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
	// StaleUserItemID names a quiet user_text row from a PREVIOUS
	// dispatch of this message whose session-death cleanup failed
	// (app-layer requeue, round-11 R11-1). The dispatcher must retry
	// that cleanup before persisting a fresh row — dispatching over
	// the stale row would show the message twice in the timeline.
	// Empty for normal queue items.
	StaleUserItemID string
	// Settlement is opaque app-layer bookkeeping whose lifetime is the
	// message's. The app settles it when the message reaches either durable
	// endpoint: the provider, or the composer draft used to recover a dead
	// session. Failed dispatches and failed draft restores keep the same
	// settlement on the requeued item.
	//
	// Nil for user-typed messages. Workflow wakes use it for the durable wake
	// signature and provider-usage attention claim that suppress duplicates.
	Settlement *FlushSettlement
}

type UnconfirmedFlushItem struct {
	QueueItemID  string
	UserItemID   string
	Message      string
	Payload      json.RawMessage
	EnqueuedAt   int64
	DeferredItem *store.Item
	QuietItem    *store.Item
	Settlement   *FlushSettlement
	// StaleUserItemID carries a requeued item's failed-cleanup row id
	// (QueuedFlushItem.StaleUserItemID) through a session-death drain
	// that catches the item still queued, so the obligation survives
	// repeated deaths. Entries drained from the pending-send FIFO
	// never carry it: their dispatch already ran the cleanup before
	// persisting.
	StaleUserItemID string
}

// FlushSettlement owns the one durable-delivery transition for an injected
// queue item. A provider write and session-death recovery can race over the
// same item, so the exactly-once guarantee belongs to the value both paths
// carry rather than to caller ordering.
//
// It is process-local by design. If the process dies before settlement, the
// workflow's durable pending records are what re-surface the wake on boot.
type FlushSettlement struct {
	once sync.Once
	fn   func()
}

// NewFlushSettlement binds one injected queue item's durable bookkeeping.
func NewFlushSettlement(fn func()) *FlushSettlement {
	if fn == nil {
		return nil
	}
	return &FlushSettlement{fn: fn}
}

// Settle runs the durable bookkeeping at most once. Nil and zero values are
// no-ops so ordinary user-authored queue items need no conditional path.
func (s *FlushSettlement) Settle() {
	if s == nil || s.fn == nil {
		return
	}
	s.once.Do(s.fn)
}

// CombineFlushSettlements preserves every obligation when two in-memory
// representations of the same queue item meet during session-death recovery.
// Normally both carry the same pointer; accepting distinct values prevents a
// future producer from being silently dropped by deduplication.
func CombineFlushSettlements(left, right *FlushSettlement) *FlushSettlement {
	switch {
	case left == nil:
		return right
	case right == nil || left == right:
		return left
	default:
		return NewFlushSettlement(func() {
			left.Settle()
			right.Settle()
		})
	}
}

// FlushDispatcher is the app-layer callback invoked when triage drains
// queued user messages.
// Triage hands ownership of the consumed batch over; the dispatcher is
// responsible for allocating AO item ids, persisting user rows,
// registering pending-send markers, and writing to the provider.
//
// Invoked after r.mu is released so the dispatcher can call back into the
// router (RegisterPendingSend, PersistItem, etc) without re-entrancy. The
// callback must return quickly; provider writes belong behind the app-layer
// async/FIFO dispatcher.
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

// SetFlushUserTextConfirmedHook wires the app-layer callback that runs
// after a flush-queued user_text row is confirmed — by its provider
// echo (persisted deferred rows / stamped eager quiet rows) or by the
// interrupt paths that anchor a row at its final position
// (PromoteQuietFlushSends, EagerPersistDeferredFlushSends). Nil
// disables the callback.
//
// CONTRACT: the hook runs outside r.mu but UNDER the thread's flush
// anchor lock — the
// in-lock invocation is load-bearing (the message anchor it records
// must exist before the mutex releases, or an echo in the gap stamps
// ids onto an anchor that isn't there yet; round-4 CT4-1) — and
// often on the provider read loop. It must not call back into any
// Router method (the anchor lock is not reentrant; DrainUnconfirmedFlushItems,
// PromoteQuietFlushSends etc. would deadlock) and must not block
// indefinitely. Store/emit work, as the anchor record does, is fine.
func (r *Router) SetFlushUserTextConfirmedHook(fn func(threadID string, item store.Item)) {
	r.mu.Lock()
	r.flushUserTextConfirmed = fn
	r.mu.Unlock()
}

// FlushQueuedItems drains the queue immediately if a dispatcher is wired.
// RegisterQueueItem calls this after emitting the pending snapshot so the
// frontend can keep showing the submitted message above the composer while
// the app writes it to the provider and waits for the provider-visible echo.
func (r *Router) FlushQueuedItems(threadID string) bool {
	return r.tryFlushQueue(threadID)
}

// RegisterQueueItem appends a queued user message to the per-thread
// flush queue. Returns the resolved EnqueuedAt timestamp so callers
// can surface it on the wire (provider:queue_added). No-op when
// threadID or item.ID is empty — both are required for provider echo
// correlation.
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
	// claimedFlushItems keeps a batch mid-handoff (deleted from the
	// queue, not yet recorded in-flight by the App dispatcher) visible
	// to the revert predicate — see tryFlushQueue.
	return len(r.queuedFlushItems[threadID]) + r.claimedFlushItems[threadID]
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
//
// Handoff visibility: the boundary-drain triggers (turn / tool /
// background-task completion) run lock-free on this goroutine, without
// App.flushHandoffMu — only the RegisterQueueItem trigger holds that
// mutex against the revert-on-interrupt predicate. A batch mid-handoff
// must therefore stay visible to the predicate on its own: the
// claimedFlushItems count is bumped under r.mu alongside the queue
// delete and dropped only after the dispatcher has synchronously
// recorded the batch in-flight (enqueueFlushDispatch bumps its
// inflight counter before returning). Folded into QueuedFlushItemCount,
// the batch reads as queued → claimed → in-flight, never invisible —
// without it, a Stop landing in the gap on a turn with no durable agent
// rows (a top-level Codex unified-exec completing outside an active
// wire round persists nothing) would wrongly revert the turn's prompt
// while the dispatcher delivers the follow-up (round-14 close-out,
// C14-1). The overlap is airtight only if the predicate reads the
// triage counts BEFORE the App inflight count — pendingFlushWorkCount
// does. A dispatch that fails and requeues between those two reads is
// covered separately: the failure persists an error row before the
// inflight count drops, so the turn is no longer clean.
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
	r.claimedFlushItems[threadID] += len(batch)
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if r.claimedFlushItems[threadID] -= len(batch); r.claimedFlushItems[threadID] <= 0 {
			delete(r.claimedFlushItems, threadID)
		}
		r.mu.Unlock()
	}()
	dispatcher(threadID, batch)
	return true
}

func (r *Router) maybeFlushQueueAtBoundary(threadID string) bool {
	if threadID == "" || !r.HasQueuedFlushItems(threadID) {
		return false
	}
	active, err := r.hasQueueBlockingWork(threadID)
	if err != nil {
		// Failing closed avoids racing a message into provider context on
		// incomplete lifecycle knowledge.
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
	// The watch-task-excluding variant: a running Monitor observes
	// rather than works, and a persistent one runs until session end —
	// it must not starve a queued user send (claude-wire.md §E7).
	active, err = r.store.HasQueueBlockingBackgroundToolCall(threadID)
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	active, err = r.store.HasLiveCodexSubagentLaunch(threadID)
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
		if tracker != nil {
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

func (r *Router) DrainUnconfirmedFlushItems(threadID string) []UnconfirmedFlushItem {
	if threadID == "" {
		return nil
	}

	// Serialize with the interrupt anchor transitions and the echo
	// pop+write (lock order: flush anchor before r.mu, always). Without
	// this a session-death drain could slip between an eager persist's
	// claim (DeferredItem moved to QuietItem under r.mu) and its store
	// write: the drain would restore the message to the draft and try to
	// delete a row that doesn't exist yet, then the persist would commit
	// AFTER teardown — the next session shows the message in both the
	// draft and the timeline (round-4 review, CT4-2). Under the mutex an
	// in-flight transition fully commits (or unclaims) before the drain
	// snapshots, so a returned QuietItem always reflects the store.
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()

	r.mu.Lock()
	var drained []UnconfirmedFlushItem
	for _, item := range r.queuedFlushItems[threadID] {
		payload := append(json.RawMessage(nil), item.Payload...)
		drained = append(drained, UnconfirmedFlushItem{
			QueueItemID:     item.ID,
			Message:         item.Message,
			Payload:         payload,
			EnqueuedAt:      item.EnqueuedAt,
			StaleUserItemID: item.StaleUserItemID,
			Settlement:      item.Settlement,
		})
	}
	delete(r.queuedFlushItems, threadID)

	pending := r.pendingByThread[threadID]
	var echoConsumed []pendingSend
	if len(pending) > 0 {
		kept := make([]pendingSend, 0, len(pending))
		for _, entry := range pending {
			if entry.QueueItemID == "" || !strings.Contains(entry.AOItemID, ":flush:") {
				kept = append(kept, entry)
				continue
			}
			if entry.EchoConsumed {
				// The echo arrived (the provider transcript contains the
				// message) but its write failed pre-durability. NOT
				// restorable: a draft restore would re-send content the
				// provider context already has, duplicating it on the
				// next session (round-5, R5-3). Self-healed below.
				echoConsumed = append(echoConsumed, entry)
				continue
			}
			restored := UnconfirmedFlushItem{
				QueueItemID: entry.QueueItemID,
				UserItemID:  entry.AOItemID,
				EnqueuedAt:  entry.EnqueuedAt,
			}
			if entry.DeferredItem != nil {
				item := *entry.DeferredItem
				restored.DeferredItem = &item
				restored.Message = item.Summary
			}
			if entry.QuietItem != nil {
				item := *entry.QuietItem
				restored.QuietItem = &item
				restored.Message = item.Summary
			}
			drained = append(drained, restored)
		}
		if len(kept) == 0 {
			delete(r.pendingByThread, threadID)
		} else {
			r.pendingByThread[threadID] = kept
		}
	}
	r.mu.Unlock()

	// Still under the anchor lock: a re-delivered echo retrying one of
	// these entries cannot interleave with the heal.
	for _, entry := range echoConsumed {
		r.selfHealEchoConsumedFlushRow(threadID, entry)
	}
	return drained
}

// selfHealEchoConsumedFlushRow makes sure the timeline carries a flush
// message whose provider consumption was proven by an echo that then
// failed before its durable write (round-5, R5-3), and stamps the
// echo's stashed wire identity onto the row and its message anchor
// (round-6, R6-1): the entry is dropped after this, so an unstamped
// row would permanently lack its slice anchor — a later revert would
// ordinal-walk or full-clone while the provider transcript still
// contains the message. When the row already exists (eager interrupt
// persist committed; only the echo's stamp was lost) the stamp is the
// heal; when it is missing, the retained copy persists with the ids
// (and, for promoted rows, the echo-time boundary) already merged.
// Caller MUST hold the thread's flush anchor lock. Session-death
// path: failures are logged loudly rather than returned — the
// retained copy is lost only if the store itself is failing.
func (r *Router) selfHealEchoConsumedFlushRow(threadID string, entry pendingSend) {
	stampMeta := func(meta string) (string, error) {
		merged, err := usermessage.MergeProviderIDs(meta, entry.EchoProviderItemID, entry.EchoParentUUID)
		if err != nil {
			return "", err
		}
		if entry.EchoPromotedBoundary >= 0 {
			// Stashed at echo time for promoted rows (whose store meta
			// already carries the marker) AND for unanchored eager rows
			// whose tail bump then failed (round-10, R10-1) — the healed
			// row sits at its dispatch-time index, so the marker+boundary
			// pair is what keeps the revert cut on provider order.
			// Written together so the boundary is never orphaned.
			if merged, err = itemmeta.MarkPromotedAtInterrupt(merged); err != nil {
				return "", err
			}
			if merged, err = itemmeta.MarkPromotedEchoBoundary(merged, entry.EchoPromotedBoundary); err != nil {
				return "", err
			}
		}
		return merged, nil
	}
	hasEchoIDs := entry.EchoProviderItemID != "" || entry.EchoParentUUID != ""

	retained := entry.QuietItem
	if retained == nil {
		retained = entry.DeferredItem
	}
	_, found, err := r.store.GetThreadItem(threadID, entry.AOItemID)
	if err != nil {
		log.Printf("triage: self-heal lookup for echo-consumed flush row %s/%s: %v", threadID, entry.AOItemID, err)
		return
	}
	switch {
	case found:
		if hasEchoIDs {
			updated, _, err := r.store.UpdateItemMetaMerge(threadID, entry.AOItemID, stampMeta, time.Now().UnixMilli())
			if err != nil {
				log.Printf("triage: self-heal stamp for echo-consumed flush row %s/%s: %v", threadID, entry.AOItemID, err)
			} else {
				// A non-interrupt quiet row was never emitted (quiet
				// persist at dispatch; the emitting echo bump failed) —
				// without this upsert it stays invisible until a thread
				// reload (round-7, R7-7). Idempotent for rows the
				// promote path already emitted.
				r.emitItemUpsertWithActivity(updated, false)
			}
		}
	case retained == nil:
		log.Printf("triage: echo-consumed flush entry %s/%s has no retained copy — the message survives only in the provider transcript", threadID, entry.AOItemID)
		return
	default:
		item := *retained
		if stamped, stampErr := stampMeta(item.Meta); stampErr != nil {
			// Content preservation trumps id enrichment: persist the
			// retained copy as-is rather than lose the message.
			log.Printf("triage: self-heal id merge for echo-consumed flush row %s/%s: %v — persisting the retained copy unstamped", threadID, entry.AOItemID, stampErr)
		} else {
			item.Meta = stamped
		}
		var persistErr error
		if entry.QuietItem == nil && entry.EchoTurnWasEmpty {
			// Deferred-origin retained copy whose turn was EMPTY at the
			// first echo — the turn is the prompt's own, and the failed
			// echo opened it (R6-3): response rows now occupy 0..n and
			// an append would sort the prompt after its own response
			// (round-7, R7-4). Steer-shape deferred rows
			// (EchoTurnWasEmpty false) keep the append — pre-dispatch
			// content correctly precedes them.
			_, persistErr = r.persistUserPromptAtTurnHead(item)
		} else {
			persistErr = r.persistItem(item, nil)
		}
		if persistErr != nil {
			log.Printf("triage: self-heal persist for echo-consumed flush row %s/%s: %v", threadID, entry.AOItemID, persistErr)
			return
		}
		log.Printf("triage: echo-consumed flush row %s/%s was missing at session-death drain — re-persisted the retained copy", threadID, entry.AOItemID)
	}
	if hasEchoIDs {
		// Fold the ids into the row's message anchor too — the anchor
		// copy is the rollback path's first slice candidate. Rows that
		// existed at the failed echo already hold an anchor recorded
		// at the true consumption boundary (recordEchoBoundaryAnchor,
		// round-10 R10-2). Rows healed from a never-persisted deferred
		// copy have no anchor row (the upsert needs the row's FK); the
		// update is then a no-op and rollback falls back to the
		// item-meta synthesis.
		if err := r.store.UpdateMessageAnchorProviderIDs(threadID, entry.AOItemID, entry.EchoProviderItemID, entry.EchoParentUUID); err != nil {
			log.Printf("triage: self-heal anchor ids for echo-consumed flush row %s/%s: %v", threadID, entry.AOItemID, err)
		}
	}
}
