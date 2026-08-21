package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/itemmeta"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// This file is the PROVIDER-OWNED half of the flush queue: everything that
// exists because a Codex app-server >= 0.148 has a durable user-message queue
// of its own (`thread/queue/*`), so a message AO hands it stops being AO's to
// recover. The dispatch decision itself stays in app_flush_queue.go — this is
// what follows from it: the ownership marker's recovery-path reads, the
// ambiguous-add read-back, the rollback purge, and the two things a fresh
// session has to do about rows that outlived the process that queued them.
//
// See internal/provider/codex/AGENTS.md §"The provider-owned queue" for the
// wire contract every rule here is derived from.

// dropProviderQueuedFlushItems removes rows handed to the PROVIDER's own queue
// from a session-death drain, so they are neither restored to the composer
// draft nor requeued. Their user-visible fate is the dispatch the provider
// runs on the next resume, against the row that is already in the timeline.
//
// Ownership is read off the row itself (itemmeta.IsProviderQueued), never off
// process memory: the same question is asked by a NEW process after an app
// restart, and a map would answer it wrong exactly then. A drained entry with
// no quiet row was never persisted and therefore never handed over — the
// provider-queue dispatch path eager-persists before it writes.
//
// Rows whose hand-off is still UNPROVEN drop here too, and deliberately.
// Reaching this point with the marker means the dispatcher already ran the
// item to completion under the thread lock, so unproven can only mean the add
// timed out after its write — genuinely ambiguous, and restoring it to the
// draft would offer the user a re-send of a message the provider may be about
// to run. The next session start resolves it against the queue itself
// (reconcileCodexProviderQueueOnResume), which is the only place the evidence
// exists.
func dropProviderQueuedFlushItems(
	threadID string, drained []triage.UnconfirmedFlushItem,
) []triage.UnconfirmedFlushItem {
	if len(drained) == 0 {
		return drained
	}
	kept := drained[:0]
	for _, item := range drained {
		if item.QuietItem != nil && itemmeta.IsProviderQueued(item.QuietItem.Meta) {
			log.Printf(
				"flush queue: thread %s: message %s stays with the Codex queue across the session death (it will run on resume); not restoring it to the draft",
				threadID, item.QuietItem.ID)
			// No settlement to run here: the row only carries the marker
			// because it was persisted as the durable record of the message,
			// and that persistence already settled it (the provider is one of
			// the two durable endpoints).
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// confirmAmbiguousQueueAdd asks the provider's queue whether a timed-out
// `thread/queue/add` actually landed.
//
// The timeout says only that the ack never came back; the row may be
// persisted, may already have been dispatched, or may never have existed.
// `thread/queue/list` answers the first of those definitively, and
// `clientUserMessageId` is what identifies AO's row inside a queue that can
// also hold a foreign producer's.
//
// A miss is NOT a verdict — see the caller for why absence is still
// ambiguous. This reports only "the row is there", plus the submission the
// caller needs for the queued ack.
func (a *App) confirmAmbiguousQueueAdd(
	sess session, clientMessageID string,
) (codex.QueuedSubmission, bool, error) {
	if sess.codex == nil {
		return codex.QueuedSubmission{}, false, fmt.Errorf("session has no codex")
	}
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexQueueConfirmTimeout)
	defer cancel()
	submissions, err := sess.codex.QueueList(ctx)
	// A partial listing still PROVES anything it contains: presence is
	// positive evidence and completeness has no bearing on it. Only the
	// absence conclusion needs the whole queue, and that is the branch below
	// that keeps the error.
	for _, submission := range submissions {
		if submission.ClientUserMessageID == clientMessageID {
			return submission, true, nil
		}
	}
	if err != nil {
		return codex.QueuedSubmission{}, false, err
	}
	return codex.QueuedSubmission{}, false, nil
}

// codexQueueConfirmTimeout bounds the read-back above. Short on purpose: the
// dispatcher already spent a full request timeout on the add, and the caller's
// fallback (leave the pending entry for the echo) is safe, so a slow answer
// must not hold the flush loop a second time.
const codexQueueConfirmTimeout = 10 * time.Second

// codexQueueHandoff is what one provider-queue dispatch ESTABLISHED about the
// message it wrote — not whether the call errored, which the error return
// still says.
//
// The distinction is durable state, not bookkeeping: `thread/queue/add` has no
// idempotency key, so an unacked write can never be retried, and the only
// thing that can later tell "the provider has this message" from "AO was
// about to ask it to" is what the row says. See
// itemmeta.MarkProviderQueueHandoff.
type codexQueueHandoff uint8

const (
	// codexQueueHandoffNone — not a provider-queue dispatch at all (Claude,
	// or a pre-0.148 Codex steer), or one that failed definitively.
	codexQueueHandoffNone codexQueueHandoff = iota
	// codexQueueHandoffConfirmed — the app-server acked the add, or a
	// `thread/queue/list` read the row back. The provider owns the message.
	codexQueueHandoffConfirmed
	// codexQueueHandoffUnconfirmed — the add was WRITTEN and never proven:
	// the ack was lost and the read-back either failed or found nothing.
	// Neither owner can be asserted, so the row stays unproven and the next
	// session start asks the queue.
	codexQueueHandoffUnconfirmed
)

// confirmProviderQueueHandoff promotes a row from "AO was handing this to the
// provider queue" to "the provider owns it".
//
// Best-effort: leaving a row unproven costs one extra `thread/queue/list`
// comparison at the next session start, which is a walk that runs anyway.
// Claiming ownership AO cannot substantiate is what must not happen, so there
// is no fallback that assumes success.
func (a *App) confirmProviderQueueHandoff(threadID, itemID string) {
	if a == nil || a.store == nil || threadID == "" || itemID == "" {
		return
	}
	if _, _, err := a.store.UpdateItemMetaMerge(
		threadID, itemID, itemmeta.ConfirmProviderQueueHandoff, time.Now().UnixMilli(),
	); err != nil {
		log.Printf("codex queue: thread %s: could not record the confirmed hand-off of %s (it will be reconciled against the queue at the next session start): %v",
			threadID, itemID, err)
	}
}

// providerQueuedRowsForThread returns this thread's user rows that AO handed
// to the provider's queue and that no provider echo has claimed, in timeline
// order.
//
// **This is the ownership authority for a queued `clientUserMessageId`**, and
// it replaces recognising AO's id GRAMMAR. The grammar (`user:<turn>` /
// `user:<turn>:flush:<n>`) is not a credential: it is deterministic, so a
// second Agent Overflow process — another profile, another machine sharing the
// Codex home, a second window on the same thread — mints exactly the same ids,
// and `codex queue --thread` (or anything else speaking `thread/queue/add`)
// may simply supply one. Either way a submission this app never wrote would be
// re-armed as ours and its author's message would render as the user's own.
//
// A persisted row cannot be forged that way: every provider-queue add
// eager-persists and marks the row BEFORE the write, keyed by the very id that
// goes on the wire, so the row's existence in THIS app's store is the token
// that says the id is this app's. Nothing else needs to be persisted for it.
func (a *App) providerQueuedRowsForThread(threadID string) ([]store.Item, error) {
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

// purgeCodexProviderQueueForRollback empties the PROVIDER's queue for a thread
// the user is about to truncate.
//
// `clearFlushDispatchForRollback` handles AO's own in-process queue, but a
// message already accepted by `thread/queue/add` is durable in codex's SQLite:
// it outlives the session stop and the app-server's idle hook dispatches it on
// the next resume — re-running, against a truncated thread, exactly the
// message the rollback removed.
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
// rows a newer Codex accepted, which that Codex will dispatch the next time it
// opens the thread. The store is asked, because it is the only thing here that
// can see them, and the rollback is refused if any exist. Refusing is
// recoverable (run them, or upgrade); a message replaying onto history the
// user deleted is not.
func (a *App) purgeCodexProviderQueueForRollback(threadID string) error {
	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return nil
	}
	if !sess.usesProviderQueue() {
		if sess.codex == nil {
			return nil
		}
		return a.refuseRollbackOverUnreachableQueue(threadID)
	}
	return a.purgeCodexProviderQueue(threadID, sess.codex)
}

// refuseRollbackOverUnreachableQueue is the pre-0.148 half of the same check.
//
// Store-only by necessity: there is no `thread/queue/list` on this connection,
// so the rows AO marked before each add are the only evidence that anything is
// parked. A read error refuses too — an unknown queue state is exactly what
// the caller must not truncate over.
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
	ctx, cancel := context.WithTimeout(a.lifeCtx(), codexQueueConfirmTimeout)
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
// purge already removed rows. Terse and sentence case, same posture as the
// queue notices: say where the messages went, and do not ask the user to
// re-type anything.
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

// restorePurgedProviderQueueRows returns the messages a PARTIAL purge already
// deleted from the provider's queue to the composer, and reports how many it
// could not.
//
// The purge deletes row by row, so an abort can land with rows already gone.
// Those rows are marked provider-queued in this app's store, which is what
// every recovery path steps around — and the provider no longer has them, so
// no dispatch, no echo, and no later reconcile will ever run them. Without
// this they are exactly the shape the hand-off split exists to prevent: a
// message owned by nobody.
//
// Ownership is the persisted row, never the id grammar (see
// providerQueuedRowsForThread). A deleted submission is restored only when
// THIS app's store holds an unclaimed provider-queued row under that
// `clientUserMessageId`; everything else is somebody else's message, and there
// is no honest way to put it back — re-adding it would announce its author's
// text as this user's own. Those are counted and named in the log instead.
//
// A store read that fails restores nothing: without the rows there is no way
// to tell AO's own submissions from a foreign producer's, and guessing in
// either direction is worse than the log line.
//
// No lock is taken and none is needed, same as restoreNeverTakenProviderQueue
// Rows: both purge call sites run under the per-thread action lock the
// rollback holds, which is the lock the flush dispatcher takes around its
// persist-mark-add sequence.
func (a *App) restorePurgedProviderQueueRows(
	threadID string, deleted []codex.QueuedSubmission,
) (restored, unrestorable int) {
	if len(deleted) == 0 {
		return 0, 0
	}
	rows, err := a.providerQueuedRowsForThread(threadID)
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

// reconcileCodexProviderQueueOnResume is the whole of what a fresh Codex
// session does about a provider queue the PREVIOUS one left behind. It is the
// body of `Config.BeforeResume` (app_session.go) and runs in that hook's one
// window: after the handshake has frozen `ThreadQueueNative`, before the
// `thread/resume` that loads the thread and lets its idle hook dispatch.
//
// The two branches are not two degrees of the same thing. A queue-native
// session can READ the queue, so it relearns which rows are AO's, re-arms the
// pending sends their dispatch echoes will claim, and settles the rows whose
// hand-off was never proven. A session on an OLDER app-server has no
// `thread/queue/*` at all: the rows are still in codex's SQLite and will still
// run, but nothing on this connection can see them, list them, or purge them.
// The only correct move there is to say so.
func (a *App) reconcileCodexProviderQueueOnResume(threadID string, sess *codex.Session) {
	if sess == nil {
		return
	}
	if !sess.ThreadQueueNative() {
		a.noticeCodexProviderQueueUnreadable(threadID)
		return
	}
	a.rearmCodexProviderQueueClaims(threadID, sess)
}

// codexQueueUnsupportedNotificationKind is the `meta.kind` discriminator the
// frontend's NotificationRow matches on. Typed constant so a rename stays in
// one place, same as triage's own notification kinds.
const codexQueueUnsupportedNotificationKind = "codex_queue_unsupported"

// codexQueueUnsupportedNotificationID keys the notice row per THREAD rather
// than per turn or per session. The condition it reports is a property of the
// thread's queue (rows sitting in codex's SQLite that this binary cannot
// reach), not of any one session, so every downgraded session start on that
// thread must upsert the same row instead of stacking a new one — and the
// count it carries is re-derived from the store each time, so an upsert is
// also how a stale figure corrects itself.
const codexQueueUnsupportedNotificationID = "codex_queue_unsupported"

// noticeCodexProviderQueueUnreadable tells the user, on the timeline, that
// this thread has messages parked in Codex's own queue that the connected
// Codex cannot see.
//
// The gap it covers: a 0.148+ session accepts `thread/queue/add`, so the
// message becomes durable in codex's SQLite and its AO row is persisted and
// marked (itemmeta.MarkProviderQueued). If the NEXT session for that thread
// runs on an older Codex, there is no `thread/queue/list` to read it back
// with, no `thread/queue/delete` to purge it with, and no idle-hook dispatch
// to run it — the message simply waits, invisibly, until a 0.148+ binary
// opens the thread again and drains it into a turn the user has long since
// stopped expecting. Silence there is the failure: errors are user-facing
// state, not log entries (CLAUDE.md principle 5).
//
// What this deliberately does NOT do is as load-bearing as what it does. The
// rows are NOT deleted, NOT restored to the composer draft, and NOT re-armed
// or requeued onto AO's in-process queue. They are the provider's, and every
// one of those moves would either lose the message or set it up to run twice
// when the user upgrades — the recovery paths already read ownership off the
// row itself (dropProviderQueuedFlushItems), so leaving them alone is what
// keeps them queued and visible exactly where the user left them.
//
// Best-effort and non-fatal: a store read that fails, or a triage that is not
// wired, costs the notice and nothing else. The session start it rides must
// not fail over a message that is safe where it is.
func (a *App) noticeCodexProviderQueueUnreadable(threadID string) {
	if a == nil || a.store == nil || a.triage == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	stranded, err := a.store.ListUnclaimedProviderQueuedUserItemIDs(threadID)
	if err != nil {
		log.Printf("codex queue: thread %s: could not check for messages left with a newer Codex queue: %v", threadID, err)
		return
	}
	if len(stranded) == 0 {
		return
	}
	log.Printf("codex queue: thread %s: %d message(s) are held by the Codex queue this app-server has no API for (%s); they run when Codex is upgraded",
		threadID, len(stranded), strings.Join(stranded, ", "))

	a.emitCodexQueueNotice(threadID,
		codexQueueUnsupportedNotificationID,
		codexQueueUnsupportedNotificationKind,
		codexQueueUnsupportedSummary(len(stranded)))
}

// emitCodexQueueNotice is the shared timeline write behind both queue
// notices. Upserts on (threadID, itemID), so a condition that recurs across
// session starts corrects its own row rather than stacking duplicates.
func (a *App) emitCodexQueueNotice(threadID, itemID, kind, summary string) {
	if a == nil || a.triage == nil {
		return
	}
	meta, err := json.Marshal(map[string]any{
		"kind":  kind,
		"title": summary,
	})
	if err != nil {
		// Unreachable for a two-string map, but the notice is worth more
		// than the meta: fall back to the kind alone so the row still
		// renders as its own notice rather than a generic bell.
		meta = json.RawMessage(`{"kind":"` + kind + `"}`)
	}
	// HandleSynthetic, not Handle: this fires from inside the session
	// handshake, before MarkThreadActive has cleared the stopped-thread gate,
	// and it is a host observation rather than a wire frame from a torn-down
	// subprocess — exactly the split HandleSynthetic exists for.
	if err := a.triage.HandleSynthetic(provider.ProviderEvent{
		Kind:      provider.EventNotification,
		ThreadID:  threadID,
		ItemID:    itemID,
		Content:   summary,
		Meta:      meta,
		Timestamp: time.Now(),
	}); err != nil {
		log.Printf("codex queue: thread %s: persist the %s notice: %v", threadID, kind, err)
	}
}

// codexQueueUnreconciledNotificationKind / ...ID are the second notice: the
// connected Codex DOES have `thread/queue/*`, but this session could not read
// the queue, so AO knows which rows it handed over and not which the provider
// still holds.
//
// Separate kind and separate row id from the unsupported notice on purpose.
// The two say different things to the user (upgrade Codex versus this may
// resolve itself), and sharing the row id would let one overwrite the other on
// a thread that hit both.
const (
	codexQueueUnreconciledNotificationKind = "codex_queue_unreconciled"
	codexQueueUnreconciledNotificationID   = "codex_queue_unreconciled"
)

// noticeCodexProviderQueueUnreconciled reports a queue read that failed on a
// session that should have been able to make it.
//
// It exists because the fallback is PARTIAL, and silently partial is the
// failure mode CLAUDE.md principle 5 is about. The two counts are two
// different states, and the notice names both rather than folding them:
//
//   - `proven` rows are the ones the provider acked. They were re-armed, so an
//     automatic dispatch is still attributed to this app and the user's own
//     message is never announced as arriving from outside.
//   - `unproven` rows are the ones whose `thread/queue/add` was written and
//     never acked. Nothing here can tell a row the provider holds from one it
//     never took, so neither recovery is applied and they are named instead —
//     otherwise the user's only signal that a message may still need sending
//     is its absence.
//
// So the notice says what the user can act on: a message may be waiting, and
// reopening the thread later is what resolves it. It is upserted per thread,
// so a run of failed starts leaves one row, not one per attempt.
func (a *App) noticeCodexProviderQueueUnreconciled(threadID string, proven, unproven int) {
	if a == nil || a.triage == nil || strings.TrimSpace(threadID) == "" {
		return
	}
	if proven <= 0 && unproven <= 0 {
		return
	}
	a.emitCodexQueueNotice(threadID,
		codexQueueUnreconciledNotificationID,
		codexQueueUnreconciledNotificationKind,
		codexQueueUnreconciledSummary(proven, unproven))
}

// codexQueueUnreconciledSummary is that row's one sentence. Same posture as
// the unsupported summary: name the counts, state the outcome rather than the
// JSON-RPC cause, and do not ask the user to re-type anything, because a
// duplicate send is the one outcome this whole path is built to avoid.
func codexQueueUnreconciledSummary(proven, unproven int) string {
	const lead = "Codex couldn't be asked about its message queue when this thread reopened"
	switch {
	case proven > 0 && unproven > 0:
		return fmt.Sprintf(
			"%s. %s may still be waiting there, and %s may never have reached Codex. Reopen the thread later to check.",
			lead, pluralQueuedMessages(proven), pluralQueuedMessages(unproven))
	case unproven > 0:
		return fmt.Sprintf(
			"%s, so %s may never have reached it. Reopen the thread later to check.",
			lead, pluralQueuedMessages(unproven))
	default:
		return fmt.Sprintf(
			"%s, so %s can't be confirmed. Reopen the thread later to check.",
			lead, pluralQueuedMessages(proven))
	}
}

// pluralQueuedMessages is the shared count phrase for the queue notices and
// the partial-purge refusal, so one rename cannot leave two of them disagreeing
// about what to call a queued message.
func pluralQueuedMessages(n int) string {
	if n == 1 {
		return "1 queued message"
	}
	return fmt.Sprintf("%d queued messages", n)
}

// codexQueueUnsupportedSummary is the row's one sentence. It names the count
// because that is the only thing about the situation the user can act on
// (whether upgrading Codex is worth doing now), and it states the outcome
// rather than the cause: the messages are not lost and nothing needs
// re-typing.
func codexQueueUnsupportedSummary(count int) string {
	if count == 1 {
		return "1 queued message was handed to Codex 0.148+ and this Codex version can't see it. It runs when Codex is upgraded."
	}
	return fmt.Sprintf(
		"%d queued messages were handed to Codex 0.148+ and this Codex version can't see them. They run when Codex is upgraded.",
		count)
}

// rearmCodexProviderQueueClaims rebuilds a fresh session's self-queued claim
// ledger from rows that outlived the previous one.
//
// Without it, a message AO queued before a crash / restart dispatches on
// resume as a `turn/started` this connection never asked for, finds no claim,
// and gets stamped `external-queue` — telling the user their own prompt came
// from outside Agent Overflow. The ledger is in-memory by design (a claim is
// about a turn THIS connection will see), which is exactly why it has to be
// rebuilt rather than persisted.
//
// Ownership is decided here rather than in the provider package because the
// evidence is AO's store: `clientUserMessageId` is the optimistic row id the
// flush dispatcher allocated and persisted before the add, so the set of rows
// this thread has marked (providerQueuedRowsForThread) is exactly the set of
// queue entries this app wrote. The provider package cannot see that, and
// nothing on the wire distinguishes AO's ids from anyone else's — the grammar
// is deterministic, so a second AO profile sharing the Codex home would mint
// the same ids and any producer may simply supply one.
//
// The queue read can FAIL, and what follows from that is the part worth
// stating. `thread/resume` runs immediately after this hook and loads the
// thread; a loaded thread's idle hook dispatches the queue. So a session that
// gave up here would watch its own queued rows start turns it cannot account
// for: the echo is stamped `external-queue`, triage deliberately refuses to
// pop the pending send for a foreign-provenance echo, and the user's own
// prompt lands as a second `user:queue:*` row while the original stays
// stranded. That is a lost message and a corrupted transcript, not "a wrong
// marker on one row".
//
// So the read is retried once, and if it still fails the ownership question is
// answered from the STORE instead — the rows AO persisted and marked before
// each add are a complete record of this app's participation in this thread's
// queue, and they need no live connection to read. Only the PROVEN ones are
// re-armed there: for an unproven row the store cannot tell "the provider has
// it" from "the add never landed", and the two want opposite recoveries, so
// neither is applied and the notice names them. See
// rearmProviderQueueFromStoreAlone.
func (a *App) rearmCodexProviderQueueClaims(threadID string, sess *codex.Session) {
	if sess == nil || !sess.ThreadQueueNative() {
		return
	}
	// The ownership authority, read first so a failed queue list still has it.
	// A failure here leaves no answer to give: the notice this would otherwise
	// raise is itself a store write, and every branch below needs these rows
	// to mean anything. The walk continues over an empty set, which claims
	// nothing rather than claiming wrongly.
	rows, storeErr := a.providerQueuedRowsForThread(threadID)
	if storeErr != nil {
		log.Printf("codex queue: thread %s: could not read this thread's queued rows at session start: %v", threadID, storeErr)
	}

	submissions, listErr := a.listCodexProviderQueueWithRetry(threadID, sess)
	if listErr != nil {
		a.rearmProviderQueueFromStoreAlone(threadID, sess, rows, listErr)
		return
	}

	held := make(map[string]struct{}, len(submissions))
	for _, submission := range submissions {
		held[submission.ClientUserMessageID] = struct{}{}
	}
	ownedIDs := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		ownedIDs[row.ID] = struct{}{}
	}

	// Split the STORE's rows by what the provider still holds. Iterating the
	// store rather than the listing keeps timeline order, which is the order
	// the provider will run them in and the order the pending sends want.
	inQueue := make([]store.Item, 0, len(rows))
	var neverTaken []store.Item
	for _, row := range rows {
		if _, ok := held[row.ID]; ok {
			inQueue = append(inQueue, row)
			continue
		}
		if itemmeta.IsProviderQueueHandoffPending(row.Meta) {
			// Marked, unproven, and the provider does not have it: the add
			// never landed. Nothing else will ever run this message.
			neverTaken = append(neverTaken, row)
			continue
		}
		// Marked, PROVEN, and gone from the queue: it dispatched. Its echo
		// either landed on the old session or was lost with it; either way the
		// row is history and re-arming it would let an unrelated later echo
		// claim it.
	}
	foreign := 0
	for _, submission := range submissions {
		if _, mine := ownedIDs[submission.ClientUserMessageID]; !mine {
			foreign++
		}
	}

	ids := make([]string, 0, len(inQueue))
	for _, row := range inQueue {
		ids = append(ids, row.ID)
	}
	rearmed := sess.RearmSelfQueuedClaims(ids)
	a.rearmProviderQueuedFlushRows(threadID, inQueue)
	// The listing is proof the add landed, for any row still carrying an
	// unproven hand-off — the ambiguous-timeout case resolving late.
	for _, row := range inQueue {
		if itemmeta.IsProviderQueueHandoffPending(row.Meta) {
			a.confirmProviderQueueHandoff(threadID, row.ID)
		}
	}
	a.restoreNeverTakenProviderQueueRows(threadID, neverTaken)
	if rearmed > 0 || foreign > 0 {
		log.Printf("codex queue: thread %s resumed with %d queued message(s) Agent Overflow owns and %d it does not",
			threadID, rearmed, foreign)
	}
}

// listCodexProviderQueueWithRetry reads the provider queue, retrying once.
//
// One retry, not a loop: the caller runs inside the session handshake, so
// every second spent here is a second before the thread loads. The single
// retry covers the shape that actually happens — a first request racing an
// app-server that has just finished its own startup work — and anything
// persistent falls through to the store-only reconcile, which is a real
// answer rather than a longer wait.
func (a *App) listCodexProviderQueueWithRetry(
	threadID string, sess *codex.Session,
) ([]codex.QueuedSubmission, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		ctx, cancel := context.WithTimeout(a.lifeCtx(), codexQueueConfirmTimeout)
		submissions, err := sess.QueueList(ctx)
		cancel()
		if err == nil {
			return submissions, nil
		}
		lastErr = err
		log.Printf("codex queue: thread %s: could not read the provider queue at session start (attempt %d): %v",
			threadID, attempt+1, err)
	}
	return nil, lastErr
}

// rearmProviderQueueFromStoreAlone is the reconcile with no readable queue.
//
// Only PROVEN rows are re-armed. For those the store is a complete answer to
// the only question the re-arm asks: the provider acked the add, so the row is
// the provider's, and either it is still queued (the claim and the pending send
// are exactly what its dispatch needs) or it already ran (both are inert —
// they are consumed BY CLIENT ID, codex.takeSelfQueuedClaimForClientLocked and
// triage.consumeMatchingPendingSendForEcho, so only that row's own echo can
// take them and no such echo can arrive for a row that is gone). Re-arming is
// what keeps the provider from dispatching AO's own message as an injected
// turn.
//
// UNPROVEN rows get NEITHER re-arm nor restore, and that asymmetry is the
// point. Their `thread/queue/add` was written and never acked, so the provider
// may hold the row or may never have seen it, and the two answers want
// opposite recoveries:
//
//   - Restoring is wrong: if the add did land, the user gets a draft of a
//     message that is also scheduled to run, and re-sending it is a duplicate.
//   - Re-arming is wrong for the commoner case: if the add never landed, no
//     echo can ever consume the claim or the pending send, so the pending send
//     sits in the FIFO forever — where HasPendingSendForThread reads it and
//     refuses every revert-and-resend on the thread (app_revert_and_resend.go),
//     while the message itself is stranded outside both the provider and the
//     composer.
//
// So they are left exactly as they are, the next session start that CAN read
// the queue resolves them, and the user is told — which is the whole reason
// the notice names them separately rather than in the log.
func (a *App) rearmProviderQueueFromStoreAlone(
	threadID string, sess *codex.Session, rows []store.Item, listErr error,
) {
	if len(rows) == 0 {
		// Nothing of AO's is outstanding on this thread, so an unreadable
		// queue costs nothing worth telling the user about. A foreign
		// producer's rows are still reported by the session's own
		// `thread/queue/changed` reconciliation when it can read them.
		log.Printf("codex queue: thread %s: the provider queue could not be read at session start and this thread has no outstanding queued rows: %v",
			threadID, listErr)
		return
	}
	proven := make([]store.Item, 0, len(rows))
	var unproven []store.Item
	for _, row := range rows {
		if itemmeta.IsProviderQueueHandoffPending(row.Meta) {
			unproven = append(unproven, row)
			continue
		}
		proven = append(proven, row)
	}
	ids := make([]string, 0, len(proven))
	for _, row := range proven {
		ids = append(ids, row.ID)
	}
	rearmed := sess.RearmSelfQueuedClaims(ids)
	a.rearmProviderQueuedFlushRows(threadID, proven)
	if len(unproven) > 0 {
		unprovenIDs := make([]string, 0, len(unproven))
		for _, row := range unproven {
			unprovenIDs = append(unprovenIDs, row.ID)
		}
		log.Printf("codex queue: thread %s: %d row(s) whose hand-off was never proven are left alone until a session can read the queue (%s)",
			threadID, len(unproven), strings.Join(unprovenIDs, ", "))
	}
	log.Printf("codex queue: thread %s: the provider queue could not be read at session start (%v); re-armed %d proven row(s) from this app's own record instead",
		threadID, listErr, rearmed)
	a.noticeCodexProviderQueueUnreconciled(threadID, len(proven), len(unproven))
}

// restoreNeverTakenProviderQueueRows returns messages the provider never took
// to the composer.
//
// These are rows AO marked before a `thread/queue/add` that the provider does
// not have: the process went away between the persist and the write, or the
// add failed and its in-memory requeue died with the process. Nothing will
// ever run them — the provider does not know about them, and every recovery
// path steps around a marked row — so without this they sit in the timeline
// forever as a prompt with no answer.
//
// The composer, not AO's queue: a queue entry would send the message the
// moment the thread went idle, and this row's history is a message whose
// delivery already failed once in a way nobody observed. Handing the text back
// to the person who typed it is the recovery every other flush failure takes.
//
// No lock is taken and none is needed: the caller runs inside the session
// spawn, which the start path holds the per-thread action lock across, and
// that is the same lock the flush dispatcher holds around its persist-mark-add
// sequence. A row being marked right now therefore cannot be read as
// never-taken by this walk. Taking the lock HERE would deadlock that path.
func (a *App) restoreNeverTakenProviderQueueRows(threadID string, rows []store.Item) {
	a.restoreProviderQueueRowsToComposer(threadID, rows,
		queueRestoredReasonNeverQueued,
		"was never taken by the Codex queue; returning it to the composer")
}

// restoreProviderQueueRowsToComposer is the shared body behind the two ways a
// provider-queued row can end up owned by nobody: the provider never took it
// (restoreNeverTakenProviderQueueRows) and a partial rollback purge took it
// back out (restorePurgedProviderQueueRows). Both hand the text to the person
// who typed it, and both clear the row's pending send first — no echo can
// arrive for a row the provider does not hold.
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

// rearmProviderQueuedFlushRows re-arms the pending-send entries that make a
// queue-dispatched turn's `userMessage` echo land on the row it was minted
// for.
//
// The claim ledger above answers "is this turn ours"; this answers "which row
// is it". They are rebuilt from the same list for the same reason — both are
// process memory describing a message that outlived the process — but this
// one is the half an APP restart breaks: a fresh process has the persisted
// row (itemmeta.MarkProviderQueued put it there before the add) and no
// pending send, so the echo would find nothing to claim and be persisted as
// injected provider context under a row the user already sees.
//
// Scope is deliberately narrow. Only a row that (a) still exists, (b) carries
// the provider-queued marker, and (c) has not already been stamped with a
// provider item id is re-armed: a row already claimed by an echo is history,
// and re-arming it would let the NEXT echo overwrite it.
func (a *App) rearmProviderQueuedFlushRows(threadID string, candidates []store.Item) {
	if a.triage == nil || a.store == nil || len(candidates) == 0 {
		return
	}
	rows := make([]store.Item, 0, len(candidates))
	for _, item := range candidates {
		if item.Kind != "user_text" {
			continue
		}
		if !itemmeta.IsProviderQueued(item.Meta) {
			continue
		}
		if usermessage.ReadProviderItemID(item.Meta) != "" {
			continue
		}
		rows = append(rows, item)
	}
	if len(rows) == 0 {
		return
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	// Idempotent by construction: a session that died with these entries still
	// in the FIFO drains them (they leave through dropProviderQueuedFlushItems
	// with the row intact), but a path that does not drain would otherwise
	// leave a duplicate here — and two entries for one row means the second
	// echo on the thread claims a row that already has its message.
	a.triage.ClearPendingSendsByItemIDs(threadID, ids)
	for _, row := range rows {
		// A FRESH queue id, not the dead one: the flushqueue row it named is
		// gone with the process that held it, and the id is only ever used
		// downstream as "this echo came from a queued send, record its message
		// anchor at the echo" (handle_user_text) plus as an event key. An
		// empty one would read as a DIRECT send whose anchor was recorded at
		// send time — which for this row nobody did.
		a.triage.RegisterPendingQuietFlushSend(
			threadID, flushqueue.NewItemID(), row, row.TurnIndex, row.CreatedAt, "")
	}
	log.Printf("codex queue: thread %s: re-armed %d persisted row(s) waiting on a queued dispatch", threadID, len(rows))
}
