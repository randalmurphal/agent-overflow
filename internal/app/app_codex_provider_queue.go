package app

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// This file is everything AO still does about the PROVIDER's own user-message
// queue (`thread/queue/*`, codex >= 0.148) — which is nothing that puts a
// message INTO it. AO dispatches every mid-turn message with `turn/steer`
// (app_flush_queue.go) and never calls `thread/queue/add`, because that queue
// dispatches on the app-server's clock: a message handed over is one AO's own
// flush queue and the provider's could both decide to run.
//
// Two things are left, and they are unrelated to each other:
//
//   - the LEGACY SUNSET: rows AO did add during the 2026-08-21..24 window,
//     which are still sitting in codex's SQLite waiting for an idle hook to
//     run them. A session start deletes them and hands the text back to the
//     user (sunsetLegacyProviderQueueRows).
//   - the ROLLBACK PURGE: a thread being truncated must not leave ANY queued
//     row behind — a foreign producer's included — because the idle hook
//     would run it against history the user just removed.
//
// See internal/provider/codex/AGENTS.md §"The provider-owned queue" for the
// wire contract both rules are derived from.

// codexQueueRequestTimeout bounds the queue reads and deletes below. Short on
// purpose: both callers run inside a session handshake or a rollback the user
// is waiting on, and both have a safe answer for a slow queue (leave the rows
// alone / refuse the rollback), so a hung app-server must not hold either one.
const codexQueueRequestTimeout = 10 * time.Second

// ownsLegacyProviderQueuedClientID is `codex.Config.OwnsQueuedClientID`: the
// app layer's answer to "is this queued `clientUserMessageId` one of ours".
//
// **The persisted row IS the credential.** The id grammar
// (`user:<turn>[:flush:<n>]`) is not: it is deterministic, so a second Agent
// Overflow process — another profile, another machine sharing the Codex home,
// a second window on the same thread — mints exactly the same ids, and
// `codex queue --thread` (or anything else speaking `thread/queue/add`) may
// simply supply one. What cannot be forged is a row in THIS app's store,
// marked provider-queued, under that exact id: only AO's own (now retired)
// add path ever wrote one, and it wrote it before the add.
//
// A read error answers false. Claiming a row AO cannot substantiate would
// delete a stranger's message and announce their text as this user's own; the
// cost of the other direction is a legacy row left where it is until the next
// session start.
func (a *App) ownsLegacyProviderQueuedClientID(threadID, clientID string) bool {
	if a == nil || a.store == nil || strings.TrimSpace(clientID) == "" {
		return false
	}
	row, found, err := a.store.GetThreadItem(threadID, clientID)
	if err != nil {
		log.Printf("codex queue: thread %s: could not check whether queued submission %q is this app's: %v",
			threadID, clientID, err)
		return false
	}
	return found && row.Kind == "user_text" && itemmeta.IsProviderQueued(row.Meta)
}

// legacyProviderQueuedRowsForThread returns this thread's user rows that AO
// handed to the provider's queue during the add window and that no provider
// echo has claimed, in timeline order. Empty for every thread that never ran
// during that window, which is every thread on a fresh install.
func (a *App) legacyProviderQueuedRowsForThread(threadID string) ([]store.Item, error) {
	ids, err := a.store.ListUnclaimedProviderQueuedUserItemIDs(threadID)
	if err != nil {
		return nil, err
	}
	rows := make([]store.Item, 0, len(ids))
	for _, id := range ids {
		item, found, err := a.store.GetThreadItem(threadID, id)
		if err != nil {
			return nil, fmt.Errorf("read queued row %s: %w", id, err)
		}
		if !found {
			continue
		}
		rows = append(rows, item)
	}
	return rows, nil
}

