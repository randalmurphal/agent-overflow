package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
	"agent-overflow/internal/usermessage"
)

// This file is the RECOVERY half of the flush queue: everything that runs
// when a queued message could not complete its trip to the provider — a
// session death mid-dispatch, a definite Codex resend failure — and has to
// end up somewhere durable instead. The two destinations are the composer
// draft (restore) and the queue itself (requeue); the dispatch half lives in
// app_flush_queue.go and what remains of the PROVIDER's own queue — the legacy
// sunset and the rollback purge — in app_codex_provider_queue.go.

// restoreUnconfirmedQueueOnSessionDeath drains queued and unconfirmed
// flush state after a session death, restoring what it can to the
// composer draft. Items it REQUEUES instead (failed stale-row cleanup,
// failed draft restore) are also returned: a caller that runs a triage
// CleanupThread afterwards wipes the queue those items just re-entered
// and must re-register them once the cleanup has run (round-13,
// D13-1). Callers with no subsequent cleanup may ignore the return.
func (a *App) restoreUnconfirmedQueueOnSessionDeath(threadID string) []triage.UnconfirmedFlushItem {
	return a.restoreUnconfirmedQueueOnSessionDeathIf(threadID, nil)
}

// restoreUnconfirmedQueueOnSessionDeathIf is the guarded form used by the
// provider event handler. The guard runs after the thread action lock is held and
// before any queue state is drained. That makes a session token plus triage
// epoch check atomic with respect to a replacement start or send on the same
// thread.
func (a *App) restoreUnconfirmedQueueOnSessionDeathIf(
	threadID string,
	guard func() bool,
) []triage.UnconfirmedFlushItem {
	if a.triage == nil || strings.TrimSpace(threadID) == "" {
		return nil
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if guard != nil && !guard() {
		return nil
	}
	return a.restoreUnconfirmedQueueLocked(threadID)
}

// restoreUnconfirmedQueueLocked is the restore itself, for a caller that
// already holds the thread action lock: a session the app is about to stop
// for a reason other than the person's own Stop hands its queued messages
// back to the composer the same way a death does.
func (a *App) restoreUnconfirmedQueueLocked(threadID string) []triage.UnconfirmedFlushItem {
	restore := a.restoreUnconfirmedQueueOwing(threadID)
	a.finishFlushRestore(threadID, restore)
	return restore.requeued
}

// restoreUnconfirmedQueueOwing is restoreUnconfirmedQueueLocked leaving the
// settlement of what the app wrote to the caller (finishFlushRestore), for a
// teardown that restores under the thread's mutation lock: settling
// requeues other threads' requests, a queue write that takes that lock.
func (a *App) restoreUnconfirmedQueueOwing(threadID string) flushRestore {
	a.forgetAgentWakeTurns(threadID)
	if a.triage == nil {
		return flushRestore{}
	}
	// threadLock -> a.flushDispatch.mu is the established lock order documented
	// by RegisterQueueItem. Draining here, rather than before the thread lock,
	// keeps the guard and the destructive operation in one critical section.
	dispatchItems := a.drainFlushDispatchForSessionEnd(threadID)

	drained := a.triage.DrainUnconfirmedFlushItems(threadID)
	for _, item := range dispatchItems {
		payload := append(json.RawMessage(nil), item.Payload...)
		drained = append(drained, triage.UnconfirmedFlushItem{
			QueueItemID:     item.ID,
			Message:         item.Message,
			Payload:         payload,
			EnqueuedAt:      item.EnqueuedAt,
			StaleUserItemID: item.StaleUserItemID,
			Settlement:      item.Settlement,
		})
	}
	drained = dedupeUnconfirmedFlushItems(drained)
	sortByEnqueuedAt(drained)
	if len(drained) == 0 {
		return flushRestore{}
	}
	return a.restoreFlushItemsToDraftOwing(threadID, drained, queueRestoredReasonSessionDied)
}

// restoreUndeliveredSends returns the sends Claude cancelled without running
// (command_lifecycle `cancelled` with no `started`, triage retireUnrunSend)
// to the composer in one batch, oldest first. A row one already has in the
// timeline is removed exactly as an unconsumed quiet row is.
func (a *App) restoreUndeliveredSends(threadID string) {
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	a.restoreUndeliveredSendsLocked(threadID)
}

// restoreUndeliveredSendsLocked is restoreUndeliveredSends for a caller that
// holds the thread action lock.
func (a *App) restoreUndeliveredSendsLocked(threadID string) {
	if a.triage == nil {
		return
	}
	drained := a.triage.DrainUndeliveredSends(threadID)
	if len(drained) == 0 {
		return
	}
	sortByEnqueuedAt(drained)
	a.restoreFlushItemsToDraftLocked(threadID, drained, queueRestoredReasonNotDelivered)
}

// sortByEnqueuedAt orders restored messages oldest first; an item with no
// enqueue time goes last.
func sortByEnqueuedAt(items []triage.UnconfirmedFlushItem) {
	sort.SliceStable(items, func(i, j int) bool {
		left := items[i].EnqueuedAt
		right := items[j].EnqueuedAt
		if left == 0 || right == 0 {
			return left != 0
		}
		return left < right
	})
}

// restoreFlushItemsToDraftLocked returns unconsumed messages to the composer
// draft, removing any row one already has in the timeline. Caller holds the
// thread action lock. Returns the items it requeued instead.
//
// What the app wrote is not the person's and never enters the composer
// (agentPartsNotTakenIn): a person's text joined with it goes back alone, and
// the app's messages are settled (settleNotTakenIn) once the restore of the
// rest has committed. A restore that fails requeues the items whole, the
// app's messages included, and they are still queued.
func (a *App) restoreFlushItemsToDraftLocked(threadID string, drained []triage.UnconfirmedFlushItem, reason string) []triage.UnconfirmedFlushItem {
	restore := a.restoreFlushItemsToDraftOwing(threadID, drained, reason)
	a.finishFlushRestore(threadID, restore)
	return restore.requeued
}

// flushRestore is a restore whose composer write is done and whose
// settlement of what the app wrote is owed (finishFlushRestore).
type flushRestore struct {
	// requeued are the items a failed restore requeued instead; nothing
	// else is owed then.
	requeued []triage.UnconfirmedFlushItem
	// restored is set when the composer write committed.
	restored bool
	owed     []notTakenIn
	event    QueueRestoredEvent
}

// finishFlushRestore settles what the app wrote among a committed restore's
// messages (settleNotTakenIn) and tells clients what the restore changed.
// The caller holds the thread action lock and not its mutation lock.
func (a *App) finishFlushRestore(threadID string, restore flushRestore) {
	if !restore.restored {
		return
	}
	a.settleNotTakenIn(threadID, restore.owed)
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	a.emitQueueStateChanged(threadID)
	a.emit(eventchan.ProviderQueueRestored, restore.event)
}

// restoreFlushItemsToDraftOwing is restoreFlushItemsToDraftLocked leaving
// the settlement and the announcement to finishFlushRestore.
func (a *App) restoreFlushItemsToDraftOwing(threadID string, drained []triage.UnconfirmedFlushItem, reason string) flushRestore {
	// The composer write, the queue rows' removal and the retire of every
	// row the messages have (a quiet row, a stale row a requeue carries, or
	// the id a deferred message's review drafts were claimed for) are one
	// store transaction (restoreQueuedDraft): a message is never in the
	// composer while its row still sits in the timeline, where a send of
	// the draft would mint a second one (round-10, R10-5), nor in neither
	// after a crash. When the write fails nothing changed, so every message
	// is requeued as it was, carrying its row's id so the redispatch retires
	// the row before persisting a fresh one (round-11, R11-1). Requeued
	// items caught still-queued by a LATER death re-enter here through
	// StaleUserItemID.
	var retire, queueItemIDs, userItemIDs []string
	parts := make([]composerdraft.Part, 0, len(drained))
	settlements := make([]*triage.FlushSettlement, 0, len(drained))
	var owed []notTakenIn
	for i, item := range drained {
		staleID := unconfirmedRowID(item)
		drained[i].StaleUserItemID = staleID
		if staleID != "" {
			retire = append(retire, staleID)
		}
		if item.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, item.QueueItemID)
		}
		if item.UserItemID != "" {
			userItemIDs = append(userItemIDs, item.UserItemID)
		}
		part, restore, itemOwed := a.agentPartsNotTakenIn(threadID, item, a.restoredFlushDraftPart(threadID, item), reason)
		owed = append(owed, itemOwed)
		if !restore {
			settlements = append(settlements, item.Settlement)
			continue
		}
		if !emptyDraftPart(part) {
			parts = append(parts, part)
			settlements = append(settlements, item.Settlement)
		}
	}
	if len(parts) > 0 || len(retire) > 0 || len(queueItemIDs) > 0 {
		if err := a.restoreQueuedDraft(threadID, parts, store.QueueDraftRestore{
			QueueItemIDs: queueItemIDs, RetireItemIDs: retire,
		}); err != nil {
			log.Printf("flush queue: restore draft (%s) for %s: %v — requeueing the messages", reason, threadID, err)
			a.emitWireErrorToThread(threadID, "Could not restore queued messages: "+err.Error())
			a.requeueUnconfirmedFlushItems(threadID, drained)
			a.emitQueueStateChanged(threadID)
			return flushRestore{requeued: drained}
		}
	}
	for _, settlement := range settlements {
		settlement.Settle("")
	}
	return flushRestore{restored: true, owed: owed, event: QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       reason,
		QueueItemIDs: queueItemIDs,
		UserItemIDs:  userItemIDs,
	}}
}

