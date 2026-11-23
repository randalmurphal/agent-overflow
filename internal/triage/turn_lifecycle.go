package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
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
// It opens the turn's first round: per-turn state (setOpenTurn) and a
// round snapshot whose id `provider:turn_started` carries as TurnID. A
// continuation after a soft close reopens the same turn
// (reopenSoftClosedTurn), keeping its id-allocating counters.
// See docs/architecture/turn-lifecycle.md §Rounds and turns.
func (r *Router) handleTurnStart(evt provider.ProviderEvent) error {
	return r.startTurn(evt, func() (int, error) {
		return r.resolveTurnIndexOnStart(evt), nil
	})
}

// startTurn opens the logical turn resolve names. Every turn opens here:
// an AO send's (handleTurnStart) and one the provider started
// (openProviderStartedTurn). What the turn took in is not decided here:
// its deliveries name it (SendDelivery). A retired session's frame opens
// nothing.
func (r *Router) startTurn(evt provider.ProviderEvent, resolve func() (int, error)) error {
	started, published := r.beginTurnStart(evt)
	if started == nil {
		return nil
	}
	defer published()
	// An interrupt sampled from here until the open turn is set waits
	// for it (SampleInterrupt): the row written below is already the
	// active turn GetActiveTurn-gated callers stop.
	defer started()
	turnIndex, err := resolve()
	if err != nil {
		return err
	}
	startedAt := eventTimestampMillis(evt)
	// A turn still open when another starts never gets a completion of
	// its own (the CLI's one result settles only the newest turn), so it
	// settles here, as a mid-loop echo's predecessor does.
	if open, ok := r.openTurnIndex(evt.ThreadID); ok && open != turnIndex {
		r.settleQueuedEchoPredecessor(evt.ThreadID, open, startedAt, r.predecessorStopReason(evt.ThreadID, open, -1), evt.SessionToken)
	}

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

	r.mu.Lock()
	gate := r.turnRowWrittenGate
	r.mu.Unlock()
	if gate != nil {
		gate(evt.ThreadID)
	}

	// A failed ownership read is reported once the turn is open: the turn
	// starts either way.
	opened, openErr := r.setOpenTurn(evt.ThreadID, turnIndex, evt.SessionToken)
	started()
	if !opened {
		return openErr
	}
	r.releaseDeliveriesAtTurnStart(evt.ThreadID, turnIndex, evt.SessionToken)
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
	if !r.setOpenRoundSnapshot(snapshot, evt.SessionToken) {
		return openErr
	}
	r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
	return openErr
}

