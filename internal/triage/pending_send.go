package triage

import (
	"log"
	"strings"
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
	QuietItem    *store.Item
	// ExpectedProviderItemID is the wire id the send's echo will carry,
	// when the app knows it at dispatch time. Claude-family sends mint a
	// uuidv4 and pass it as the outbound envelope's top-level `uuid`; the
	// CLI echoes it verbatim (verified for direct sends on 2.1.150 and
	// for queued mid-turn sends on 2.1.202 — spike 2026-07-09, see
	// claude-wire.md §Outbound user message). An entry carrying this id
	// is consumed by ID EQUALITY only, so a provider-injected user
	// envelope (whose uuid the CLI minted) can never pop it — the
	// injection-during-queue-wait collision. Empty for Codex (item ids
	// are provider-assigned, unknowable at send time), which keeps the
	// FIFO head-pop semantics.
	ExpectedProviderItemID string
	// AnchoredAtInterrupt marks a flush entry whose row was already
	// placed at its user-visible timeline position by the interrupt
	// handler — either bumped to the turn tail (PromoteQuietFlushSends,
	// quiet eager-persist rows) or persisted at the turn tail
	// (EagerPersistDeferredFlushSends, deferred rows). The echo-side
	// reposition in attachProviderItemIDToUserRow must skip these: rows
	// landing between the interrupt and the echo are the interrupted
	// turn's post-interrupt tail, which the user already watched stream
	// BELOW the anchored message — an echo-time bump would leapfrog the
	// message over them and persist that inverted order.
	AnchoredAtInterrupt bool
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
	// ExpectedProviderItemID is the client-minted uuid the provider echo must
	// carry to consume this entry (empty when the send used FIFO matching).
	// Integration tests fabricate echoes with this value.
	ExpectedProviderItemID string
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
		AOItemID:               head.AOItemID,
		TurnIndex:              head.TurnIndex,
		HasDeferred:            head.DeferredItem != nil,
		ExpectedProviderItemID: head.ExpectedProviderItemID,
	}, true
}

// RegisterPendingSend appends a new entry to the per-thread FIFO with no
// expected wire id (FIFO-consumed). Codex send paths use this — Codex
// assigns its own item ids, so identity is unknowable at dispatch.
// EnqueuedAt is stamped from time.Now here — Phase E uses it for
// diagnostics on stranded entries and the wall clock at the register
// call is the natural reference.
func (r *Router) RegisterPendingSend(threadID, aoItemID string, turnIndex int) {
	r.RegisterPendingSendExpecting(threadID, aoItemID, turnIndex, "")
}

// RegisterPendingSendExpecting registers a direct send whose wire echo
// will carry expectedProviderItemID (the app-minted uuid Claude echoes
// verbatim). The entry is consumed by id equality — see
// pendingSend.ExpectedProviderItemID. Pass "" for providers with no
// pre-known identity; that entry falls back to FIFO consumption.
func (r *Router) RegisterPendingSendExpecting(threadID, aoItemID string, turnIndex int, expectedProviderItemID string) {
	r.registerPendingSend(threadID, aoItemID, turnIndex, "", 0, nil, nil, expectedProviderItemID)
}

// RegisterPendingFlushSend registers a deferred user_text row whose
// persistence is gated on the wire echo, with no expected wire id. The
// row's item_index is recomputed at echo time (persistDeferredUserText)
// so the queued message lands after content the model emitted between
// dispatch and echo — see the pendingSend doc comment.
func (r *Router) RegisterPendingFlushSend(threadID, queueItemID string, item store.Item) {
	r.RegisterPendingFlushSendWithEnqueuedAt(threadID, queueItemID, item, 0, "")
}

func (r *Router) RegisterPendingFlushSendWithEnqueuedAt(threadID, queueItemID string, item store.Item, enqueuedAt int64, expectedProviderItemID string) {
	r.registerPendingSend(threadID, item.ID, item.TurnIndex, queueItemID, enqueuedAt, &item, nil, expectedProviderItemID)
}

