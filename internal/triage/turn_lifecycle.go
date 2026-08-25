package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// handleTurnStart opens the per-turn span, seeds bookkeeping, writes the
// `turns` row for this turn (completed_at NULL), and emits
// `provider:turn_started` so the frontend flips `pane.activeTurn` on.
//
// Wire-round vs logical-turn cadence: this handler is the canonical
// start of round 1 of every logical turn. It both seeds per-turn
// flow-control state (setOpenTurn) AND opens a fresh wire round
// (setOpenRound). The `provider:turn_started` payload carries the
// per-round id as TurnID — the frontend treats each round as its own
// active turn for indicator / composer / Stop-button purposes.
// Subsequent rounds within the same logical turn are opened in
// handleInit's re-round branch, which only calls setOpenRound (not
// setOpenTurn) so id-allocating counters survive the multi-result-
// per-turn cascade. See internal/triage/AGENTS.md "Wire-round vs
// logical-turn".
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	turnIndex := r.resolveTurnIndexOnStart(evt)
	startedAt := eventTimestampMillis(evt)

	// The turns row is reconciled BEFORE any router state is seeded on
	// the index. upsertTurnRow can move the turn to a free index when a
	// DIFFERENT logical turn already occupies this one, and open-turn
	// bookkeeping seeded on the pre-reconcile index would then name a
	// row this turn does not own — id-allocating counters, the span, and
	// the round snapshot all key off it.
	turnIndex = r.upsertTurnRow(store.Turn{
		TurnID:    resolveTurnID(evt, turnIndex),
		ThreadID:  evt.ThreadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
		// Wire-supplied only (Codex): the fork/revert anchor resolver
		// keys on this, and Claude's synthesized turn id must not
		// masquerade as a provider id there.
		ProviderTurnID: strings.TrimSpace(evt.TurnID),
	})

	r.setOpenTurn(evt.ThreadID, turnIndex)
	r.openTurnSpan(evt, turnIndex)

	// Open the first wire round for this logical turn. Frontend
	// idempotency keys on TurnID, so a fresh roundID per round means a
	// duplicate EventTurnStart for the same logical turn (Codex retry,
	// Claude system.init replay before any complete) replaces the prior
	// active-turn entry rather than accumulating stale state.
	snapshot := ActiveTurnSnapshot{
		ThreadID:  evt.ThreadID,
		TurnID:    uuid.NewString(),
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}
	r.setOpenRoundSnapshot(snapshot)
	r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
	return nil
}

// resolveTurnIndexOnStart picks the right turn index for an incoming
// EventTurnStart / EventInit-promoted-to-start. Three sources, in
// priority order:
//
//  1. evt.TurnIndex when the wire carries it directly (Codex
//     turn/started, recovery replays).
//
//  2. The pending-send FIFO head when the wire carries no index but
//     an AO send is awaiting echo. The dispatcher
//     (RegisterPendingSend / RegisterPendingFlushSend) stamps the
//     AO-decided turn index BEFORE the provider write, so the FIFO
//     head carries the authoritative answer for the new turn. The
//     peek is non-destructive — the pop is owned by handleUserText
//     when the matching user_text echo arrives.
//
//  3. store.LastTurnIndex as the no-pending-send fallback (idle
//     session attach via EventInit; tests that exercise this handler
//     directly).
//
// Source 2 is skipped entirely for a turn the provider attributed to
// another producer (`Meta.origin = external-queue`); see the body.
//
// For queue dispatches, source 3 returns the PREVIOUS turn because
// the deferred user_text for the new turn has not been persisted yet
// (handleUserText persists on echo, which lands AFTER system.init).
// Resolving on source 2 first is what stops setOpenTurn from
// re-wiping the previous turn's id-allocating counters and silently
// overwriting its trailing text via UpsertItem. See
// queue_dispatch_turn_test.go.
func (r *Router) resolveTurnIndexOnStart(evt provider.ProviderEvent) int {
	if evt.TurnIndex != 0 {
		return evt.TurnIndex
	}
	// Source 2 is an ATTRIBUTION, not a counter, and it is only valid for
	// a turn this app provoked. A turn start the provider stamped
	// `origin: external-queue` was dispatched by another producer off the
	// provider's own queue (`codex queue --thread`), so the pending head
	// names a message that is still waiting — reading its index makes the
	// foreign turn SQUAT on the AO send's turn, and the send's own echo
	// then hits UNIQUE(thread_id, turn_index) when it opens the turn it
	// was promised (2026-08-24). A foreign dispatch gets a turn of its
	// own, after everything known.
	if r.turnStartIsForeign(evt) {
		return r.nextTurnIndexAfterKnown(evt.ThreadID)
	}
	if pending, ok := r.peekPendingSendHead(evt.ThreadID); ok {
		return pending.TurnIndex
	}
	turnIndex, err := r.store.LastTurnIndex(evt.ThreadID)
	if err != nil {
		log.Printf("triage: last turn index on turn start: %v", err)
		return 0
	}
	return turnIndex
}

// turnStartIsForeign reports whether the provider attributed this turn
// start to a producer other than this app. Reuses the user-echo meta
// decoder — the `origin` key is the same top-level string on both
// envelopes, stamped by the same `stampExternalOrigin` pass.
func (r *Router) turnStartIsForeign(evt provider.ProviderEvent) bool {
	return decodeUserTextMeta(evt.Meta).text("origin") == externalQueueOrigin
}

// nextTurnIndexAfterKnown allocates the first turn index nothing has
// claimed: past the last persisted turn/item AND past every pending
// send's dispatcher-stamped index, because a deferred row is not in
// SQLite until its echo (the same reason MaxPendingSendTurnIndex exists
// for the flush allocator). An empty thread keeps index 0 rather than
// leaving a hole at the start.
func (r *Router) nextTurnIndexAfterKnown(threadID string) int {
	next := 0
	last, err := r.store.LastTurnIndex(threadID)
	if err != nil {
		log.Printf("triage: last turn index for foreign turn start on %s: %v", threadID, err)
	} else {
		occupied, occErr := r.turnIndexOccupied(threadID, last)
		if occErr != nil {
			log.Printf("triage: probe turn index %s/%d: %v", threadID, last, occErr)
		}
		if occupied || occErr != nil {
			next = last + 1
		}
	}
	if maxPending, ok := r.MaxPendingSendTurnIndex(threadID); ok && maxPending+1 > next {
		next = maxPending + 1
	}
	return next
}

// turnIndexOccupied reports whether anything already stands at
// (threadID, turnIndex). LastTurnIndex answers 0 both for "this thread's
// last turn is 0" and for "this thread is empty", so an allocator that
// wants the NEXT index has to tell those apart. HasItems is not
// index-scoped and does not need to be: it is only consulted for the MAX
// index, and if no turns row holds that index then an item must.
func (r *Router) turnIndexOccupied(threadID string, turnIndex int) (bool, error) {
	if _, found, err := r.store.GetTurnByThreadIndex(threadID, turnIndex); err != nil {
		return false, err
	} else if found {
		return true, nil
	}
	return r.store.HasItems(threadID)
}

// resolveTurnID builds the thread-scoped `turns` primary key for the incoming
// turn. The provider's wire id remains separate in provider_turn_id; provider
// ids are not globally unique across sessions, while turns.turn_id is.
func resolveTurnID(evt provider.ProviderEvent, turnIndex int) string {
	return store.ScopedTurnID(evt.ThreadID, evt.TurnID, turnIndex)
}

// upsertTurnRow inserts the turns row for a freshly-opened turn or
// preserves the existing row if the provider re-sends EventTurnStart.
// An existing row means we've seen this turn before (re-init after
// interrupt, recovery replay) and the original started_at is the
// authoritative wall time — don't overwrite it.
// Errors are logged but not propagated: turn-start emission must fire
// even if persistence hiccups, so the frontend's working indicator
// still tracks the wire signal.
//
// Returns the turn index the row ACTUALLY occupies. It differs from
// turn.TurnIndex only when a different logical turn already stands
// there, and the caller's open-turn bookkeeping must follow the return
// value rather than what it asked for.
//
// One logical turn can be asked for under two ids, which is why the
// identity probe is not `GetTurn` alone: openQueuedEchoTurn mints the
// synthesized `<thread>:<index>` shape while a Codex turn start mints
// `<thread>:<providerTurnID>` (store.ScopedTurnID). Either order is
// reachable — the echo can open the turn before the wire start arrives —
// and re-inserting the second shape hits UNIQUE(thread_id, turn_index),
// which this used to log and swallow, leaving a turn with no row of its
// own (2026-08-24). So the probe also asks by INDEX, and two ids at one
// index are reconciled rather than raced.
func (r *Router) upsertTurnRow(turn store.Turn) int {
	_, found, err := r.store.GetTurn(turn.TurnID)
	if err != nil {
		log.Printf("triage: turn start get %s: %v", turn.TurnID, err)
	}
	if found {
		// A re-sent EventTurnStart for the same (thread, turn). The
		// existing row is the authoritative one — preserve started_at
		// and completed_at. Refreshing started_at would show a
		// later clock on the working indicator each time the
		// provider re-initialises.
		return turn.TurnIndex
	}
	if resolved, done := r.reconcileTurnIndexCollision(turn); done {
		return resolved
	}
	if err := r.store.InsertTurn(turn); err != nil {
		if !isTurnIndexCollisionError(err) {
			log.Printf("triage: turn start insert %s: %v", turn.TurnID, err)
			return turn.TurnIndex
		}
		// A row landed at this index between the probe above and this
		// insert. Same reconciliation, now with the winner visible.
		log.Printf("triage: turn start insert %s raced another row at %s/%d — re-resolving",
			turn.TurnID, turn.ThreadID, turn.TurnIndex)
		if resolved, done := r.reconcileTurnIndexCollision(turn); done {
			return resolved
		}
		log.Printf("triage: turn start insert %s: %v", turn.TurnID, err)
	}
	return turn.TurnIndex
}