// cleanupStaleFlushRow takes back a message the provider never consumed,
// whose text is being redispatched: it removes the row the
// message has (a quietly-persisted flush row, a direct send's row) and
// returns the review drafts the message claimed to drafts, in one
// transaction (store.RetireUndeliveredUserRow), and reports them with the
// bodies the message carries. The FK cascade on message_anchors takes any
// anchor row with it. A row already gone (an earlier retry finished the
// job, or a deferred message never had one) is success.
func (a *App) cleanupStaleFlushRow(threadID, userItemID string) ([]store.DiffReviewReturnedComments, error) {
	returned, err := a.store.RetireUndeliveredUserRow(threadID, userItemID)
	if err != nil {
		return nil, err
	}
	a.announceReturnedReviewComments(returned)
	return returned, nil
}

// announceReturnedReviewComments announces the review comments a retire
// returned to drafts on their threads. A PR's drafts have no thread to
// announce on; the page re-reads them itself.
func (a *App) announceReturnedReviewComments(returned []store.DiffReviewReturnedComments) {
	for _, source := range returned {
		if source.Owner.ThreadID != "" {
			a.announceDiffCommentsChanged(source.Owner.ThreadID, source.Scope, source.SourceKey)
		}
	}
}

// unconfirmedRowID is the row a message taken back from the provider has in
// the store, which its cleanup retires (cleanupStaleFlushRow): its quiet row,
// the row an earlier failed cleanup left (StaleUserItemID), or, for a
// deferred message with no row, the id its review drafts were claimed for.
// Empty for a message with neither.
func unconfirmedRowID(item triage.UnconfirmedFlushItem) string {
	if item.QuietItem != nil {
		return item.QuietItem.ID
	}
	if item.StaleUserItemID != "" {
		return item.StaleUserItemID
	}
	if item.DeferredItem != nil {
		return item.DeferredItem.ID
	}
	return ""
}