// resolveTurnIndexOnStart picks the right turn index for an incoming
// EventTurnStart / EventInit-promoted-to-start. Three sources, in
// priority order:
//
//  1. evt.TurnIndex when the wire carries it directly (Codex
//     turn/started, recovery replays).
//
//  2. The first pending send that has not opened a turn yet, when the
//     wire carries no index but an AO send is awaiting confirmation.
//     The dispatcher (RegisterPendingSend / RegisterPendingFlushSend)
//     stamps the AO-decided turn index BEFORE the provider write, so
//     that entry carries the authoritative answer for the new turn.
//     The claim marks the entry OpenedTurn and leaves it queued; the
//     pop is owned by its confirmation. A send whose confirmation never
//     arrives therefore costs one turn, never every later one.
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
	if turnIndex, ok := r.claimTurnOpeningPendingSend(evt.ThreadID); ok {
		return turnIndex
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
				r.providerWriteFailed(turn.ThreadID, fmt.Sprintf("backfill provider turn id %s onto %s", incomingProvider, existing.TurnID), err)
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
	// A retired session's in-flight complete must not take the replacement's
	// round, settle or fold into its turn, or flush its queue.
	if r.frameRetired(evt.ThreadID, evt.SessionToken) {
		return nil
	}
	flushQueueAtBoundary := true
	defer func() {
		if flushQueueAtBoundary {
			r.maybeFlushQueueAtBoundary(evt.ThreadID, evt.SessionToken)
		}
	}()
	// The turn's writes are done once settlement below returns; push the
	// anchors they stamped now rather than a second later (wire_items.go).
	defer r.flushWireItemRefresh(evt.ThreadID)

	// A turn boundary always closes the live compacting window: a failed
	// Codex compaction abandons its contextCompaction item without
	// completing it, so no boundary or explicit close frame ever arrives —
	// the turn's own completion is the only reliable close. No-op when no
	// window is open (compaction_status.go).
	r.clearCompacting(evt.ThreadID)

	// Two cadences (docs/architecture/turn-lifecycle.md §Rounds and
	// turns):
	//
	//   1. Per-ROUND emission (claimed below, emitted when this handler
	//      returns). takeOpenRound clears the round slot in the same
	//      critical section it returns it; an empty slot means the round
	//      already ended (a soft close, or a synthetic complete that raced
	//      ahead on handleError's fatal path), so no second emit.
	//
	//   2. Per-LOGICAL-TURN settlement (claimTurnSettlement-gated below):
	//      the `turns` row, streaming items and orphan tool calls, once
	//      per (thread, turn_index). A soft close skips it; the result or
	//      the next turn's start settles the turn. A second complete for a
	//      settled turn (handleError's synthetic complete, then the real
	//      result) folds its late payload only.
	//
	// Live fast-mode state rides the wire result envelope. Emitted before
	// every settlement branch below (including the orphan-error and
	// not-activity early returns) because it is a report about the
	// SESSION, not about this turn's outcome — a turn that errored still
	// tells the truth about whether fast mode was serving it.
	r.emitFastModeState(evt.ThreadID, fastModeStatusFromTurnComplete(evt))

	countsAsActivity := turnCountsAsThreadActivity(evt)
	turnIndex, owned, err := r.completionTurnIndex(evt.ThreadID)
	meta, metaErr := turnCompleteMetaFromEvent(evt)
	if metaErr != nil {
		log.Printf("triage: %v", metaErr)
		if err == nil {
			err = metaErr
		}
	}
	now := eventTimestampMillis(evt)

	// Per-round emission. The round is claimed before the
	// claimTurnSettlement gate so a second `result` envelope for the same
	// logical turn still emits the round-end signal. takeOpenRound is a
	// read-and-clear: at most one emit per round, regardless of synthesis
	// races. The event is announced only after every settlement write
	// below: a client that reacts by reading turn state (send admission,
	// the workspace lock, revert eligibility) must find the turns row and
	// its streaming items already settled. Registered after the other
	// defers so it runs before them, and in particular before the queue
	// boundary flush can start the next turn.
	//
	// A failed turn-index or payload read still ends the round it took:
	// the event carries the round's own index, and nothing else would
	// ever tell a client the round is over.
	//
	// quiet is set below when the turn is one nobody needs to read
	// (turnIsQuiet), and the event says so.
	quiet := false
	round, hasRound := r.takeOpenRound(evt.ThreadID, evt.SessionToken)
	if hasRound {
		emitIndex := turnIndex
		if err != nil {
			emitIndex = round.TurnIndex
		}
		defer func() {
			event := r.buildRoundCompletedEvent(evt, round.TurnID, emitIndex, now, meta)
			event.Quiet = quiet
			// The round ends before its announcement, so whatever the
			// announcement starts (a worker's reassessment, a wait's
			// read) finds it ended. The settle writes are done by now,
			// so a reader between the two reads the turn as its row
			// does.
			r.endRound(evt.ThreadID, round.TurnID)
			r.emit(eventchan.ProviderTurnCompleted, event)
		}()
	}

	// Orphan error result: an error turn-complete with no open round, no
	// open turn, and no prior settlement means the provider failed before
	// the turn machinery ever engaged — e.g. Claude dying pre-init when
	// a fork start's --resume-session-at cursor is unusable (the resulting
	// result{error_during_execution} is the only wire output such a
	// session ever produces). There is no round to emit and no turns row
	// to settle, so without this branch the error reaches nothing the
	// user can see: persist a system error item instead. The
	// provider:item_event upsert it emits is the surface the frontend
	// already clears its optimistic pending-send "Working" state on.
	//
	// The settled-turn guard — checked against the event's NATURAL turn
	// (currentTurnIndex fallback) — keeps the legitimate late-fold case
	// flowing past this branch: a trailing result{is_error} after a soft
	// close settled the turn must reach persistLateTurnPayload below, not
	// mint an error item. The attributed turn prefers the pending send
	// that has not opened a turn yet when one exists (the
	// dispatcher-stamped index for the send that provoked the failing
	// start; deferred flush items are not in SQLite yet, so LastTurnIndex
	// would point one turn early). The entry is NOT consumed —
	// CleanupThread or its confirmation owns the pop.
	//
	// A retired session's result is no orphan of the replacement's, whose
	// empty ledger only makes it look like one, and writes nothing
	// (claimOrphanError).
	if err == nil && meta.Error != "" && !hasRound {
		switch attributedTurn, claim := r.claimOrphanError(evt.ThreadID, turnIndex, evt.SessionToken); claim {
		case orphanErrorRetired:
			flushQueueAtBoundary = false
			return nil
		case orphanErrorClaimed:
			// A session that just reported a hard failure cannot serve
			// queued sends — leave the flush queue for the next session.
			flushQueueAtBoundary = false
			return r.persistProviderErrorItem(evt.ThreadID, attributedTurn, meta.Error, nil, "", "", now)
		case orphanErrorNone:
		}
	}

	if err == nil && !countsAsActivity {
		return nil
	}
	// The fallback turn is a waiting message's or a previous session's
	// finished turn: nothing here owns the completion.
	if err == nil && !owned {
		log.Printf("triage: turn complete for %s matches no turn of this session; nothing to settle", evt.ThreadID)
		return nil
	}

	// A Stop held for this turn (stop_waiting_send.go) is resolved in the
	// same step that claims the settlement: owed means the turn was cut and
	// the stop row is written below.
	_, softClose := evt.TurnComplete.(*provider.SoftRoundCloseMeta)
	cut := canonicalStopReason(meta) == "interrupted"
	var heldStop waitingStop
	var stopOwed bool
	if err == nil {
		// claimTurnSettlement is sticky on partial failure (same trade-off
		// as markTurnCaptured): once the FIRST handleTurnComplete reaches
		// here, subsequent invocations for the same (thread, turn) skip
		// lifecycle settlement. Late usage on a duplicate is still folded
		// into the existing turn row below so accounting is not lost.
		var claim settlementClaim
		claim, heldStop, stopOwed = r.claimTurnSettlement(evt.ThreadID, turnIndex, evt.SessionToken, cut, softClose)
		if claim == settlementRetired {
			// The session was replaced while this result was handled:
			// the turn it resolved to is the replacement's.
			flushQueueAtBoundary = false
			return nil
		}
		if claim == settlementTaken {
			// Late-fold path: a second activity-counting complete for an
			// already-settled logical turn (handleError's synthetic
			// complete, then the real result). persistLateTurnPayload folds
			// the late turns-row columns below, but the turn can still carry
			// streaming items or running foreground tool_calls that no
			// content_block_stop or tool_result ever closed. Without
			// settling them here they leak a permanent status=streaming /
			// status=running row: the user's stuck "thinking" spinner.
			//
			// This is a deliberate SUBSET of the full settle path: the
			// once-per-logical-turn steps (turns-row settle,
			// markTurnItemsErrored, forced interrupt-drain) already ran at the
			// first settlement, so only the orphan settle + force-close repeat
			// here — both idempotent on the common cascade (drained active
			// maps / no running foreground tool_calls → no-ops). It must NOT
			// touch the id-allocating counters, which keep later row ids
			// from colliding with the turn's earlier rows. Settle failures are returned (the wire
			// pump logs a returned error and keeps reading) so they surface
			// the same way the full path's persistErr does.
			//
			// A soft close kept a Stop held for this turn; its result
			// resolves it here.
			lateStop, lateStopOwed := r.settleWaitingStop(evt.ThreadID, turnIndex, evt.SessionToken, cut, softClose)
			lateStatus := settledTurnStatus(meta)
			agentScopes, lateErr := r.agentOwnedOpenScopes(evt.ThreadID)
			if lateStopOwed {
				if _, err := r.writeTurnStop(evt.ThreadID, turnIndex, lateStop.summary); err != nil {
					lateErr = errors.Join(lateErr, fmt.Errorf("write held stop of %s/%d: %w", evt.ThreadID, turnIndex, err))
				}
			}
			lateErr = errors.Join(lateErr, r.settleTurnStreaming(evt.ThreadID, turnIndex, lateStatus, agentScopes))
			lateErr = errors.Join(lateErr, r.forceCloseOrphanToolCalls(evt.ThreadID, turnIndex, now))
			// A round of its own announced this completion time, which a
			// client reads as the thread's newest activity; the row
			// follows so a read clears it everywhere.
			var roundCompletedAt int64
			if hasRound {
				roundCompletedAt = now
			}
			foldErr := r.persistLateTurnPayload(evt, turnIndex, meta, roundCompletedAt)
			lateErr = errors.Join(lateErr, foldErr)
			// A later round of a quiet turn is as quiet: a read thread
			// stays read past the completion it advanced to. A turn whose
			// earlier round failed stays the user's.
			if foldErr == nil && roundCompletedAt != 0 && r.turnIsQuiet(evt.ThreadID, turnIndex, meta) &&
				!r.turnFailed(evt.ThreadID, turnIndex) {
				quiet = true
				r.keepThreadReadPast(evt.ThreadID, r.persistedTurnID(evt, turnIndex), roundCompletedAt)
			}
			r.FlushUsageEmitThrottle(evt.ThreadID)
			r.noteTurnSettled(evt.ThreadID, turnIndex, evt)
			return lateErr
		}
	}
	var persistErr error
	// A held Stop the turn's end proves was delivered settles the turn as
	// stopped: its rows flip errored with the " — stopped" suffix.
	truncated := turnCompleteIsTruncated(meta) || stopOwed
	if truncated {
		flushQueueAtBoundary = false
	}
	// The streams and queued rows an agent owns outlive this turn: its
	// end settles them. A failed ownership read leaves them all to the
	// turn, as before agents outlived turns, and is reported.
	agentScopes, scopeErr := r.agentOwnedOpenScopes(evt.ThreadID)
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
			if err := r.drainTurnQueue(evt.ThreadID, agentScopes, true); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			status := settledTurnStatus(meta)
			if stopOwed {
				status = statusErrored
			}
			if err := r.settleTurnStreaming(evt.ThreadID, turnIndex, status, agentScopes); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil && truncated {
			summary := interruptedSummary
			if stopOwed {
				summary = stoppedSummary
			}
			if err := r.flipTurnItemsErrored(evt.ThreadID, turnIndex, now, summary); err != nil {
				persistErr = err
			}
		}
		if persistErr == nil {
			// For the non-truncated path this drains any queued events
			// that settled outside settleTurnStreaming's idle-drain
			// window (rare but possible). For the truncated path the
			// queue is already empty so this is a cheap no-op.
			if err := r.drainTurnQueue(evt.ThreadID, agentScopes, truncated); err != nil {
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
	// The per-round `provider:turn_completed` emission follows when this
	// handler returns.
	//
	// Anchor recording does NOT happen here. Message anchors are
	// recorded by app_send.go before provider stdin/RPC dispatch so
	// the anchor maps directly to "before this user message".
	//
	// A held Stop's row is written first, so a reader that finds the turn
	// complete finds how it was stopped.
	var stopErr error
	if stopOwed {
		if _, err := r.writeStopRow(evt.ThreadID, turnIndex, heldStop.summary, now); err != nil {
			stopErr = fmt.Errorf("write held stop of %s/%d: %w", evt.ThreadID, turnIndex, err)
		}
	}
	quiet = err == nil && persistErr == nil && r.turnIsQuiet(evt.ThreadID, turnIndex, meta)
	persistErr = errors.Join(persistErr, r.settleTurnRow(evt, turnIndex, now, meta, persistErr, quiet), scopeErr, stopErr)

	r.clearOpenTurn(evt.ThreadID, agentScopes)
	r.finishTurnSpan(evt.ThreadID, completedTurnOutcome(meta, persistErr))
	r.FlushUsageEmitThrottle(evt.ThreadID)
	r.noteTurnSettled(evt.ThreadID, turnIndex, evt)

	return persistErr
}

// noteTurnSettled records whether the CLI turn behind a settled logical
// turn can still continue it. A soft round close (the parent's message
// ended with end_turn, stop_sequence or refusal) settles the turn while
// the CLI turn's `result` is still to come, and the model can resume
// first (a ToolSearch or ExitPlanMode continuation): reopenSoftClosedTurn
// continues it then. The result ends that possibility.
func (r *Router) noteTurnSettled(threadID string, turnIndex int, evt provider.ProviderEvent) {
	_, soft := evt.TurnComplete.(*provider.SoftRoundCloseMeta)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(threadID, evt.SessionToken) {
		return
	}
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return
	}
	if !soft {
		st.softClosedTurnSet = false
		return
	}
	st.softClosedTurn, st.softClosedTurnSet = turnIndex, true
}

// reopenSoftClosedTurn continues the turn a soft round close settled when
// the model resumes it before the CLI turn's `result`. The turns row is
// open again, so GetActiveTurn-gated callers (send placement, revert)
// see the running work, and the open-turn state is restored without
// resetting the turn's id-allocating counters, so the continuation's
// rows extend the turn without colliding with the rows before the close.
// A new round announces it. Its next soft close or result settles it
// again. Reports whether a turn was reopened.
func (r *Router) reopenSoftClosedTurn(evt provider.ProviderEvent) (bool, error) {
	r.mu.Lock()
	st := r.threadStateIfPresent(evt.ThreadID)
	if st == nil || !st.softClosedTurnSet || st.openTurnSet || r.frameRefusalLocked(evt) != "" {
		r.mu.Unlock()
		return false, nil
	}
	turnIndex := st.softClosedTurn
	st.softClosedTurnSet = false
	st.openTurn, st.openTurnSet = turnIndex, true
	delete(st.settledTurns, turnIndex)
	opening := r.markTurnOpeningLocked(evt.ThreadID, st)
	defer opening()
	r.mu.Unlock()

	err := r.store.ReopenTurn(r.persistedTurnID(evt, turnIndex))
	if err != nil {
		err = fmt.Errorf("reopen soft-closed turn %s/%d: %w", evt.ThreadID, turnIndex, err)
	} else {
		// After the row is open again: a reader told this turn answers a
		// send must not find the completion the soft close wrote.
		r.releaseDeliveriesAtTurnStart(evt.ThreadID, turnIndex, evt.SessionToken)
	}
	r.openTurnSpan(evt, turnIndex)
	snapshot := ActiveTurnSnapshot{
		ThreadID:  evt.ThreadID,
		TurnID:    uuid.NewString(),
		TurnIndex: turnIndex,
		StartedAt: eventTimestampMillis(evt),
	}
	if r.setOpenRoundSnapshot(snapshot, evt.SessionToken) {
		r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
	}
	return true, err
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
// (one TX, one thread-touch) to cut per-orphan roundtrips, and so is the
// page read behind their upserts; the frontend-bound
// `provider:item_event` upserts stay per-row so the UI still updates
// each card independently.
func (r *Router) forceCloseOrphanToolCalls(threadID string, turnIndex int, now int64) error {
	flipped, err := r.store.ForceCloseRunningToolCallsInTurn(threadID, turnIndex, ForceCloseSummary, now)
	if err != nil {
		return fmt.Errorf("force-close orphan tool calls: %w", err)
	}
	r.emitItemUpserts(threadID, flipped)
	for _, item := range flipped {
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
// because the two consumers run in different cadences (logical turn
// vs round) but the projection rules — stop_reason fallback to
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
// facing `provider:turn_completed` emission follows this write (see
// handleTurnComplete: takeOpenRound at the top, claimTurnSettlement
// gate, this, then the deferred emit).
//
// Top-level turn settle bumps thread activity through
// Store.MarkThreadActivity. Nested/internal turns only update turn state,
// so subagent completions do not reorder the sidebar. Synthesized
// session-died / abort flavors flow through this same path when they
// belong to the top-level turn. A quiet turn (turnIsQuiet) bumps nothing
// and keeps a read thread read past its completion.
func (r *Router) settleTurnRow(evt provider.ProviderEvent, turnIndex int, now int64, meta turnCompleteMeta, persistErr error, quiet bool) error {
	fields := decodeTurnCompleteFields(evt, r.persistedTurnID(evt, turnIndex), meta, persistErr)

	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}

	// A settle with no stop reason and no error is a wire result that named
	// no outcome (Claude answers a slash command that way). Settling such a
	// result while the send that opened this turn still awaits its echo
	// contradicts the wire order, so name it: the row is re-settled if a
	// later init reopens the turn, but nothing else records that it was.
	if fields.stopReason == "" && fields.errorMessage == "" {
		if head, ok := r.peekPendingSendHead(evt.ThreadID); ok && head.TurnIndex == turnIndex {
			log.Printf("triage: turn %s/%d settled without a stop reason before its send %s was echoed", evt.ThreadID, turnIndex, head.AOItemID)
		}
	}
	r.mu.Lock()
	gate := r.turnSettleGate
	r.mu.Unlock()
	if gate != nil {
		gate(evt.ThreadID)
	}
	settled := r.writeTurnEnd(evt.ThreadID, fields.logicalTurnID, now, fields.stopReason, fields.assistantMessageID, usageJSON, fields.errorMessage, "update turn "+fields.logicalTurnID) == nil
	usageErr := r.appendUsageLedger(evt, fields.logicalTurnID, meta, now)
	if quiet {
		if settled {
			r.keepThreadReadPast(evt.ThreadID, fields.logicalTurnID, now)
		}
	} else if turnCountsAsThreadActivity(evt) {
		r.bumpThreadActivity(evt.ThreadID, now, "turn settle")
	}
	return usageErr
}

// writeTurnEnd writes turnID's end. Every write of a turn's end goes
// through it, so the user hears of a failure the same way whichever path
// wrote it (reportUnsavedTurnEnd), and a later end of the same turn that
// saves withdraws what the user was told (reportSavedTurnEnd). Nothing
// retries the write: an open row stays open until a later end saves or the
// next boot's sweep settles it as interrupted (turn-lifecycle.md §Crash
// behavior). The failure is reported here and returned.
//
// A turn's end has writers on more than one goroutine (the thread's event
// worker, and a stop or replacement that ends the turn itself), so the
// write and its report are one step under turnEndMu: the entry the user is
// left with is the outcome of the write that landed last. The store's one
// writer connection already runs the writes one at a time.
func (r *Router) writeTurnEnd(threadID, turnID string, completedAt int64, stopReason, assistantMessageID, tokenUsageJSON, errorMessage, what string) error {
	r.turnEndMu.Lock()
	defer r.turnEndMu.Unlock()
	if err := r.store.UpdateTurnCompleted(turnID, completedAt, stopReason, assistantMessageID, tokenUsageJSON, errorMessage); err != nil {
		told := r.providerWriteFailed(threadID, what, err)
		r.reportUnsavedTurnEnd(threadID, turnID, err, told)
		return fmt.Errorf("%s: %w", what, err)
	}
	r.reportSavedTurnEnd(threadID, turnID)
	return nil
}

func turnCountsAsThreadActivity(evt provider.ProviderEvent) bool {
	return strings.TrimSpace(evt.ParentToolUseID) == ""
}

// turnIsQuiet reports whether the top-level turn turnIndex of threadID,
// ending with meta, is one nobody needs to read: the classifier names it
// (SetQuietTurnClassifier) and it ended cleanly, with no error and no
// interrupt. A turn that failed is the user's to see whoever started it.
// The classifier reads what the turn took in from its deliveries
// (SetSendDeliveredHook), which are reported before its end.
func (r *Router) turnIsQuiet(threadID string, turnIndex int, meta turnCompleteMeta) bool {
	if meta.Error != "" || turnCompleteIsTruncated(meta) || canonicalStopReason(meta) == "error" {
		return false
	}
	r.quietTurnMu.Lock()
	classify := r.quietTurn
	r.quietTurnMu.Unlock()
	return classify != nil && classify(threadID, turnIndex)
}

// turnFailed reports whether threadID's turn turnIndex is recorded as
// failed, as an earlier round of it leaves it. A failed read answers that it
// failed, the direction that leaves the turn the user's, and is logged.
func (r *Router) turnFailed(threadID string, turnIndex int) bool {
	if r.store == nil {
		return false
	}
	turn, found, err := r.store.GetTurnByThreadIndex(threadID, turnIndex)
	if err != nil {
		log.Printf("triage: read turn %s/%d: %v", threadID, turnIndex, err)
		return true
	}
	return found && turn.StopReason == "error"
}

// keepThreadReadPast keeps threadID read past at, the completion of its
// quiet turn turnID, when it was read up to the turn
// (store.KeepThreadReadPast), and announces the row it moved. A failed
// write leaves the thread owed a read, which is the safe direction, and is
// logged.
func (r *Router) keepThreadReadPast(threadID, turnID string, at int64) {
	if r.store == nil {
		return
	}
	if moved, err := r.store.KeepThreadReadPast(threadID, turnID, at); err != nil {
		log.Printf("triage: keep %s read past its quiet turn %s: %v", threadID, turnID, err)
	} else if moved {
		r.emitThreadRow(threadID)
	}
}

// buildRoundCompletedEvent builds the frontend-facing
// TurnCompletedEvent payload for a single round. The TurnID is
// the per-round uuid (allocated where the round opened: startTurn,
// openQueuedEchoTurn, reopenSoftClosedTurn) — the frontend treats each round as a distinct active
// turn, replacing its activeTurn entry on each round-start and
// clearing on the matching complete. StartedAt prefers the persisted
// `turns` row's started_at (logical-turn clock) and falls back to
// `now` when no row exists yet (mid-crash-recovery edge case).
//
// Pure projection — does not write the `turns` row. settleTurnRow
// owns the UPDATE and runs once per logical turn under the
// claimTurnSettlement gate.
//
// No persistErr parameter: the payload reports what the provider said
// about the round. A persistence failure in handleTurnComplete surfaces
// through its returned error, not through the round's stop_reason or
// error_message.
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
		ThreadID:           evt.ThreadID,
		TurnID:             roundID,
		TurnIndex:          turnIndex,
		StartedAt:          startedAt,
		CompletedAt:        now,
		StopReason:         fields.stopReason,
		AssistantMessageID: fields.assistantMessageID,
		TokenUsage:         meta.Usage,
		ErrorMessage:       fields.errorMessage,
		Aborted:            meta.Aborted || meta.Truncated,
		CountsAsActivity:   turnCountsAsThreadActivity(evt),
		HistoryRev:         stamp.Rev,
		HistoryEpoch:       stamp.Epoch,
	}
}

// persistLateTurnPayload folds late-arriving payload onto the existing
// turn row when handleTurnComplete fires after the turn was already
// settled: handleError's synthetic truncated complete settles it, then
// the real `result` arrives because the subprocess kept writing. The
// result carries the usage and the final assistant message id the
// synthetic complete lacked.
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
//   - stop_reason / error_message: late error wins, so an error the
//     trailing `result{is_error:true}` reports stays visible in
//     persisted history.
//   - completed_at: advances to roundCompletedAt, the CompletedAt the
//     round this completion took carried (0 when it took none). A
//     thread the user read before that round ended is unread again
//     until read after it, on whichever client.
//
// Folded as a single UPDATE so the common case (both fields arrive
// on the trailing `result`) pays one autocommit boundary.
func (r *Router) persistLateTurnPayload(evt provider.ProviderEvent, turnIndex int, meta turnCompleteMeta, roundCompletedAt int64) error {
	turnID := r.persistedTurnID(evt, turnIndex)
	// Ledger rows append on every settle event: the provider emits
	// per-turn DELTAS, so a late fold's usage is new spend the first
	// settle could not have carried (a synthetic complete has none).
	usageErr := r.appendUsageLedger(evt, turnID, meta, eventTimestampMillis(evt))
	usageJSON := ""
	if len(meta.Usage) > 0 {
		usageJSON = string(meta.Usage)
	}
	amid := meta.AssistantMessageID
	stopReason, errorMessage := lateErrorTurnPayload(meta)
	if usageJSON == "" && amid == "" && stopReason == "" && errorMessage == "" && roundCompletedAt == 0 {
		return usageErr
	}
	if err := r.store.UpdateTurnLatePayload(turnID, store.LateTurnPayload{
		TokenUsageJSONIfEmpty:       usageJSON,
		AssistantMessageIDOverwrite: amid,
		StopReasonOverwrite:         stopReason,
		ErrorMessageOverwrite:       errorMessage,
		CompletedAtAdvance:          roundCompletedAt,
	}); err != nil {
		return errors.Join(usageErr, fmt.Errorf("update turn %s late payload: %w", turnID, err))
	}
	return usageErr
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

// completionTurnIndex is the turn a turn-complete settles: the open turn,
// else the soft-closed turn whose CLI turn the trailing result ends, else
// the newest turn that started (the newest row when none has a turns row).
// owned reports whether the completion may settle that fallback: not when
// it is a sent message's turn that has not started, as currentTurnIndex's
// newest-row fallback would pick once the message's row holds the next
// index, and not when a previous session finished it (an AO internal
// command's result after a resume). Unfinished work and a turn this
// session settled (a duplicate result) are owned.
func (r *Router) completionTurnIndex(threadID string) (turnIndex int, owned bool, err error) {
	r.mu.Lock()
	if st := r.threadStateIfPresent(threadID); st != nil && (st.openTurnSet || st.softClosedTurnSet) {
		turnIndex := st.openTurn
		if !st.openTurnSet {
			turnIndex = st.softClosedTurn
		}
		r.mu.Unlock()
		return turnIndex, true, nil
	}
	r.mu.Unlock()
	turns, err := r.store.ListRecentTurns(threadID, 1)
	if err != nil {
		return 0, false, err
	}
	finished := false
	if len(turns) == 1 {
		turnIndex, finished = turns[0].TurnIndex, turns[0].CompletedAt != nil
	} else if turnIndex, err = r.store.LastTurnIndex(threadID); err != nil {
		return 0, false, err
	}
	if pending, ok := r.turnOpeningPendingSend(threadID); ok && pending.TurnIndex == turnIndex {
		return turnIndex, false, nil
	}
	return turnIndex, !finished || r.isTurnSettled(threadID, turnIndex), nil
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
// or -1 when none is in flight. Interrupt paths sample
// SampleInterrupt instead, which waits out a turn start in progress.
func (r *Router) OpenTurnIndex(threadID string) int {
	if turnIndex, ok := r.openTurnIndex(threadID); ok {
		return turnIndex
	}
	return -1
}

// InterruptSample is what an interrupt of a thread cuts, sampled by
// SampleInterrupt before the provider's interrupt ack.
type InterruptSample struct {
	// Turn is the open turn, or -1 when none was open.
	Turn int
	// waiting reports a send the provider held whose turn had not opened,
	// when Turn is -1; waitingTurn is the turn it was dispatched to open.
	waiting     bool
	waitingTurn int
}

// CutTurn is the turn the interrupt cuts: the open turn, else the turn
// the waiting send opens, else -1.
func (s InterruptSample) CutTurn() int {
	if s.Turn < 0 && s.waiting {
		return s.waitingTurn
	}
	return s.Turn
}

// SampleInterrupt returns what an interrupt of threadID cuts: the open
// turn, or, when none is in flight, the turn of a send the provider holds
// whose turn has not opened yet. Every send is written to the provider
// before an interrupt is (the app serializes them under the thread action
// lock), so the provider starts that turn and the interrupt stops it,
// while the turn start can still be on its way to the event worker.
//
// While handleTurnStart runs it first waits for the start to finish,
// whether or not a turn is already open. That handler writes the active
// turns row before it sets the open turn, and GetActiveTurn-gated callers
// (workflow takeover, revert) can interrupt in between; an immediate
// sample would read -1 and lose the user's stop. A turn a queued echo
// already opened at the same index is waited out the same way.
//
// The store's row is never consulted. An active row with no open turn
// and no start in progress is a turn the router already closed: a fatal
// provider error awaiting its wire completion, or a settlement whose
// write failed. Stopping then cuts nothing.
//
// Interrupt paths sample this BEFORE awaiting the provider's interrupt
// ack and pass Turn to MarkFlushSendsInterrupted and
// EagerPersistDeferredFlushSends and the sample to MarkInterrupt: the ack
// wait processes wire events, so the turn the user cut can settle during
// it, and a post-ack sample would report -1 for exactly that turn — its
// echo would then settle it "end_turn" instead of "interrupted"
// (round-5, R5-4).
//
// The wait is bounded by the start's own store writes, and nothing it
// runs takes the app's thread action lock the interrupt paths hold.
// It must not be called from the event worker.
func (r *Router) SampleInterrupt(threadID string) InterruptSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		st := r.threadStateIfPresent(threadID)
		if st == nil {
			return InterruptSample{Turn: -1}
		}
		if st.turnStarting == nil {
			if st.openTurnSet {
				return InterruptSample{Turn: st.openTurn}
			}
			for _, entry := range st.pendingSends {
				if entry.canOpenTurn() {
					return InterruptSample{Turn: -1, waiting: true, waitingTurn: entry.TurnIndex}
				}
			}
			return InterruptSample{Turn: -1}
		}
		// The event worker can finish this start, complete its turn and
		// begin the next one before this waiter resumes, so the state is
		// read again after every wait until no start is in progress.
		starting := st.turnStarting
		hook := r.interruptWaitHook
		r.mu.Unlock()
		if hook != nil {
			hook(threadID, false)
		}
		<-starting
		if hook != nil {
			hook(threadID, true)
		}
		r.mu.Lock()
	}
}

// beginTurnStart admits the start for both interrupt and cleanup. The caller
// releases interrupt waiters when the turn is set and cleanup when its round is published.
func (r *Router) beginTurnStart(evt provider.ProviderEvent) (started, published func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRefusalLocked(evt) != "" {
		return nil, nil
	}
	threadID := evt.ThreadID
	st := r.state(threadID)
	starting := make(chan struct{})
	st.turnStarting = starting
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if st := r.threadStateIfPresent(threadID); st != nil && st.turnStarting == starting {
				st.turnStarting = nil
			}
			r.mu.Unlock()
			close(starting)
		})
	}, r.markTurnOpeningLocked(threadID, st)
}