// reconcileTurnIndexCollision decides what to do about a row already
// standing at (turn.ThreadID, turn.TurnIndex) under a different turn id.
// Reports (index, true) when the caller must stop — either the standing
// row IS this turn, or this turn was relocated and inserted elsewhere.
//
// The discriminator is the PROVIDER turn id, the only thing on either
// row that names a provider turn:
//
//   - Both rows carry one and they differ → two provably distinct
//     provider turns on one index. That is the corruption case: the
//     incoming turn is relocated to a free index and logged loudly with
//     both ids, because letting it share the row would settle one turn's
//     stop reason, usage and error onto the other's history.
//   - Anything else → the same logical turn under its two spellings (an
//     echo-opened `<thread>:<index>` row later hit by the wire start
//     that carries the provider's id, or the reverse). Adopt the
//     standing row: turn ids are resolved by index at settle time
//     (persistedTurnID), so nothing downstream needs the second id —
//     but the wire's provider turn id is BACKFILLED onto the adopted
//     row when it has none, because that id is the discriminator this
//     very function relies on (a blank always reads as "adopt") and
//     the Codex fork/revert anchor. First-write-wins in the store, so
//     the backfill can never displace a real id.
func (r *Router) reconcileTurnIndexCollision(turn store.Turn) (int, bool) {
	existing, found, err := r.store.GetTurnByThreadIndex(turn.ThreadID, turn.TurnIndex)
	if err != nil {
		log.Printf("triage: turn start lookup %s/%d: %v", turn.ThreadID, turn.TurnIndex, err)
		return turn.TurnIndex, false
	}
	if !found || existing.TurnID == turn.TurnID {
		return turn.TurnIndex, false
	}
	incomingProvider := strings.TrimSpace(turn.ProviderTurnID)
	standingProvider := strings.TrimSpace(existing.ProviderTurnID)
	if incomingProvider == "" || standingProvider == "" || incomingProvider == standingProvider {
		log.Printf("triage: turn start %s adopts the row already open at %s/%d (%s) — same logical turn, two id shapes",
			turn.TurnID, turn.ThreadID, turn.TurnIndex, existing.TurnID)
		if incomingProvider != "" && standingProvider == "" {
			if err := r.store.BackfillTurnProviderID(existing.TurnID, incomingProvider); err != nil {
				log.Printf("triage: backfill provider turn id %s onto %s: %v", incomingProvider, existing.TurnID, err)
			}
		}
		return existing.TurnIndex, true
	}
	relocated := turn
	relocated.TurnIndex = r.nextFreeTurnIndex(turn.ThreadID, turn.TurnIndex)
	relocated.TurnID = store.ScopedTurnID(turn.ThreadID, turn.ProviderTurnID, relocated.TurnIndex)
	log.Printf("triage: turn index collision on %s/%d — %s (provider turn %s) already holds it, relocating %s (provider turn %s) to turn %d",
		turn.ThreadID, turn.TurnIndex, existing.TurnID, standingProvider, turn.TurnID, incomingProvider, relocated.TurnIndex)
	if err := r.store.InsertTurn(relocated); err != nil {
		log.Printf("triage: turn start insert %s at relocated index %d: %v", relocated.TurnID, relocated.TurnIndex, err)
	}
	return relocated.TurnIndex, true
}

// nextFreeTurnIndex finds the first index at or after the allocator's
// answer that no turns row holds. Bounded: a scan that cannot find a
// free slot returns its last candidate rather than looping, and the
// insert's own error is then the report.
func (r *Router) nextFreeTurnIndex(threadID string, collidedAt int) int {
	candidate := r.nextTurnIndexAfterKnown(threadID)
	if candidate <= collidedAt {
		candidate = collidedAt + 1
	}
	for attempts := 0; attempts < maxTurnIndexProbes; attempts++ {
		_, found, err := r.store.GetTurnByThreadIndex(threadID, candidate)
		if err != nil {
			log.Printf("triage: probe free turn index %s/%d: %v", threadID, candidate, err)
			return candidate
		}
		if !found {
			return candidate
		}
		candidate++
	}
	log.Printf("triage: no free turn index found for %s within %d probes — using %d", threadID, maxTurnIndexProbes, candidate)
	return candidate
}

// maxTurnIndexProbes bounds the free-index scan. Relocation is already
// the rare path, and every extra probe is one indexed read; a thread
// that would need more than this has something wrong with it that a
// longer scan will not fix.
const maxTurnIndexProbes = 64

// isTurnIndexCollisionError reports whether err is SQLite refusing a
// second turns row at one (thread_id, turn_index). modernc.org/sqlite
// formats these as "UNIQUE constraint failed: turns.thread_id,
// turns.turn_index" and exposes no typed error, so this matches by
// substring the way store.isUniqueConstraintError does.
func isTurnIndexCollisionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "turns.turn_index")
}