// dropUnconfirmedSends is a dropped queue's other half: the messages the
// session was sent and never confirmed, and the queued ones that still have
// a row from an earlier dispatch (dispatched, the batches dropFlushQueueLocked
// took from the worker, and triage's own queue). They go with the queue, so
// each row leaves the timeline with the review drafts its message claimed,
// and a connected client drops it (queue_restored, reason dropped). A message
// the provider did consume is confirmed instead (DrainUnconfirmedFlushItems).
// A row whose retire fails stays, and the thread shows the error.
//
// The app's messages among them are not the person's. The answers,
// reminders and completions go back to their owners through the teardown's
// held list (unconfirmedOwnedMessages), which the caller read before this
// drain; the requests wholly in one went back to their queue
// (returnRequestsAtTeardown); a loop or watch wake, an outside send or a
// worker's prompt is recorded as dropped, as a queued one is
// (noteUnconfirmedAppMessagesDropped).
//
// A send wholly other threads' requests, or wakes going on to an assistant's
// current thread, is not dropped: the teardown took it back (taken), and its
// give-back removes its row before it queues them again
// (giveBackTeardownRequests), or leaves the row for the boot when it is
// refused.
func (a *App) dropUnconfirmedSends(threadID string, dispatched []triage.QueuedFlushItem, taken teardownRequests) {
	if a.triage == nil {
		return
	}
	drained := a.triage.DrainUnconfirmedFlushItems(threadID)
	for _, item := range dispatched {
		if item.StaleUserItemID != "" {
			drained = append(drained, triage.UnconfirmedFlushItem{QueueItemID: item.ID, Payload: item.Payload, StaleUserItemID: item.StaleUserItemID})
		}
	}
	var queueItemIDs, userItemIDs []string
	var failed []error
	for _, item := range drained {
		rowID := unconfirmedRowID(item)
		if rowID == "" || taken.tookSend(item) {
			continue
		}
		a.noteUnconfirmedAppMessagesDropped(threadID, item)
		returned, err := a.store.RetireUndeliveredUserRow(threadID, rowID)
		if err != nil {
			failed = append(failed, fmt.Errorf("%s: %w", rowID, err))
			continue
		}
		a.announceReturnedReviewComments(returned)
		if item.QuietItem == nil && item.StaleUserItemID == "" {
			// A deferred message never had a row to remove.
			continue
		}
		if item.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, item.QueueItemID)
		}
		userItemIDs = append(userItemIDs, rowID)
	}
	if len(failed) > 0 {
		err := errors.Join(failed...)
		log.Printf("flush queue: drop unconfirmed sends of %s: %v", threadID, err)
		a.emitErrorToThread(threadID, "A message the stopped session never confirmed could not be removed from the timeline: "+err.Error())
	}
	if len(userItemIDs) == 0 {
		return
	}
	a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       queueRestoredReasonDropped,
		QueueItemIDs: queueItemIDs,
		UserItemIDs:  userItemIDs,
	})
}

// noteUnconfirmedAppMessagesDropped records the loop and watch wakes, outside
// sends and workers' prompts an unconfirmed message carries as a wholesale
// drop records queued ones (noteAgentWakesDropped): a wake's row records
// `not delivered` and a watch keeps its text, a worker's prompt goes back to
// the user in the worker, and an outside send leaves its thread a notice.
// An assistant's opening goes with the drop, as a queued one does. Answers,
// reminders, completions and requests are the teardown's to give back; a
// person's text goes with the drop.
func (a *App) noteUnconfirmedAppMessagesDropped(threadID string, item triage.UnconfirmedFlushItem) {
	members := unconfirmedMembersOf(item)
	if !slices.ContainsFunc(members.ids, func(id string) bool { return agentAppPartRoot(id) != "" }) {
		return
	}
	messages, _, _ := splitSessionEndParts(members, a.restoredFlushDraftPart(threadID, item).Content)
	for _, id := range members.ids {
		switch {
		case agentAppPartRoot(id) == "":
		case a.handBackWorkerPromptWake(id):
		case strings.HasPrefix(id, agentSendIDRoot):
			a.noteAgentSendNotDelivered(threadID, "the thread's queue was cleared before the agent took it in")
		default:
			if wakeID, _, ok := agentWakeFromSendID(id); ok {
				a.noteAgentWakeUndelivered(threadID, wakeID, agentWakeOutcomeDropped, messages[id].Text())
			}
		}
	}
}

// restoreEagerPersistedFlushesToDraft returns eagerly-persisted
// interrupt rows to the composer draft after a definite Codex resend
// failure: the store rows are deleted and
// the content lands back in the composer, merged ahead of any draft
// text the user already typed. This mirrors the Codex TUI, whose
// recovery posture for input the model never consumed is
// restore-to-composer, not leave-as-history (input_restore.rs
// drain_pending_messages_for_restore).
//
// The composer write and the retire of the rows are one store transaction
// (restoreQueuedDraft), as in the session-death restore: a message is never
// in the draft while its row still sits in the timeline (round-10 R10-5),
// nor in neither after a crash. When the write fails nothing changed, and
// the messages fall back to the stale-marker requeue so the redispatch
// retires the rows.
func (a *App) restoreEagerPersistedFlushesToDraft(threadID string, persisted []triage.EagerPersistedFlush) {
	a.restoreEagerPersistedFlushesForReason(threadID, persisted, queueRestoredReasonResendFailed)
}

// The three `provider:queue_restored` reasons this restore emits.
//
// They are not interchangeable labels for one event. The first says a send AO
// attempted came back as a definite failure; the second says nothing will
// ever run the message — a legacy row an older build left with the Codex
// queue, which a session start has now taken back out (or which that queue
// never had); the third says the provider DID have it and a
// rollback took it back out before refusing, so the message the user typed
// exists nowhere else. The frontend does the same thing with all three today,
// but a user asking "why is this back in my composer" has different answers,
// and folding them would make the others indistinguishable from a provider
// that refused the message.
const (
	queueRestoredReasonSessionDied  = "session_died"
	queueRestoredReasonNotDelivered = "not_delivered"
	// The thread had no session for a message queued on it, and starting
	// one failed.
	queueRestoredReasonSessionStartFailed = "session_start_failed"
	queueRestoredReasonResendFailed       = "resend_failed"
	queueRestoredReasonNeverQueued        = "provider_queue_never_taken"
	queueRestoredReasonPurgeAborted       = "provider_queue_purge_aborted"
	// A Stop dropped the queue: the rows named left the timeline and their
	// text went nowhere (dropUnconfirmedSends).
	queueRestoredReasonDropped = "dropped"
	// queueRestoredReasonRequestRequeued removes the rows of other threads'
	// requests a teardown took back before the agent took them in; their
	// messages are queued again rather than restored to the composer
	// (giveBackTeardownRequests).
	queueRestoredReasonRequestRequeued = "request_requeued"
)

