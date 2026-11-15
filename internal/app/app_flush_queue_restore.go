package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"

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
	if a.triage == nil {
		return nil
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
		return nil
	}
	return a.restoreFlushItemsToDraftLocked(threadID, drained, queueRestoredReasonSessionDied)
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
func (a *App) restoreFlushItemsToDraftLocked(threadID string, drained []triage.UnconfirmedFlushItem, reason string) []triage.UnconfirmedFlushItem {
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
	for i, item := range drained {
		staleID := item.StaleUserItemID
		if item.QuietItem != nil {
			staleID = item.QuietItem.ID
		}
		if item.DeferredItem != nil && staleID == "" {
			// A deferred message has no row, but its review drafts were
			// claimed for the row it would have landed as.
			staleID = item.DeferredItem.ID
		}
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
		part := a.restoredFlushDraftPart(threadID, item)
		if strings.TrimSpace(part.Content) != "" || len(part.AttachmentIDs) > 0 || part.SourceProposedPlan != nil {
			parts = append(parts, part)
			settlements = append(settlements, item.Settlement)
		}
	}
	if len(parts) > 0 || len(retire) > 0 || len(queueItemIDs) > 0 {
		if err := a.restoreQueuedDraft(threadID, parts, queueItemIDs, retire); err != nil {
			log.Printf("flush queue: restore draft (%s) for %s: %v — requeueing the messages", reason, threadID, err)
			a.emitWireErrorToThread(threadID, "Could not restore queued messages: "+err.Error())
			a.requeueUnconfirmedFlushItems(threadID, drained)
			a.emitQueueStateChanged(threadID)
			return drained
		}
	}
	for _, settlement := range settlements {
		settlement.Settle()
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})

	a.emitQueueStateChanged(threadID)
	a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       reason,
		QueueItemIDs: queueItemIDs,
		UserItemIDs:  userItemIDs,
	})
	return nil
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
)

// restoreEagerPersistedFlushesForReason is the body, parameterised by the
// reason that reaches the frontend.
func (a *App) restoreEagerPersistedFlushesForReason(
	threadID string, persisted []triage.EagerPersistedFlush, reason string,
) {
	if len(persisted) == 0 {
		return
	}
	queueItemIDs := make([]string, 0, len(persisted))
	userItemIDs := make([]string, 0, len(persisted))
	parts := make([]composerdraft.Part, 0, len(persisted))
	for _, p := range persisted {
		if p.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, p.QueueItemID)
		}
		userItemIDs = append(userItemIDs, p.UserItemID)
		part := draftPartFromUserItem(store.Item{ThreadID: threadID, ID: p.UserItemID, Summary: p.Content, Meta: p.Meta})
		if strings.TrimSpace(part.Content) != "" || len(part.AttachmentIDs) > 0 || part.SourceProposedPlan != nil {
			parts = append(parts, part)
		}
	}
	if err := a.restoreQueuedDraft(threadID, parts, queueItemIDs, userItemIDs); err != nil {
		// Nothing was written: every row and its review comments are as
		// they were, so the messages go back on the queue naming their rows.
		log.Printf("flush queue: restore draft after failed resend for %s: %v — requeueing the messages", threadID, err)
		a.requeueEagerPersistedFlushes(threadID, persisted)
		return
	}
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
// gets the row again, and a settlement that retires it when the requeued
// message is dispatched or restored. One whose earlier row is still in the
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
			queued.Settlement = triage.CombineFlushSettlements(queued.Settlement, a.flushQueueSettlement(threadID, queued.ID, nil))
		}
		a.triage.RegisterQueueItem(threadID, queued)
	}
}