func (r *Router) handleTurnComplete(evt provider.ProviderEvent) error {
	flushQueueAtBoundary := true
	defer func() {
		if flushQueueAtBoundary {
			r.maybeFlushQueueAtBoundary(evt.ThreadID)
		}
	}()

	// A turn boundary always closes the live compacting window: a failed
	// Codex compaction abandons its contextCompaction item without
	// completing it, so no boundary or explicit close frame ever arrives —
	// the turn's own completion is the only reliable close. No-op when no
	// window is open (compaction_status.go).
	r.clearCompacting(evt.ThreadID)

	// Two-cadence shape (see internal/triage/AGENTS.md "Wire-round vs
	// logical-turn"):
	//
	//   1. Per-WIRE-ROUND emission (top of this handler).
	//      `provider:turn_completed` fires once per `result` envelope so
	//      the frontend's working indicator, Stop button, and composer
	//      block correctly reflect "model is engaged right now."
	//      takeOpenRound clears the round slot in the same critical
	//      section it returns it; an empty slot means a synthetic
	//      complete already raced ahead (handleError fatal path) — skip
	//      the second emit so the frontend sees exactly one
	//      turn_completed per round.
	//
	//   2. Per-LOGICAL-TURN persistence (claimTurnSettlement-gated below).
	//      The `turns` row UPDATE, streaming-item settlement, and
	//      force-close-orphans pass run once per
	//      (thread, turn_index). A second wire complete for the same
	//      logical turn — the multi-result-per-turn pattern, where
	//      Claude's CLI synthesizes a `type:"user"` envelope from a
	//      task_notification and emits another `result` — folds late
	//      token usage onto the existing turns row and otherwise
	//      no-ops.
	//
	// Known sources of a second wire complete on an already-settled
	// logical turn:
	//
	//   1. Claude task_notification → CLI synthesizes a `type:"user"`
	//      envelope → model emits another response → second `result`
	//      envelope. Both `result`s belong to one logical agent-overflow
	//      turn; persistence settles on the first. The wire-round emit
	//      at the top fires for the second too (each round gets its
	//      own indicator on/off).
	//   2. handleError synthesizes an EventTurnComplete with TruncatedTurnCompleteMeta,
	//      then a real wire EventTurnComplete arrives anyway because the
	//      subprocess kept streaming. takeOpenRound returns "" on the
	//      second, so the wire-round emit is suppressed.
	//
	// Live fast-mode state rides the wire result envelope. Emitted before
	// every settlement branch below (including the orphan-error and
	// not-activity early returns) because it is a report about the
	// SESSION, not about this turn's outcome — a turn that errored still
	// tells the truth about whether fast mode was serving it.
	r.emitFastModeState(evt.ThreadID, fastModeStatusFromTurnComplete(evt))

	countsAsActivity := turnCountsAsThreadActivity(evt)
	turnIndex, err := r.currentTurnIndex(evt.ThreadID)
	meta, metaErr := turnCompleteMetaFromEvent(evt)
	if metaErr != nil {
		log.Printf("triage: %v", metaErr)
		if err == nil {
			err = metaErr
		}
	}
	now := eventTimestampMillis(evt)

	// Per-round emission. Always runs before the claimTurnSettlement gate
	// so a second `result` envelope for the same logical turn still
	// emits the round-end signal. takeOpenRound is a read-and-clear:
	// at most one emit per round, regardless of synthesis races.
	round, hasRound := r.takeOpenRound(evt.ThreadID)
	if hasRound && err == nil {
		r.emit(eventchan.ProviderTurnCompleted, r.buildRoundCompletedEvent(evt, round.TurnID, turnIndex, now, meta))
	}

	// Orphan error result: an error turn-complete with no open round, no
	// open turn, and no prior settlement means the provider failed before
	// the turn machinery ever engaged — e.g. Claude dying pre-init when
	// its --resume-session-at cursor is unusable (the resulting
	// result{error_during_execution} is the only wire output such a
	// session ever produces). There is no round to emit and no turns row
	// to settle, so without this branch the error reaches nothing the
	// user can see: persist a system error item instead. The
	// provider:item_event upsert it emits is the surface the frontend
	// already clears its optimistic pending-send "Working" state on.
	//
	// The isTurnSettled guard — checked against the event's NATURAL turn
	// (currentTurnIndex fallback) — keeps the legitimate late-fold case
	// flowing past this branch: a trailing result{is_error} after a soft
	// close settled the turn must reach persistLateTurnPayload below, not
	// mint an error item. The attributed turn prefers the pending-send
	// head when one exists (the dispatcher-stamped index for the send
	// that provoked the failing start; deferred flush items are not in
	// SQLite yet, so LastTurnIndex would point one turn early). The head
	// is NOT consumed — CleanupThread or the wire user echo owns the pop.
	if err == nil && meta.Error != "" && !hasRound && !r.isTurnSettled(evt.ThreadID, turnIndex) {
		if _, open := r.openTurnIndex(evt.ThreadID); !open {
			attributedTurn := turnIndex
			if head, ok := r.peekPendingSendHead(evt.ThreadID); ok {
				attributedTurn = head.TurnIndex
			}
			// A session that just reported a hard failure cannot serve
			// queued sends — leave the flush queue for the next session.
			flushQueueAtBoundary = false
			return r.persistProviderErrorItem(evt.ThreadID, attributedTurn, meta.Error, nil, "", "", now)
		}
	}

	if err == nil && !countsAsActivity {
		return nil
	}

	if err == nil {
		// claimTurnSettlement is sticky on partial failure (same trade-off
		// as markTurnCaptured): once the FIRST handleTurnComplete reaches
		// here, subsequent invocations for the same (thread, turn) skip
		// lifecycle settlement. Late usage on a duplicate is still folded
		// into the existing turn row below so accounting is not lost.
		if !r.claimTurnSettlement(evt.ThreadID, turnIndex) {
			// Late-fold path: a second activity-counting result for an
			// already-settled logical turn (the multi-result / re-round
			// pattern). persistLateTurnPayload folds the late turns-row
			// columns below, but the turn can still carry streaming items or
			// running foreground tool_calls opened on the LATER round that no
			// content_block_stop or tool_result ever closed — e.g. a
			// CLI-internal retry whose first attempt stalled mid-thinking
			// (thread fc24607e). Without settling them here they leak a
			// permanent status=streaming / status=running row: the user's
			// stuck "thinking" spinner.
			//
			// This is a deliberate SUBSET of the full settle path: the
			// once-per-logical-turn steps (turns-row settle,
			// markTurnItemsErrored, forced interrupt-drain) already ran at the
			// first settlement, so only the orphan settle + force-close repeat
			// here — both idempotent on the common cascade (drained active
			// maps / no running foreground tool_calls → no-ops). It must NOT
			// touch the id-allocating counters: invariant 27 keeps re-round
			// row ids from colliding. Settle failures are returned (the wire
			// pump logs a returned error and keeps reading) so they surface
			// the same way the full path's persistErr does.
			lateStatus := settledTurnStatus(meta)
			var lateErr error
			if settleErr := r.settleTurnStreaming(evt.ThreadID, turnIndex, lateStatus); settleErr != nil {
				lateErr = settleErr
			}
			if fcErr := r.forceCloseOrphanToolCalls(evt.ThreadID, turnIndex, now); fcErr != nil && lateErr == nil {
				lateErr = fcErr
			}
			r.persistLateTurnPayload(evt, turnIndex, meta)
			return lateErr
		}
	}
	var persistErr error
	truncated := turnCompleteIsTruncated(meta)
	if truncated {
		flushQueueAtBoundary = false
	}
	if err != nil {
		persistErr = err
	} else {
		// On truncation, drain the interrupt queue as errored BEFORE
		// settling the streaming items. settleTurnStreaming fires
		// drainInterruptQueueIfIdle on the last streaming close, which
		// would otherwise persist the queued items as-is (status
		// completed, no " — interrupted" suffix). Draining first means
		// the subsequent idle-drain finds an empty queue and the
		// queued rows correctly reflect the interrupted turn. For
		// non-truncated completions we keep the original order —
		// settle → (idle-drain persists normally) → forced drain no-op.
		if truncated {
			if err := r.drainInterruptQueue(evt.ThreadID, true); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			if err := r.settleTurnStreaming(evt.ThreadID, turnIndex, settledTurnStatus(meta)); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil && truncated {
			if err := r.markTurnItemsErrored(evt.ThreadID, turnIndex, now); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			// For the non-truncated path this drains any queued events
			// that settled outside settleTurnStreaming's idle-drain
			// window (rare but possible). For the truncated path the
			// queue is already empty so this is a cheap no-op.
			if err := r.drainInterruptQueue(evt.ThreadID, truncated); err != nil {
				persistErr = err
			}
		}
	}

	// Codex background-terminal turn-boundary cleanup. Turn completion settles
	// visible terminal-wait carriers and drops pending spawn-agent starts that
	// never reached a terminal spawn completion. UnifiedExec trackers stay
	// transient after turn close because Codex `Op::Interrupt` does not kill
	// the PTY; typed item/completed later clears the live tracker, and only
	// persists transcript history if a Codex wire round is active at that time.
	if persistErr == nil && err == nil {
		r.observeCodexTurnComplete(evt.ThreadID)
	}

	// Invariant 23: force-close orphan running non-background tool_calls.
	// Backgrounded launches are exempt (invariant 24). Runs after the
	// truncation flip so we don't double-flip items that
	// markTurnItemsErrored already settled — the check targets status=
	// running tool_calls only, which persistItem-via-markTurnItemsErrored
	// has already moved to status=errored in the truncated path.
	if persistErr == nil && err == nil {
		if fcErr := r.forceCloseOrphanToolCalls(evt.ThreadID, turnIndex, now); fcErr != nil {
			persistErr = fcErr
		}
	}

	// Settle the turns row even on persist error so the persisted
	// `completed_at` reflects whatever clock we have at the point the
	// wire said the turn was over. Persistence failures are logged
	// inside settleTurnRow; we keep persistErr as the return so
	// upstream (observability, error reporting) sees the real failure.
	// The frontend's working indicator was already cleared by the
	// per-round `provider:turn_completed` emission at the top of this
	// handler.
	//
	// Anchor recording does NOT happen here. Message anchors are
	// recorded by app_send.go before provider stdin/RPC dispatch so
	// the anchor maps directly to "before this user message".
	r.settleTurnRow(evt, turnIndex, now, meta, persistErr)

	r.clearOpenTurn(evt.ThreadID)
	r.finishTurnSpan(evt.ThreadID, completedTurnOutcome(meta, persistErr))
	r.FlushUsageEmitThrottle(evt.ThreadID)

	// Opportunistic WAL passive checkpoint at the idle boundary. PASSIVE
	// is non-blocking: it reclaims whatever pages the WAL can free
	// without stalling readers, and skips the rest. We only fire it
	// when this thread has no remaining streaming items — the counter
	// is the freshest signal that "the stream burst just ended" — and
	// we run the actual PRAGMA on a goroutine so we never block the
	// provider read-loop on its syscall. The autocheckpoint at 1000
	// pages is still the steady-state mechanism; this is an extra
	// nudge at the natural quiet point.
	r.mu.Lock()
	idleState := r.threadStateIfPresent(evt.ThreadID)
	thisThreadIdle := idleState == nil || idleState.streamingItemCount == 0
	r.mu.Unlock()
	if thisThreadIdle {
		threadID := evt.ThreadID
		go func() {
			if cpErr := r.store.PassiveCheckpoint(); cpErr != nil {
				log.Printf("triage: passive checkpoint after turn complete (%s): %v", threadID, cpErr)
			}
		}()
	}

	return persistErr
}

// turnCompleteIsTruncated reports whether the turn ended via interruption /
// truncation.
func turnCompleteIsTruncated(meta turnCompleteMeta) bool {
	return meta.Truncated || meta.Aborted
}

// settledTurnStatus maps a completed turn's meta to the status its
// streaming items settle to: errored when the turn was truncated/aborted,
// completed otherwise. Shared by the full settlement path and the
// late-fold orphan settle so the two can't drift.
func settledTurnStatus(meta turnCompleteMeta) string {
	if turnCompleteIsTruncated(meta) {
		return statusErrored
	}
	return statusCompleted
}

// forceCloseOrphanToolCalls flips every status=running +
// is_background=0 tool_call row in (threadID, turnIndex) to errored
// with a synthesized completion summary. This is the safety net for
// invariant 23: the provider/parser can drop a tool_result and leave a
// row stuck at running; a clean turn-complete should settle the
// timeline regardless. Backgrounded launches are exempt (invariant 24
// — they legitimately outlive their turn); the exemption is pushed
// into the SQL accessor so we don't deserialize settled or
// already-exempt rows at all.
//
// The DB writes are batched inside store.ForceCloseRunningToolCallsInTurn
// (one TX, one thread-touch) to cut per-orphan roundtrips; the
// frontend-bound `provider:item_event` upserts stay per-row so the
// UI still updates each card independently.
func (r *Router) forceCloseOrphanToolCalls(threadID string, turnIndex int, now int64) error {
	flipped, err := r.store.ForceCloseRunningToolCallsInTurn(threadID, turnIndex, ForceCloseSummary, now)
	if err != nil {
		return fmt.Errorf("force-close orphan tool calls: %w", err)
	}
	for _, item := range flipped {
		r.emitItemUpsert(item)
		r.metrics.ItemsPersisted.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("kind", item.Kind)))
	}
	return nil
}

const forceCloseSuffix = " — turn ended with tool unresolved"

// ForceCloseSummary adds the force-close marker to the tool_call's
// summary. Idempotent — a second call leaves the string unchanged, so
// a repeated turn-complete (Claude re-init → re-complete) doesn't
// accumulate suffixes.
func ForceCloseSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Turn ended with tool unresolved"
	}
	if strings.HasSuffix(summary, forceCloseSuffix) {
		return summary
	}
	return summary + forceCloseSuffix
}

// turnCompleteFields holds the small set of derived values both
// settleTurnRow (persistence) and buildRoundCompletedEvent (wire
// payload) compute from the same EventTurnComplete inputs. Extracted
// because the two consumers run in different cadences (logical-turn
// vs wire-round) but the projection rules — stop_reason fallback to
// "error" on persistErr, assistant_message_id resolution across
// provider shapes, and error_message derived from meta or persistErr
// — are exactly the same. logicalTurnID is caller-supplied because
// completion paths may need to reconcile an event without TurnID back
// to the already-persisted (thread_id, turn_index) row.
type turnCompleteFields struct {
	logicalTurnID      string
	stopReason         string
	assistantMessageID string
	errorMessage       string
}