// restoreEagerPersistedFlushesForReason is the body, parameterised by the
// reason that reaches the frontend. What the app wrote is settled as a
// session end settles it (agentPartsNotTakenIn), once the draft has
// committed.
func (a *App) restoreEagerPersistedFlushesForReason(
	threadID string, persisted []triage.EagerPersistedFlush, reason string,
) {
	if len(persisted) == 0 {
		return
	}
	queueItemIDs := make([]string, 0, len(persisted))
	userItemIDs := make([]string, 0, len(persisted))
	parts := make([]composerdraft.Part, 0, len(persisted))
	var owed []notTakenIn
	for _, p := range persisted {
		if p.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, p.QueueItemID)
		}
		userItemIDs = append(userItemIDs, p.UserItemID)
		row := store.Item{ThreadID: threadID, ID: p.UserItemID, Summary: p.Content, Meta: p.Meta}
		part, restore, itemOwed := a.agentPartsNotTakenIn(threadID, triage.UnconfirmedFlushItem{
			QueueItemID: p.QueueItemID, UserItemID: p.UserItemID, EnqueuedAt: p.EnqueuedAt, QuietItem: &row,
		}, draftPartFromUserItem(row), reason)
		owed = append(owed, itemOwed)
		if restore && !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	if err := a.restoreQueuedDraft(threadID, parts, store.QueueDraftRestore{
		QueueItemIDs: queueItemIDs, RetireItemIDs: userItemIDs,
	}); err != nil {
		// Nothing was written: every row and its review comments are as
		// they were, so the messages go back on the queue naming their rows.
		log.Printf("flush queue: restore draft after failed resend for %s: %v — requeueing the messages", threadID, err)
		a.requeueEagerPersistedFlushes(threadID, persisted)
		return
	}
	a.settleNotTakenIn(threadID, owed)
	a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       reason,
		QueueItemIDs: queueItemIDs,
		UserItemIDs:  userItemIDs,
	})
}

// requeueEagerPersistedFlushes returns eagerly-persisted interrupt
// rows to the flush queue. Since the composer-restore became the
// primary recovery for a failed Codex resend
// (restoreEagerPersistedFlushesToDraft), this is its fallback: rows
// whose restore failed keep a delivery vehicle here (round-13, CT13-2).
// Each item carries StaleUserItemID = its persisted row id so the
// redispatch's cleanup removes the eager row, and returns the review
// comments it claimed for the resend to claim again, before persisting
// again — the timeline never shows the message twice — and the payload is
// rebuilt from the row's usermessage meta so attachments and plan/diff
// provenance survive the round trip. Queue identity (QueueItemID,
// EnqueuedAt) is preserved so the Zone 1 entry reappears as the same
// message the user queued.
func (a *App) requeueEagerPersistedFlushes(threadID string, persisted []triage.EagerPersistedFlush) {
	if a.triage == nil || len(persisted) == 0 {
		return
	}
	for _, p := range persisted {
		payload, err := flushPayloadFromUserMeta(p.Meta)
		if err != nil {
			// Requeue anyway: the message text survives; only the
			// attachment/provenance refs on this corrupt meta are lost.
			log.Printf("flush queue: rebuild payload for requeue %s/%s: %v", threadID, p.UserItemID, err)
		}
		queueID := p.QueueItemID
		if queueID == "" {
			queueID = flushqueue.NewItemID()
		}
		a.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{
			ID:              queueID,
			Message:         p.Content,
			Payload:         payload,
			EnqueuedAt:      p.EnqueuedAt,
			StaleUserItemID: p.UserItemID,
		})
	}
	a.emitQueueStateChanged(threadID)
}

// flushPayloadFromUserMeta rebuilds a flush-queue payload from a
// persisted user row's usermessage meta (requeuePayloadFromMeta).
func flushPayloadFromUserMeta(meta string) (json.RawMessage, error) {
	if meta == "" {
		return nil, nil
	}
	if strings.TrimSpace(meta) == "null" {
		// json.Unmarshal accepts a literal null without touching the
		// struct — corrupt meta must fail loudly, not decode as empty.
		return nil, fmt.Errorf("user meta is JSON null")
	}
	var m usermessage.Meta
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return nil, err
	}
	return json.Marshal(requeuePayloadFromMeta(m))
}

// requeuePayloadFromMeta is the payload that sends a message again from the
// row an earlier dispatch of it landed as, the inverse of the
// resolveUserMessageEnvelope -> Marshal step that dispatch ran; its text is
// the row's summary. The summary renders the revision comments the message
// carried, with what only that dispatch had (a PR's hunk excerpts, every
// joined member's comments), so the resend sends it as it is and renders
// none of them again. Plan comments stay sent with that dispatch, so their
// ids drop out: carrying them would append the excerpts a second time
// (round-14, CT14-4). The diff review comments go back to drafts with the
// row (store.RetireUndeliveredUserRow); the resend claims again the ones the
// retire returns (addReturnedReviewComments), so a comment edited since
// stays a draft, and a restore of the requeued message gives the composer
// the text before them. The source refs and diff comment ids stay so
// provenance survives on the re-persisted row.
func requeuePayloadFromMeta(m usermessage.Meta) flushQueuePayload {
	payload := flushQueuePayload{
		SourceProposedPlan:         m.SourceProposedPlan,
		RevisionSourceProposedPlan: m.RevisionSourceProposedPlan,
		RevisionSourceDiffReview:   m.RevisionSourceDiffReview,
		// Composer provenance survives the requeue. It controls both AO
		// command expansion and whether an otherwise-unexpanded leading slash
		// reaches Claude's native command router.
		ExpandComposerCommands: m.ExpandComposerCommands,
		// The send id travels with the message, not with the row it happened
		// to be in: a requeued message that is dispatched again persists a
		// fresh row, and that row is the idempotency record from then on.
		// A joined row answers for several sends, so all of them travel.
		SendID:                   m.SendID,
		JoinedSendIDs:            m.JoinedSendIDs,
		TextBeforeReviewComments: m.TextBeforeReviewComments,
		// An agent's message requeued stays the agent's, so its next row
		// is an agent row and never the person's to take back.
		Origin:       m.Origin,
		OriginThread: m.OriginThread,
		OriginWake:   m.OriginWake,
	}
	if m.TextBeforeReviewComments != nil {
		payload.RevisionSourceDiffCommentIDs = m.RevisionSourceDiffCommentIDs
	}
	for _, att := range m.Attachments {
		if id := strings.TrimSpace(att.ID); id != "" {
			payload.AttachmentIDs = append(payload.AttachmentIDs, id)
		}
	}
	return payload
}

