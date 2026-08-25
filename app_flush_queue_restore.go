package main

import (
	"encoding/json"
	"fmt"
	"log"
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
// provider read loop. The guard runs after the thread action lock is held and
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
	sort.SliceStable(drained, func(i, j int) bool {
		left := drained[i].EnqueuedAt
		right := drained[j].EnqueuedAt
		if left == 0 || right == 0 {
			return left != 0
		}
		return left < right
	})
	if len(drained) == 0 {
		return nil
	}

	// Quiet-row cleanup runs BEFORE the draft restore: a row whose
	// cleanup fails must stay OUT of the restored draft — sending that
	// draft after the store recovers would mint a second flush row while
	// the original unconsumed row still sits in the timeline (round-10,
	// R10-5). A failed-cleanup item is requeued instead, carrying the
	// stale row's id so the redispatch retries the cleanup before
	// persisting a fresh row (round-11, R11-1): queue item plus
	// persisted quiet row is exactly the pre-drain state. Requeued items
	// caught still-queued by a LATER death re-enter here through
	// StaleUserItemID and get the same retry.
	restorable := make([]triage.UnconfirmedFlushItem, 0, len(drained))
	var cleanupFailed []triage.UnconfirmedFlushItem
	for _, item := range drained {
		staleID := item.StaleUserItemID
		if item.QuietItem != nil {
			staleID = item.QuietItem.ID
		}
		if staleID == "" {
			restorable = append(restorable, item)
			continue
		}
		if err := a.cleanupStaleFlushRow(threadID, staleID); err != nil {
			log.Printf("flush queue: cleanup unconfirmed quiet row %s/%s failed — requeueing the message instead of restoring it to the draft: %v", threadID, staleID, err)
			item.StaleUserItemID = staleID
			cleanupFailed = append(cleanupFailed, item)
			continue
		}
		item.StaleUserItemID = ""
		restorable = append(restorable, item)
	}
	requeued := make([]triage.UnconfirmedFlushItem, 0, len(cleanupFailed))
	if len(cleanupFailed) > 0 {
		a.requeueUnconfirmedFlushItems(threadID, cleanupFailed)
		requeued = append(requeued, cleanupFailed...)
	}

	// queue_restored reports only the items actually restored to the
	// draft. A cleanup-failed item is REQUEUED — it is still queued, and
	// listing its ids here would make the frontend remove the Zone 2
	// entry that the queue_state_changed emission re-adds (round-12,
	// D12-3).
	queueItemIDs := make([]string, 0, len(restorable))
	userItemIDs := make([]string, 0, len(restorable))
	for _, item := range restorable {
		if item.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, item.QueueItemID)
		}
		if item.UserItemID != "" {
			userItemIDs = append(userItemIDs, item.UserItemID)
		}
	}

	parts := make([]composerdraft.Part, 0, len(restorable))
	settlements := make([]*triage.FlushSettlement, 0, len(restorable))
	for _, item := range restorable {
		part := a.restoredFlushDraftPart(threadID, item)
		if strings.TrimSpace(part.Content) != "" || len(part.AttachmentIDs) > 0 || part.SourceProposedPlan != nil {
			parts = append(parts, part)
			settlements = append(settlements, item.Settlement)
		}
	}
	if len(parts) > 0 {
		if _, err := a.mergeAndUpsertThreadDraft(threadID, parts); err != nil {
			log.Printf("flush queue: restore draft after session death for %s: %v", threadID, err)
			// The quiet rows behind these items are already deleted, so
			// the requeued entries carry deferred semantics — the retained
			// copies supply message and payload; nothing references a
			// store row (R6-6 stays intact).
			a.requeueUnconfirmedFlushItems(threadID, restorable)
			a.emitQueueStateChanged(threadID)
			return append(requeued, restorable...)
		}
	}
	for _, settlement := range settlements {
		settlement.Settle()
	}

	a.emitQueueStateChanged(threadID)
	a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       "session_died",
		QueueItemIDs: queueItemIDs,
		UserItemIDs:  userItemIDs,
	})
	return requeued
}

// cleanupStaleFlushRow removes a quietly-persisted flush row whose
// message is being restored or redispatched. The FK cascade on
// message_anchors takes any anchor row with it. A row already gone
// (an earlier retry finished the job) is success.
func (a *App) cleanupStaleFlushRow(threadID, userItemID string) error {
	if _, found, err := a.store.GetThreadItem(threadID, userItemID); err != nil {
		return fmt.Errorf("lookup stale flush row: %w", err)
	} else if !found {
		return nil
	}
	return a.store.DeleteThreadItem(threadID, userItemID)
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
// A row whose store cleanup fails must stay OUT of the restored draft
// — sending that draft would mint a second row while the original
// still sits in the timeline (same posture as the session-death
// restore, round-10 R10-5). Those items fall back to the stale-marker
// requeue so the redispatch retries the cleanup. A failed draft write
// likewise falls back to the requeue; by then the rows are already
// deleted, so the stale markers resolve as no-ops and the emitted row
// removal keeps the frontend timeline in sync with the store.
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
	queueRestoredReasonResendFailed = "resend_failed"
	queueRestoredReasonNeverQueued  = "provider_queue_never_taken"
	queueRestoredReasonPurgeAborted = "provider_queue_purge_aborted"
)