func decodeTurnCompleteFields(evt provider.ProviderEvent, logicalTurnID string, meta turnCompleteMeta, persistErr error) turnCompleteFields {
	stopReason := canonicalStopReason(meta)
	if persistErr != nil && stopReason == "" {
		stopReason = "error"
	}
	errorMessage := meta.Error
	if errorMessage == "" && persistErr != nil {
		errorMessage = persistErr.Error()
	}
	return turnCompleteFields{
		logicalTurnID:      logicalTurnID,
		stopReason:         stopReason,
		assistantMessageID: meta.AssistantMessageID,
		errorMessage:       errorMessage,
	}
}

func (r *Router) persistedTurnID(evt provider.ProviderEvent, turnIndex int) string {
	if existing, found, err := r.store.GetTurnByThreadIndex(evt.ThreadID, turnIndex); err == nil && found {
		return existing.TurnID
	} else if err != nil {
		log.Printf("triage: lookup turn %s/%d: %v", evt.ThreadID, turnIndex, err)
	}
	return resolveTurnID(evt, turnIndex)
}

// settleTurnRow updates the persisted `turns` row at logical-turn
// granularity. Persists on best-effort: missing turn rows (e.g. a
// turn_complete with no matching turn_start because the first event
// arrived mid-crash-recovery) are tolerated — the UPDATE's
// sql.ErrNoRows is logged and the function returns. The frontend-
// facing `provider:turn_completed` emission has already fired by the
// time this runs (see handleTurnComplete: takeOpenRound + emit at the
// top, then claimTurnSettlement gate, then this).
//
// Top-level turn settle bumps thread activity through
// Store.MarkThreadActivity. Nested/internal turns only update turn state,
// so subagent completions do not reorder the sidebar. Synthesized
// session-died / abort flavors flow through this same path when they
// belong to the top-level turn.
func (r *Router) settleTurnRow(evt provider.ProviderEvent, turnIndex int, now int64, meta turnCompleteMeta, persistErr error) {
	fields := decodeTurnCompleteFields(evt, r.persistedTurnID(evt, turnIndex), meta, persistErr)

	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}

	if err := r.store.UpdateTurnCompleted(fields.logicalTurnID, now, fields.stopReason, fields.assistantMessageID, usageJSON, fields.errorMessage); err != nil {
		log.Printf("triage: update turn %s: %v", fields.logicalTurnID, err)
	}
	r.appendUsageLedger(evt, fields.logicalTurnID, meta, now)
	if turnCountsAsThreadActivity(evt) {
		r.bumpThreadActivity(evt.ThreadID, now, "turn settle")
	}
}

func turnCountsAsThreadActivity(evt provider.ProviderEvent) bool {
	return strings.TrimSpace(evt.ParentToolUseID) == ""
}

// buildRoundCompletedEvent builds the frontend-facing
// TurnCompletedEvent payload for a single wire round. The TurnID is
// the per-round uuid (allocated in handleTurnStart / handleInit
// re-round) — the frontend treats each round as a distinct active
// turn, replacing its activeTurn entry on each round-start and
// clearing on the matching complete. StartedAt prefers the persisted
// `turns` row's started_at (logical-turn clock) and falls back to
// `now` when no row exists yet (mid-crash-recovery edge case).
//
// Pure projection — does not write the `turns` row. settleTurnRow
// owns the UPDATE and runs once per logical turn under the
// claimTurnSettlement gate.
//
// No persistErr parameter: this fires at the TOP of handleTurnComplete
// before any persistence runs, so there is no upstream persistence
// error to fold into the payload's stop_reason / error_message
// fields. If a persistence failure occurs later in handleTurnComplete,
// it surfaces through `persistErr` on the function return rather than
// retroactively rewriting the wire-round emission the frontend has
// already received.
func (r *Router) buildRoundCompletedEvent(
	evt provider.ProviderEvent,
	roundID string,
	turnIndex int,
	now int64,
	meta turnCompleteMeta,
) TurnCompletedEvent {
	fields := decodeTurnCompleteFields(evt, r.persistedTurnID(evt, turnIndex), meta, nil)
	startedAt := now
	if existing, found, err := r.store.GetTurn(fields.logicalTurnID); err == nil && found {
		startedAt = existing.StartedAt
	}
	// ThreadHistoryStamp returns the zero stamp for both failure shapes —
	// a read error and a thread row that is gone — so there is nothing to
	// normalise here. The stamp is an optimization: without it the client
	// re-fetches its window once, which is why a read error is logged
	// rather than allowed to fail a turn boundary.
	stamp, _, err := r.store.ThreadHistoryStamp(evt.ThreadID)
	if err != nil {
		log.Printf("triage: read history stamp for %s: %v", evt.ThreadID, err)
	}
	return TurnCompletedEvent{
		ThreadID:            evt.ThreadID,
		TurnID:              roundID,
		TurnIndex:           turnIndex,
		StartedAt:           startedAt,
		CompletedAt:         now,
		StopReason:          fields.stopReason,
		AssistantMessageID:  fields.assistantMessageID,
		TokenUsage:          meta.Usage,
		ErrorMessage:        fields.errorMessage,
		Aborted:             meta.Aborted || meta.Truncated,
		RevertedUserMessage: r.consumeRevertedTurn(evt.ThreadID),
		CountsAsActivity:    turnCountsAsThreadActivity(evt),
		HistoryRev:          stamp.Rev,
		HistoryEpoch:        stamp.Epoch,
	}
}

// persistLateTurnPayload folds late-arriving payload onto the existing
// turn row when handleTurnComplete fires after the turn was already
// settled. Two known sources:
//
//  1. Multi-result cascade — Claude's CLI emits a second `result`
//     envelope for the same logical turn after a task_notification
//     re-round. Cumulative `usage` lands on the second envelope; the
//     amid points to the final round's assistant message.
//  2. Soft round-close — `parse_stream.go` emits an
//     soft EventTurnComplete from `message_delta.stop_reason` before the
//     wire `result` envelope. The soft event carries the
//     parser's PEEKED last assistant_message_id (the consume happens
//     when the trailing `result` calls takeLastAssistantMessageID).
//     The trailing `result` carries cumulative usage that the soft
//     event lacks.
//
// Per-column semantics (different intentionally, see store
// UpdateTurnLatePayload):
//
//   - token_usage_json: first non-empty wins. The first settle's
//     usage stays on the row; later folds skip if already populated.
//   - assistant_message_id: last non-empty wins. Each subsequent
//     round overwrites so the persisted column is the FINAL
//     assistant message of the turn — what
//     `SettledTurn.assistantMessageId` is documented to be.
//   - stop_reason / error_message: late error wins. A soft
//     message_delta close can settle a turn as `end_turn` before the
//     trailing wire `result{is_error:true}` arrives; the error must
//     still be visible in persisted history.
//
// Folded as a single UPDATE so the common case (both fields arrive
// on the trailing `result`) pays one autocommit boundary.
func (r *Router) persistLateTurnPayload(evt provider.ProviderEvent, turnIndex int, meta turnCompleteMeta) {
	turnID := r.persistedTurnID(evt, turnIndex)
	// Ledger rows append on every settle event: the provider emits
	// per-turn DELTAS, so a late fold's usage is new spend the first
	// settle could not have carried (soft round-close settles with no
	// usage at all; a multi-result cascade's second envelope deltas only
	// the re-round's growth).
	r.appendUsageLedger(evt, turnID, meta, eventTimestampMillis(evt))
	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}
	amid := meta.AssistantMessageID
	stopReason, errorMessage := lateErrorTurnPayload(meta)
	if usageJSON == "" && amid == "" && stopReason == "" && errorMessage == "" {
		return
	}
	if err := r.store.UpdateTurnLatePayload(turnID, store.LateTurnPayload{
		TokenUsageJSONIfEmpty:       usageJSON,
		AssistantMessageIDOverwrite: amid,
		StopReasonOverwrite:         stopReason,
		ErrorMessageOverwrite:       errorMessage,
	}); err != nil {
		log.Printf("triage: update turn %s late payload: %v", turnID, err)
	}
}

func lateErrorTurnPayload(meta turnCompleteMeta) (string, string) {
	if canonicalStopReason(meta) != "error" && meta.Error == "" {
		return "", ""
	}
	return "error", meta.Error
}

func (r *Router) currentTurnIndex(threadID string) (int, error) {
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil && st.openTurnSet {
		turnIndex := st.openTurn
		r.mu.Unlock()
		return turnIndex, nil
	}
	r.mu.Unlock()
	return r.store.LastTurnIndex(threadID)
}

// openTurnIndex returns the router's currently-open turn for threadID,
// or (_, false) when no turn is in flight. Distinct from
// currentTurnIndex: this does NOT fall back to the store's
// LastTurnIndex, so callers that must drop rather than attach a live
// event to a closed turn can do so without a separate is-turn-open
// check.
func (r *Router) openTurnIndex(threadID string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.openTurnSet {
		return 0, false
	}
	return st.openTurn, true
}

// OpenTurnIndex returns the currently-open logical turn for threadID,
// or -1 when none is in flight. Interrupt paths sample this BEFORE
// awaiting the provider's interrupt ack and pass the value to
// EagerPersistDeferredFlushSends: the ack wait processes wire events,
// so the turn the user cut can settle during it, and a post-ack sample
// would report -1 for exactly that turn — its echo would then settle
// it "end_turn" instead of "interrupted" (round-5, R5-4).
func (r *Router) OpenTurnIndex(threadID string) int {
	if turnIndex, ok := r.openTurnIndex(threadID); ok {
		return turnIndex
	}
	return -1
}