// queuedFlushItemFromUnconfirmed is the queue item that sends a message
// taken back from the provider again: its row's summary and the payload
// rebuilt from the row (requeuePayloadFromMeta), or the item's own message
// and payload when it has no row.
func queuedFlushItemFromUnconfirmed(item triage.UnconfirmedFlushItem) (triage.QueuedFlushItem, bool) {
	queueItemID := strings.TrimSpace(item.QueueItemID)
	message := strings.TrimSpace(item.Message)
	payload := append(json.RawMessage(nil), item.Payload...)
	if item.DeferredItem != nil {
		message = strings.TrimSpace(item.DeferredItem.Summary)
		payload = queuePayloadFromUserItem(*item.DeferredItem, payload)
	}
	if item.QuietItem != nil {
		message = strings.TrimSpace(item.QuietItem.Summary)
		payload = queuePayloadFromUserItem(*item.QuietItem, payload)
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

// queuePayloadFromUserItem is the requeue payload of a row's message
// (requeuePayloadFromMeta), or fallback when the row's meta does not decode.
func queuePayloadFromUserItem(item store.Item, fallback json.RawMessage) json.RawMessage {
	meta, err := usermessage.FromItem(item)
	if err != nil {
		return fallback
	}
	payload, err := json.Marshal(requeuePayloadFromMeta(meta))
	if err != nil {
		return fallback
	}
	return payload
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
// It NEVER re-dispatches. A queued message was written against a turn that no
// longer exists, on a provider session that is gone, in a conversation the
// user has not looked at since; sending it hours later on their behalf is the
// one outcome nobody asked for. The composer is where a message that has not
// been delivered belongs, and it is exactly where a session death already
// puts one — same merge rule, ahead of whatever the composer itself holds,
// same `draft:updated` event.
//
// Runs at boot, after the store is open and before any session can start, so
// every remaining row is provably residue: nothing else could still be
// holding one.
func (a *App) restoreDurableFlushQueueAtBoot() {
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
// before anything still on the queue. A queue row that outlived its message's
// provider write (an answer to an agent's question keeps it until the echo)
// is the same message as that write's row, so the row lends no second copy.
//
// The draft, the queue rows and the unconfirmed rows with the review
// comments they claimed are written in one transaction, so a failure or a
// crash leaves every message where it was for the next boot.
func (a *App) restoreThreadQueueAtBoot(threadID string, unconfirmedIDs []string) error {
	rows, err := a.store.ListFlushQueueItems(threadID)
	if err != nil {
		return fmt.Errorf("list queued messages: %w", err)
	}
	queuedSends := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.SendID != "" {
			queuedSends[row.SendID] = true
		}
	}
	parts := make([]composerdraft.Part, 0, len(unconfirmedIDs)+len(rows))
	var retire []string
	var restoredSends []string
	for _, id := range unconfirmedIDs {
		item, found, err := a.store.GetThreadItem(threadID, id)
		if err != nil {
			return fmt.Errorf("read unconfirmed message %s: %w", id, err)
		}
		if !found {
			continue
		}
		retire = append(retire, id)
		sends := userRowSendIDs(item)
		if slices.ContainsFunc(sends, func(sendID string) bool { return queuedSends[sendID] }) {
			continue
		}
		restoredSends = append(restoredSends, sends...)
		if part := draftPartFromUserItem(item); !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
		restoredSends = append(restoredSends, row.SendID)
		if part := draftPartFromQueueRow(row); !emptyDraftPart(part) {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 && len(ids) == 0 && len(retire) == 0 {
		return nil
	}
	if err := a.restoreQueuedDraft(threadID, parts, ids, retire); err != nil {
		// Nothing was written: the next boot tries again, which is the
		// whole point of the message being durable.
		return fmt.Errorf("restore into the draft: %w", err)
	}
	for _, sendID := range restoredSends {
		noteThreadWakeRestoredToDraft(a, sendID)
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	log.Printf("app: restored %d queued message(s) into the composer for thread %s", len(parts), threadID)
	return nil
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

// noteThreadWakeRestoredToDraft corrects a thread request whose wake this
// restore put in the composer instead of sending. The request was marked
// delivered when the message was queued, which was true then and is still
// true now: the answer reached the thread. Only the route changed, and a
// reader that cannot tell a queued wake from one sitting in a draft would
// report an answer as on its way when it is waiting on the person.
func noteThreadWakeRestoredToDraft(a *App, sendID string) {
	token, late, ok := threadWakeTokenFromSendID(sendID)
	if !ok {
		return
	}
	if _, err := a.store.MarkThreadRequestDeliveredAsDraft(token, late); err != nil {
		log.Printf("thread tools: mark request %s restored to draft: %v", token, err)
	}
}

// threadWakeTokenFromSendID recovers the request a queued wake belongs to.
// The two prefixes are disjoint, so either order reads the same send id; what
// the token check rejects is a bare prefix, which names no request.
func threadWakeTokenFromSendID(sendID string) (token string, late bool, ok bool) {
	if rest, found := strings.CutPrefix(sendID, threadWakeLateSendID("")); found {
		return rest, true, rest != ""
	}
	if rest, found := strings.CutPrefix(sendID, threadWakeSendID("")); found {
		return rest, false, rest != ""
	}
	return "", false, false
}