func (r *Router) RegisterPendingQuietFlushSend(threadID, queueItemID string, item store.Item, turnIndex int, enqueuedAt int64, expectedProviderItemID string) {
	r.registerPendingSend(threadID, item.ID, turnIndex, queueItemID, enqueuedAt, nil, &item, expectedProviderItemID)
}

func (r *Router) registerPendingSend(threadID, aoItemID string, turnIndex int, queueItemID string, enqueuedAt int64, deferredItem, quietItem *store.Item, expectedProviderItemID string) {
	if threadID == "" || aoItemID == "" {
		return
	}
	if enqueuedAt == 0 {
		enqueuedAt = time.Now().UnixMilli()
	}
	r.mu.Lock()
	r.pendingByThread[threadID] = append(r.pendingByThread[threadID], pendingSend{
		AOItemID:               aoItemID,
		QueueItemID:            queueItemID,
		TurnIndex:              turnIndex,
		EnqueuedAt:             enqueuedAt,
		DeferredItem:           deferredItem,
		QuietItem:              quietItem,
		ExpectedProviderItemID: expectedProviderItemID,
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

// consumeMatchingPendingSend pops the pending entry that corresponds to
// a top-level wire user echo carrying providerItemID. Two consumption
// modes, decided by how the entry was registered:
//
//   - Identity: an entry with ExpectedProviderItemID set (Claude-family
//     sends — AO mints the uuid and the CLI echoes it verbatim; verified
//     for direct sends on 2.1.150 and queued mid-turn sends on 2.1.202,
//     spike 2026-07-09) is consumed ONLY when the echo's id equals it.
//     A provider-injected user envelope carries a CLI-minted uuid that
//     can never equal an AO-minted one, so an injection arriving while a
//     queued send waits out a running turn (the collision window is the
//     whole remaining turn — the CLI echoes queued sends at turn pickup,
//     not enqueue) can no longer pop the real send's entry.
//   - FIFO: an entry with no expected id (Codex — item ids are
//     provider-assigned, unknowable at dispatch) pops at the head,
//     preserving the original ordering semantics.
//
// Carve-out: an echo with NO providerItemID pops the head even in
// identity mode. Injected envelopes always carry a top-level uuid, so an
// id-less echo cannot be an injection — but it IS the shape both
// downstream branches log loudly about (stuck queue-confirm, parser gap),
// and consuming keeps those diagnostics reachable instead of stranding
// the entry silently.
//
// Returns (zero, false) when nothing matches; handleUserText then routes
// the echo to the wire-only paths (subagent prompt / injected context).
func (r *Router) consumeMatchingPendingSend(threadID, providerItemID string) (pendingSend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return pendingSend{}, false
	}
	if queue[0].ExpectedProviderItemID == "" || providerItemID == "" {
		return r.popPendingSendAtLocked(threadID, 0), true
	}
	for i := range queue {
		if queue[i].ExpectedProviderItemID == providerItemID {
			return r.popPendingSendAtLocked(threadID, i), true
		}
	}
	return pendingSend{}, false
}

// popPendingSendAtLocked removes and returns the entry at index i of
// threadID's queue, preserving the relative order of the survivors.
// Caller MUST hold r.mu and guarantee i is in range. Copies into a
// fresh slice so the map doesn't pin the previous backing array (which
// still references the popped entry) — queues are tiny, so the
// allocation is cheap and the GC behavior is obvious.
func (r *Router) popPendingSendAtLocked(threadID string, i int) pendingSend {
	queue := r.pendingByThread[threadID]
	entry := queue[i]
	if len(queue) == 1 {
		delete(r.pendingByThread, threadID)
		return entry
	}
	next := make([]pendingSend, 0, len(queue)-1)
	next = append(next, queue[:i]...)
	next = append(next, queue[i+1:]...)
	r.pendingByThread[threadID] = next
	return entry
}

// ClearPendingSendForFailure removes a single matching entry from the
// FIFO. Distinct from consumeMatchingPendingSend because failure recovery
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
// pending send entry. Nilling routes the echo path to
// attachProviderItemIDToUserRow instead of persistDeferredUserText
// (re-persist); the AnchoredAtInterrupt marker set after the persist is
// what keeps that path stamp-only (without it, the :flush: echo bump
// would reposition the row a second time).
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
	persistedIDs := make(map[string]struct{}, len(snapshots))
	for _, item := range snapshots {
		if err := r.persistItem(item, nil); err != nil {
			log.Printf("triage: eager persist deferred flush %s/%s: %v", item.ThreadID, item.ID, err)
			continue
		}
		persistedIDs[item.ID] = struct{}{}
		result = append(result, EagerPersistedFlush{
			UserItemID: item.ID,
			TurnIndex:  item.TurnIndex,
			Content:    item.Summary,
			Meta:       item.Meta,
		})
	}
	// The row now sits at its user-visible interrupt position (the
	// persist landed at the turn tail). Without the marker the echo's
	// :flush: bump would move it again — past any post-interrupt tail
	// the provider flushes while stopping.
	r.markPendingSendsAnchoredAtInterrupt(threadID, persistedIDs)
	return result
}

// PromoteQuietFlushSends emits provider:item_event for any
// non-deferred flush sends already persisted in the store. Called on
// user interrupt so quietly-persisted flush messages transition from
// Zone 2 (queued marker) to the timeline immediately, rather than
// waiting for the provider echo. The pending send entries stay in the
// FIFO — the echo still stamps provider_item_id via
// attachProviderItemIDToUserRow.
func (r *Router) PromoteQuietFlushSends(threadID string) int {
	if threadID == "" || r.store == nil {
		return 0
	}

	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	var ids []string
	for _, p := range pending {
		if p.DeferredItem == nil && strings.Contains(p.AOItemID, ":flush:") {
			ids = append(ids, p.AOItemID)
		}
	}
	r.mu.Unlock()

	if len(ids) == 0 {
		return 0
	}

	promoted := 0
	promotedIDs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		item, err := r.store.BumpItemToTurnEnd(threadID, id)
		if err != nil {
			log.Printf("triage: promote quiet flush %s/%s: %v", threadID, id, err)
			continue
		}
		promotedIDs[id] = struct{}{}
		r.emitItemUpsertWithActivity(item, false)
		promoted++
	}
	// Mark the successfully-bumped entries so the echo-side reposition
	// (attachProviderItemIDToUserRow) knows the row is already at its
	// user-visible interrupt position and stamps without re-bumping.
	r.markPendingSendsAnchoredAtInterrupt(threadID, promotedIDs)
	return promoted
}