// hasInFlightTurnOrRound reports whether the router still has any
// turn or wire-round state for threadID worth synthesizing a
// truncated turn-complete against. Used by CleanupThread and
// handleSessionDied so the round-2+ re-round case (handleInit's
// maybeEmitReRoundOnInit branch sets currentRoundByThread WITHOUT
// calling setOpenTurn — counters survive the multi-result boundary
// by design) still produces a frontend `provider:turn_completed`
// when the session dies mid-round-2. Without the currentRoundByThread
// arm of this check, the openTurns map alone misses the round-2+
// case because clearOpenTurn cleared it at the end of round 1.
func (r *Router) hasInFlightTurnOrRound(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	return st.openTurnSet || st.currentRoundOpen
}

func (r *Router) setOpenTurn(threadID string, turnIndex int) {
	r.mu.Lock()
	st := r.state(threadID)
	st.openTurn = turnIndex
	st.openTurnSet = true
	key := scopeCounterKey(turnIndex, "")
	if st.segmentIndexByScope == nil {
		st.segmentIndexByScope = make(map[string]int)
	}
	if st.blockIndexByScope == nil {
		st.blockIndexByScope = make(map[string]int)
	}
	st.segmentIndexByScope[key] = -1
	st.blockIndexByScope[key] = -1
	r.clearActiveStreamBlocksForTurnLocked(st, turnIndex)
	st.streamingItemCount = 0
	clear(st.streamingScopeCounts)
	delete(st.errorSeqByScope, key)
	// Clear the settled marker so a re-init (Claude resend system.init
	// after an interrupt; Codex resend turn/started) can settle the
	// same turn again. The multi-result-per-turn case does NOT re-fire
	// EventTurnStart between the two completes, so the marker survives
	// there and the second complete returns early.
	delete(st.settledTurns, turnIndex)
	r.mu.Unlock()
}

// openQueuedEchoTurn establishes logical turn `turnIndex` when a queued
// user message's wire echo lands without any init having opened it.
// Claude's CLI can consume a queued stdin envelope mid-loop (persisted
// in its transcript as a `queued_command` attachment) instead of at
// turn pickup — no system.init, no EventTurnStart — so the deferred
// user_text persists at the dispatcher-stamped index while the router
// still carries the previous turn (or no turn at all, in the
// task-notification cascade case). Everything keyed on the turn
// boundary then drifts: no turns row (0s settled duration, missing
// durable history), the frontend active-turn registry keeps the old
// index (its response-pill active-turn exclusion misses every new
// assistant text — the 2026-07-12 moving-RESPONSE-pill bug), and the
// revert flow's active-turn guard reads idle mid-stream.
//
// This is the wire-echo twin of handleTurnStart: turns row, open-turn
// bookkeeping, turn span, fresh wire-round snapshot, and the
// `provider:turn_started` emission — WITHOUT setOpenTurn's thread-wide
// streaming sweeps. Those sweeps are keyed to "a brand-new wire
// conversation is starting"; here the previous turn's tail can still
// be settling on the same live round, and `turnIndex` is a fresh index
// with no stale state of its own to clear (the counter seeds below are
// exactly the fresh-index subset of setOpenTurn). This is NOT the
// forbidden re-round setOpenTurn (see maybeReopenSettledRound): a
// re-round reuses the SAME index whose counters must survive; this
// opens a NEW index.
//
// No-op when `turnIndex` is already the open logical turn: the queue
// consumed at turn pickup with a real system.init (handleTurnStart
// already ran, in either order relative to the echo — upsertTurnRow
// and the counter seeds are idempotent), and every Codex steer (the
// deferred row persists at the active turn's own index). Also refuses
// to roll the open turn BACKWARD or reopen a settled turn — rows are
// already persisted under those indexes and a reopen would reset their
// id-allocating context (the same hazard invariant 27 forbids for
// re-rounds). Both guards protect the attachProviderItemIDToUserRow
// caller, which fires on every WasDeferred echo including replays.
//
// When an UNSETTLED earlier turn is open — a second queued message
// consumed in the same wire round, or an interrupt that raced the CLI's
// queue drain — that predecessor will never receive a wire completion
// of its own (the round's single `result` settles only the last open
// turn), so it is settled here before the new turn replaces it.
// interruptedTurnIndex names the ONE turn a user interrupt provably cut
// short (the turn open when the interrupt was issued — stamped onto
// the pending entry pre-ack by MarkFlushSendsInterrupted and
// again by the eager persist; -1 when none): the settle records
// "interrupted" only when the predecessor IS that turn, "end_turn"
// otherwise — including the sibling case where one interrupt
// eager-persisted several deferred rows and a later echo settles an
// earlier queued message's turn, which ended naturally as the CLI
// drained the next message. The settlement claim blocks the eventual
// wire result from correcting the reason later, so it must be right
// here.
func (r *Router) openQueuedEchoTurn(threadID string, turnIndex int, startedAt int64, interruptedTurnIndex int) {
	r.mu.Lock()
	st := r.state(threadID)
	open, hasOpen := st.openTurn, st.openTurnSet
	if hasOpen && open == turnIndex {
		r.mu.Unlock()
		return
	}
	if hasOpen && turnIndex < open {
		r.mu.Unlock()
		log.Printf("triage: queued echo turn %s/%d ignored — open turn %d is already past it", threadID, turnIndex, open)
		return
	}
	if st.settledTurns[turnIndex] {
		r.mu.Unlock()
		log.Printf("triage: queued echo turn %s/%d ignored — turn already settled", threadID, turnIndex)
		return
	}
	st.openTurn = turnIndex
	st.openTurnSet = true
	key := scopeCounterKey(turnIndex, "")
	if st.segmentIndexByScope == nil {
		st.segmentIndexByScope = make(map[string]int)
	}
	if st.blockIndexByScope == nil {
		st.blockIndexByScope = make(map[string]int)
	}
	if _, ok := st.segmentIndexByScope[key]; !ok {
		st.segmentIndexByScope[key] = -1
	}
	if _, ok := st.blockIndexByScope[key]; !ok {
		st.blockIndexByScope[key] = -1
	}
	delete(st.settledTurns, turnIndex)
	// The thread-level mark list covers the entry the per-entry stamp
	// structurally cannot: one POPPED from the FIFO just before
	// MarkFlushSendsInterrupted ran, whose echo is settling here with
	// its retained -1 stamp (round-10, R10-6). Within a session a mark
	// lingering after a SUCCESSFUL interrupt matches only the turn that
	// really was cut — and once that turn settles, the claim below
	// makes any re-match a no-op. MarkThreadActive clears the list, so
	// a replacement session's reused turn indexes (revert paths) never
	// meet a dead session's marks (round-11, CT11-2).
	markedInterruptTurn := -1
	if id := r.identityIfPresent(threadID); id != nil && len(id.interruptMarks) > 0 {
		markedInterruptTurn = id.interruptMarks[len(id.interruptMarks)-1].turn
	}
	r.mu.Unlock()

	if hasOpen {
		reason := "end_turn"
		if open == interruptedTurnIndex || open == markedInterruptTurn {
			reason = "interrupted"
		}
		r.settleQueuedEchoPredecessor(threadID, open, startedAt, reason)
	}

	// Same deterministic id shape resolveTurnID synthesizes for Claude
	// turns (this path never has a wire-supplied turn id). Idempotent:
	// if a racing system.init inserted the row first, the existing
	// started_at is preserved — and a racing CODEX init inserted it
	// under `<thread>:<providerTurnID>`, which upsertTurnRow adopts
	// rather than re-inserting.
	//
	// It can never RELOCATE this turn: relocation needs two differing
	// provider turn ids and this row has none. The check is here anyway
	// because the open-turn state above was seeded on turnIndex, so a
	// relocation would leave that state naming a row this turn does not
	// own, and a silent divergence is exactly the shape the index
	// reconciliation exists to stop being silent.
	placedIndex := r.upsertTurnRow(store.Turn{
		TurnID:    fmt.Sprintf("%s:%d", threadID, turnIndex),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	})
	if placedIndex != turnIndex {
		log.Printf("triage: queued echo turn %s/%d was placed at %d — open-turn state still names %d",
			threadID, turnIndex, placedIndex, turnIndex)
	}
	r.openTurnSpan(provider.ProviderEvent{ThreadID: threadID}, turnIndex)

	// Re-mint the wire round under the new logical turn. The frontend
	// replaces its active-turn entry on the fresh TurnID, so the pill
	// exclusion, elapsed timer, and Stop button all track the queued
	// message's turn; the eventual wire turn-complete takes THIS
	// snapshot via takeOpenRound and clears it.
	snapshot := ActiveTurnSnapshot{
		ThreadID:  threadID,
		TurnID:    uuid.NewString(),
		TurnIndex: turnIndex,
		StartedAt: startedAt,
	}
	r.setOpenRoundSnapshot(snapshot)
	r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
}