// SetTurnOpeningObserver wires the callback told that threadID marked a
// turn opening (markTurnOpeningLocked), before the turn's row is written.
// It runs under r.mu, so it must not block or call back into the router.
func (r *Router) SetTurnOpeningObserver(fn func(threadID string)) {
	r.mu.Lock()
	r.turnOpening = fn
	r.mu.Unlock()
}

// markTurnOpeningLocked marks st, threadID's state, as opening a turn,
// under r.mu, and returns the function that ends the mark, which takes
// r.mu. Every turn open marks itself before it writes its turn row, until
// its round is published or it opened nothing: a start (startTurn), a
// soft-closed turn's continuation and a queued message's echo. The mark
// is told to the turn-opening observer under the same lock.
func (r *Router) markTurnOpeningLocked(threadID string, st *threadState) func() {
	st.turnOpenings++
	if r.turnOpening != nil {
		r.turnOpening(threadID)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			st.turnOpenings--
			if st.turnOpenings == 0 && st.openingsDone != nil {
				close(st.openingsDone)
				st.openingsDone = nil
			}
			r.mu.Unlock()
		})
	}
}

// HasInFlightTurnOrRound reports whether the router still has any
// turn or round state for threadID worth synthesizing a truncated
// turn-complete against. Used by CleanupThread and handleSessionDied.
// An open turn whose round a soft close ended counts: its result never
// came. A round counts on its own too, so a round whose turn state was
// already cleared still gets its `provider:turn_completed`.
func (r *Router) HasInFlightTurnOrRound(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		return false
	}
	return st.openTurnSet || st.currentRoundOpen
}