// addReturnedReviewComments adds to the payload of a message whose text
// renders its review comments (flushQueuePayload.TextBeforeReviewComments)
// the ones a retire of its row returned, for its dispatch to claim again,
// and reports whether it added any. A payload that renders its comments at
// dispatch reads them from the drafts itself.
func addReturnedReviewComments(payload *flushQueuePayload, returned []store.DiffReviewReturnedComments) bool {
	if payload.TextBeforeReviewComments == nil {
		return false
	}
	added := false
	for _, source := range returned {
		ref := store.DiffReviewSourceRef{ThreadID: source.Owner.ThreadID, PRKey: source.Owner.PRKey, Scope: source.Scope, SourceKey: source.SourceKey}
		index := slices.IndexFunc(payload.RenderedReviewComments, func(r flushqueue.RenderedReviewComments) bool {
			return r.Source.ThreadID == ref.ThreadID && r.Source.PRKey == ref.PRKey && r.Source.Scope == ref.Scope && r.Source.SourceKey == ref.SourceKey
		})
		if index < 0 {
			index = len(payload.RenderedReviewComments)
			payload.RenderedReviewComments = append(payload.RenderedReviewComments, flushqueue.RenderedReviewComments{Source: ref})
		}
		entry := &payload.RenderedReviewComments[index]
		for _, comment := range source.Comments {
			if slices.ContainsFunc(entry.Comments, func(c store.DiffReviewCommentSnapshot) bool { return c.ID == comment.ID }) {
				continue
			}
			entry.Comments = append(entry.Comments, comment)
			added = true
		}
	}
	return added
}

// rowTextWithoutReviewComments is the text of the user row whose summary
// and meta are given, without the diff review comments rendered into it
// (usermessage.Meta.TextWithoutReviewComments): what the composer gets back
// when the message is taken back undelivered and its comments return to
// drafts.
func rowTextWithoutReviewComments(summary, meta string) string {
	decoded, err := usermessage.FromItem(store.Item{Summary: summary, Meta: meta})
	if err != nil {
		return summary
	}
	return decoded.TextWithoutReviewComments(summary)
}

// attachmentIDsFromUserMeta extracts the attachment ids recorded on a
// persisted user row's usermessage meta, for re-resolving provider
// attachments on the Codex interrupt resend (round-14, CT14-2).
func attachmentIDsFromUserMeta(meta string) ([]string, error) {
	if meta == "" {
		return nil, nil
	}
	if strings.TrimSpace(meta) == "null" {
		return nil, fmt.Errorf("user meta is JSON null")
	}
	var m usermessage.Meta
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m.Attachments))
	for _, att := range m.Attachments {
		ids = append(ids, att.ID)
	}
	return ids, nil
}

func (a *App) restoredFlushDraftPart(threadID string, item triage.UnconfirmedFlushItem) composerdraft.Part {
	switch {
	case item.DeferredItem != nil:
		return draftPartFromUserItem(*item.DeferredItem)
	case item.QuietItem != nil:
		return draftPartFromUserItem(*item.QuietItem)
	default:
		return a.restoredDraftPartFromQueuedPayload(threadID, item)
	}
}

func dedupeUnconfirmedFlushItems(items []triage.UnconfirmedFlushItem) []triage.UnconfirmedFlushItem {
	out := make([]triage.UnconfirmedFlushItem, 0, len(items))
	indexByKey := make(map[string]int, len(items))
	for _, item := range items {
		key := item.QueueItemID
		if key == "" {
			key = item.UserItemID
		}
		if key == "" {
			out = append(out, item)
			continue
		}
		index, exists := indexByKey[key]
		if !exists {
			indexByKey[key] = len(out)
			out = append(out, item)
			continue
		}
		existing := out[index]
		settlement := triage.CombineFlushSettlements(existing.Settlement, item.Settlement)
		if unconfirmedFlushRestoreScore(item) > unconfirmedFlushRestoreScore(existing) {
			item.Settlement = settlement
			out[index] = item
		} else {
			existing.Settlement = settlement
			out[index] = existing
		}
	}
	return out
}

func unconfirmedFlushRestoreScore(item triage.UnconfirmedFlushItem) int {
	score := 0
	if item.UserItemID != "" {
		score++
	}
	if item.DeferredItem != nil {
		score += 2
	}
	if item.QuietItem != nil {
		score += 2
	}
	return score
}