// settleQueuedEchoPredecessor closes the logical turn a queued echo is
// replacing. A deliberate SUBSET of handleTurnComplete's settle path:
// streaming rows settle as completed (or flip errored+stopped when the
// turn was cut) and the turns row closes at the successor's start time
// with the caller-supplied stop reason — "end_turn" when the model's
// response to that message ended naturally as the CLI drained the next
// queued message, "interrupted" when a user interrupt provably cut the
// predecessor short (the caller decides; see openQueuedEchoTurn). No frontend emission (the successor's
// provider:turn_started replaces the round snapshot in place), no usage
// fold (the round's single `result` carries round-cumulative usage that
// lands on the final turn, same as the re-round fold), and no orphan
// tool-call force-close (the CLI drains its queue between API
// iterations, after the prior iteration's tool results — a tool
// completion that hasn't arrived yet still will, and belongs to the row
// it opened). claimTurnSettlement both gates a duplicate settle (a soft
// round close may have beaten us) and routes any later wire result for
// this index into persistLateTurnPayload instead of the orphan-error
// path.
func (r *Router) settleQueuedEchoPredecessor(threadID string, turnIndex int, completedAt int64, stopReason string) {
	if !r.claimTurnSettlement(threadID, turnIndex) {
		return
	}
	var persistErr error
	if stopReason == "interrupted" {
		// A user interrupt provably cut this turn: its in-flight rows are
		// partial output and must carry the errored + " — stopped" state
		// the interrupt spec promises. The flip must run BEFORE the
		// streaming settle — the settlement claim above routes the later
		// truncated wire result away from the normal interrupt cleanup,
		// so completing these rows here would permanently display partial
		// text as a finished answer (round-10, R10-4). The settle below
		// still runs to drain the in-memory streaming slots; it skips
		// rows the flip already moved out of streaming status.
		if err := r.flipTurnItemsErrored(threadID, turnIndex, completedAt, stoppedSummary); err != nil {
			log.Printf("triage: flip interrupted queued-echo predecessor rows %s/%d: %v", threadID, turnIndex, err)
			persistErr = errors.Join(persistErr, err)
		}
	}
	if err := r.settleTurnStreaming(threadID, turnIndex, statusCompleted); err != nil {
		log.Printf("triage: settle queued-echo predecessor streaming %s/%d: %v", threadID, turnIndex, err)
		persistErr = errors.Join(persistErr, err)
	}
	turnID := r.persistedTurnID(provider.ProviderEvent{ThreadID: threadID}, turnIndex)
	if err := r.store.UpdateTurnCompleted(turnID, completedAt, stopReason, "", "", ""); err != nil {
		log.Printf("triage: settle queued-echo predecessor turn %s: %v", turnID, err)
		persistErr = errors.Join(persistErr, err)
	}
	r.finishTurnSpan(threadID, completedTurnOutcome(turnCompleteMeta{
		StopReason: stopReason,
	}, persistErr))
}

// clearActiveStreamBlocksForTurnLocked drops one turn's streaming
// bookkeeping from the thread's state. Caller holds r.mu.
func (r *Router) clearActiveStreamBlocksForTurnLocked(st *threadState, turnIndex int) {
	if st == nil {
		return
	}
	prefix := fmt.Sprintf("%d|", turnIndex)
	deleteByPrefix(st.activeTextBlocks, prefix)
	deleteByPrefix(st.activeThinkingBlocks, prefix)
	deleteByPrefix(st.activeTextBlockRefs, prefix)
	deleteByPrefix(st.activeThinkingBlockRefs, prefix)
	// NOTE: this routine used to also sweep streamPersistBuffers with the
	// same "<threadID>|<turnIndex>|" prefix, but those are keyed by ITEM
	// ID ("text:<turn>:<n>", a provider tool id, ...), never by
	// "<turn>|…", so the loop could never match an entry. It is dropped
	// rather than re-pointed: buffers are extracted at their own settle
	// (flushStreamingItem) and swept wholesale at session teardown, and
	// dropping a live buffer at a turn boundary WITHOUT flushing it would
	// discard streamed bytes SQLite has not seen. Surfaced, not changed.
	//
	// streamingPathRefsLast is keyed by itemID. Assistant_text ids carry
	// the turn index in their suffix (TextItemID →
	// "text:<turnIndex>:[scope:]<n>"), so the prefix "text:<turnIndex>:"
	// sweeps every entry this turn allocated — scoped (subagent)
	// variants share the same turn-index segment because scope appears
	// AFTER it.
	deleteByPrefix(st.streamingPathRefsLast, "text:"+fmt.Sprintf("%d:", turnIndex))
}

// claimTurnSettlement records that handleTurnComplete has begun logical-turn
// settlement for (threadID, turnIndex). It returns true only for the first
// claimant; later callers should take the duplicate/late-payload path.
func (r *Router) claimTurnSettlement(threadID string, turnIndex int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.state(threadID)
	if st.settledTurns[turnIndex] {
		return false
	}
	if st.settledTurns == nil {
		st.settledTurns = make(map[int]bool)
	}
	st.settledTurns[turnIndex] = true
	return true
}

// isTurnSettled reports whether logical-turn settlement has already run
// for (threadID, turnIndex). Read-only companion to claimTurnSettlement
// for callers that need to discriminate "late result for a settled
// turn" from "result for a turn that never started" without claiming
// the settlement slot themselves.
func (r *Router) isTurnSettled(threadID string, turnIndex int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	return st != nil && st.settledTurns[turnIndex]
}

// setOpenRoundSnapshot records the active wire-round snapshot for threadID. A round
// begins on either:
//
//   - handleTurnStart (per user-typed send) — the canonical first
//     round of every logical turn.
//   - handleInit when the prior round of the same logical turn already
//     settled — the re-round case for Claude's multi-result-per-turn
//     wire pattern, where the CLI synthesizes a `type:"user"` envelope
//     from a task_notification and emits a fresh `system.init`
//     followed by another `result`. Each `result` is its own wire
//     round from the model's POV.
//
// Round ids are uuids; the frontend treats each round as a distinct
// active turn for indicator / composer / Stop-button purposes. The
// persisted `turns` row id stays on `resolveTurnID` (logical-turn
// granularity); only the wire payload's TurnID field carries the round
// id. See internal/triage/AGENTS.md "Wire-round vs logical-turn" for
// the full mental model.
//
// Overwrites any prior round snapshot for the thread — a leaked round (e.g.
// if currentTurnIndex failed in the prior handleTurnComplete and the
// emit at the top was skipped) cannot survive into the next round.
// CleanupThread also sweeps this map.
func (r *Router) setOpenRoundSnapshot(snapshot ActiveTurnSnapshot) {
	if snapshot.ThreadID == "" || snapshot.TurnID == "" {
		return
	}
	r.mu.Lock()
	st := r.state(snapshot.ThreadID)
	st.currentRound = snapshot
	st.currentRoundOpen = true
	r.mu.Unlock()
}

// takeOpenRound returns the active wire-round snapshot for threadID and
// clears the slot in the same critical section. Empty string when no
// round is open — the caller (handleTurnComplete) skips the per-round
// `provider:turn_completed` emission in that case. The
// "read-and-clear" semantics handle the synthetic-truncate-then-real
// race naturally: the synthetic complete takes the slot and emits;
// the real complete finds it empty and skips, so the frontend sees
// exactly one `provider:turn_completed` per round even when handleError
// synthesizes ahead of the wire's own turn-complete.
func (r *Router) takeOpenRound(threadID string) (ActiveTurnSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.currentRoundOpen {
		return ActiveTurnSnapshot{}, false
	}
	round := st.currentRound
	st.currentRound = ActiveTurnSnapshot{}
	st.currentRoundOpen = false
	return round, true
}

// openRoundID returns the active wire-round id for threadID without
// clearing the slot. Read-only counterpart to takeOpenRound for
// callers that observe the round id mid-round (handleToolStart's
// flush trigger). Returns "" when no round is open — the caller
// short-circuits in that case (a tool_start with no open round means
// the wire is mid-init and the trigger would have nowhere to anchor
// its "fired this round" marker).
func (r *Router) openRoundID(threadID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return ""
	}
	return st.currentRound.TurnID
}

// ActiveTurnSnapshot returns the current live wire-round snapshot for a
// thread without clearing it. Most production hydration goes through
// LiveStateSnapshotForThread; this narrow accessor remains for tests and
// diagnostics that only need the active-turn slot.
func (r *Router) ActiveTurnSnapshot(threadID string) (ActiveTurnSnapshot, bool) {
	if r == nil || threadID == "" {
		return ActiveTurnSnapshot{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.currentRoundOpen {
		return ActiveTurnSnapshot{}, false
	}
	return st.currentRound, true
}

// activeRoundTurnIndex returns the turn index for the frontend-visible active
// wire round. Unlike currentTurnIndex, this never falls back to the newest
// stored turn; callers use it when an event should be dropped from chat history
// once the user-facing task indicator is no longer running.
func (r *Router) activeRoundTurnIndex(threadID string) (int, bool) {
	if r == nil || threadID == "" {
		return 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.currentRoundOpen {
		return 0, false
	}
	return st.currentRound.TurnIndex, true
}

// clearOpenTurn drops per-turn flow-control state at EventTurnComplete.
//
// Three distinct lifecycles intersect here and MUST stay separate (see
// internal/triage/AGENTS.md "Correlation state" for the full taxonomy):
//
//   - **Per-turn flow-control state** swept HERE: openTurns,
//     interruptQueue, streamingItemCounts, activeTextBlocks/Thinking,
//     pendingCommandDiffs, pendingApprovals (and siblings). These maps
//     answer "what's mid-turn right now."
//   - **Id-allocating counters** (segmentIndexByScope, blockIndexByScope,
//     errorSeqByScope, compactionSeqByScope, terminalInteractionSeq):
//     cleared at CleanupThread, with a selective re-init reset in
//     setOpenTurn. Wiping HERE would
//     cause id collisions when the wire emits two `result` envelopes
//     for one logical turn (Claude task_notification → CLI-synthesized
//     `user` envelope → second result, fatal-error synthetic-truncate
//     then real wire complete) — see the regression coverage in
//     multi_result_test.go.
//   - **Logical-turn settlement state** (`settledTurns`) survives
//     turn-end by design. It clears in setOpenTurn (so a re-init can
//     re-settle) and CleanupThread.
//
// activeTextBlocks/Thinking ARE per-turn flow-control (they guard
// against re-creating a row mid-stream), so they stay swept here as a
// safety net for any block that didn't settle through the normal close
// path.
func (r *Router) clearOpenTurn(threadID string) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		r.mu.Unlock()
		return
	}
	if st.openTurnSet {
		r.clearActiveStreamBlocksForTurnLocked(st, st.openTurn)
		// pendingCommandDiffs is keyed by `<threadID>:<itemID>` and
		// stages an inline-diff preview between EventToolStart and
		// EventToolComplete for command_execution rows. If the matching
		// completion never arrived in this turn (interrupted, crashed),
		// the entry would otherwise leak until CleanupThread.
		clear(st.pendingCommandDiffs)
		// pendingApprovals / pendingApprovalItems / pendingUserInputs
		// are keyed by `<threadID>:<requestID-or-itemID>`. Approvals are
		// inherently mid-turn — the model issues a control_request, the
		// user resolves, the model continues. If EventTurnComplete fires
		// while one of these is still pending, the turn ended without
		// resolution (subprocess died, fatal error, model declined to
		// emit the resolved meta). Sweep them so the next turn doesn't
		// inherit a stale request id.
		clear(st.pendingApprovals)
		clear(st.pendingApprovalItems)
		clear(st.pendingUserInputs)
		st.pendingApprovalOrder = nil
		st.pendingUserInputOrder = nil
	}
	st.openTurn = 0
	st.openTurnSet = false
	st.interruptQueue = nil
	st.streamingItemCount = 0
	clear(st.streamingScopeCounts)
	// openAPIRetryRows tracks "thread has a running api_retry row that
	// still needs flipping". By turn-end the row was either flipped
	// already or the turn closed without forward progress; either way
	// the next turn starts with a clean flag.
	st.openAPIRetryRow = false
	// revertedTurns is normally read-and-cleared inside
	// buildRoundCompletedEvent. Defensive sweep here covers the case
	// where MarkTurnReverted was set but no turn-completed actually
	// fires (rare — e.g. the thread had no open turn so no
	// synthesizeTruncatedTurnComplete ran).
	st.revertedTurn = false
	r.mu.Unlock()
}

func isFatalProviderError(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return false
	}
	fatal, _ := meta["fatal"].(bool)
	return fatal
}