// AnyInFlightTurnOrRound reports whether ANY thread currently has an
// open logical turn or wire round. This is the renderer-is-painting
// signal the webview trim gate consults (app_webview_trim.go): Claude
// sessions never emit EventTurnStart, so the App-side activeTurns
// counter stays at zero for them and cannot answer "is a turn open" —
// the router's wire-driven round state is the provider-agnostic truth.
// Same predicate as HasInFlightTurnOrRound, across all threads; the
// walk is bounded by the number of threads with live correlation state.
func (r *Router) AnyInFlightTurnOrRound() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.threads {
		if st != nil && (st.openTurnSet || st.currentRoundOpen) {
			return true
		}
	}
	return false
}

// setOpenTurn opens turnIndex. A stream an earlier turn left open is over
// once another turn starts, and is forgotten with its count; an agent's
// streams are the agent's and stay. A failed ownership read forgets them
// too, as before agents outlived turns, and is returned.
func (r *Router) setOpenTurn(threadID string, turnIndex int, sessionToken string) (bool, error) {
	agentScopes, scopeErr := r.agentOwnedOpenScopes(threadID)
	r.mu.Lock()
	if r.frameRetiredLocked(threadID, sessionToken) {
		r.mu.Unlock()
		return false, nil
	}
	st := r.state(threadID)
	st.openTurn = turnIndex
	st.openTurnSet = true
	st.softClosedTurnSet = false
	st.seedStreamIndexes(turnIndex)
	r.dropTurnStreamsLocked(threadID, st, turnIndex, true, agentScopes)
	// Clear the settled marker so a re-init (Claude resend system.init
	// after an interrupt; Codex resend turn/started) can settle the
	// same turn again. A synthetic complete followed by the real result
	// has no turn start between them, so the marker survives there and
	// the second complete takes the late-fold path.
	delete(st.settledTurns, turnIndex)
	r.mu.Unlock()
	return true, scopeErr
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
// bookkeeping, turn span, fresh round snapshot, and the
// `provider:turn_started` emission — WITHOUT setOpenTurn's thread-wide
// streaming sweeps. Those sweeps are keyed to "a brand-new wire
// conversation is starting"; here the previous turn's tail can still
// be settling on the same live round, and `turnIndex` is a fresh index
// with no stale state of its own to clear (the counter seeds below are
// exactly the fresh-index subset of setOpenTurn). A continuation of
// the SAME turn never comes here: reopenSoftClosedTurn reopens it and
// keeps its counters; this opens a NEW index.
//
// No-op when `turnIndex` is already the open logical turn: the queue
// consumed at turn pickup with a real system.init (handleTurnStart
// already ran, in either order relative to the echo — upsertTurnRow
// and the counter seeds are idempotent), and every Codex steer (the
// deferred row persists at the active turn's own index). Also refuses
// to roll the open turn BACKWARD or reopen a settled turn — rows are
// already persisted under those indexes and a reopen would reset their
// id-allocating context, so new rows would collide with them. Both guards protect the attachProviderItemIDToUserRow
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
func (r *Router) openQueuedEchoTurn(threadID string, turnIndex int, startedAt int64, interruptedTurnIndex int, sessionToken string) {
	r.mu.Lock()
	if r.frameRefusalLocked(provider.ProviderEvent{ThreadID: threadID, SessionToken: sessionToken}) != "" {
		r.mu.Unlock()
		return
	}
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
	st.softClosedTurnSet = false
	st.seedStreamIndexes(turnIndex)
	delete(st.settledTurns, turnIndex)
	// Its predecessor's round is settled below before this turn's is
	// published.
	opening := r.markTurnOpeningLocked(threadID, st)
	defer opening()
	// The thread-level mark list covers the entry the per-entry stamp
	// structurally cannot: one POPPED from the FIFO just before
	// MarkFlushSendsInterrupted ran, whose echo is settling here with
	// its retained -1 stamp (round-10, R10-6). Within a session a mark
	// lingering after a SUCCESSFUL interrupt matches only the turn that
	// really was cut — and once that turn settles, the claim below
	// makes any re-match a no-op. MarkThreadActive clears the list, so
	// a replacement session's reused turn indexes (revert paths) never
	// meet a dead session's marks (round-11, CT11-2).
	r.mu.Unlock()

	if hasOpen {
		r.settleQueuedEchoPredecessor(threadID, open, startedAt, r.predecessorStopReason(threadID, open, interruptedTurnIndex), sessionToken)
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
	// A `started` that arrived with no logical turn open (a mid-loop drain
	// after a re-round) is waiting for this turn: no init will open it.
	r.releaseDeliveriesAtTurnStart(threadID, turnIndex, sessionToken)
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
	if !r.setOpenRoundSnapshot(snapshot, sessionToken) {
		return
	}
	r.emit(eventchan.ProviderTurnStarted, TurnStartedEvent(snapshot))
}

// predecessorStopReason is the stop reason of an open turn a new turn
// replaces: "interrupted" when a user interrupt provably cut it (the turn
// the pending entry's stamp names, or the thread's newest interrupt mark),
// "end_turn" otherwise.
func (r *Router) predecessorStopReason(threadID string, open, interruptedTurnIndex int) string {
	if open == interruptedTurnIndex {
		return "interrupted"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.identityIfPresent(threadID); id != nil && len(id.interruptMarks) > 0 &&
		id.interruptMarks[len(id.interruptMarks)-1].turn == open {
		return "interrupted"
	}
	return "end_turn"
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
// fold (the CLI turn's single `result` carries cumulative usage that
// lands on the final turn), and no orphan
// tool-call force-close (the CLI drains its queue between API
// iterations, after the prior iteration's tool results — a tool
// completion that hasn't arrived yet still will, and belongs to the row
// it opened). claimTurnSettlement both gates a duplicate settle (a soft
// round close may have beaten us) and routes any later wire result for
// this index into persistLateTurnPayload instead of the orphan-error
// path.
func (r *Router) settleQueuedEchoPredecessor(threadID string, turnIndex int, completedAt int64, stopReason, sessionToken string) {
	claim, heldStop, stopOwed := r.claimTurnSettlement(threadID, turnIndex, sessionToken, stopReason == "interrupted", false)
	if claim != settlementClaimed {
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
	agentScopes, err := r.agentOwnedOpenScopes(threadID)
	if err != nil {
		log.Printf("triage: settle queued-echo predecessor streaming %s/%d: %v", threadID, turnIndex, err)
		persistErr = errors.Join(persistErr, err)
	}
	if err := r.settleTurnStreaming(threadID, turnIndex, statusCompleted, agentScopes); err != nil {
		log.Printf("triage: settle queued-echo predecessor streaming %s/%d: %v", threadID, turnIndex, err)
		persistErr = errors.Join(persistErr, err)
	}
	if stopOwed {
		if _, err := r.writeStopRow(threadID, turnIndex, heldStop.summary, completedAt); err != nil {
			log.Printf("triage: write held stop of queued-echo predecessor %s/%d: %v", threadID, turnIndex, err)
			persistErr = errors.Join(persistErr, err)
		}
	}
	turnID := r.persistedTurnID(provider.ProviderEvent{ThreadID: threadID}, turnIndex)
	if err := r.writeTurnEnd(threadID, turnID, completedAt, stopReason, "", "", "", "settle queued-echo predecessor turn "+turnID); err != nil {
		persistErr = errors.Join(persistErr, err)
	}
	r.finishTurnSpan(threadID, completedTurnOutcome(turnCompleteMeta{
		StopReason: stopReason,
	}, persistErr))
}

// settlementClaim is the outcome of claimTurnSettlement.
type settlementClaim uint8

const (
	// settlementClaimed: the caller is the turn's first settler.
	settlementClaimed settlementClaim = iota
	// settlementTaken: the turn has settled; a later result folds its late
	// payload only.
	settlementTaken
	// settlementRetired: the frame's session was replaced. The turn it
	// resolved to is the replacement's, so it settles and folds nothing.
	settlementRetired
)

// claimTurnSettlement records that handleTurnComplete has begun logical-turn
// settlement for (threadID, turnIndex). A retired session's frame claims
// nothing (settlementRetired); a turn already settled is settlementTaken,
// whose caller takes the duplicate/late-payload path; otherwise the caller
// is the first settler. cut and soft are how the settlement ends: in the
// step that claims it, it resolves a Stop held for the turn
// (settleWaitingStopLocked), returned with owed set when the settlement
// writes its stop row. Only a claimed settlement resolves a Stop.
func (r *Router) claimTurnSettlement(threadID string, turnIndex int, sessionToken string, cut, soft bool) (claim settlementClaim, stop waitingStop, owed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(threadID, sessionToken) {
		return settlementRetired, waitingStop{}, false
	}
	st := r.state(threadID)
	if st.settledTurns[turnIndex] {
		return settlementTaken, waitingStop{}, false
	}
	if st.settledTurns == nil {
		st.settledTurns = make(map[int]bool)
	}
	st.settledTurns[turnIndex] = true
	stop, owed = st.settleWaitingStopLocked(turnIndex, cut, soft)
	return settlementClaimed, stop, owed
}

// orphanErrorClaim is the outcome of claimOrphanError.
type orphanErrorClaim uint8

const (
	// orphanErrorNone: a turn is open or settled, and the result is its.
	orphanErrorNone orphanErrorClaim = iota
	// orphanErrorClaimed: the result is an orphan error of this session.
	orphanErrorClaimed
	// orphanErrorRetired: the frame's session was replaced; nothing on
	// the thread is its own.
	orphanErrorRetired
)

// claimOrphanError reports whether an error result with no round is an
// orphan (no open turn, and turnIndex not settled) of the session that
// sent it, and the turn its error item is attributed to: the pending send
// still to open a turn when there is one, otherwise turnIndex. It reads
// the retirement and the ledger under one r.mu, so a replacement's commit,
// which resets the ledger, cannot make a retired session's result look
// like an orphan of the replacement's.
func (r *Router) claimOrphanError(threadID string, turnIndex int, sessionToken string) (int, orphanErrorClaim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(threadID, sessionToken) {
		return 0, orphanErrorRetired
	}
	if st := r.threadStateIfPresent(threadID); st != nil && (st.openTurnSet || st.settledTurns[turnIndex]) {
		return 0, orphanErrorNone
	}
	for _, entry := range r.pendingSendsLocked(threadID) {
		if entry.canOpenTurn() {
			return entry.TurnIndex, orphanErrorClaimed
		}
	}
	return turnIndex, orphanErrorClaimed
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

// setOpenRoundSnapshot records the active round snapshot for threadID. A
// round begins with every turn (startTurn, openQueuedEchoTurn) and again
// when the model continues a turn after a soft close (reopenSoftClosedTurn).
//
// Round ids are uuids; the frontend treats each round as a distinct
// active turn for indicator / composer / Stop-button purposes. The
// persisted `turns` row id stays on `resolveTurnID` (logical-turn
// granularity); only the wire payload's TurnID field carries the round
// id. See docs/architecture/turn-lifecycle.md §Rounds and turns.
//
// Overwrites any prior round snapshot for the thread — a leaked round (e.g.
// if currentTurnIndex failed in the prior handleTurnComplete and the
// round-end emit was skipped) cannot survive into the next round.
// CleanupThread also sweeps this map.
func (r *Router) setOpenRoundSnapshot(snapshot ActiveTurnSnapshot, sessionToken string) bool {
	if snapshot.ThreadID == "" || snapshot.TurnID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(snapshot.ThreadID, sessionToken) {
		return false
	}
	st := r.state(snapshot.ThreadID)
	st.currentRound = snapshot
	st.currentRoundOpen = true
	return true
}

// takeOpenRound returns the active round snapshot for threadID and
// clears the slot in the same critical section. Empty string when no
// round is open — the caller (handleTurnComplete) skips the per-round
// `provider:turn_completed` emission in that case. The
// "read-and-clear" semantics handle the synthetic-truncate-then-real
// race naturally: the synthetic complete takes the slot and emits;
// the real complete finds it empty and skips, so the frontend sees
// exactly one `provider:turn_completed` per round even when handleError
// synthesizes ahead of the wire's own turn-complete.
func (r *Router) takeOpenRound(threadID, sessionToken string) (ActiveTurnSnapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frameRetiredLocked(threadID, sessionToken) {
		return ActiveTurnSnapshot{}, false
	}
	st := r.threadStateIfPresent(threadID)
	if st == nil || !st.currentRoundOpen {
		return ActiveTurnSnapshot{}, false
	}
	round := st.currentRound
	st.currentRound = ActiveTurnSnapshot{}
	st.currentRoundOpen = false
	st.endingRound, st.endingRoundSet = round, true
	return round, true
}

// endRound clears the ending round once the turn's settle writes are done,
// right before its provider:turn_completed, unless a later round took the
// slot.
func (r *Router) endRound(threadID, roundID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.threadStateIfPresent(threadID); st != nil && st.endingRoundSet && st.endingRound.TurnID == roundID {
		st.endingRound, st.endingRoundSet = ActiveTurnSnapshot{}, false
	}
}

// liveRoundLocked is the round the live-state snapshots report: the open
// round, else the one whose end is not announced yet. Caller holds r.mu.
func (st *threadState) liveRoundLocked() (ActiveTurnSnapshot, bool) {
	if st.currentRoundOpen {
		return st.currentRound, true
	}
	return st.endingRound, st.endingRoundSet
}

// openRoundID returns the active round id for threadID without
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

// ActiveTurnSnapshot returns the current live round snapshot for a
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
//   - **Per-turn flow-control state** swept HERE: openTurns, the turn's
//     own activeTextBlocks/Thinking, pendingCommandDiffs, and the
//     turn's own pendingApprovals (and siblings; dropTurnRequestsLocked).
//     These maps answer "what's mid-turn right now." The streaming
//     counts move with the blocks, and the interrupt queue is drained,
//     not swept: the turn's rows left it before this runs, and an
//     agent's rows wait for the agent's own streams.
//   - **Id-allocating counters** (segmentIndexByScope, blockIndexByScope,
//     errorSeqByScope, compactionSeqByScope, terminalInteractionSeq):
//     cleared at CleanupThread and never restarted while the state
//     lives, a repeated turn start included (seedStreamIndexes). A row
//     id built from a sequence counter also goes through reserveItemID,
//     so a counter that CleanupThread restarted cannot name a saved row.
//     Wiping HERE would
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
// against re-creating a row mid-stream), so the turn's own stay swept
// here as a safety net for any block that didn't settle through the
// normal close path. A block or prompt in one of agentScopes is an
// agent's and stays open (agentOwnedOpenScopes).
func (r *Router) clearOpenTurn(threadID string, agentScopes map[string]bool) {
	r.mu.Lock()
	st := r.threadStateIfPresent(threadID)
	if st == nil {
		r.mu.Unlock()
		return
	}
	if st.openTurnSet {
		r.dropTurnStreamsLocked(threadID, st, st.openTurn, false, agentScopes)
		// pendingCommandDiffs is keyed by `<threadID>:<itemID>` and
		// stages an inline-diff preview between EventToolStart and
		// EventToolComplete for command_execution rows. If the matching
		// completion never arrived in this turn (interrupted, crashed),
		// the entry would otherwise leak until CleanupThread.
		clear(st.pendingCommandDiffs)
		dropTurnRequestsLocked(st, agentScopes)
	}
	st.openTurn = 0
	st.openTurnSet = false
	// openAPIRetryRows tracks "thread has a running api_retry row that
	// still needs flipping". By turn-end the row was either flipped
	// already or the turn closed without forward progress; either way
	// the next turn starts with a clean flag.
	st.openAPIRetryRow = false
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
// settles. Mirrors the exemption in forceCloseOrphanToolCalls. So is
// every row a background agent owns: the agent's end settles it
// (agent_end.go).
//
// The flip reads the turn's rows after the thread's settles in flight, so
// a block whose stop arrived before the Stop settles completed, with its
// final text and meta, instead of racing the flip.
func (r *Router) flipTurnItemsErrored(
	threadID string,
	turnIndex int,
	now int64,
	summaryFn func(string) string,
) error {
	r.waitThreadSettles(threadID)
	if err := r.flushStreamingThread(threadID); err != nil {
		return fmt.Errorf("error flip flush streaming buffers: %w", err)
	}
	items, err := r.store.ListUnsettledTurnItems(threadID, turnIndex)
	if err != nil {
		return fmt.Errorf("error flip list turn items: %w", err)
	}
	var parents []string
	for _, item := range items {
		if (item.Status == statusRunning || item.Status == statusStreaming) && item.ParentID != "" {
			parents = append(parents, item.ParentID)
		}
	}
	var owners map[string]string
	if len(parents) > 0 {
		if owners, err = r.store.AgentOwners(threadID, parents); err != nil {
			return fmt.Errorf("error flip ownership: %w", err)
		}
	}
	for _, item := range items {
		if owners[item.ParentID] != "" {
			// An agent's row: the agent's end settles it.
			continue
		}
		for (item.Status == statusRunning || item.Status == statusStreaming) && !(item.IsBackground && item.Kind == itemKindToolCall) {
			var persisted store.Item
			var changed bool
			err := r.withSubagentCard(threadID, item.ParentID, func(card *store.SubagentCard) error {
				var err error
				persisted, changed, err = r.store.ErrorActiveItemIfRevision(threadID, item.ID, item.Rev, summaryFn(item.Summary), now, card)
				return err
			})
			if err != nil {
				return fmt.Errorf("error flip item %s: %w", item.ID, err)
			}
			if changed {
				if item.Kind == itemKindAssistantText && item.Status == statusStreaming {
					// The flip ends the row's stream as a settle does, with
					// the model's text before the stopped or interrupted
					// suffix.
					r.endAssistantText(threadID, item.ID, item.Summary)
				}
				r.emitItemUpsert(persisted)
				r.emitErrorNotice(persisted)
				r.metrics.ItemsPersisted.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", persisted.Kind)))
				break
			}
			var found bool
			item, found, err = r.store.GetThreadItem(threadID, item.ID)
			if err != nil {
				return fmt.Errorf("error flip reread item: %w", err)
			}
			if !found {
				break
			}
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
		return len(crashed), fmt.Errorf("triage: recover crashed turns: %w", err)
	}
	return len(crashed), nil
}

// StoppedByUser is the stop row of the user's own Stop.
const StoppedByUser = "Stopped by user"

// MarkInterrupt is the public chokepoint for the app-layer
// interrupt flow. It flips every streaming/running item in the
// interrupted turn to errored with a " — stopped" suffix and records a
// new system error row reading summary, which names who stopped the turn
// (StoppedByUser for the user's own Stop), so the timeline carries the
// explicit user-facing signal.
//
// sample is the caller's PRE-ACK SampleInterrupt — the turn the user
// actually cut. It must not be re-resolved here: a queued echo consumed
// during the ack wait opens the queued message's turn, and a post-ack
// "current turn" read would land the stopped flips and the error row on
// that new turn, which drains naturally (round-11, C11-1). A sample with
// no open turn and no waiting send is a no-op — nothing was cut
// (round-12, CT12-1). A sample whose cut turn is a waiting send's holds
// the stop until that turn settles (stop_waiting_send.go): the provider
// may abort the turn, run it to a normal end, or retire the send unrun,
// and only the first is a stopped turn. A Stop of the open turn writes at
// once and withdraws a stop held for it. A turn carries one stop row: a
// later Stop of a turn whose row is written flips its rows and writes no
// second one (writeStopRow). The flip is idempotent against
// the echo path's own
// interrupted-predecessor flip (R10-4): rows it already moved out of
// streaming/running are skipped. tok fences the whole call against
// session replacement, like the rest of the post-ack block (round-11,
// CT11-1).
//
// Returns the error id that was persisted so the caller can surface
// diagnostics or correlate with downstream emissions; empty string
// when no turn could be resolved, the stop is held for its turn, the
// turn's stop row was already written, or the session was replaced.
func (r *Router) MarkInterrupt(threadID string, sample InterruptSample, tok FlushStampToken, summary string) (string, error) {
	if summary == "" {
		return "", fmt.Errorf("triage: mark interrupt of %s: no stop summary", threadID)
	}
	if r.ThreadEpoch(threadID) != tok.threadEpoch {
		return "", nil
	}
	turnIndex := sample.CutTurn()
	if turnIndex < 0 {
		// No turn was open at the pre-ack sample, which waits out a
		// turn start in progress (SampleInterrupt), and no send was
		// waiting for one: nothing was cut. The turn completed or failed
		// in the click-to-sample race and the interrupt ack'd an idle
		// CLI. Resolving a fallback here would relabel completed history
		// (LastTurnIndex) or a queued echo's freshly opened turn as
		// "Stopped by user" (round-12, CT12-1).
		return "", nil
	}
	if sample.Turn < 0 {
		switch r.holdWaitingStop(threadID, turnIndex, summary) {
		case stopHeld, stopCutNothing:
			return "", nil
		case stopSettledEarlier:
			cut, err := r.settledTurnWasCut(threadID, turnIndex)
			if err != nil {
				return "", fmt.Errorf("triage: mark interrupt of %s: read settled turn %d: %w", threadID, turnIndex, err)
			}
			if !cut {
				return "", nil
			}
		case stopOwed:
		}
	} else {
		r.dropWaitingStop(threadID, turnIndex)
	}
	return r.writeTurnStop(threadID, turnIndex, summary)
}

// writeTurnStop flips the turn's unsettled rows to errored with the
// " — stopped" suffix and writes its stop row reading summary, unless the
// turn has one (writeStopRow).
func (r *Router) writeTurnStop(threadID string, turnIndex int, summary string) (string, error) {
	now := time.Now().UnixMilli()
	if err := r.flipTurnItemsErrored(threadID, turnIndex, now, stoppedSummary); err != nil {
		return "", err
	}
	return r.writeStopRow(threadID, turnIndex, summary, now)
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
		r.DrainWireItemRefresh()
		close(done)
	}()
	select {
	case <-done:
		r.flushAllUsage()
		return r.flushAllStreamPersistence()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// threadSettles is one thread's settle goroutines in flight. idle closes
// when the count returns to zero.
type threadSettles struct {
	n    int
	idle chan struct{}
}

// goSettle runs settle on a settle goroutine of threadID, counted by
// settleWG for shutdown and by the thread's entry for its in-turn barrier.
func (r *Router) goSettle(threadID string, settle func()) {
	r.settleWG.Add(1)
	r.settleMu.Lock()
	ts := r.settles[threadID]
	if ts == nil {
		ts = &threadSettles{idle: make(chan struct{})}
		r.settles[threadID] = ts
	}
	ts.n++
	r.settleMu.Unlock()
	go func() {
		defer r.settleWG.Done()
		defer func() {
			r.settleMu.Lock()
			defer r.settleMu.Unlock()
			if ts.n--; ts.n == 0 {
				close(ts.idle)
				delete(r.settles, threadID)
			}
		}()
		settle()
	}()
}

// waitThreadSettles blocks until threadID has no settle goroutine in
// flight: a read of the thread's streamed rows after it sees every settle
// the thread's earlier events started. Other threads' settles do not hold
// it. It must not run on a settle goroutine or under the thread's drain
// lock, which a settle takes to finish (finishSettle).
func (r *Router) waitThreadSettles(threadID string) {
	r.settleMu.Lock()
	ts := r.settles[threadID]
	r.settleMu.Unlock()
	if ts != nil {
		<-ts.idle
	}
}

// WaitForPendingSettles blocks until every settle goroutine of every
// thread (goSettle) has completed. Used by app shutdown so SQLite
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

// EndReplacedSessionTurn ends, as truncated, the turn a session being
// replaced left open, as CleanupThread does for a stop. A start that replaces
// a live session keeps the thread's state live apart from its process's
// (processBound), but the process that ran this turn is closed and its
// replacement never continues it: left open, the replacement's first output
// would land in it and complete it. The caller has closed that session and
// drained its frames.
func (r *Router) EndReplacedSessionTurn(threadID string) {
	if !r.HasInFlightTurnOrRound(threadID) {
		return
	}
	if err := r.synthesizeTruncatedTurnComplete(threadID, time.Now().UnixMilli()); err != nil {
		r.providerWriteFailed(threadID, "synthesize turn-complete for replaced session on thread "+threadID, err)
	}
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

// closeTurnAdmission fences new wire frames before cleanup settles the last
// turn. A turn already writing its row must publish its round first.
func (r *Router) closeTurnAdmission(threadID string, requireEpoch *uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	identity := r.identity(threadID)
	if requireEpoch != nil && identity.epoch != *requireEpoch {
		return false
	}
	epoch := identity.epoch
	identity.closing = true
	if st := r.threadStateIfPresent(threadID); st != nil && st.turnOpenings != 0 {
		if st.openingsDone == nil {
			st.openingsDone = make(chan struct{})
		}
		done := st.openingsDone
		r.mu.Unlock()
		<-done
		r.mu.Lock()
	}
	return identity.epoch == epoch
}

// cleanupThread is the shared body behind CleanupThread (requireEpoch
// nil: unconditional) and CleanupThreadIfEpoch (requireEpoch set: the
// locked sweep aborts when the thread's epoch no longer matches).
// Returns false only on an epoch-abort.
func (r *Router) cleanupThread(threadID string, requireEpoch *uint64) bool {
	if !r.closeTurnAdmission(threadID, requireEpoch) {
		return false
	}
	cleanupAt := time.Now().UnixMilli()
	if r.HasInFlightTurnOrRound(threadID) {
		if err := r.synthesizeTruncatedTurnComplete(threadID, cleanupAt); err != nil {
			r.providerWriteFailed(threadID, "synthesize turn-complete on cleanup for thread "+threadID, err)
		}
	}
	if err := r.flushStreamingThread(threadID); err != nil {
		r.providerWriteFailed(threadID, "cleanup flush stream buffers for thread "+threadID, err)
	}
	// A row still queued behind a stream is persisted before the queue
	// goes with the state: no stream of this session settles any more.
	if err := r.drainAll(threadID); err != nil {
		r.providerWriteFailed(threadID, "cleanup persist queued rows for thread "+threadID, err)
	}
	// The state below is about to go, and its refresh timer with it.
	r.flushWireItemRefresh(threadID)

	// The pending-send sweep below must be total: an echo whose store
	// write is in flight holds the anchor lock and, on failure, reinserts
	// its popped entry — a sweep that ran between the pop and the
	// reinsert would lose to it, resurrecting a stale entry into the
	// replacement session (round-5, R5-2). Anchor before r.mu, per the
	// lock order.
	anchor := r.flushAnchor(threadID)
	anchor.Lock()
	defer anchor.Unlock()
	r.usageEmitMu.Lock()
	defer r.usageEmitMu.Unlock()

	r.mu.Lock()
	identity := r.identity(threadID)
	if requireEpoch != nil && identity.epoch != *requireEpoch {
		r.mu.Unlock()
		return false
	}
	// Set the stopped flag BEFORE dropping other state so Handle observes
	// a consistent snapshot: any concurrent Handle call either sees a live
	// thread with full state, or a stopped thread with no state.
	identity.closing = false
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
		pendingProgress        provider.UsageEvent
		hasPendingProgress     bool
		closedCodexAgents      []closedCodexAgent
		heldCodexCompletions   map[string]pendingCodexCompletion
		idleSubagentCards      []*subagentCardEntry
		strandedRound          ActiveTurnSnapshot
		hasStrandedRound       bool
	)
	if st != nil {
		// If settlement failed before announcing the round's end, release its clients.
		strandedRound, hasStrandedRound = st.currentRound, st.currentRoundOpen
		// The cards are store handles: closed below, once r.mu is
		// released, or by the write still holding one.
		idleSubagentCards = st.retireSubagentCardsLocked("")
		if st.codexBackground != nil {
			for _, item := range st.codexBackground.agents {
				if codexRuntimeActive(item) {
					closedCodexAgents = append(closedCodexAgents, closedCodexAgent{item: item, progress: st.subagentProgress[item.ID]})
				}
			}
			heldCodexCompletions = takePendingCodexCompletionsLocked(st.codexBackground)
		}
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
		pendingProgress, hasPendingProgress = takeUsagePending(st.usageProgressThrottle)
	}
	delete(r.threads, threadID)
	r.noteSendsPendingLocked(threadID)
	if hadEffectiveModel {
		// The revision counter lives on the identity, so it is still
		// here after the delete: the frontend needs a STRICTLY newer
		// revision to accept the clear.
		effectiveModelRevision = r.nextEffectiveModelRevisionLocked(threadID)
	}
	r.mu.Unlock()
	closeSubagentCards(threadID, idleSubagentCards)

	if hasStrandedRound {
		synth := provider.ProviderEvent{
			Kind:         provider.EventTurnComplete,
			ThreadID:     threadID,
			TurnComplete: &provider.TruncatedTurnCompleteMeta{Synthetic: true},
			Timestamp:    time.UnixMilli(cleanupAt),
		}
		completed := r.buildRoundCompletedEvent(synth, strandedRound.TurnID, strandedRound.TurnIndex, cleanupAt, turnCompleteMeta{Truncated: true})
		// No turns row settles for this round, so it must not move the
		// read clock a client derives unread from.
		completed.CountsAsActivity = false
		r.emit(eventchan.ProviderTurnCompleted, completed)
	}

	if hadEffectiveModel {
		r.emit(eventchan.ProviderModelFallback, ModelFallbackEvent{ThreadID: threadID, Revision: effectiveModelRevision})
	}

	if hasPendingProgress {
		r.emit(eventchan.ProviderUsage, pendingProgress)
	}
	if hasPendingUsage {
		r.emit(eventchan.ProviderUsage, pendingUsage)
	}

	if orphanSpan != nil {
		r.recordTurnSpanOutcome(orphanSpan, cleanupTurnOutcome())
	}
	// A completion held for its answer outlives nothing: the session that
	// would have delivered the envelope is gone, so the row is written
	// answerless before the runtime it belonged to is forgotten.
	r.persistHeldCodexCompletionsAnswerless(threadID, heldCodexCompletions)
	r.finishClosedCodexAgents(closedCodexAgents, cleanupAt)
	r.emit(eventchan.ProviderBackgroundTasksChanged, BackgroundTasksChangedEvent{ThreadID: threadID, ResetCodexAgents: true})
	r.emitBackgroundTrayRefresh(threadID)

	return true
}

// frameRefusal is Handle's gate: it names why evt is refused, or returns
// "" when evt is admitted. See frameRefusalLocked.
func (r *Router) frameRefusal(evt provider.ProviderEvent) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frameRefusalLocked(evt)
}

// frameRefusalLocked refuses every event for a thread CleanupThread
// stopped and not since re-activated, and a provider frame whose session
// token is not the one MarkThreadActive last committed the thread to: a
// frame of a session a replacement has already claimed. Host-originated
// events carry no token and are refused only while the thread is stopped.
// Caller holds r.mu.
func (r *Router) frameRefusalLocked(evt provider.ProviderEvent) string {
	if id := r.identityIfPresent(evt.ThreadID); id != nil && (id.closing || id.stopped) {
		return "stopped thread"
	}
	if r.frameRetiredLocked(evt.ThreadID, evt.SessionToken) {
		return "retired session"
	}
	return ""
}

// frameRetiredLocked reports whether the session that produced a frame
// (its token) no longer owns the thread: a replacement committed the
// thread to another session, or a stop marked it stopped. Host-originated
// writes carry no token and are never retired. Every writer of
// process-bound state, and of a turn, round, stream or prompt, asks it under the r.mu it writes under, so a frame
// that passed Handle's gate before a replacement committed cannot write
// that state for the replacement. Caller holds r.mu.
func (r *Router) frameRetiredLocked(threadID, sessionToken string) bool {
	if sessionToken == "" {
		return false
	}
	id := r.identityIfPresent(threadID)
	return id == nil || id.stopped || id.sessionToken != sessionToken
}

// frameRetired is frameRetiredLocked for a caller that does not hold r.mu.
func (r *Router) frameRetired(threadID, sessionToken string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frameRetiredLocked(threadID, sessionToken)
}

// SessionCurrent reports whether sessionToken is the session
// MarkThreadActive last committed threadID to. The host asks it before
// acting on a provider frame itself, with the same answer Handle's gate
// gives that frame's token.
func (r *Router) SessionCurrent(threadID, sessionToken string) bool {
	if r == nil || sessionToken == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.identityIfPresent(threadID)
	return id != nil && id.sessionToken == sessionToken
}

// MarkThreadActive commits the thread to the session sessionToken names:
// it clears the stopped flag and makes that session's frames the only
// ones Handle admits. The host calls this from the session-start path
// BEFORE spawning the new subprocess. It must not wait for a wire
// signal, because a session that dies during startup (e.g. Claude
// rejecting a fork start's --resume-session-at pre-init) emits its only diagnostics
// before anything init-like, and those must route. No stale frame passes
// the gate after this call: every provider frame carries the token of
// the session that produced it, and a frame of any earlier session no
// longer matches, even when the host's drain of the prior session gave
// up at its stall bound and left that session's frames queued. A frame
// already being handled when this call lands is refused by every writer
// of process-bound state (processBound). No wire event may clear
// the stopped flag: see the threadIdentity.stopped field comment.
//
// Reactivation also resets the thread's logical-turn settlement ledger.
// A start that replaces a live session (the session-config restart
// reconcileSessionConfig schedules) deliberately skips CleanupThread, so
// settledTurns can still hold entries from the prior subprocess; a
// replacement session reuses turn indexes, and a stale "settled" marker
// would misroute its first error result into persistLateTurnPayload
// (folding into a finished turn's row) instead of the orphan error-item
// branch the user actually needs to see. Clearing here is safe for the
// soft-close late-fold path: the fold's trailing result rides the SAME
// session's read loop, whose frames the gate refuses once this call
// names another session.
//
// Each call bumps the thread's reactivation epoch (see ThreadEpoch),
// which lets asynchronous teardowns detect that a replacement session
// claimed the thread mid-teardown and abort their cleanup.
func (r *Router) MarkThreadActive(threadID, sessionToken string) {
	identity := r.identity(threadID)
	r.mu.Lock()
	identity.closing = false
	identity.stopped = false
	identity.sessionToken = sessionToken
	identity.epoch++
	// This is NOT the cleanup sweep: a start that replaces a live session
	// reaches here without CleanupThread, so the thread's state entry can be fully
	// live and must survive. Only the state bound to the process that
	// just died is replaced (processBound).
	st := r.threadStateIfPresent(threadID)
	hadEffectiveModel := st != nil && st.effectiveModelSet
	// The replaced process's commands end with it; the tray drops them.
	hadCodexCommands := st != nil && len(st.codexExec.byLaunch) > 0
	if st != nil {
		st.processBound = processBound{}
		// A Stop held for a turn of the previous process names an index
		// the replacement reuses. It is host state, so the reset above
		// leaves it.
		st.waitingStop, st.waitingStopSet = waitingStop{}, false
	}
	// An in-flight echo cannot survive the session whose read loop carries
	// it, and a replacement after revert reuses turn indexes, so a
	// lingering mark would settle a reused-index predecessor
	// "interrupted". Entries are keyed by router-unique seqs, so a stale
	// restore's removal against the fresh list finds nothing.
	identity.interruptMarks = nil
	identity.stopRow = stopRowClaim{}
	var effectiveModelRevision uint64
	if hadEffectiveModel {
		effectiveModelRevision = r.nextEffectiveModelRevisionLocked(threadID)
	}
	r.mu.Unlock()
	if hadEffectiveModel {
		r.emit(eventchan.ProviderModelFallback, ModelFallbackEvent{ThreadID: threadID, Revision: effectiveModelRevision})
	}
	if hadCodexCommands {
		r.emitCodexBackgroundChanged(threadID)
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