// restoreEagerPersistedFlushesForReason is the body, parameterised by the
// reason that reaches the frontend.
func (a *App) restoreEagerPersistedFlushesForReason(
	threadID string, persisted []triage.EagerPersistedFlush, reason string,
) {
	if len(persisted) == 0 {
		return
	}
	restorable := make([]triage.EagerPersistedFlush, 0, len(persisted))
	var cleanupFailed []triage.EagerPersistedFlush
	for _, p := range persisted {
		if err := a.cleanupStaleFlushRow(threadID, p.UserItemID); err != nil {
			log.Printf("flush queue: cleanup eager flush row %s/%s failed — requeueing the message instead of restoring it to the draft: %v", threadID, p.UserItemID, err)
			cleanupFailed = append(cleanupFailed, p)
			continue
		}
		restorable = append(restorable, p)
	}
	if len(cleanupFailed) > 0 {
		a.requeueEagerPersistedFlushes(threadID, cleanupFailed)
	}
	if len(restorable) == 0 {
		return
	}

	// Restorable-only id lists, same rule as the session-death restore
	// (round-12, D12-3): a requeued item keeps its timeline row and its
	// Zone 1 entry, so listing it here would make the frontend remove
	// state that still exists.
	queueItemIDs := make([]string, 0, len(restorable))
	userItemIDs := make([]string, 0, len(restorable))
	parts := make([]composerdraft.Part, 0, len(restorable))
	for _, p := range restorable {
		if p.QueueItemID != "" {
			queueItemIDs = append(queueItemIDs, p.QueueItemID)
		}
		userItemIDs = append(userItemIDs, p.UserItemID)
		part := draftPartFromUserItem(store.Item{ThreadID: threadID, ID: p.UserItemID, Summary: p.Content, Meta: p.Meta})
		if strings.TrimSpace(part.Content) != "" || len(part.AttachmentIDs) > 0 || part.SourceProposedPlan != nil {
			parts = append(parts, part)
		}
	}
	if len(parts) > 0 {
		if _, err := a.mergeAndUpsertThreadDraft(threadID, parts); err != nil {
			log.Printf("flush queue: restore draft after failed resend for %s: %v", threadID, err)
			// The rows are already deleted — emit the removal so the
			// frontend drops them, then requeue so the messages keep a
			// delivery vehicle (their redispatch persists fresh rows).
			a.emit(eventchan.ProviderQueueRestored, QueueRestoredEvent{
				ThreadID:    threadID,
				Reason:      reason,
				UserItemIDs: userItemIDs,
			})
			a.requeueEagerPersistedFlushes(threadID, restorable)
			return
		}
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
// whose cleanup or draft write failed keep a delivery vehicle here
// (round-13, CT13-2). Each item carries StaleUserItemID = its
// persisted row id so the redispatch's cleanup removes the eager row
// before persisting again — the timeline never shows the message twice
// — and the payload is rebuilt from the row's usermessage meta so
// attachments and plan/diff provenance survive the round trip. Queue
// identity (QueueItemID, EnqueuedAt) is preserved so the Zone 1 entry
// reappears as the same message the user queued.
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
// persisted user row's usermessage meta — the inverse of the
// resolveUserMessageEnvelope → Marshal step the original dispatch ran.
//
// The revision COMMENT ID lists are deliberately dropped: the original
// resolution already appended their excerpts into the persisted content
// this requeue re-sends, and carrying the IDs would make the redispatch
// append them a second time (round-14, CT14-4). The source refs stay so
// provenance survives on the re-persisted row.
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
	payload := flushQueuePayload{
		SourceProposedPlan:         m.SourceProposedPlan,
		RevisionSourceProposedPlan: m.RevisionSourceProposedPlan,
		RevisionSourceDiffReview:   m.RevisionSourceDiffReview,
		// Composer provenance survives the requeue. It controls both AO
		// command expansion and whether an otherwise-unexpanded leading slash
		// reaches Claude's native command router.
		ExpandComposerCommands: m.ExpandComposerCommands,
	}
	for _, att := range m.Attachments {
		payload.AttachmentIDs = append(payload.AttachmentIDs, att.ID)
	}
	return json.Marshal(payload)
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

func (a *App) requeueUnconfirmedFlushItems(threadID string, items []triage.UnconfirmedFlushItem) {
	if a.triage == nil {
		return
	}
	for _, item := range items {
		queued, ok := queuedFlushItemFromUnconfirmed(item)
		if !ok {
			continue
		}
		a.triage.RegisterQueueItem(threadID, queued)
	}
}

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

func queuePayloadFromUserItem(item store.Item, fallback json.RawMessage) json.RawMessage {
	meta, err := usermessage.FromItem(item)
	if err != nil {
		return fallback
	}
	attachmentIDs := make([]string, 0, len(meta.Attachments))
	for _, attachment := range meta.Attachments {
		id := strings.TrimSpace(attachment.ID)
		if id != "" {
			attachmentIDs = append(attachmentIDs, id)
		}
	}
	payload, err := json.Marshal(flushQueuePayload{
		AttachmentIDs:                attachmentIDs,
		SourceProposedPlan:           meta.SourceProposedPlan,
		RevisionSourceProposedPlan:   meta.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     meta.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     meta.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: meta.RevisionSourceDiffCommentIDs,
		ExpandComposerCommands:       meta.ExpandComposerCommands,
	})
	if err != nil {
		return fallback
	}
	return payload
}

func draftPartFromUserItem(item store.Item) composerdraft.Part {
	part, err := composerdraft.PartFromUserItem(item)
	if err != nil {
		log.Printf("flush queue: decode restored user item meta %s/%s: %v", item.ThreadID, item.ID, err)
		return composerdraft.Part{Content: item.Summary}
	}
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
	return composerdraft.Part{
		Content:            item.Message,
		AttachmentIDs:      payload.AttachmentIDs,
		SourceProposedPlan: payload.SourceProposedPlan,
	}
}
