package triage

import (
	"sync"

	"agent-overflow/internal/provider"
	"go.opentelemetry.io/otel/trace"
)

// threadState is the Router's per-thread correlation state — everything
// cleanupThread used to delete one map at a time. The whole struct is
// dropped in a single `delete(r.threads, threadID)`, which is what makes
// the sweep complete BY CONSTRUCTION: a new field added here cannot be
// forgotten by a cleanup path, because there is no per-field cleanup path
// left to forget it in.
//
// Every field keeps the ownership comment it carried as a Router map;
// those comments are the review record for why the state exists and when
// it may be dropped, and they move with the field, not with the file.
//
// Lock: r.mu guards every field, exactly as it guarded the maps these
// came from. There is deliberately no per-thread mutex here — the two
// per-thread LOCKS the router does own (flush anchor, drain) live on
// threadIdentity, because losing them at cleanup would split their
// serialization domain.
//
// Keys: fields that used to be composite-keyed maps (threadID|scope,
// threadID:itemID, ...) are re-keyed on the INNER key only. The thread
// dimension is the map this struct hangs off, so a per-thread sweep is a
// single delete instead of a full-map prefix scan.
type threadState struct {
	// openTurn is the turn index currently open on the thread;
	// openTurnSet distinguishes "turn 0 is open" from "no open turn"
	// (the map's comma-ok).
	openTurn    int
	openTurnSet bool

	// interruptQueue holds persistence work deferred behind an active
	// stream (invariant 11), drained once the thread stops streaming.
	interruptQueue []queuedPersistence

	// streamingItemCount is the thread-wide count of streaming rows that
	// have not finished settling; it gates the interrupt-queue DRAIN.
	streamingItemCount int
	// streamingScopeCounts mirrors streamingItemCount at SCOPE
	// granularity (key: scope). The thread-wide counter gates the
	// interrupt-queue DRAIN (drain once the whole thread is idle); the
	// scoped counter gates the QUEUE decision so a new row defers
	// (invariant 11) only behind a SAME-scope stream. A main-scope
	// completion must not queue behind a concurrent subagent-scope stream.
	streamingScopeCounts map[string]int

	// Id-allocating counters, keyed by scopeCounterKey(turnIndex, scope).
	// These allocate thread-lifetime `items.id` values, so they are
	// cleaned at session teardown, not at turn boundaries.
	segmentIndexByScope  map[string]int
	blockIndexByScope    map[string]int
	errorSeqByScope      map[string]int
	compactionSeqByScope map[string]int
	// timelineSeqByScope allocates per-(turn, label) ids for the timeline
	// rows whose wire envelope carries no usable provider id of its own —
	// notifications, and command results whose synthetic envelope omitted
	// `message.id`. The label dimension keeps those namespaces apart; go
	// through nextScopeSequence rather than indexing it directly.
	timelineSeqByScope map[string]int

	// Active streaming block bookkeeping, keyed by
	// activeStreamKey(turnIndex, scope, providerItemID).
	activeTextBlocks        map[string]bool
	activeThinkingBlocks    map[string]bool
	activeTextBlockRefs     map[string]activeStreamBlock
	activeThinkingBlockRefs map[string]activeStreamBlock

	// streamPersistBuffers decouple the live UI stream from durable
	// history writes. Text/thinking deltas emit immediately on ordered
	// provider:item_event deltas, then flush to SQLite by interval, byte
	// threshold, or lifecycle boundary. Codex command-output deltas
	// buffer BOTH the SQLite append and the wire-visible item upsert
	// (no per-chunk delta channel exists for command rows). Keyed by
	// itemID.
	//
	// Lock order for the buffers: r.streamFlushMu is taken FIRST and
	// r.mu nested inside it (every flush funnel does exactly this —
	// flushStreamPersistenceKey, flushStreamingItem,
	// flushStreamingThread, flushAllStreamPersistence, and the
	// replaceCommandOutput rewrite). Never acquire streamFlushMu while
	// holding r.mu.
	//
	// streamFlushMu and the per-thread drain lock
	// (threadIdentity.drainLock) are DISJOINT: no path holds one while
	// acquiring the other, and neither is ordered against the other.
	// The near-miss is doSettleStreamingText, which flushes this buffer
	// under streamFlushMu and then drains the interrupt queue under the
	// drain lock — but the drain is a `defer r.finishSettle(...)`, so it
	// runs after flushStreamingItem has already released streamFlushMu,
	// and the drain's own persistItem path never reaches a flush funnel.
	// cleanupThread has the same shape in the other direction: it calls
	// flushStreamingThread (streamFlushMu, released on return) BEFORE it
	// takes the flush anchor. Keep it that way — nesting them would
	// create the first order to get wrong.
	streamPersistBuffers map[string]*streamPersistBuffer
	// streamingPathRefsLast carries the live-stream pathRefs state per
	// streaming assistant_text row: the incremental pathlinks scanner
	// (regex only over each flush's appended tail; stat re-validation
	// of all known candidates per tick — the full rescan used to make
	// total scan work quadratic in message length) plus the dedupe
	// snapshot that lets unchanged windows skip the meta JSON
	// round-trip, the SQLite UPDATE, and the action:"meta" emission.
	// Keyed by itemID; cleared at doSettleStreamingText and
	// clearActiveStreamBlocksForTurnLocked so a torn-down streaming row
	// can't leak its last-seen state into the next turn.
	streamingPathRefsLast map[string]*streamingPathRefsState

	// settledTurns marks turns whose handleTurnComplete has already run
	// to completion (turns row UPDATE-d, streaming items settled). A
	// second EventTurnComplete for a settled turn is the
	// multi-result-per-turn wire pattern (Claude CLI synthesizes a
	// `type:"user"` envelope from a task_notification → second `result`
	// envelope) or the synthetic-truncate-then-real race; in either
	// case the second handler invocation is a persistence no-op so
	// the turns row isn't re-stamped. Cleared by setOpenTurn (so a
	// re-init can re-settle the same turn) and by MarkThreadActive.
	// Key = turnIndex.
	//
	// Note: this gate operates at LOGICAL-TURN granularity. The
	// frontend-facing `provider:turn_completed` emission is gated
	// independently per WIRE ROUND via currentRound/takeOpenRound below —
	// so a multi-result-per-turn cascade emits one turn_completed per
	// `result` envelope while persistence stays at one settle per
	// logical turn.
	settledTurns map[int]bool

	// currentRound names the active wire-round for the thread.
	// Frontend `provider:turn_started` / `provider:turn_completed`
	// emissions are gated per round via this slot — handleTurnStart and
	// the re-round branch of handleInit allocate a fresh round id;
	// handleTurnComplete reads-and-clears it via takeOpenRound. A wire
	// round corresponds to one Claude `result` envelope (or one Codex
	// `turn/completed`); a logical agent-overflow turn can span multiple
	// rounds when Claude's CLI synthesizes a `type:"user"` envelope from
	// a task_notification and the model issues another response. The
	// per-round cadence is what drives the working indicator, Stop
	// button, and composer-block state — all of which want "model is
	// engaged right now" semantics rather than "user-typed prompt is in
	// flight." Cleared by takeOpenRound (every wire complete).
	currentRound     ActiveTurnSnapshot
	currentRoundOpen bool

	// effectiveModel is the session-scoped model actually serving the
	// thread after a provider fallback. The durable threads.model remains
	// the user's requested model; this live projection is cleared with the
	// provider session and included in GetThreadLiveState hydration. Its
	// monotonic revision counter lives on threadIdentity — it must
	// survive the session that produced it.
	effectiveModel    string
	effectiveModelSet bool

	// tasks is the per-thread mirror of the Claude Task* family task
	// list. Survives any number of Parser recreations within the process
	// lifetime so a TaskUpdate against an id created before session
	// resume still routes correctly. Bounded by maxTasksPerThread on
	// insert (cap-and-reject).
	tasks *threadTasks

	// turnSpan holds the active span for the in-flight turn so we can
	// close it when the matching EventTurnComplete arrives. Its
	// generation counter lives on threadIdentity: resetting one would let
	// an old in-flight generation match a later session that reused the
	// thread id.
	turnSpan trace.Span

	// codexBackground is the Codex-specific background-terminal projector
	// state. Tracks inProgress unifiedExec items + spawn_agent rows with
	// running children so we can stamp is_background=true on the first
	// wire-typed yield signal (text / reasoning delta) or at
	// turn/completed (the catchall). See codex_background.go for the
	// lifecycle details and invariant 25 for the wire-typed-signal rule
	// this implements. A restarted session never inherits trackers from a
	// prior session — the wire replays item/started for still-inProgress
	// items and the projector re-observes them fresh.
	codexBackground *codexBackgroundState

	// terminalInteractionSeq counts terminal interaction carriers per
	// (turn, processID). Empty wait polls may reuse the latest carrier
	// for that process; forwarded-stdin interactions always take the next
	// id. Bounded the same way other id-allocating counters are: retained
	// across clearOpenTurn to avoid multi-result id collisions and swept
	// at session teardown / selective turn re-init reset. See
	// terminal_interaction.go for the handler.
	terminalInteractionSeq map[string]int

	// openAPIRetryRow flags a thread whose current api_retry row is in
	// status=running and therefore eligible to flip on the next
	// forward-progress event. The hot streaming path
	// (maybeMarkAPIRetryCompleted, called per text/thinking/tool event)
	// short-circuits when the flag is unset so the common case avoids
	// a SQLite GetThreadItem on every text delta. Set in handleAPIRetry
	// when persisting a running row; cleared after the flip completes
	// or when the turn closes via clearOpenTurn.
	openAPIRetryRow bool

	// pendingSends is the FIFO of AO-initiated user sends awaiting wire
	// confirmation. Triage's send path appends an entry when it dispatches
	// a user message; the matching wire EventUserText pops the head.
	// Bounded by user attention (typically 0-1 entries per thread).
	// Lifecycle: user-send-time carry-over — swept at cleanup as a safety
	// net. See pending_send.go.
	pendingSends []pendingSend
	// wireOnlyUserTextSeen dedupes wire EventUserText events that don't
	// match any pending AO send (the "agent prompted itself" or
	// session-resume replay case). Set of providerItemIDs already seen.
	wireOnlyUserTextSeen map[string]struct{}

	// queuedFlushItems is the "queued user message awaiting provider
	// boundary" state. Populated when the user types into the composer
	// mid-turn and submits; drained when no top-level foreground tool or
	// queue-blocking background task remains (watch tasks — Monitor,
	// meta.watch_task — never block the drain; see
	// HasQueueBlockingBackgroundToolCall). Lifecycle: spans turn
	// boundaries by design, so NOT swept by clearOpenTurn — only at
	// session teardown. See flush_queue.go.
	queuedFlushItems []QueuedFlushItem

	// pendingWakeupAt is the fire time (epoch ms) of the Claude harness's
	// pending ScheduleWakeup timer. The timer is in-process CLI state with
	// no task lifecycle, so this field is the only record that an
	// idle-looking session will resume itself; the idle-session reaper
	// reads it via PendingWakeupAt. Session-scoped: cleared at teardown
	// AND by MarkThreadActive (a replacement process never inherits the
	// timer). See session_wakeup.go.
	pendingWakeupAt  int64
	pendingWakeupSet bool

	// subagentProgress is the latest live progress tick per launch
	// tool_use, keyed by itemID. Live UI state only; the terminal path
	// consumes it into the launch row's meta. Swept with pendingWakeupAt
	// (same session-scoped lifetime). See subagent_progress.go.
	subagentProgress map[string]provider.SubagentProgressMeta

	// pendingToolCorrelations holds correlation metadata (task_id /
	// subagent_model / parent_tool_use_id) from a metaUpdateOnly
	// EventToolStart that arrived BEFORE its tool_call row was persisted,
	// keyed by tool_use_id. Claude emits `system/task_started` on the
	// main wire the moment ANY agent — including a nested async
	// subagent — backgrounds a shell, but the launch row for a
	// subagent-owned Bash only lands later, when the subagent transcript
	// projection catches up. Dropping the update there permanently
	// strips the row of its task_id: no Stop button, no terminal
	// correlation, a tray zombie. Held entries are applied and cleared
	// by persistToolCallLaunch's create/update path. Bounded by
	// maxPendingToolCorrelationsPerThread; swept with the threadState.
	pendingToolCorrelations map[string]itemMetaCorrelationFields

	// carrierRoots maps a §E6 resume CARRIER's tool_use id to the
	// transcript ROOT it is a lifecycle row for, so Handle can rewrite a
	// carrier-parented live event onto the root before any handler sees
	// it. Populated wherever transcriptRoot resolves (the keep-running
	// flip, the terminal replay, the resume prompt row). Session-scoped:
	// the durable answer is the carrier row's own `transcript_root_id`
	// stamp, so losing this map costs a lookup, never correctness.
	// Bounded by maxCarrierRootsPerThread; swept with the threadState.
	carrierRoots map[string]string

	// compactingSince is the open compacting window: the epoch-ms
	// timestamp of the frame that opened it. Live-only projection behind
	// `provider:compacting` (compaction_status.go); closed by the explicit
	// close frame, the compact boundary, or turn completion, and swept at
	// teardown AND by MarkThreadActive (a replacement process is never
	// mid-compaction).
	compactingSince    int64
	compactingSinceSet bool

	// commandLifecycle correlates Claude's `command_lifecycle` acks back
	// to the AO row and the wire round each stdin user message was queued
	// into, keyed by command_uuid. Send-time carry-over, released on the
	// terminal ack and swept at teardown; bounded per thread by
	// maxCommandLifecycleEntriesPerThread. See command_lifecycle.go for
	// why neither value can be recovered lazily.
	commandLifecycle map[string]commandLifecycleEntry

	// workspacePath is a small read-through cache for the thread row's
	// WorkspacePath, populated lazily by path-ref enrichment (the only hot
	// caller). A thread's workspace is set at create time and effectively
	// immutable, so the cache is safe without invalidation beyond session
	// teardown. Without it, every assistant_text settle ran a SQLite
	// GetThread JUST to read a stable string — fine on its own, but adds
	// up across the 10-30 text blocks per heavy turn.
	workspacePath    string
	workspacePathSet bool

	// revertedTurn marks a thread whose next provider:turn_completed
	// emission should carry RevertedUserMessage=true. Set by the App
	// layer's revert-on-interrupt path BEFORE it tears down the session;
	// consumed (read-and-clear) inside buildRoundCompletedEvent.
	// Defensively cleared by clearOpenTurn too, so a stale flag never
	// leaks into a future turn. See revert_marker.go.
	revertedTurn bool

	// usageEmitThrottle rate-limits provider:usage emissions to at most
	// one per usageEmitMinInterval. The context-window meter changes
	// gradually during streaming; Claude can fire 10-50 token usage
	// events/second but the UI doesn't benefit from updates faster than
	// ~2/sec. The pending window is flushed on turn-complete and at
	// teardown so the final reading always reaches the frontend.
	usageEmitThrottle *usageEmitThrottle

	// pendingCommandDiffs holds command-execution inline-diff capture
	// state awaiting its matching tool result, keyed by itemID.
	pendingCommandDiffs map[string]pendingCommandInlineDiff

	// Interactive-request state, keyed by requestID, plus the display
	// order each family renders in. Cleaned at the correlated resolver or
	// at turn/session boundaries.
	pendingApprovals      map[string]pendingApprovalState
	pendingApprovalOrder  []string
	pendingApprovalItems  map[string]string
	pendingUserInputs     map[string]provider.UserInputRequest
	pendingUserInputOrder []string
}