func (r *Router) markTurnItemsErrored(threadID string, turnIndex int, now int64) error {
	return r.flipTurnItemsErrored(threadID, turnIndex, now, interruptedSummary)
}

// flipTurnItemsErrored flips every running/streaming item in the turn
// to errored and rewrites its summary via summaryFn. The function-over-
// string indirection lets the user-interrupt path ("— stopped") and
// the truncated-turn path ("— interrupted") share the same iteration
// logic without either branch learning about the other.
//
// Backgrounded tool_call launches are EXEMPT (invariant 24): their work
// legitimately outlives the launching turn, and the sibling
// tool_completion row (written by EventBackgroundTaskTerminal) is the
// thing that carries the interrupted/stopped marker when the task
// settles. Mirrors the exemption in forceCloseOrphanToolCalls.
func (r *Router) flipTurnItemsErrored(
	threadID string,
	turnIndex int,
	now int64,
	summaryFn func(string) string,
) error {
	if err := r.flushStreamingThread(threadID); err != nil {
		return fmt.Errorf("error flip flush streaming buffers: %w", err)
	}
	items, err := r.store.ListTurnItems(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("error flip list turn items: %w", err)
	}
	for _, item := range items {
		if item.Status != statusRunning && item.Status != statusStreaming {
			continue
		}
		if item.IsBackground && item.Kind == itemKindToolCall {
			continue
		}
		item.Status = statusErrored
		item.Summary = summaryFn(item.Summary)
		item.UpdatedAt = now
		if err := r.persistItem(item, nil); err != nil {
			return fmt.Errorf("error flip item %s: %w", item.ID, err)
		}
	}
	return nil
}

// stoppedSummary is the user-interrupt sibling of interruptedSummary.
// Spec wording: "stopped" for a user-initiated interrupt, "interrupted"
// for a fatal crash / truncation. Both suffixes are idempotent so
// re-applying leaves the summary unchanged.
func stoppedSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return "Stopped"
	}
	const suffix = " — stopped"
	if strings.HasSuffix(summary, suffix) {
		return summary
	}
	return summary + suffix
}

// RecoverCrashedTurns settles every turn row the previous app instance
// left in-flight (completed_at=NULL) and flips that turn's stranded
// streaming/running items to errored with the same " — interrupted"
// suffix a live truncated turn-complete applies. It is the turn-level
// sibling of RecoverOrphanedBackgroundTasks: an in-app session death
// settles its turn through the synthesized truncated turn-complete,
// but nothing runs when the whole app dies — leaving a NULL row that
// GetActiveTurn reads as "turn still active", which wedged the revert
// flow behind an interrupt that had nothing to interrupt.
//
// Called once during App.ServiceStartup, after the store is open and
// before any provider session can spawn — at that point every NULL row
// is provably a crash leftover. Idempotent: the swept rows are settled,
// so a second run finds nothing. Crash-safe: the store runs the sweep
// in one transaction, so dying mid-sweep leaves every row NULL for the
// next boot. No frontend emission is needed — recovery runs before any
// client connects, and the repaired rows are what the initial loads
// read.
//
// Returns the count of settled turns.
func (r *Router) RecoverCrashedTurns() (int, error) {
	crashed, err := r.store.RecoverCrashedTurns(interruptedSummary, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("triage: recover crashed turns: %w", err)
	}
	return len(crashed), nil
}