// requeueUnconfirmedFlushItems puts messages a restore could not return to
// the composer back on the queue, durable first like any queued message. One
// that had reached the provider lost its durable row at that write, so it
// gets the row again under its queue id, which the dispatch or restore of the
// requeued message retires. One whose earlier row is still in the
// store (StaleUserItemID) keeps that row as its durable copy instead: the
// boot restore takes an unconfirmed row back with its text, so a queue row
// as well would put the message in the composer twice.
func (a *App) requeueUnconfirmedFlushItems(threadID string, items []triage.UnconfirmedFlushItem) {
	if a.triage == nil {
		return
	}
	for _, item := range items {
		queued, ok := queuedFlushItemFromUnconfirmed(item)
		if !ok {
			continue
		}
		if queued.StaleUserItemID == "" {
			var payload flushQueuePayload
			if len(queued.Payload) > 0 {
				if err := json.Unmarshal(queued.Payload, &payload); err != nil {
					log.Printf("flush queue: decode requeued payload %s/%s for its send id: %v", threadID, queued.ID, err)
				}
			}
			if err := a.store.EnsureFlushQueueItem(store.FlushQueueItem{
				ID:         queued.ID,
				ThreadID:   threadID,
				SendID:     payload.SendID,
				Message:    queued.Message,
				Payload:    queued.Payload,
				EnqueuedAt: queued.EnqueuedAt,
			}); err != nil {
				log.Printf("flush queue: persist requeued message %s/%s: %v", threadID, queued.ID, err)
				a.emitErrorToThread(threadID, "Saving a queued message failed. It stays queued but will not survive an app restart: "+err.Error())
			}
		}
		a.triage.RegisterQueueItem(threadID, queued)
	}
}

// queuedFlushItemFromUnconfirmed is the queue item that sends a message
// taken back from the provider again: its row's summary and the payload
// rebuilt from the row (requeuePayloadFromMeta), or the item's own message
// and payload when it has no row.
//
// The person's text goes back without the white space around it. A
// message that holds an agent's or the app's goes back as it was written:
// the facts it carries locate its body by offsets into that text
// (threadtools.MessageFacts), and a reader shows text they do not describe
// whole.
func queuedFlushItemFromUnconfirmed(item triage.UnconfirmedFlushItem) (triage.QueuedFlushItem, bool) {
	queueItemID := strings.TrimSpace(item.QueueItemID)
	message := item.Message
	payload := append(json.RawMessage(nil), item.Payload...)
	if !queuedPayloadAgentWritten(payload) {
		message = strings.TrimSpace(message)
	}
	if item.DeferredItem != nil {
		message, payload = queuePayloadFromUserItem(*item.DeferredItem, item.JoinedParts, payload)
	}
	if item.QuietItem != nil {
		message, payload = queuePayloadFromUserItem(*item.QuietItem, item.JoinedParts, payload)
	}
	if queueItemID == "" {
		queueItemID = flushqueue.NewItemID()
	}
	if message == "" && len(payload) == 0 {
		return triage.QueuedFlushItem{}, false
	}
	return triage.QueuedFlushItem{
		ID:              queueItemID,
		Message:         message,
		Payload:         payload,
		EnqueuedAt:      item.EnqueuedAt,
		StaleUserItemID: item.StaleUserItemID,
		Settlement:      item.Settlement,
	}, true
}

// queuePayloadFromUserItem is the text and requeue payload of a row's
// message (requeuePayloadFromMeta), or its text and fallback when the row's
// meta does not decode. parts are a joined row's members
// (agentmeta.JoinedPart), kept while they describe the requeued text. The
// text is the row's without the white space around it, unless the row holds
// an agent's message, which goes back as written (queuedFlushItemFromUnconfirmed).
func queuePayloadFromUserItem(item store.Item, parts []agentmeta.JoinedPart, fallback json.RawMessage) (string, json.RawMessage) {
	meta, err := usermessage.FromItem(item)
	if err != nil {
		return strings.TrimSpace(item.Summary), fallback
	}
	requeued := requeuePayloadFromMeta(meta)
	text := item.Summary
	if agentWritten(meta.Origin, parts) {
		if _, ok := agentmeta.SplitJoinedParts(text, parts); ok {
			requeued.JoinedParts = parts
		}
	} else {
		text = strings.TrimSpace(text)
		requeued.JoinedParts = usermessage.TrimJoinedParts(item.Summary, parts)
	}
	payload, err := json.Marshal(requeued)
	if err != nil {
		return text, fallback
	}
	return text, payload
}

// agentWritten reports whether a message of this origin, or any of the
// members parts records, is an agent's or the app's.
func agentWritten(origin string, parts []agentmeta.JoinedPart) bool {
	return agentmeta.IsAgentMessage(agentmeta.Meta{Origin: origin}) || slices.ContainsFunc(parts, agentmeta.JoinedPart.IsAgent)
}

// queuedPayloadAgentWritten is agentWritten of a queue item's payload. A
// payload that does not decode is the person's.
func queuedPayloadAgentWritten(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var payload flushQueuePayload
	if json.Unmarshal(raw, &payload) != nil {
		return false
	}
	return agentWritten(payload.Origin, payload.JoinedParts)
}

// draftPartFromUserItem is the composer part of a message taken back
// undelivered from its row: its text without the review comments, which
// the row's retire returned to drafts, so the composer and the drafts never
// both carry them.
func draftPartFromUserItem(item store.Item) composerdraft.Part {
	part, err := composerdraft.PartFromUserItem(item)
	if err != nil {
		log.Printf("flush queue: decode restored user item meta %s/%s: %v", item.ThreadID, item.ID, err)
		return composerdraft.Part{Content: item.Summary}
	}
	part.Content = rowTextWithoutReviewComments(item.Summary, item.Meta)
	return part
}

func (a *App) restoredDraftPartFromQueuedPayload(threadID string, item triage.UnconfirmedFlushItem) composerdraft.Part {
	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			log.Printf("flush queue: decode queued restore payload %s/%s: %v", threadID, item.QueueItemID, err)
			return composerdraft.Part{Content: item.Message}
		}
	}
	content := item.Message
	if payload.TextBeforeReviewComments != nil {
		// A requeued message renders its review comments, which the
		// restore returned to drafts.
		content = *payload.TextBeforeReviewComments
	}
	return composerdraft.Part{
		Content:            content,
		AttachmentIDs:      payload.AttachmentIDs,
		SourceProposedPlan: payload.SourceProposedPlan,
	}
}