// markPendingSendsAnchoredAtInterrupt flips AnchoredAtInterrupt on the
// pending-send entries whose AOItemID appears in ids. Both interrupt
// eager-persist paths (quiet promote, deferred persist) call this after
// their store write succeeds; entries whose write failed stay unmarked
// so the echo-time bump remains their fallback positioning.
func (r *Router) markPendingSendsAnchoredAtInterrupt(threadID string, ids map[string]struct{}) {
	if len(ids) == 0 {
		return
	}
	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	for i := range pending {
		if _, ok := ids[pending[i].AOItemID]; ok {
			pending[i].AnchoredAtInterrupt = true
		}
	}
	r.mu.Unlock()
}

// MarkPendingSendAnchoredAtInterrupt is the app-layer entry point for
// pending entries REGISTERED during interrupt handling for a row that
// is already at its user-visible position — the Codex re-send after
// interrupt registers a fresh non-deferred entry for the
// eagerly-persisted flush row, and its echo must stamp without
// re-bumping, exactly like the entries the triage-internal interrupt
// paths mark themselves.
func (r *Router) MarkPendingSendAnchoredAtInterrupt(threadID, aoItemID string) {
	r.markPendingSendsAnchoredAtInterrupt(threadID, map[string]struct{}{aoItemID: {}})
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
