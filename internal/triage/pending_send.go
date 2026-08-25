package triage

import (
	"log"
	"strings"
	"sync"
	"time"

	"agent-overflow/internal/itemmeta"
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

	// ExpectedClientID is the `clientUserMessageId` this send was
	// dispatched with — always the entry's own AOItemID, so the field is
	// really a flag: "this send's echo names itself". Codex threads it
	// back on the `userMessage` echo as `clientId`
	// (`TurnInput::UserInput{client_id}` → `UserMessageItem.client_id`,
	// rust-v0.149.0), which is the only identity key that works there at
	// all — item ids are provider-assigned, so ExpectedProviderItemID is
	// structurally unavailable.
	//
	// Set means the entry is consumable ONLY by an echo carrying this
	// client id. An echo with NO client id must skip it, head or not:
	// that shape is either another producer's row off the provider's own
	// queue or a pre-identity send, and FIFO-popping an entry that is
	// waiting to name itself is the 2026-08-24 mispop — a direct-send
	// echo popped a queued entry, stamping one message onto the other's
	// row and leaving the real echo to persist as injected context.
	// Empty for Claude-family sends (identity is
	// ExpectedProviderItemID) and for any Codex send predating the
	// stamp, both of which keep the FIFO head-pop.
	ExpectedClientID string
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

	// WasDeferred marks an entry whose row was originally deferred
	// (persist-at-echo) and reached SQLite only through the interrupt
	// eager-persist. Its row owns a FRESH turn index, so if its echo
	// lands with no init having opened that turn — the interrupt raced
	// the CLI's mid-loop queue drain and the old round is still live —
	// attachProviderItemIDToUserRow must open the logical turn exactly
	// like the still-deferred path does. Promoted eager rows (shared
	// turn) must NOT get that call: their index belongs to an earlier,
	// possibly settled turn.
	WasDeferred bool

	// InterruptedTurnIndex is the logical turn that was open when a
	// user interrupt claimed this entry (-1 when none has). Stamped
	// pre-ack by MarkFlushSendsInterrupted — the CLI's mid-loop
	// queue drain can echo the entry back during the ack wait, before
	// the eager persist runs (round-6, R6-4) — and again by
	// EagerPersistDeferredFlushSends. The echo's openQueuedEchoTurn
	// settles its still-open predecessor as "interrupted" only when
	// that predecessor IS this turn — the one the user provably cut.
	// When one interrupt eager-persists several deferred rows and the
	// CLI echoes them back to back, the later echoes settle their
	// SIBLINGS' turns, which ended naturally as the CLI drained the
	// next queued message and must settle as "end_turn".
	InterruptedTurnIndex int

	// EchoConsumed marks an entry that was reinserted after its echo's
	// store write failed: the provider PROVABLY consumed the message
	// (the echo arrived), only AO's write didn't stick. A session-death
	// drain must not hand such an entry back as restorable — restoring
	// it to the draft/queue would re-send a message the resumed
	// transcript already contains, duplicating it in provider context
	// (round-5, R5-3). The drain instead self-heals the timeline row
	// from the retained copy and drops the entry.
	EchoConsumed bool

	// EchoProviderItemID / EchoParentUUID carry the consumed echo's
	// wire identity across a failed write: handleUserText stashes them
	// on the popped entry before dispatching to the fallible handlers,
	// so a reinserted EchoConsumed entry still knows the transcript
	// uuid and parent that the failed stamp lost. The session-death
	// self-heal merges them into the healed row (and its anchor) —
	// without them the healed row has no slice anchor and a later
	// revert degrades to the ordinal fallback, or full-clones, while
	// the provider transcript still contains the message (round-6,
	// R6-1).
	EchoProviderItemID string
	EchoParentUUID     string

	// EchoPromotedBoundary is the promoted-echo provider-order boundary
	// (see itemmeta.MarkPromotedEchoBoundary) computed by
	// attachProviderItemIDToUserRow before its fallible write, or -1.
	// Stashed for the same reason as the ids above: the boundary is
	// echo-time information — by session-death drain time the response
	// rows have persisted and a recomputed MAX would misclassify them
	// as interrupted tail, so the failed write's value is the only
	// correct source (round-6, R6-1).
	EchoPromotedBoundary int

	// EchoTurnWasEmpty records whether the deferred prompt's turn had
	// no rows when its FIRST echo arrived — true for the turn the echo
	// itself opens (Claude mid-loop pickup), false for a steer into an
	// occupied turn where pre-dispatch content correctly precedes the
	// prompt. Stashed like the fields above because it is first-echo
	// information: a replay retry or session-death self-heal after a
	// failed first persist finds the RESPONSE occupying the turn and
	// can no longer tell response rows (persist the prompt above them)
	// from pre-dispatch content (persist below) (round-7, R7-4).
	// Always populated before a reinsert can mark the entry
	// EchoConsumed: when the first echo's row sample fails, the
	// router's turn-open state substitutes — a turn nobody opened yet
	// is the prompt's own — so the retry / self-heal never reads an
	// unrecorded zero value and appends an empty-turn prompt below its
	// own response (round-14, D14-1).
	EchoTurnWasEmpty bool

	// AnchorRecordedAtEcho marks an entry whose confirmed-hook message
	// anchor was recorded on the FIRST echo's failure path (the row
	// existed; only the stamp write failed). That first echo is the
	// true consumption boundary, so a retry's success path must not
	// run the hook again — it only folds the retry's provider ids
	// into the existing anchor via UpdateMessageAnchorProviderIDs
	// (round-10, R10-2).
	AnchorRecordedAtEcho bool

	// NeedsTailRebump marks an anchored quiet entry whose sibling
	// re-bump failed after an earlier promoted echo drained rows past
	// it (rebumpAnchoredQuietSiblings): its row sits below content
	// that precedes it in provider order, and nothing else revisits
	// the position — the entry's own echo skips the bump as
	// already-anchored. attachProviderItemIDToUserRow treats the flag
	// like rebumpOverDrained, so that echo forces the turn-tail bump
	// and repairs both display order and the revert cut (round-11,
	// R11-5).
	NeedsTailRebump bool
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

// PendingSendExpectation names the wire identity a send's echo will carry
// back, so the registry can consume the entry by IDENTITY rather than by
// position. The two axes are per-provider and mutually exclusive in
// practice, but the struct does not enforce that: a provider that grows
// both is a stronger match, not a contradiction.
//
// Registering with the zero value is the historical FIFO behaviour and
// stays available for a send whose echo names nothing.
type PendingSendExpectation struct {
	// ProviderItemID is the app-minted uuid the CLI echoes verbatim as
	// the envelope's top-level id (Claude family). See
	// pendingSend.ExpectedProviderItemID.
	ProviderItemID string

	// ByClientID marks a send dispatched with `clientUserMessageId` set
	// to the entry's own AOItemID — every Codex `turn/start` and
	// `turn/steer`. The registrar copies the AOItemID into
	// pendingSend.ExpectedClientID, so the caller never restates it and
	// the two can never disagree. See pendingSend.ExpectedClientID for
	// what it costs an echo that carries no client id.
	ByClientID bool
}

// RegisterPendingSendWithExpectation registers a direct send. The echo
// consumes the entry by the identity `expect` declares: ProviderItemID
// for the Claude family, {ByClientID: true} for every Codex send and
// steer, and the zero value for a send whose echo names nothing
// (claude-tui) — that entry falls back to FIFO consumption.
// EnqueuedAt is stamped from time.Now here — Phase E uses it for
// diagnostics on stranded entries and the wall clock at the register
// call is the natural reference.
func (r *Router) RegisterPendingSendWithExpectation(threadID, aoItemID string, turnIndex int, expect PendingSendExpectation) {
	r.registerPendingSend(threadID, aoItemID, turnIndex, "", 0, nil, nil, expect)
}

// RegisterPendingFlushSendWithExpectation registers a deferred user_text
// row whose persistence is gated on the wire echo. The row's item_index
// is recomputed at echo time (persistDeferredUserText) so the queued
// message lands after content the model emitted between dispatch and
// echo — see the pendingSend doc comment.
func (r *Router) RegisterPendingFlushSendWithExpectation(threadID, queueItemID string, item store.Item, enqueuedAt int64, expect PendingSendExpectation) {
	r.registerPendingSend(threadID, item.ID, item.TurnIndex, queueItemID, enqueuedAt, &item, nil, expect)
}

// RegisterPendingQuietFlushSendWithExpectation is the quiet-flush
// registration: the row was already persisted (eager persist) and the
// echo only confirms it, at the caller-chosen turnIndex.
func (r *Router) RegisterPendingQuietFlushSendWithExpectation(threadID, queueItemID string, item store.Item, turnIndex int, enqueuedAt int64, expect PendingSendExpectation) {
	r.registerPendingSend(threadID, item.ID, turnIndex, queueItemID, enqueuedAt, nil, &item, expect)
}

func (r *Router) registerPendingSend(threadID, aoItemID string, turnIndex int, queueItemID string, enqueuedAt int64, deferredItem, quietItem *store.Item, expect PendingSendExpectation) {
	if threadID == "" || aoItemID == "" {
		return
	}
	if enqueuedAt == 0 {
		enqueuedAt = time.Now().UnixMilli()
	}
	// The expected client id is never a caller-supplied value: it IS the
	// AO row id that went on the wire as `clientUserMessageId`, so
	// deriving it here is what keeps the dispatched id and the expected
	// one from drifting apart.
	expectedClientID := ""
	if expect.ByClientID {
		expectedClientID = aoItemID
	}
	r.mu.Lock()
	r.pendingByThread[threadID] = append(r.pendingByThread[threadID], pendingSend{
		AOItemID:               aoItemID,
		QueueItemID:            queueItemID,
		TurnIndex:              turnIndex,
		EnqueuedAt:             enqueuedAt,
		DeferredItem:           deferredItem,
		QuietItem:              quietItem,
		ExpectedProviderItemID: expect.ProviderItemID,
		ExpectedClientID:       expectedClientID,
		InterruptedTurnIndex:   -1,
		EchoPromotedBoundary:   -1,
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
//   - FIFO: an entry with no expected id (legacy Codex — item ids are
//     provider-assigned, unknowable at dispatch) pops at the head,
//     preserving the original ordering semantics.
//
// A third mode, client id, is reachable only through
// consumeMatchingPendingSendForEcho and is documented there. It also
// SUBTRACTS from both modes above: an entry expecting a client id is
// invisible to an id-less echo, head or not.
//
// Carve-out: an echo with NO providerItemID pops the head even in
// identity mode. Injected envelopes always carry a top-level uuid, so an
// id-less echo cannot be an injection — but it IS the shape both
// downstream branches log loudly about (stuck queue-confirm, parser gap),
// and consuming keeps those diagnostics reachable instead of stranding
// the entry silently. "The head" there means the head of the entries an
// id-less echo can consume at all.
//
// Returns (zero, false) when nothing matches; handleUserText then routes
// the echo to the wire-only paths (subagent prompt / injected context).
func (r *Router) consumeMatchingPendingSend(threadID, providerItemID string) (pendingSend, bool) {
	return r.consumeMatchingPendingSendForEcho(threadID, providerItemID, "")
}

// consumeMatchingPendingSendForEcho is the full match, including the echo's
// `client_id` — the third and STRONGEST key, and the only one that works on
// Codex.
//
// A Codex entry has no expected provider item id (item ids are
// provider-assigned), so it falls to FIFO — and FIFO is wrong the moment the
// provider's own queue can hold rows this app did not write. `codex queue
// --thread` writes into the same FIFO the app-server drains, so a foreign row
// AHEAD of AO's dispatches first, and its echo would pop AO's entry: the
// foreign message would be stamped onto AO's optimistic row, and AO's own echo
// would arrive later with nothing left to match and persist as injected
// provider context. Both halves of the user's transcript wrong, from one
// mismatched pop.
//
// `clientId` is what upstream threads through for exactly this
// (`TurnInput::UserInput{client_id}` → `UserMessageItem.client_id`,
// rust-v0.149.0): AO passes the optimistic row id on `thread/queue/add`, so an
// echo carrying one names the entry it belongs to. Hence:
//
//   - clientID present and equal to an entry's AOItemID → that entry, wherever
//     it sits in the queue.
//   - clientID present and matching nothing → NO match, deliberately. A client
//     id AO does not hold is not AO's message; `codex queue` mints a v7 uuid
//     and cannot collide with AO's `user:<turn>[:flush:<n>]` grammar. Falling
//     back to FIFO here would reintroduce the exact mispop above.
//   - clientID absent → the provider-item-id / FIFO rules above, over the
//     entries that are NOT waiting to be named. Every non-Codex path and every
//     pre-identity Codex session lands here.
//
// That last clause is the structural half of the fix, and it holds
// independently of how many senders stamp an id. An entry with
// ExpectedClientID set has ANNOUNCED that its echo will name it, so an echo
// carrying no client id is provably not that entry's — and both id-less modes
// must skip it, the FIFO head-pop as well as the ExpectedProviderItemID scan.
// Without the skip a direct-send echo (no `clientId` on the wire) FIFO-pops
// whatever sits at the head, which on 2026-08-24 was a queued entry still
// waiting for its own `clientId` echo: the direct send's text landed on the
// queued row and the queued send's echo arrived with nothing left to match, so
// it persisted as "Injected provider context". Once every Codex send stamps an
// id the mixed case disappears; until then the skip alone already prevents it.
func (r *Router) consumeMatchingPendingSendForEcho(threadID, providerItemID, clientID string) (pendingSend, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	queue := r.pendingByThread[threadID]
	if len(queue) == 0 {
		return pendingSend{}, false
	}
	if clientID != "" {
		for i := range queue {
			if queue[i].AOItemID == clientID {
				return r.popPendingSendAtLocked(threadID, i), true
			}
		}
		return pendingSend{}, false
	}
	// The head for an id-less echo is the first entry NOT expecting to be
	// named by one. Everything below reads from that entry, including the
	// identity-vs-FIFO mode decision.
	head := -1
	for i := range queue {
		if queue[i].ExpectedClientID == "" {
			head = i
			break
		}
	}
	if head < 0 {
		return pendingSend{}, false
	}
	// The claude-tui id-less-echo carve-out, and the Codex-shaped FIFO pop.
	if queue[head].ExpectedProviderItemID == "" || providerItemID == "" {
		return r.popPendingSendAtLocked(threadID, head), true
	}
	for i := range queue {
		if queue[i].ExpectedClientID != "" {
			continue
		}
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
	// Anchor lock so the removal is total against an in-flight echo's
	// failure reinsert (round-5, R5-2).
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()
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

// flushAnchor returns threadID's anchor mutex, creating it on first
// use. Entries are never deleted (see the flushAnchorLocks field doc).
func (r *Router) flushAnchor(threadID string) *sync.Mutex {
	r.flushAnchorLocksMu.Lock()
	defer r.flushAnchorLocksMu.Unlock()
	mu, ok := r.flushAnchorLocks[threadID]
	if !ok {
		mu = &sync.Mutex{}
		r.flushAnchorLocks[threadID] = mu
	}
	return mu
}

// isWireOnlyUserTextSeen reports whether providerItemID was already
// recorded by markWireOnlyUserTextSeen. Both echo-handling paths mark
// exactly at their durability point (row persisted / stamp committed),
// so a false here after an error means no durable write happened for
// the envelope — the caller can safely reinsert the popped pending
// entry for retry or death-drain recovery.
func (r *Router) isWireOnlyUserTextSeen(threadID, providerItemID string) bool {
	if threadID == "" || providerItemID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, seen := r.wireOnlyUserTextSeen[threadID][providerItemID]
	return seen
}

// reinsertPendingSendHead puts a popped pending-send entry back at the
// FIFO head after its echo handling failed before reaching a durable
// write. Head position restores the FIFO-pop semantics (the entry was
// at or ahead of the head when consumed); identity-matched entries
// don't care about position. Keeping the entry live means a
// re-delivered echo can retry instead of persisting an injected-context
// duplicate (round-4 review, CT4-3).
//
// The reinserted entry is marked EchoConsumed: the arrival of the echo
// proved the provider transcript contains the message even though AO's
// write failed, so a session-death drain must self-heal the timeline
// row from the retained copy instead of restoring the message to the
// draft — a restore would re-send content the provider context already
// has (round-5, R5-3).
func (r *Router) reinsertPendingSendHead(threadID string, entry pendingSend) {
	entry.EchoConsumed = true
	r.mu.Lock()
	r.pendingByThread[threadID] = append([]pendingSend{entry}, r.pendingByThread[threadID]...)
	r.mu.Unlock()
}

// clearWireOnlyUserTextLocked clears the dedup set for threadID. Caller must
// hold r.mu.
func (r *Router) clearWireOnlyUserTextLocked(threadID string) {
	delete(r.wireOnlyUserTextSeen, threadID)
}

// EagerPersistedFlush describes a deferred flush send that was eagerly
// persisted during interrupt. The app layer uses it for the Codex
// re-send; anchor recording happens inside
// EagerPersistDeferredFlushSends itself via the confirmed hook.
type EagerPersistedFlush struct {
	UserItemID string
	TurnIndex  int
	// ItemIndex is the store-assigned position within the turn.
	// Fork/revert slicing is position-ordered, so tests pin that a
	// real index (not a zero placeholder) was threaded through — a
	// zero would place a Codex steer persisted mid-turn ahead of the
	// content it actually followed.
	ItemIndex int
	Content   string
	Meta      string
	// QueueItemID and EnqueuedAt carry the original queue identity so a
	// failed Codex resend can return the message to the flush queue
	// with its Zone 1 identity and FIFO position intact (round-13,
	// CT13-2).
	QueueItemID string
	EnqueuedAt  int64
}

// EagerPersistDeferredFlushSends immediately persists all deferred
// pending flush sends for threadID into the timeline. Called on user
// interrupt so queued messages become visible without waiting for the
// provider echo.
//
// interruptedTurnIndex is the turn that was open when the user issued
// the interrupt (-1 when none), sampled by the caller via
// OpenTurnIndex BEFORE awaiting the provider's interrupt ack. Sampling
// here instead would race the ack wait: the read loop keeps processing
// wire events during it, so the cut turn can settle (clearing
// openTurns) before this runs, and its echo would then settle it
// "end_turn" instead of "interrupted" (round-5, R5-4).
//
// The whole transition + persist runs under the thread's flush anchor lock, which the
// echo path (handleUserText) also holds across its pending-send pop and
// row write: an echo therefore consumes each entry either strictly
// before this transition (still DeferredItem — the normal deferred
// persist; the snapshot loop below then skips the popped entry) or
// strictly after the persist committed or failed-and-unclaimed (the
// attach path finds the row, or self-heals from QuietItem on failure).
// The mid-transition window that stranded a consumed message
// unstamped (round-3 review, cold-1/CT-2) cannot be observed.
//
// Under r.mu: MOVES each entry's DeferredItem to QuietItem, sets
// WasDeferred, and claims AnchoredAtInterrupt — all before any store
// write. Nilling DeferredItem routes the echo path to
// attachProviderItemIDToUserRow instead of persistDeferredUserText
// (re-persist); the QuietItem copy keeps the message recoverable when
// the session dies before the echo (DrainUnconfirmedFlushItems
// restores the draft from it and the death path deletes the persisted
// row). If the persist fails, the WHOLE transition is undone
// (restorePendingSendDeferred) — not just the claim: an entry left
// with QuietItem set but no persisted row would make a second
// interrupt take the quiet-promote path and bump a row that doesn't
// exist, so repeated interrupts could never make the message visible
// (round-6, R6-6). Restored entries retry the eager persist on the
// next interrupt and route their echo through the normal deferred
// persist.
//
// Each item is persisted via persistItem (UpsertItem + emit) so the
// frontend receives a provider:item_event upsert immediately.
//
// The confirmed hook (message anchor record) runs here for each
// persisted row, still under the thread's flush anchor lock: this is
// the rows' baseline record — fork/revert stays offered even if the
// session dies before the echo — and the echo later fills the anchor's
// provider ids at the true consumption boundary (round-7, R7-1). The
// record must commit before the mutex releases, or an echo in the gap
// would stamp the row while UpdateMessageAnchorProviderIDs no-ops
// against an anchor that doesn't exist yet, leaving it permanently
// without provider ids (round-4 review, CT4-1).
func (r *Router) EagerPersistDeferredFlushSends(threadID string, interruptedTurnIndex int, tok FlushStampToken) []EagerPersistedFlush {
	if threadID == "" || r.store == nil {
		return nil
	}
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()

	r.mu.Lock()
	// Whole-transition fence, not just the stamp write: a session
	// replacement recycles deterministic flush IDs, and death recovery
	// re-registers same-ID entries that a stale interrupt returning
	// post-ack must not claim, persist, or (Codex) re-send through the
	// dead session (round-11, CT11-1). Replacement registration can
	// only follow the death drain, which serializes behind this
	// function's anchor lock, so the epoch check at claim time covers
	// the store writes below too.
	if r.threadEpochs[threadID] != tok.threadEpoch {
		r.mu.Unlock()
		return nil
	}
	pending := r.pendingByThread[threadID]
	// The stamp re-write covers only entries REGISTERED between this
	// interrupt's pre-ack Mark and now — Mark already stamped everything
	// else. It is fenced by the mark token's stamp epoch: a concurrent
	// interrupt that Marked during the ack wait published a NEWER stamp
	// that this pass's post-ack value must not clobber (round-9, R9-5).
	stampCurrent := r.flushStampEpochs[threadID] == tok.stampEpoch
	type eagerSnapshot struct {
		item        store.Item
		queueItemID string
		enqueuedAt  int64
	}
	var snapshots []eagerSnapshot
	for i := range pending {
		if pending[i].DeferredItem != nil && pending[i].QueueItemID != "" {
			snapshots = append(snapshots, eagerSnapshot{
				item:        *pending[i].DeferredItem,
				queueItemID: pending[i].QueueItemID,
				enqueuedAt:  pending[i].EnqueuedAt,
			})
			pending[i].QuietItem = pending[i].DeferredItem
			pending[i].DeferredItem = nil
			pending[i].WasDeferred = true
			pending[i].AnchoredAtInterrupt = true
			if stampCurrent {
				pending[i].InterruptedTurnIndex = interruptedTurnIndex
			}
		}
	}
	confirmedHook := r.flushUserTextConfirmed
	r.mu.Unlock()

	if len(snapshots) == 0 {
		return nil
	}

	result := make([]EagerPersistedFlush, 0, len(snapshots))
	for _, snap := range snapshots {
		persisted, err := r.persistItemWithEmit(snap.item, nil, nil, true)
		if err != nil {
			log.Printf("triage: eager persist deferred flush %s/%s: %v", snap.item.ThreadID, snap.item.ID, err)
			r.restorePendingSendDeferred(threadID, snap.item.ID)
			continue
		}
		if confirmedHook != nil {
			confirmedHook(threadID, persisted)
		}
		result = append(result, EagerPersistedFlush{
			UserItemID:  persisted.ID,
			TurnIndex:   persisted.TurnIndex,
			ItemIndex:   persisted.ItemIndex,
			Content:     persisted.Summary,
			Meta:        persisted.Meta,
			QueueItemID: snap.queueItemID,
			EnqueuedAt:  snap.enqueuedAt,
		})
	}
	return result
}

// PromoteQuietFlushSends emits provider:item_event for any
// non-deferred flush sends already persisted in the store. Called on
// user interrupt so quietly-persisted flush messages transition from
// Zone 2 (queued marker) to the timeline immediately, rather than
// waiting for the provider echo. The pending send entries stay in the
// FIFO — the echo still stamps provider_item_id via
// attachProviderItemIDToUserRow.
//
// The whole claim + bump-and-mark runs under the thread's flush anchor lock, which the
// echo path (handleUserText) also holds across its pending-send pop and
// row write. That makes every popped snapshot truthful: an echo that
// consumed the entry BEFORE this promote's claim removes it from the
// pending list (the snapshot loop below skips it — consumed
// pre-promotion, not promoted-above-tail, correctly unmarked); an echo
// popping AFTER sees either a committed bump+marker (claim held —
// stamp-only, boundary derivable from the marker) or a
// failed-and-unclaimed entry (claim reverted — the echo's own bump is
// the fallback positioning). The mid-flight windows where a popped
// claim had no durable marker, or a failed bump could not be unclaimed
// because the entry was already consumed, cannot be observed (round-3
// review, cold-2/cold-3/CT-1/CT-4).
//
// The confirmed hook (message anchor record) runs here for each
// promoted row, still under the thread's flush anchor lock: this is
// the promoted rows' baseline record — fork/revert stays offered even
// if the session dies before the echo — and the echo later fills the
// anchor's provider ids at the true consumption boundary (round-7,
// R7-1). The record must commit before the mutex releases, or an echo
// in the gap would stamp the row while UpdateMessageAnchorProviderIDs
// no-ops against an anchor that doesn't exist yet, leaving it
// permanently without provider ids (round-4 review, CT4-1). Returns
// the promoted rows at their post-bump position.
//
// tok is the interrupt's pre-ack mark token; its thread reactivation
// epoch fences the whole claim + bump against a session replacement
// that re-registered same-ID entries during the ack wait — a stale
// interrupt must not promote (or anchor) the replacement's rows
// (round-11, CT11-1).
func (r *Router) PromoteQuietFlushSends(threadID string, tok FlushStampToken) []store.Item {
	if threadID == "" || r.store == nil {
		return nil
	}
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()

	r.mu.Lock()
	if r.threadEpochs[threadID] != tok.threadEpoch {
		r.mu.Unlock()
		return nil
	}
	pending := r.pendingByThread[threadID]
	var ids []string
	for i := range pending {
		// Already-anchored entries were promoted (and anchor-recorded) by
		// an earlier interrupt; a second interrupt before the echo must
		// not bump them again — the row's anchored position IS the
		// boundary a later fork/revert slices at, and a re-bump would
		// move it past content that arrived after the first interrupt.
		if pending[i].DeferredItem == nil && !pending[i].AnchoredAtInterrupt && strings.Contains(pending[i].AOItemID, ":flush:") {
			ids = append(ids, pending[i].AOItemID)
			pending[i].AnchoredAtInterrupt = true
		}
	}
	r.mu.Unlock()

	if len(ids) == 0 {
		return nil
	}

	r.mu.Lock()
	confirmedHook := r.flushUserTextConfirmed
	r.mu.Unlock()

	promoted := make([]store.Item, 0, len(ids))
	for _, id := range ids {
		// Bump and promotion marker land in ONE transaction: the marker is
		// the durable twin of the AnchoredAtInterrupt claim — the
		// interrupted round's trailing rows will persist BELOW this row,
		// yet precede it in the provider transcript (Claude appends the
		// queued_command attachment only at consumption), and revert /
		// fork-from-message read the marker to cut at the message in
		// PROVIDER order. A bump whose marker didn't stick would hand those
		// paths a repositioned row with display-order semantics.
		item, err := r.store.BumpItemToTurnEnd(threadID, id, itemmeta.MarkPromotedAtInterrupt, time.Now().UnixMilli())
		if err != nil {
			log.Printf("triage: promote quiet flush %s/%s: %v", threadID, id, err)
			r.unclaimPendingSendAnchor(threadID, id)
			continue
		}
		r.emitItemUpsertWithActivity(item, false)
		if confirmedHook != nil {
			confirmedHook(threadID, item)
		}
		promoted = append(promoted, item)
	}
	return promoted
}

// rebumpAnchoredQuietSiblings restores FIFO order after a promoted
// echo re-bumped its row over freshly drained content: later-FIFO
// anchored quiet rows in the same turn were positioned below the
// echoed row at promote time and now sit below the drained rows too —
// both inversions of provider order (all interrupted-tail content
// precedes every queued message's attachment, and the siblings'
// echoes come after this one). Bumping each sibling in FIFO order
// puts the layout back to drained-content < echoed row < siblings,
// and lifts the siblings past the revert cut at the echoed message
// (the user-row predicate cuts item_index >= anchor) to match the
// session slice, which removes them (round-7, R7-2).
//
// WasDeferred entries are skipped: their rows live in their own fresh
// turns, so same-turn drained content cannot reorder them. Per-sibling
// failures log loudly, record the repair obligation on the entry
// (pendingSend.NeedsTailRebump — the sibling's own echo forces the
// tail bump an anchored row otherwise skips), and continue. Caller
// holds the thread's flush anchor lock, so no promote or pop can
// interleave.
func (r *Router) rebumpAnchoredQuietSiblings(threadID, echoedItemID string, turnIndex int, now int64) {
	r.mu.Lock()
	var siblingIDs []string
	for _, entry := range r.pendingByThread[threadID] {
		if entry.AOItemID == echoedItemID || entry.WasDeferred || !entry.AnchoredAtInterrupt {
			continue
		}
		if entry.QuietItem == nil || entry.QuietItem.TurnIndex != turnIndex {
			continue
		}
		if !strings.Contains(entry.AOItemID, ":flush:") {
			continue
		}
		siblingIDs = append(siblingIDs, entry.AOItemID)
	}
	r.mu.Unlock()
	for _, id := range siblingIDs {
		item, err := r.store.BumpItemToTurnEnd(threadID, id, nil, now)
		if err != nil {
			log.Printf("triage: re-bump anchored flush sibling %s/%s over drained rows: %v", threadID, id, err)
			r.markSiblingNeedsTailRebump(threadID, id)
			continue
		}
		r.emitItemUpsertWithActivity(item, false)
	}
}

// markSiblingNeedsTailRebump records the repair obligation for a
// sibling whose re-bump failed — see pendingSend.NeedsTailRebump.
// Idempotent; a no-op when the entry was consumed in the meantime
// (impossible while the caller holds the flush anchor lock, but the
// scan is cheap insurance either way).
func (r *Router) markSiblingNeedsTailRebump(threadID, aoItemID string) {
	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	for i := range pending {
		if pending[i].AOItemID == aoItemID {
			pending[i].NeedsTailRebump = true
			break
		}
	}
	r.mu.Unlock()
}

// restorePendingSendDeferred undoes EagerPersistDeferredFlushSends'
// deferred→quiet transition after its store persist failed: the
// retained copy moves back to DeferredItem, and the WasDeferred /
// AnchoredAtInterrupt claims clear. The entry is then
// indistinguishable from a never-eager-persisted deferred send — a
// second interrupt retries the persist instead of quiet-promoting a
// row that doesn't exist, and the echo routes through
// persistDeferredUserText (round-6, R6-6). InterruptedTurnIndex is
// deliberately KEPT: the interrupt that failed to persist still cut
// its turn, and the echo's predecessor settle needs that fact.
// Runs under the anchor lock held by the caller, so no echo can
// observe the mid-restore entry.
func (r *Router) restorePendingSendDeferred(threadID, aoItemID string) {
	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	for i := range pending {
		if pending[i].AOItemID == aoItemID {
			if pending[i].QuietItem != nil {
				pending[i].DeferredItem = pending[i].QuietItem
				pending[i].QuietItem = nil
			}
			pending[i].WasDeferred = false
			pending[i].AnchoredAtInterrupt = false
			break
		}
	}
	r.mu.Unlock()
}

// MarkFlushSendsInterrupted stamps interruptedTurnIndex onto every
// unconsumed pending flush entry for threadID and returns each stamped
// entry's PREVIOUS value keyed by AOItemID. Called by the interrupt
// paths BEFORE awaiting the provider's interrupt ack: the read loop
// keeps settling wire events during that wait, so the CLI's mid-loop
// queue drain can consume an entry before the post-ack eager persist
// ever sees it — the echo would then settle the cut turn "end_turn"
// instead of "interrupted" (round-6, R6-4). Deferred-ORIGIN entries
// (WasDeferred, eager-persisted by an EARLIER interrupt, awaiting
// echo) need the refresh too, not just still-deferred ones: a second
// interrupt cuts the first prompt's response turn, and a stale
// first-interrupt stamp would settle that turn "end_turn" with the
// settlement claim blocking the truncated result from correcting it
// (round-7, R7-5).
//
// FlushStampToken fences one MarkFlushSendsInterrupted call's
// follow-ups — RestoreFlushSendsInterrupted on a failed interrupt and
// the post-ack eager stamp — against concurrent interrupts (stamp
// epoch) and session replacement (thread reactivation epoch: death
// recovery drains the FIFO and deterministic flush IDs let a
// replacement session re-register same-ID entries that a stale
// follow-up must not touch — round-9, R9-6). Opaque to callers.
type FlushStampToken struct {
	prev        map[string]int
	stampEpoch  uint64
	threadEpoch uint64
	// seq uniquely identifies the Mark call. Stamp epochs are REUSED
	// after restores step them back, so epoch alone cannot tell a live
	// token from a duplicate restore of an already-applied one — a
	// duplicate would park and later chain-apply over a fresh mark
	// sharing the recycled epoch (round-10, R10-3). Also keys this
	// Mark's interruptMarks entry, which a failed interrupt's restore
	// removes (round-11, R11-3).
	seq uint64
}

// interruptMark is one live MarkFlushSendsInterrupted call in a
// thread's interruptMarks list: the turn it named as interrupted,
// keyed by the call's token seq so a failed interrupt's restore can
// remove exactly its own entry regardless of restore order.
type interruptMark struct {
	seq  uint64
	turn int
}

// When the interrupt request itself fails, the caller passes the
// returned token to RestoreFlushSendsInterrupted — restore, not a flat
// -1 clear: entries re-stamped by this call may carry a PRIOR
// interrupt's still-valid stamp that a wipe would destroy. The token's
// stamp epoch (bumped only when at least one entry was stamped) fences
// the restore against interrupts that are not serialized with each
// other: a stop press racing the revert path's interrupt (round-8,
// R8-2).
func (r *Router) MarkFlushSendsInterrupted(threadID string, interruptedTurnIndex int) FlushStampToken {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.pendingByThread[threadID]
	var prev map[string]int
	for i := range pending {
		if pending[i].QueueItemID == "" {
			continue
		}
		if prev == nil {
			prev = make(map[string]int)
		}
		prev[pending[i].AOItemID] = pending[i].InterruptedTurnIndex
		pending[i].InterruptedTurnIndex = interruptedTurnIndex
	}
	if prev != nil {
		r.flushStampEpochs[threadID]++
	}
	r.flushStampSeq++
	// The thread-level mark list gets an entry even when nothing was
	// stamped: an entry the CLI's queue drain popped just before this
	// call is mid-persist with a -1 stamp retained, and the list is the
	// only way its openQueuedEchoTurn can still learn which turn the
	// interrupt cut (round-10, R10-6).
	r.interruptMarks[threadID] = append(r.interruptMarks[threadID], interruptMark{
		seq:  r.flushStampSeq,
		turn: interruptedTurnIndex,
	})
	return FlushStampToken{
		prev:        prev,
		stampEpoch:  r.flushStampEpochs[threadID],
		threadEpoch: r.threadEpochs[threadID],
		seq:         r.flushStampSeq,
	}
}

// RestoreFlushSendsInterrupted reverts MarkFlushSendsInterrupted after
// a failed interrupt request, writing back each entry's previous stamp.
// Entries consumed in the meantime are gone from the FIFO and skipped —
// their echo already settled under the tentative stamp, the safe side
// of that race (the provider provably drained them mid-interrupt).
//
// The restore applies only while the token's stamp epoch is still the
// thread's current one: a concurrent interrupt that Marked after this
// call re-stamped every surviving entry, and its stamp is live — a
// succeeded later interrupt's provenance must not be reverted by an
// earlier call's failure. A restore arriving under a newer epoch is
// PARKED instead of dropped, and an applied restore steps the epoch
// back and chains through parked epochs — overlapping interrupts that
// all fail unwind fully regardless of failure order, while an unwind
// parked under a succeeded interrupt's epoch stays (correctly)
// unreachable (round-9, R9-2). A token from before a session
// replacement (thread reactivation epoch moved) no-ops entirely, and
// stale-generation parked unwinds are dropped when the chain reaches
// them (round-9, R9-6).
func (r *Router) RestoreFlushSendsInterrupted(threadID string, tok FlushStampToken) {
	if tok.seq == 0 {
		// Zero-value token — no Mark ever ran for it.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.threadEpochs[threadID] != tok.threadEpoch {
		return
	}
	if _, applied := r.flushStampApplied[tok.seq]; applied {
		// Duplicate restore of a token that already applied: parking it
		// would let a recycled epoch chain-apply it over a fresh mark's
		// live stamp (round-10, R10-3).
		return
	}
	// The thread-level mark list unwinds independently of the stamp
	// epochs — Mark appends even when it stamped no entries, so this
	// must run before every stamp-path early return (round-10, R10-6).
	// Removal is seq-keyed, so overlapping failed interrupts unwind
	// correctly in any restore order (round-11, R11-3).
	r.removeInterruptMarkLocked(threadID, tok.seq)
	if len(tok.prev) == 0 {
		return
	}
	if r.flushStampEpochs[threadID] != tok.stampEpoch {
		stash := r.flushStampUnwinds[threadID]
		if stash == nil {
			stash = make(map[uint64]FlushStampToken)
			r.flushStampUnwinds[threadID] = stash
		}
		stash[tok.stampEpoch] = tok
		return
	}
	r.applyFlushStampRestoreLocked(threadID, tok)
	stash := r.flushStampUnwinds[threadID]
	for {
		current := r.flushStampEpochs[threadID]
		parked, ok := stash[current]
		if !ok {
			return
		}
		delete(stash, current)
		if parked.threadEpoch != r.threadEpochs[threadID] {
			return
		}
		if _, applied := r.flushStampApplied[parked.seq]; applied {
			return
		}
		r.applyFlushStampRestoreLocked(threadID, parked)
	}
}

// applyFlushStampRestoreLocked writes back one Mark call's previous
// stamps, steps the thread's stamp epoch below it, records the token as
// consumed, and removes the Mark's interrupt-mark entry (chained parked
// tokens reach here without passing the direct path's removal). Caller
// holds r.mu and has verified the epoch is current and the token
// unapplied.
func (r *Router) applyFlushStampRestoreLocked(threadID string, tok FlushStampToken) {
	pending := r.pendingByThread[threadID]
	for i := range pending {
		if value, ok := tok.prev[pending[i].AOItemID]; ok {
			pending[i].InterruptedTurnIndex = value
		}
	}
	r.flushStampEpochs[threadID] = tok.stampEpoch - 1
	r.flushStampApplied[tok.seq] = struct{}{}
	r.removeInterruptMarkLocked(threadID, tok.seq)
}

// removeInterruptMarkLocked deletes seq's entry from the thread's
// interrupt-mark list, wherever it sits — a failed interrupt withdraws
// exactly its own claim, leaving every other live mark in place, so
// overlapping failures unwind correctly in any restore order.
// Idempotent: a duplicate removal finds nothing. Caller holds r.mu.
func (r *Router) removeInterruptMarkLocked(threadID string, seq uint64) {
	marks := r.interruptMarks[threadID]
	for i := range marks {
		if marks[i].seq == seq {
			r.interruptMarks[threadID] = append(marks[:i], marks[i+1:]...)
			return
		}
	}
}

// unclaimPendingSendAnchor reverts an AnchoredAtInterrupt claim whose
// store write failed, restoring the echo-time bump as the row's
// fallback positioning. No-op when the entry was consumed in the
// meantime — the echo already handled the row under the claimed
// (stamp-only) semantics, which is the safe side of the race.
func (r *Router) unclaimPendingSendAnchor(threadID, aoItemID string) {
	r.mu.Lock()
	pending := r.pendingByThread[threadID]
	for i := range pending {
		if pending[i].AOItemID == aoItemID {
			pending[i].AnchoredAtInterrupt = false
			break
		}
	}
	r.mu.Unlock()
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
	// Anchor lock so the removal is total against an in-flight echo's
	// failure reinsert (round-5, R5-2).
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()
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