// sunsetLegacyProviderQueueRows retires the rows AO left in the provider's
// queue while it briefly dispatched there (2026-08-21..24). It is the body of
// `Config.BeforeResume` (app_session.go) and runs in that hook's one window:
// after the handshake has frozen `ThreadQueueNative`, before the
// `thread/resume` that loads the thread and lets its idle hook dispatch.
//
// Deleting and restoring, rather than leaving them to run, is the whole point.
// A row left in place dispatches as a `turn/started` this connection never
// asked for, minutes or days after the user typed it, against a thread that
// has moved on — and AO no longer keeps any of the machinery that used to make
// such a turn attributable (the self-queued claim ledger, the resume re-arm).
// So the message goes back to the person who typed it, through the same
// composer restore every other undelivered flush takes, and the row it was
// occupying is cleared out of the timeline with it.
//
// Ordering is load-bearing: the DELETE lands before the restore. A restore
// that ran first would offer the user a re-send of a message codex is still
// holding, which is the one duplicate this whole path exists to avoid.
//
// A pre-0.148 app-server has no `thread/queue/*` at all, so it cannot see or
// delete them; the rows are named in the log and left exactly where they are,
// for a later session on a newer binary to retire.
func (a *App) sunsetLegacyProviderQueueRows(threadID string, sess *codex.Session) {
	if a == nil || a.store == nil || sess == nil {
		return
	}
	rows, err := a.legacyProviderQueuedRowsForThread(threadID)
	if err != nil {
		log.Printf("codex queue: thread %s: could not read this thread's legacy provider-queued rows at session start: %v", threadID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	if !sess.ThreadQueueNative() {
		ids := make([]string, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		log.Printf("codex queue: thread %s: %d message(s) are held by a Codex queue this app-server has no API for (%s); they stay there until a newer Codex opens this thread",
			threadID, len(ids), strings.Join(ids, ", "))
		return
	}

	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexQueueRequestTimeout)
	defer cancel()
	submissions, listErr := sess.QueueList(ctx)
	if listErr != nil && len(submissions) == 0 {
		log.Printf("codex queue: thread %s: could not read the provider queue at session start, so %d legacy row(s) stay with it for now: %v",
			threadID, len(rows), listErr)
		return
	}
	if listErr != nil {
		// A PREFIX still proves what it contains, and every row it names is
		// one this walk can retire. The rows it could not reach are retried
		// at the next session start.
		log.Printf("codex queue: thread %s: the provider queue listed only partially at session start (%v); retiring the legacy rows it did name",
			threadID, listErr)
	}

	held := make(map[string]codex.QueuedSubmission, len(submissions))
	for _, submission := range submissions {
		held[submission.ClientUserMessageID] = submission
	}

	restorable := make([]store.Item, 0, len(rows))
	for _, row := range rows {
		submission, queued := held[row.ID]
		if !queued {
			if itemmeta.IsProviderQueueHandoffPending(row.Meta) {
				// Marked, unproven, and the provider does not have it: the add
				// never landed. Nothing will ever run this message either.
				restorable = append(restorable, row)
				continue
			}
			// Marked, PROVEN, and gone from the queue: it already dispatched
			// under the old code. The row is history — its echo landed on the
			// session that queued it or was lost with it — and restoring it
			// would offer a re-send of a message the transcript contains.
			continue
		}
		removed, deleteErr := sess.QueueDelete(ctx, submission.ID)
		if deleteErr != nil {
			log.Printf("codex queue: thread %s: could not remove legacy queued submission %s (%s) from the Codex queue; leaving it for the next session start: %v",
				threadID, submission.ID, row.ID, deleteErr)
			continue
		}
		if !removed {
			// Matched nothing — it dispatched between the list and the delete.
			// Same verdict as PROVEN-and-absent above: not AO's to restore.
			continue
		}
		restorable = append(restorable, row)
	}
	if len(restorable) == 0 {
		return
	}
	ids := make([]string, 0, len(restorable))
	for _, row := range restorable {
		ids = append(ids, row.ID)
	}
	log.Printf("codex queue: thread %s: retired %d message(s) Agent Overflow had left with the Codex queue (%s); returning them to the composer",
		threadID, len(ids), strings.Join(ids, ", "))
	a.restoreProviderQueueRowsToComposer(threadID, restorable,
		queueRestoredReasonNeverQueued,
		"was left with the Codex queue by an older build; returning it to the composer")
}

// purgeCodexProviderQueueForRollback empties the PROVIDER's queue for a thread
// the user is about to truncate.
//
// `clearFlushDispatchForRollback` handles AO's own in-process queue, but a
// message in `thread/queue/*` is durable in codex's SQLite: it outlives the
// session stop and the app-server's idle hook dispatches it on the next resume
// — re-running, against a truncated thread, exactly the message the rollback
// removed. Since AO stopped adding rows, every row this finds is normally a
// foreign producer's (`codex queue --thread`, another client on the same
// app-server) plus, on a thread that has not started a session since, a legacy
// row the sunset has not reached yet.
//
// Runs BEFORE stopSession because it needs the live connection, and its
// failure ABORTS the rollback: see purgeCodexProviderQueue for why a partial
// purge is not a degraded success.
//
// A thread with no live session is NOT skipped — it is purged later, over the
// throwaway connection the cut opens anyway (purgeCodexQueueBeforeCut). This
// call handles only the case where AO already holds a connection, and the two
// are deliberately allowed to overlap: after a successful purge here the
// second one lists an empty queue and deletes nothing.
//
// A live session on a PRE-0.148 app-server is the one case with no purge to
// attempt at all, and it is not automatically safe: this thread may still hold
// legacy rows a newer Codex accepted, which that Codex will dispatch the next
// time it opens the thread. The store is asked, because it is the only thing
// here that can see them, and the rollback is refused if any exist. Refusing
// is recoverable (run them, or upgrade); a message replaying onto history the
// user deleted is not.
func (a *App) purgeCodexProviderQueueForRollback(threadID string) error {
	sess, ok := a.sessionManager().get(threadID)
	if !ok || sess.Codex == nil {
		return nil
	}
	if !sess.Codex.ThreadQueueNative() {
		return a.refuseRollbackOverUnreachableQueue(threadID)
	}
	return a.purgeCodexProviderQueue(threadID, sess.Codex)
}

// refuseRollbackOverUnreachableQueue is the pre-0.148 half of the same check.
//
// Store-only by necessity: there is no `thread/queue/list` on this connection,
// so the legacy rows AO marked before each add are the only evidence that
// anything is parked. A read error refuses too — an unknown queue state is
// exactly what the caller must not truncate over.
func (a *App) refuseRollbackOverUnreachableQueue(threadID string) error {
	if a.store == nil {
		return nil
	}
	stranded, err := a.store.ListUnclaimedProviderQueuedUserItemIDs(threadID)
	if err != nil {
		return fmt.Errorf("could not check whether Codex still has messages queued for this thread: %w", err)
	}
	if len(stranded) == 0 {
		return nil
	}
	return fmt.Errorf(
		"this thread has %d message(s) held by a newer Codex's message queue, and the connected Codex has no way to remove them; they would re-run after the rollback. Upgrade Codex to 0.148 or later, or let them run first",
		len(stranded))
}

// purgeCodexQueueBeforeCut is the same purge over the app-server connection
// the history cut had to open regardless — the no-live-session half of the
// same hazard.
//
// A rolled-back thread whose session was already stopped still has its rows in
// codex's SQLite, and the in-place `thread/revert` keeps the thread id, so the
// next resume dispatches a message the user just removed. The rollback already
// resumes that exact thread to cut it (`withCodexThreadSession`), so the purge
// costs one `thread/queue/list` on a connection that exists, not a spawn.
//
// Called BEFORE the `thread/resume` itself, through the bracket's pre-resume
// hook, and not only for tidiness: resuming a thread LOADS it, and a loaded
// thread's idle hook is what dispatches the queue — a row left in place can
// start a turn on this very connection while the cut is being issued.
// Upstream answers `thread/queue/list` and `.../delete` for an unloaded thread
// (`require_thread` falls back to a thread-store read), so the whole purge
// fits in that window. The cost of going first is that a cut which then fails
// has already dropped the queued rows; that is the direction to fail in,
// because those rows are messages the user asked to remove and each drop is
// logged.
//
// Its error is the CUT's error. The caller holds it and refuses to issue the
// truncation, because a cut landing over an unpurged queue is the exact shape
// this purge exists to prevent — see purgeCodexProviderQueue.
func (a *App) purgeCodexQueueBeforeCut(threadID string, sess *codex.Session) error {
	if sess == nil || !sess.ThreadQueueNative() {
		return nil
	}
	return a.purgeCodexProviderQueue(threadID, sess)
}

// purgeCodexProviderQueue is the shared body of the two entry points above,
// and it REPORTS completeness rather than logging it.
//
// A partial purge is not a degraded success. Every row it failed to delete is
// a message the user explicitly truncated away that the app-server's idle hook
// will still dispatch onto the shortened thread at the next resume — silently,
// minutes or days later, with no surface anywhere saying it was armed. Both
// callers therefore abort: the rollback refuses before it stops the session,
// and the cut refuses before it truncates. A refused rollback is visible and
// the user can retry it; a replay onto history they deleted is neither.
//
// (This is also why `thread/queue/list` reporting a PREFIX as the whole queue
// would defeat the check on its own — see codex.ErrThreadQueueListIncomplete.)
func (a *App) purgeCodexProviderQueue(threadID string, sess *codex.Session) error {
	if sess == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexQueueRequestTimeout)
	defer cancel()
	purge, err := sess.PurgeQueue(ctx)
	if len(purge.Deleted) > 0 {
		log.Printf("codex rollback: thread %s: dropped %d queued message(s) from the Codex queue (%d not added by Agent Overflow)",
			threadID, len(purge.Deleted), purge.Foreign)
	}
	if err == nil {
		return nil
	}
	// The rollback is about to be refused with nothing truncated — but the
	// purge is per row, so the rows it DID delete are already out of codex's
	// queue and nothing will ever dispatch them. Leaving them there would make
	// the refusal a lie: the user abandons a rollback and silently loses a
	// message they queued. Hand this app's own back to the composer, which is
	// the same route the never-queued case takes, and say what happened to the
	// rest.
	restored, unrestorable := a.restorePurgedProviderQueueRows(threadID, purge.Deleted)
	log.Printf("codex rollback: thread %s: could not fully purge the provider queue (%d removed, %d returned to the composer, %d not this app's and unrecoverable); refusing to truncate history a queued message would re-run onto: %v",
		threadID, len(purge.Deleted), restored, unrestorable, err)
	return fmt.Errorf(
		"could not clear the messages Codex still has queued for this thread; the rollback would leave the rest to re-run%s: %w",
		purgeRestoreSuffix(restored, unrestorable), err)
}

// purgeRestoreSuffix is the clause the refusal above adds when the partial
// purge already removed rows. Terse and sentence case: say where the messages
// went, and do not ask the user to re-type anything.
func purgeRestoreSuffix(restored, unrestorable int) string {
	switch {
	case restored > 0 && unrestorable > 0:
		return fmt.Sprintf(" (%s put back in the composer, and %d added outside Agent Overflow could not be recovered)",
			pluralQueuedMessages(restored), unrestorable)
	case restored > 0:
		return fmt.Sprintf(" (%s put back in the composer)", pluralQueuedMessages(restored))
	case unrestorable > 0:
		return fmt.Sprintf(" (%d already-removed message(s) were not added by Agent Overflow and could not be recovered)", unrestorable)
	default:
		return ""
	}
}

// pluralQueuedMessages is the shared count phrase for the purge refusal, so
// one rename cannot leave two callers disagreeing about what to call a queued
// message.
func pluralQueuedMessages(n int) string {
	if n == 1 {
		return "1 queued message"
	}
	return fmt.Sprintf("%d queued messages", n)
}

// restorePurgedProviderQueueRows returns the messages a PARTIAL purge already
// deleted from the provider's queue to the composer, and reports how many it
// could not.
//
// The purge deletes row by row, so an abort can land with rows already gone.
// Those rows are marked provider-queued in this app's store — and the provider
// no longer has them, so no dispatch, no echo, and no later reconcile will
// ever run them. Without this they are a message owned by nobody.
//
// Ownership is the persisted row, never the id grammar (see
// ownsLegacyProviderQueuedClientID). A deleted submission is restored only
// when THIS app's store holds an unclaimed provider-queued row under that
// `clientUserMessageId`; everything else is somebody else's message, and there
// is no honest way to put it back — re-adding it would announce its author's
// text as this user's own, and AO has no `thread/queue/add` caller left to do
// it with. Those are counted and named in the log instead.
//
// A store read that fails restores nothing: without the rows there is no way
// to tell AO's own submissions from a foreign producer's, and guessing in
// either direction is worse than the log line.
//
// No lock is taken and none is needed: both purge call sites run under the
// per-thread action lock the rollback holds, which is the same lock the flush
// dispatcher takes around its persist-and-send sequence.
func (a *App) restorePurgedProviderQueueRows(
	threadID string, deleted []codex.QueuedSubmission,
) (restored, unrestorable int) {
	if len(deleted) == 0 {
		return 0, 0
	}
	rows, err := a.legacyProviderQueuedRowsForThread(threadID)
	if err != nil {
		log.Printf("codex rollback: thread %s: could not read this thread's queued rows after a partial purge, so %d already-deleted message(s) cannot be returned to the composer: %v",
			threadID, len(deleted), err)
		return 0, len(deleted)
	}
	owned := make(map[string]store.Item, len(rows))
	for _, row := range rows {
		owned[row.ID] = row
	}
	mine := make([]store.Item, 0, len(deleted))
	for _, submission := range deleted {
		row, ok := owned[submission.ClientUserMessageID]
		if !ok || strings.TrimSpace(submission.ClientUserMessageID) == "" {
			unrestorable++
			log.Printf("codex rollback: thread %s: queued submission %s (clientUserMessageId=%q) was deleted from the Codex queue and is not this app's; it cannot be restored",
				threadID, submission.ID, submission.ClientUserMessageID)
			continue
		}
		mine = append(mine, row)
	}
	a.restoreProviderQueueRowsToComposer(threadID, mine,
		queueRestoredReasonPurgeAborted,
		"was deleted from the Codex queue by a rollback that then refused; returning it to the composer")
	return len(mine), unrestorable
}

// restoreProviderQueueRowsToComposer is the shared body behind the two ways a
// provider-queued row can end up owned by nobody: the legacy sunset takes it
// back out (sunsetLegacyProviderQueueRows) and a partial rollback purge takes
// it back out (restorePurgedProviderQueueRows). Both hand the text to the
// person who typed it, and both clear the row's pending send first — no echo
// can arrive for a row the provider does not hold.
//
// `why` is the per-row log clause, so the two callers stay distinguishable in
// a log without duplicating the walk.
func (a *App) restoreProviderQueueRowsToComposer(
	threadID string, rows []store.Item, reason, why string,
) {
	if len(rows) == 0 {
		return
	}
	restorable := make([]triage.EagerPersistedFlush, 0, len(rows))
	for _, row := range rows {
		log.Printf("codex queue: thread %s: message %s %s", threadID, row.ID, why)
		if a.triage != nil {
			a.triage.ClearPendingSendForFailure(threadID, row.ID)
		}
		restorable = append(restorable, triage.EagerPersistedFlush{
			UserItemID: row.ID,
			TurnIndex:  row.TurnIndex,
			ItemIndex:  row.ItemIndex,
			Content:    row.Summary,
			Meta:       row.Meta,
		})
	}
	a.restoreEagerPersistedFlushesForReason(threadID, restorable, reason)
}