// restoreDurableFlushQueueAtBoot puts every queued message the previous
// process did not deliver back into its thread's composer draft, and deletes
// the rows: the durable queue rows, and the rows reserved for messages the
// process wrote to the provider and never saw confirmed (a Claude message
// queued behind a running turn, whose quiet row is still marked
// pendingFlush). The provider may have read such a message before the crash
// or never; the composer is where one that is not proven delivered goes, so
// it never reads as sent.
//
// What the app wrote is not the person's to send and goes elsewhere: a loop
// or watch wake, an outside send, a worker's prompt or another thread's
// request is discarded (discardAgentWakeAtBoot), and an answer, a reminder or
// a remote completion goes back to its owner (giveBackToOwners), as does one
// the previous process handed a provider that never confirmed it
// (giveBackUnconfirmedAtBoot).
//
// It NEVER re-dispatches. A queued message was written against a turn that no
// longer exists, on a provider session that is gone, in a conversation the
// user has not looked at since; sending it hours later on their behalf is the
// one outcome nobody asked for. The composer is where a message that has not
// been delivered belongs, and it is exactly where a session death already
// puts one — same merge rule, ahead of whatever the composer itself holds,
// same `draft:updated` event.
//
// Runs at boot, after the store is open and before any session can start, so
// nothing else could still be holding a queue row. A marked user row is taken
// back only while it is residue, in its thread's last turn
// (store.ListUnconfirmedUserRows); one a later turn follows is history and
// stays where it is, and the store refuses to retire it.
func (a *App) restoreDurableFlushQueueAtBoot() {
	// Once the rows are settled, the answers, reminders and completions the
	// previous process handed a provider and never saw confirmed go back
	// too.
	defer a.giveBackUnconfirmedAtBoot()
	threadIDs, err := a.store.ListThreadsWithFlushQueueItems()
	if err != nil {
		a.bootPhaseFailed(fmt.Errorf("list threads with queued messages: %w", err))
		return
	}
	unconfirmed, err := a.store.ListUnconfirmedUserRows()
	if err != nil {
		// The queue rows are still restored; the unconfirmed rows wait for
		// the next boot.
		a.bootPhaseFailed(fmt.Errorf("list unconfirmed queued messages: %w", err))
	}
	unconfirmedByThread := make(map[string][]string)
	for _, row := range unconfirmed {
		if _, seen := unconfirmedByThread[row.ThreadID]; !seen {
			threadIDs = append(threadIDs, row.ThreadID)
		}
		unconfirmedByThread[row.ThreadID] = append(unconfirmedByThread[row.ThreadID], row.ItemID)
	}
	threadIDs = slices.Compact(slices.Sorted(slices.Values(threadIDs)))
	var errs []error
	for _, threadID := range threadIDs {
		if err := a.restoreThreadQueueAtBoot(threadID, unconfirmedByThread[threadID]); err != nil {
			errs = append(errs, fmt.Errorf("thread %s: %w", threadID, err))
		}
	}
	if len(errs) > 0 {
		a.bootPhaseFailed(fmt.Errorf("restore queued messages into the composer: %w", errors.Join(errs...)))
	}
}