// threadIdentity is per-thread state that must SURVIVE cleanupThread.
// Everything here is either a monotonic epoch/generation counter whose
// reset would let a stale captured value match a fresh session, or a
// per-thread lock whose replacement would silently split a serialization
// domain. Entries are never deleted — that is the whole point of the
// type, and it is why identities are stored in their own map rather than
// as a field on threadState.
//
// Locking: the identities MAP is guarded by r.identitiesMu, a LEAF lock
// that may be taken while holding anything else (it takes nothing). The
// two mutexes below guard themselves. Every other field is guarded by
// r.mu, exactly as its Router map was.
type threadIdentity struct {
	// anchorLock serializes, PER THREAD, every mutation of that thread's
	// pending-send state that pairs with a store write: the echo-time pop
	// + attach/deferred-persist (handleUserText), the interrupt-time
	// bump-and-mark / transition-and-persist (+ in-lock anchor record) in
	// PromoteQuietFlushSends and EagerPersistDeferredFlushSends, the
	// session-death drain (DrainUnconfirmedFlushItems), and the
	// pending-send sweeps (cleanupThread, ClearPendingSendsByItemIDs,
	// ClearPendingSendForFailure). r.mu alone cannot do this: the
	// interrupt paths must release it before their store writes, and an
	// echo popping in that window reads a pendingSend snapshot whose
	// AnchoredAtInterrupt claim is not yet — or never becomes — durable
	// (round-3 review, cold-1/2/3); a sweep in the same window loses to
	// the echo-failure reinsert (round-5, R5-2). Holding the anchor lock
	// across pop AND write makes every popped snapshot truthful, and
	// sweeps total.
	//
	// Per-thread — NOT router-wide — because the confirmed hook (message
	// anchor record) runs inside the lock: one thread's slow record must
	// not block every other thread's echoes, interrupts, and teardowns
	// (round-5, R5-1). Lock order: the anchor lock is always taken BEFORE
	// r.mu, never while holding it. It lives on the never-deleted
	// identity because dropping it would hand a fresh acquirer a NEW mutex
	// while a waiter still holds the old one, silently splitting the
	// serialization domain.
	anchorLock sync.Mutex
	// drainLock serializes, PER THREAD, an interrupt-queue drain's full
	// pop + persist span. r.mu alone covers only the map pop: a
	// settle-goroutine drain releases it before persisting the handed-off
	// rows, and the promoted-echo boundary path checking
	// hasQueuedInterruptItems in that window sees an empty queue while
	// rows are still uncommitted — they then land above the sampled
	// boundary and a revert cuts them as "response" although the session
	// slice keeps them (round-7, R7-3). Queue APPENDS need no covering:
	// they happen only on the serial provider read loop, which is busy
	// running the echo. Lock order: taken after the thread's flush anchor
	// (echo path), before r.mu; never replaced (same reasoning as
	// anchorLock).
	drainLock sync.Mutex

	// claimedFlushItems counts batch items mid-handoff between the
	// queue delete in tryFlushQueue and the dispatcher's synchronous
	// in-flight record. Folded into QueuedFlushItemCount so the
	// revert-on-interrupt predicate sees a draining batch as queued →
	// claimed → in-flight, never invisible (round-14 close-out, C14-1).
	// Held only across the dispatcher callback; tryFlushQueue's deferred
	// drop always runs and clamps at zero.
	//
	// On the never-deleted identity, NOT threadState, because the claim
	// must survive cleanupThread: the dispatcher callback runs outside
	// r.mu, so a session teardown can sweep the thread mid-handoff. On
	// deletable state that sweep vanished the claim (count lied to the
	// revert predicate) and a successor session's fresh claim could then
	// be eaten by the old dispatch's deferred decrement (campaign review
	// 2026-08-25, codex finding 1). Pre-split this was a Router-level map
	// that cleanup deliberately never touched; this placement restores
	// that lifetime. Guarded by r.mu, exactly as that map was.
	claimedFlushItems int

	// epoch counts MarkThreadActive calls. An asynchronous teardown
	// captures the epoch before unregistering a dead session and hands it
	// to CleanupThreadIfEpoch, which no-ops when the epoch has moved —
	// i.e. the host committed to a replacement session while the teardown
	// goroutine was still in flight. Never reset (a reset to 0 would let a
	// stale captured 0 match); growth is bounded by the number of distinct
	// threads that start a session in this process's lifetime.
	epoch uint64

	// stopped remembers that CleanupThread explicitly stopped the thread.
	// While set, Handle drops events that would persist to the store so
	// late-arriving readLoop lines from the torn-down subprocess do not
	// leave orphan rows on the stopped thread (Bug B5). Cleared ONLY by
	// the host's session-start path via MarkThreadActive — never by a wire
	// event. A session that dies before emitting anything recognizable
	// (e.g. Claude failing its --resume-session-at validation pre-init)
	// must still have its error results routed, so the host declares the
	// thread active when it commits to a replacement session rather than
	// waiting for proof of life from the wire. Host-synthesized events
	// bypass the flag via HandleSynthetic. It is set BY cleanup, which is
	// exactly why it cannot live on threadState.
	stopped bool

	// turnSpanGeneration prevents a slow or reentrant span start from
	// registering after a newer start or terminal cleanup won the thread.
	// Monotonic for the process lifetime and never reset: resetting it
	// would let an old in-flight generation match a later session that
	// reused the thread id.
	turnSpanGeneration uint64

	// effectiveModelRevision is monotonic for the process lifetime. Set /
	// clear emissions can cross between provider and teardown goroutines;
	// the revision lets the frontend reject stale delivery — which only
	// works if it outlives the session whose teardown emitted the clear.
	effectiveModelRevision uint64

	// flushStampEpoch counts the MarkFlushSendsInterrupted calls that
	// stamped at least one entry. Interrupt paths are not serialized
	// against each other (a stop press can race the revert path's
	// interrupt), so a failed interrupt's RestoreFlushSendsInterrupted
	// only applies while its own epoch is still current — a newer Mark's
	// live stamp must not be clobbered by an older call's failure. Never
	// reset: a cleanup reset would let a stale pre-cleanup token match a
	// fresh epoch.
	flushStampEpoch uint64
	// flushStampUnwinds parks, by epoch, a restore token that arrived
	// while a newer Mark's stamp was live. An applied restore chains down
	// through parked epochs so overlapping interrupts that ALL fail unwind
	// fully regardless of failure order; an unwind parked under an epoch
	// whose interrupt succeeded is permanently unreachable — the succeeded
	// stamp stays, which is correct — and its entry is the map's only
	// (negligible) growth. Parked tokens from before a session replacement
	// are dropped when the chain reaches them (thread epoch mismatch).
	flushStampUnwinds map[uint64]FlushStampToken

	// interruptMarks records every live MarkFlushSendsInterrupted call as
	// {seq, interrupted turn} in call order. The per-entry stamp cannot
	// cover an echo whose pending entry was POPPED before the mark ran but
	// is still persisting — openQueuedEchoTurn reads the newest entry's
	// turn so that in-flight echo still settles the cut turn "interrupted"
	// (round-10, R10-6). Appended even when the mark stamped no FIFO
	// entries (the popped entry is exactly why the FIFO can be empty). A
	// failed interrupt's restore removes ITS entry wherever it sits
	// (seq-keyed, so overlapping failures unwind correctly in any order —
	// round-11, R11-3); a succeeded interrupt's entry lingers until
	// MarkThreadActive clears the list — an in-flight echo cannot survive
	// session replacement, and a replacement after revert REUSES turn
	// indexes, so a cross-session record would mislabel a reused index
	// (round-11, CT11-2).
	//
	// It lives HERE, not on threadState, because cleanupThread
	// deliberately does not drop it: a teardown's own synthesized
	// truncated turn-complete runs before the sweep and an echo still
	// persisting must keep reading the mark. MarkThreadActive is the one
	// clear, and it is explicit for that reason.
	interruptMarks []interruptMark
}