// MarkUserInterrupt is the public chokepoint for the app-layer
// interrupt flow. It flips every streaming/running item in the
// interrupted turn to errored with a " — stopped" suffix and records a
// new "Stopped by user" system error row so the timeline carries the
// explicit user-facing signal.
//
// sampledTurnIndex is the caller's PRE-ACK OpenTurnIndex sample (-1
// when none was open) — the turn the user actually cut. It must not be
// re-resolved here: a queued echo consumed during the ack wait opens
// the queued message's turn, and a post-ack "current turn" read would
// land the stopped flips and the error row on that new turn, which
// drains naturally (round-11, C11-1). A negative sample is a no-op —
// no turn was open, so nothing was cut (round-12, CT12-1). The flip is idempotent against
// the echo path's own interrupted-predecessor flip (R10-4): rows it
// already moved out of streaming/running are skipped. tok fences the
// whole call against session replacement, like the rest of the
// post-ack block (round-11, CT11-1).
//
// Returns the error id that was persisted so the caller can surface
// diagnostics or correlate with downstream emissions; empty string
// when no turn could be resolved or the session was replaced.
func (r *Router) MarkUserInterrupt(threadID string, sampledTurnIndex int, tok FlushStampToken) (string, error) {
	if r.ThreadEpoch(threadID) != tok.threadEpoch {
		return "", nil
	}
	if sampledTurnIndex < 0 {
		// No turn was open at the pre-ack sample: nothing was cut. The
		// UI only offers Stop while a turn shows active, and the router
		// opens the turn before that emission — so a -1 sample means the
		// turn completed in the click-to-sample race and the interrupt
		// ack'd an idle CLI. Resolving a fallback here would relabel
		// completed history (LastTurnIndex) or a queued echo's freshly
		// opened turn as "Stopped by user" (round-12, CT12-1).
		return "", nil
	}
	turnIndex := sampledTurnIndex
	now := time.Now().UnixMilli()
	if err := r.flipTurnItemsErrored(threadID, turnIndex, now, stoppedSummary); err != nil {
		return "", err
	}
	seq := r.nextErrorSequence(threadID, turnIndex, "")
	errID := ErrorItemID(turnIndex, "", seq)
	item := store.Item{
		ID:        errID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "error",
		Role:      "system",
		Status:    statusCompleted,
		Summary:   "Stopped by user",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := r.persistItem(item, nil); err != nil {
		return "", err
	}
	return errID, nil
}

// Wait blocks until every in-flight Handle call has returned and every
// fire-and-forget settle goroutine has completed, or until ctx is
// cancelled. Shutdown uses this to drain the router before flushing
// observability writers and closing the store; a completed Wait means
// the SQLite write boundary has caught up to every event the router
// classified. Callers that pass a deadlined ctx get a
// context.DeadlineExceeded when the drain runs long.
func (r *Router) Wait(ctx context.Context) error {
	if r == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		// Settle goroutines outlive their spawning Handle call (the
		// caller has already returned), so wait for them too before
		// flushing stream persistence — flushAllStreamPersistence
		// writes to SQLite and must not race a settle's persistItem.
		r.settleWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return r.flushAllStreamPersistence()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForPendingSettles blocks until every fire-and-forget settle
// goroutine spawned by settleStreamingTextAsync /
// settleStreamingThinkingAsync (and the per-scope goroutines inside
// settleTurnStreaming) has completed. Used by app shutdown so SQLite
// isn't closed underneath an in-flight settle. Unlike Wait, this does
// NOT drain in-flight Handle calls — call Wait for the full shutdown
// drain.
func (r *Router) WaitForPendingSettles() {
	if r == nil {
		return
	}
	r.settleWG.Wait()
}

// CleanupThread removes all accumulator state for a thread. Call this when a
// session ends or disconnects to prevent memory leaks. Also flags the
// thread as "stopped" so any event that arrives afterward — typically a
// readLoop line that was already in-flight when StopSession returned — is
// dropped instead of persisting under the torn-down session (Bug B5).
// The flag stays set until the host commits to a replacement session
// and calls MarkThreadActive; wire events never clear it.
//
// As a safety net, any open turn at cleanup time gets a synthesized
// truncated EventTurnComplete so the frontend working indicator clears
// even when the session ends without a wire turn-complete (clean stdout
// EOF, host-side StopSession during a turn, etc). This pairs with
// session_status.go's handleSessionDied — which path runs first
// depends on whether `EventSessionStatus{"error"}` arrives before
// or after StopSession unwinds — and is idempotent in either order
// thanks to claimTurnSettlement.
func (r *Router) CleanupThread(threadID string) {
	r.cleanupThread(threadID, nil)
}

// CleanupThreadIfEpoch is CleanupThread for asynchronous teardowns that
// can lose a race against a replacement session start. The caller
// captures ThreadEpoch BEFORE unregistering the dead session; if
// MarkThreadActive has bumped the epoch since — the host committed to a
// replacement, whose spawn can take seconds while this teardown runs on
// its own goroutine — the cleanup aborts and returns false instead of
// stopping the live thread and sweeping its state.
//
// The epoch is checked twice: once on entry (before the flush/synthesize
// preamble) and again inside the locked sweep, atomically with setting
// the stopped marker. A reactivation landing between the two checks is
// therefore still caught before anything destructive happens — in that
// window the preamble can only have touched residual state of the dead
// session, because the replacement subprocess (spawned only after
// MarkThreadActive) cannot have produced turns or streams within the
// microseconds the preamble takes.
func (r *Router) CleanupThreadIfEpoch(threadID string, epoch uint64) bool {
	if r.ThreadEpoch(threadID) != epoch {
		return false
	}
	return r.cleanupThread(threadID, &epoch)
}

// cleanupThread is the shared body behind CleanupThread (requireEpoch
// nil: unconditional) and CleanupThreadIfEpoch (requireEpoch set: the
// locked sweep aborts when the thread's epoch no longer matches).
// Returns false only on an epoch-abort.
func (r *Router) cleanupThread(threadID string, requireEpoch *uint64) bool {
	cleanupAt := time.Now().UnixMilli()
	if r.hasInFlightTurnOrRound(threadID) {
		if err := r.synthesizeTruncatedTurnComplete(threadID, cleanupAt); err != nil {
			log.Printf("triage: synthesize turn-complete on cleanup for thread %s: %v", threadID, err)
		}
	}
	if err := r.flushStreamingThread(threadID); err != nil {
		log.Printf("triage: cleanup flush stream buffers for thread %s: %v", threadID, err)
	}

	// The pending-send sweep below must be total: an echo whose store
	// write is in flight holds the anchor lock and, on failure, reinserts
	// its popped entry — a sweep that ran between the pop and the
	// reinsert would lose to it, resurrecting a stale entry into the
	// replacement session (round-5, R5-2). Anchor before r.mu, per the
	// lock order.
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()

	r.mu.Lock()
	identity := r.identity(threadID)
	if requireEpoch != nil && identity.epoch != *requireEpoch {
		r.mu.Unlock()
		return false
	}
	// Set the stopped flag BEFORE dropping other state so Handle observes
	// a consistent snapshot: any concurrent Handle call either sees a live
	// thread with full state, or a stopped thread with no state.
	identity.stopped = true
	var orphanSpan trace.Span
	// Invalidate a span start that is still outside the lock doing thread
	// lookup or invoking the tracer. The generation remains monotonic across
	// session reuse so that stale start can never match a later session.
	identity.turnSpanGeneration++

	// The per-thread sweep is COMPLETE BY CONSTRUCTION: every map this
	// routine used to delete from one key at a time now lives on the
	// thread's *threadState (thread_state.go), so dropping that one
	// entry drops all of it. A new field added there is swept the day
	// it is added — which is the whole point of the struct, and why
	// nothing may be added back to the Router as a thread-keyed map.
	//
	// What survives, deliberately:
	//   - threadIdentity (epochs, generations, flush-stamp ledger,
	//     interrupt marks, the anchor/drain locks). Never deleted; a
	//     reset-to-zero would let a stale captured epoch match again.
	//   - threads.live_todo in SQLite. The todo list lives on the thread
	//     row (migration v65) precisely so it survives the session that
	//     reported it; only the provider emptying the list clears it.
	//
	// The three reads below happen BEFORE the delete because they need
	// state the delete is about to destroy; everything after them is a
	// side effect (timer stop, span outcome, emit), not a sweep.
	st := r.threadStateIfPresent(threadID)
	var (
		hadEffectiveModel      bool
		effectiveModelRevision uint64
		pendingUsage           provider.UsageEvent
		hasPendingUsage        bool
	)
	if st != nil {
		orphanSpan, st.turnSpan = st.turnSpan, nil
		// Stop the in-flight flush timers before the buffers become
		// unreachable — a live timer would otherwise fire against a
		// buffer nothing can ever flush again.
		for _, buffer := range st.streamPersistBuffers {
			if buffer != nil && buffer.timer != nil {
				buffer.timer.Stop()
			}
		}
		hadEffectiveModel = st.effectiveModelSet
		pendingUsage, hasPendingUsage = r.takeUsageEmitPendingLocked(threadID)
	}
	delete(r.threads, threadID)
	if hadEffectiveModel {
		// The revision counter lives on the identity, so it is still
		// here after the delete: the frontend needs a STRICTLY newer
		// revision to accept the clear.
		effectiveModelRevision = r.nextEffectiveModelRevisionLocked(threadID)
	}
	r.mu.Unlock()

	if hadEffectiveModel {
		r.emit(eventchan.ProviderModelFallback, ModelFallbackEvent{ThreadID: threadID, Revision: effectiveModelRevision})
	}

	if hasPendingUsage {
		r.emit(eventchan.ProviderUsage, pendingUsage)
	}

	if orphanSpan != nil {
		r.recordTurnSpanOutcome(orphanSpan, cleanupTurnOutcome())
	}
	if r.store != nil {
		count, err := r.store.MarkLiveCodexSubagentLaunchesInactive(threadID, cleanupAt)
		if err != nil {
			log.Printf("triage: cleanup live Codex subagent launches for thread %s: %v", threadID, err)
		} else if count > 0 {
			r.emitCodexBackgroundTasksChanged(threadID)
		}
	}
	return true
}

// isThreadStopped returns true when CleanupThread has been called for
// threadID and the host has not since re-activated it via
// MarkThreadActive.
func (r *Router) isThreadStopped(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.identityIfPresent(threadID)
	return id != nil && id.stopped
}

// MarkThreadActive clears the stopped flag so a replacement session's
// events persist again. The host calls this from the session-start
// path BEFORE spawning the new subprocess — it must not wait for a
// wire signal, because a session that dies during startup (e.g. Claude
// rejecting --resume-session-at pre-init) emits its only diagnostics
// before anything init-like, and those must route. Clearing pre-spawn
// is safe: the prior subprocess's read loop is fully drained before a
// start proceeds (provider Close blocks on read-loop exit), so no
// stale frame can slip through the freshly-cleared gate. No wire event
// may clear this flag — see the threadIdentity.stopped field comment.
//
// Reactivation also resets the thread's logical-turn settlement ledger.
// The context-repair restart deliberately skips CleanupThread, so
// settledTurns can still hold entries from the prior subprocess; a
// replacement session reuses turn indexes, and a stale "settled" marker
// would misroute its first error result into persistLateTurnPayload
// (folding into a finished turn's row) instead of the orphan error-item
// branch the user actually needs to see. Clearing here is safe for the
// soft-close late-fold path — the fold's trailing result rides the SAME
// session's read loop, which is fully drained before any restart
// reaches this call.
//
// Each call bumps the thread's reactivation epoch (see ThreadEpoch),
// which lets asynchronous teardowns detect that a replacement session
// claimed the thread mid-teardown and abort their cleanup.
func (r *Router) MarkThreadActive(threadID string) {
	identity := r.identity(threadID)
	r.mu.Lock()
	identity.stopped = false
	identity.epoch++
	// This is NOT the cleanup sweep — the repair-restart path reaches
	// here WITHOUT a CleanupThread, so the thread's state entry can be
	// fully live and must survive. Only the fields whose meaning is
	// bound to the process that just died are cleared, each for its own
	// reason. Anything not listed here deliberately carries over.
	var hadEffectiveModel bool
	var effectiveModelRevision uint64
	if st := r.threadStateIfPresent(threadID); st != nil {
		// A stale settlement marker would misroute a replacement
		// session's first error result into persistLateTurnPayload
		// (see the doc comment above).
		st.settledTurns = nil
		hadEffectiveModel = st.effectiveModelSet
		st.effectiveModel, st.effectiveModelSet = "", false
		// A pending harness wakeup is in-process state of the PREVIOUS
		// CLI process; the replacement session this call commits to
		// never inherits the timer. Without it a stale future fire time
		// would shield the fresh session from the idle reaper for up to
		// the wakeup clamp (60 min).
		st.pendingWakeupAt, st.pendingWakeupSet = 0, false
		// The ticks belong to the provider PROCESS: a dead process will
		// never deliver the terminal that would consume them.
		st.subagentProgress = nil
		// A replacement process is never mid-compaction; a stale window
		// would pin a "Compacting" label onto the fresh session.
		st.compactingSince, st.compactingSinceSet = 0, false
		// Same reasoning for the command-lifecycle correlation: the acks
		// it waits on can only come from the process that just died.
		st.commandLifecycle = nil
	}
	if hadEffectiveModel {
		effectiveModelRevision = r.nextEffectiveModelRevisionLocked(threadID)
	}
	// Interrupt marks are session-scoped: an in-flight echo cannot
	// survive the session whose read loop carries it, and a replacement
	// session after revert REUSES turn indexes — a lingering mark would
	// settle a reused-index predecessor "interrupted" with no new
	// interrupt (round-11, CT11-2). Deletion is safe here (unlike the
	// epoch maps): mark entries are keyed by router-unique seqs, so a
	// stale restore's removal against the fresh list finds nothing.
	identity.interruptMarks = nil
	r.mu.Unlock()
	if hadEffectiveModel {
		r.emit(eventchan.ProviderModelFallback, ModelFallbackEvent{ThreadID: threadID, Revision: effectiveModelRevision})
	}
}

// ThreadEpoch returns the thread's reactivation epoch: a counter bumped
// by every MarkThreadActive. Asynchronous teardowns capture it before
// unregistering a dead session and pass it to CleanupThreadIfEpoch so a
// cleanup that lost the race against a replacement session start
// no-ops instead of stopping the live thread. Epoch entries are never
// deleted — a removed entry would read as 0 again and let a stale
// teardown's captured 0 match.
func (r *Router) ThreadEpoch(threadID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.identityIfPresent(threadID); id != nil {
		return id.epoch
	}
	return 0
}