// restoreThreadQueueAtBoot is the boot restore of one thread. The rows of
// unconfirmed messages come first: they were dispatched, so they were queued
// before anything still on the queue.
//
// A queue row that is a copy of a message an unconfirmed row carries (its
// send id is among the row's) gives no text of its own. A queue row outlives
// its message's provider write when the process dies between the write and
// its settlement, or when the message is an answer to an agent's question,
// which keeps its row until the echo. The unconfirmed row is the unit of the
// person's text: restoring some members from their queue copies and the rest
// from the row would put a member in the composer twice or lose it. A joined
// row that carries the app's messages splits by the parts its dispatch
// recorded (unconfirmedRowAtBoot), so only the person's text reaches the
// composer; one whose parts no longer describe its text is restored whole.
//
// A queue row whose message a row the provider confirmed carries was
// delivered before the process died: it leaves with the rest, without
// putting the message in the composer.
//
// The draft, the queue rows and the unconfirmed rows with the review
// comments they claimed are written in one transaction, so a failure or a
// crash leaves every message where it was for the next boot. An answer, a
// reminder or a completion goes back to its owner with its queue row, in a
// transaction of its own; one that fails keeps its row for the next boot.
// One whose echo the store recorded loses its row and stays delivered: the
// owner's withdrawal never takes back a confirmed delivery
// (store.WithdrawThreadWake, store.WithdrawRemoteCompletion,
// store.ReturnAsyncAnswer).
// Nothing else writes the thread's rows while the boot runs (no session has
// started and no client call is served before Start returns), so the rows
// read here are the rows that transaction changes.
func (a *App) restoreThreadQueueAtBoot(threadID string, unconfirmedIDs []string) error {
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		return fmt.Errorf("list queued messages: %w", err)
	}
	parts := make([]composerdraft.Part, 0, len(unconfirmedIDs)+len(rows))
	var retire []string
	carried := make(map[string]bool)
	for _, id := range unconfirmedIDs {
		item, found, err := a.store.GetThreadItem(threadID, id)
		if err != nil {
			return fmt.Errorf("read unconfirmed message %s: %w", id, err)
		}
		if !found {
			continue
		}
		retire = append(retire, id)
		for _, sendID := range userRowSendIDs(item) {
			carried[sendID] = true
		}
		if part, restore := a.unconfirmedRowAtBoot(threadID, item); restore && !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	ids := make([]string, 0, len(rows))
	var owned []ownedMessage
	for _, row := range rows {
		if ownedSendID(row.SendID) {
			owned = append(owned, ownedMessage{sendID: row.SendID, rowID: row.ID})
			continue
		}
		ids = append(ids, row.ID)
		if row.SendID != "" && carried[row.SendID] {
			continue
		}
		delivered, err := a.queuedSendDelivered(threadID, row.SendID)
		if err != nil {
			return fmt.Errorf("read the row of queued message %s: %w", row.ID, err)
		}
		if delivered {
			log.Printf("app: queued message %s of thread %s was delivered before the restart; dropping its queue row", row.ID, threadID)
			continue
		}
		// Recorded on the row before the queue row goes, so a failed
		// restore leaves both for the next boot to try again.
		if a.discardAgentWakeAtBoot(row) {
			continue
		}
		if part := draftPartFromQueueRow(row); !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	if owned = a.confirmEchoedAtBoot(threadID, owned); len(owned) > 0 {
		a.giveBackToOwners(threadID, owned)
	}
	if len(parts) == 0 && len(ids) == 0 && len(retire) == 0 {
		return nil
	}
	if err := a.restoreQueuedDraft(threadID, parts, store.QueueDraftRestore{
		QueueItemIDs: ids, RetireItemIDs: retire,
	}); err != nil {
		// Nothing was written: the next boot tries again, which is the
		// whole point of the message being durable.
		return fmt.Errorf("restore into the draft: %w", err)
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	if len(parts) > 0 {
		log.Printf("app: restored %d queued message(s) into the composer for thread %s", len(parts), threadID)
	}
	return nil
}

// unconfirmedRowAtBoot is what of an unconfirmed row the boot restores to
// the composer, false when nothing is. The app's messages it carries are
// handled as if each were still queued at quit (discardAgentWakeAtBoot),
// with the text its part recorded; its answers, reminders and completions
// are giveBackUnconfirmedAtBoot's. A row whose parts no longer describe its
// text cannot be told apart from the person's and is restored whole, as a
// session end restores one (agentPartsNotTakenIn).
func (a *App) unconfirmedRowAtBoot(threadID string, item store.Item) (composerdraft.Part, bool) {
	part := draftPartFromUserItem(item)
	members := unconfirmedMembersOf(triage.UnconfirmedFlushItem{QuietItem: &item})
	if !slices.ContainsFunc(members.ids, appWrittenSendID) {
		return part, true
	}
	messages, person, split := splitSessionEndParts(members, part.Content)
	if !split {
		log.Printf("flush queue: an unconfirmed message of %s does not split into the app messages it names; restoring all of it", threadID)
		return part, true
	}
	for _, id := range members.ids {
		if ownedSendID(id) || (agentAppPartRoot(id) == "" && threadRequestSendIDToken(id) == "") {
			continue
		}
		a.discardAgentWakeAtBoot(store.FlushQueueItem{ThreadID: threadID, SendID: id, Message: messages[id].Text()})
	}
	if person == nil {
		return composerdraft.Part{}, false
	}
	part.Content = *person
	return part, true
}

// queuedSendDelivered reports whether the provider confirmed the message a
// queue row with this send id is a copy of: a user row answers for the send
// id and carries the provider's id for it. A confirmation can land between
// the provider write and the settlement that deletes the queue row, so a
// crash there leaves both. A row the provider has not confirmed (a revealed
// interrupt row still awaiting its echo) is no proof of delivery.
func (a *App) queuedSendDelivered(threadID, sendID string) (bool, error) {
	itemID, err := a.deliveredSendRow(threadID, sendID)
	return itemID != "", err
}

// deliveredSendRow is the row that proves the provider confirmed the message
// sendID (queuedSendDelivered), empty when none does.
func (a *App) deliveredSendRow(threadID, sendID string) (string, error) {
	if sendID == "" {
		return "", nil
	}
	item, found, err := a.store.FindUserTextItemBySendID(threadID, sendID)
	if err != nil || !found || usermessage.ReadProviderItemID(item.Meta) == "" {
		return "", err
	}
	return item.ID, nil
}

// confirmEchoedAtBoot writes the confirmation of each answer, reminder,
// completion or spawner's answer whose echo a row of the last run proves
// (deliveredSendRow), before the boot gives them back: the last run can die
// between the echo's row write and its confirmation, or after a
// confirmation write that failed, which only memory kept. The row is the
// provider's word that the agent took the message in, and the owner's
// withdrawal then only drops a queue row that outlived it
// (giveBackToOwners); a confirmation whose write fails again is kept as an
// echo, which giveBackToOwners skips. A message whose row cannot be read is
// left out, with its queue row or its unconfirmed mark, for the next boot.
func (a *App) confirmEchoedAtBoot(threadID string, messages []ownedMessage) []ownedMessage {
	now := time.Now().UnixMilli()
	return slices.DeleteFunc(messages, func(m ownedMessage) bool {
		itemID, err := a.deliveredSendRow(threadID, m.sendID)
		if err != nil {
			logThreadRequestSweep("read the echo of "+m.sendID+" in "+threadID+" at boot; left for the next boot", err)
			return true
		}
		if itemID != "" {
			log.Printf("app: %s of thread %s was taken in before the restart; recording its confirmation", m.sendID, threadID)
			if err := a.confirmOwnedEcho(m.sendID, ownedEcho{threadID: threadID, itemID: itemID, at: now}); err != nil {
				log.Printf("thread tools: confirm %s in %s at boot, kept to try again: %v", m.sendID, threadID, err)
			}
		}
		return false
	})
}

// userRowSendIDs is every send id a user row answers for: a joined row's
// members, or its one send id.
func userRowSendIDs(item store.Item) []string {
	meta, err := usermessage.FromItem(item)
	if err != nil {
		return nil
	}
	if len(meta.JoinedSendIDs) > 0 {
		return meta.JoinedSendIDs
	}
	if meta.SendID != "" {
		return []string{meta.SendID}
	}
	return nil
}

// emptyDraftPart reports whether a restored part carries nothing for the
// composer to hold.
func emptyDraftPart(part composerdraft.Part) bool {
	return strings.TrimSpace(part.Content) == "" && len(part.AttachmentIDs) == 0 && part.SourceProposedPlan == nil
}

// draftPartFromQueueRow projects one durable queue row into a composer part.
// A payload that no longer decodes costs the attachment and plan references
// and NOT the message: the text is the part a person typed, and it is stored
// in its own column precisely so nothing about the payload can take it away.
func draftPartFromQueueRow(row store.FlushQueueItem) composerdraft.Part {
	var payload flushQueuePayload
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			log.Printf("flush queue: decode queued row payload %s/%s: %v", row.ThreadID, row.ID, err)
			return composerdraft.Part{Content: row.Message}
		}
	}
	return composerdraft.Part{
		Content:            row.Message,
		AttachmentIDs:      payload.AttachmentIDs,
		SourceProposedPlan: payload.SourceProposedPlan,
	}
}