// state returns the thread's mutable correlation state, creating it on
// first use. Callers must hold r.mu. Use this on WRITE paths only —
// a read path must not mint state for a thread that has none, or every
// live-state query for an idle thread would leak an entry (see
// threadStateIfPresent).
func (r *Router) state(threadID string) *threadState {
	if st := r.threads[threadID]; st != nil {
		return st
	}
	st := &threadState{}
	r.threads[threadID] = st
	return st
}

// threadStateIfPresent returns the thread's correlation state, or nil
// when the thread has none. Callers must hold r.mu. Read paths use this
// so that querying an unknown thread stays allocation-free and cannot
// grow the map — the nil result reads exactly like the zero values the
// old per-field maps returned for a missing key.
func (r *Router) threadStateIfPresent(threadID string) *threadState {
	return r.threads[threadID]
}

// identity returns the thread's never-deleted identity record, creating
// it on first use. r.identitiesMu is a leaf lock: it may be taken while
// holding r.mu (the epoch/generation readers do) and also with no lock
// held at all (the anchor/drain lock lookups, which must run BEFORE r.mu
// per the documented lock order).
func (r *Router) identity(threadID string) *threadIdentity {
	r.identitiesMu.Lock()
	defer r.identitiesMu.Unlock()
	if id := r.identities[threadID]; id != nil {
		return id
	}
	id := &threadIdentity{}
	r.identities[threadID] = id
	return id
}

// identityIfPresent returns the thread's identity record or nil. Same
// leaf-lock rules as identity; used by the read-only probes (is the
// thread stopped, what is its epoch) so a query never mints a record.
func (r *Router) identityIfPresent(threadID string) *threadIdentity {
	r.identitiesMu.Lock()
	defer r.identitiesMu.Unlock()
	return r.identities[threadID]
}

// activeTurnSpanCountLocked reports how many threads currently hold an
// open turn span. Test-only helper: with the spans living per-thread
// there is no single map to len(). Caller holds r.mu.
func (r *Router) activeTurnSpanCountLocked() int {
	n := 0
	for _, st := range r.threads {
		if st.turnSpan != nil {
			n++
		}
	}
	return n
}
