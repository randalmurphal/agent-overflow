package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/composerdraft"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"agent-overflow/internal/usermessage"
	"github.com/google/uuid"
)

// QueuedItem is the wire-side projection of a triage QueuedFlushItem.
// The canonical declaration (plus the JSON tag set) lives in
// internal/flushqueue alongside the projection logic; main keeps the
// alias so the Wails binding generator still emits it under the
// agent-overflow namespace the frontend imports from.
type QueuedItem = flushqueue.QueuedItem

// QueueStateChangedEvent is the payload of `provider:queue_state_changed`,
// emitted whenever the per-thread queue mutates (register / drained /
// dropped via session teardown). Carrying the full
// post-mutation snapshot — rather than a delta — lets the frontend
// reconcile state without keeping its own ordering log; the SvelteMap
// store assigns the `Items` slice to its per-thread entry and the
// reactive bindings update.
type QueueStateChangedEvent struct {
	ThreadID string       `json:"threadId"`
	Items    []QueuedItem `json:"items"`
}

type flushDispatchBatch struct {
	items      []triage.QueuedFlushItem
	generation uint64
}

// QueueFlushedEvent is emitted by `dispatchFlush` at the start of a
// successful per-item provider dispatch. It carries the
// (queueItemId → userItemId) mapping the frontend uses to keep the
// message in the above-composer pending area while waiting for the
// provider-visible wire echo.
//
// The userItemId is the deterministic AO row id the dispatcher will
// allocate (`user:<turnIndex>:flush:<n>`). The frontend matches the
// id against incoming `provider:item_event` upserts: when the
// corresponding row's Meta carries a `provider_item_id`, the wire echo
// has arrived and the Zone 2 marker can drop.
type QueueFlushedEvent struct {
	ThreadID string             `json:"threadId"`
	Items    []QueueFlushedItem `json:"items"`
}

// QueueFlushedItem is one entry inside a QueueFlushedEvent. Carries
// the original frontend-allocated queueItemId, the backend-allocated
// userItemId (deterministic row id), and the message text so the
// frontend's Zone 2 overlay can render without re-reading the
// timeline.
type QueueFlushedItem struct {
	QueueItemID string `json:"queueItemId"`
	UserItemID  string `json:"userItemId"`
	Message     string `json:"message"`
}

type QueueRestoredEvent struct {
	ThreadID     string   `json:"threadId"`
	Reason       string   `json:"reason"`
	QueueItemIDs []string `json:"queueItemIds"`
	UserItemIDs  []string `json:"userItemIds"`
}

func (a *App) configureTriageQueueCallbacks() {
	if a.triage == nil {
		return
	}
	a.triage.SetFlushDispatcher(a.enqueueFlushDispatch)
	a.triage.SetFlushUserTextConfirmedHook(func(threadID string, item store.Item) {
		a.recordMessageAnchor(item)
	})
}

// newTriageRouter constructs the triage router with every App-owned
// observer wired. ALL construction sites must route through this —
// a router built bare would silently drop the streaming observers.
func (a *App) newTriageRouter(st *store.Store) *triage.Router {
	r := triage.NewRouter(st, a.emitWithReplay())
	r.SetAssistantTextStreamObserver(a.observeAssistantTextStream)
	r.SetDiffPayloadObserver(a.observeDiffPayloadPersisted)
	r.SetCodeSpanEnricher(a.buildPersistedCodeSpans)
	return r
}

// ensureTriageRouter lazily constructs the router for the defensive
// pre-Startup entry paths (production wires it in initSubsystems).
func (a *App) ensureTriageRouter() {
	if a.triage != nil {
		return
	}
	a.triage = a.newTriageRouter(a.store)
	a.configureTriageQueueCallbacks()
}

func (a *App) enqueueFlushDispatch(threadID string, items []triage.QueuedFlushItem) {
	if threadID == "" || len(items) == 0 {
		return
	}
	batch := make([]triage.QueuedFlushItem, len(items))
	copy(batch, items)

	a.flushDispatchMu.Lock()
	a.ensureFlushDispatchMapsLocked()
	generation := a.flushDispatchGeneration[threadID]
	a.flushDispatchQueues[threadID] = append(a.flushDispatchQueues[threadID], flushDispatchBatch{
		items:      batch,
		generation: generation,
	})
	a.flushDispatchInflightItems[threadID] += len(batch)
	if a.flushDispatchRunning[threadID] {
		a.flushDispatchMu.Unlock()
		return
	}
	a.flushDispatchRunning[threadID] = true
	a.flushDispatchWG.Add(1)
	a.flushDispatchMu.Unlock()

	go a.runFlushDispatchWorker(threadID)
}

func (a *App) runFlushDispatchWorker(threadID string) {
	defer a.flushDispatchWG.Done()
	for {
		a.flushDispatchMu.Lock()
		queue := a.flushDispatchQueues[threadID]
		if len(queue) == 0 {
			delete(a.flushDispatchQueues, threadID)
			delete(a.flushDispatchRunning, threadID)
			a.flushDispatchMu.Unlock()
			return
		}
		batch := queue[0]
		if len(queue) == 1 {
			delete(a.flushDispatchQueues, threadID)
		} else {
			a.flushDispatchQueues[threadID] = queue[1:]
		}
		a.flushDispatchCurrent[threadID] = batch
		a.flushDispatchMu.Unlock()

		a.dispatchFlushWithGeneration(threadID, batch.items, batch.generation)

		a.flushDispatchMu.Lock()
		delete(a.flushDispatchCurrent, threadID)
		if a.flushDispatchGeneration[threadID] == batch.generation {
			a.flushDispatchInflightItems[threadID] -= len(batch.items)
			if a.flushDispatchInflightItems[threadID] <= 0 {
				delete(a.flushDispatchInflightItems, threadID)
			}
		}
		a.flushDispatchMu.Unlock()
	}
}

func (a *App) flushDispatchItemCount(threadID string) int {
	a.flushDispatchMu.Lock()
	defer a.flushDispatchMu.Unlock()
	return a.flushDispatchInflightItems[threadID]
}

func (a *App) ensureFlushDispatchMapsLocked() {
	if a.flushDispatchQueues == nil {
		a.flushDispatchQueues = make(map[string][]flushDispatchBatch)
	}
	if a.flushDispatchCurrent == nil {
		a.flushDispatchCurrent = make(map[string]flushDispatchBatch)
	}
	if a.flushDispatchRunning == nil {
		a.flushDispatchRunning = make(map[string]bool)
	}
	if a.flushDispatchInflightItems == nil {
		a.flushDispatchInflightItems = make(map[string]int)
	}
	if a.flushDispatchGeneration == nil {
		a.flushDispatchGeneration = make(map[string]uint64)
	}
}

func (a *App) clearFlushDispatchForRollback(threadID string) {
	a.flushDispatchMu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatchGeneration[threadID]++
	delete(a.flushDispatchQueues, threadID)
	delete(a.flushDispatchCurrent, threadID)
	delete(a.flushDispatchInflightItems, threadID)
	a.flushDispatchMu.Unlock()
}

func (a *App) drainFlushDispatchForSessionEnd(threadID string) []triage.QueuedFlushItem {
	a.flushDispatchMu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatchGeneration[threadID]++
	var drained []triage.QueuedFlushItem
	if current, ok := a.flushDispatchCurrent[threadID]; ok {
		drained = append(drained, current.items...)
		delete(a.flushDispatchCurrent, threadID)
	}
	for _, batch := range a.flushDispatchQueues[threadID] {
		drained = append(drained, batch.items...)
	}
	delete(a.flushDispatchQueues, threadID)
	delete(a.flushDispatchInflightItems, threadID)
	a.flushDispatchMu.Unlock()
	return drained
}

func (a *App) currentFlushDispatchGeneration(threadID string) uint64 {
	a.flushDispatchMu.Lock()
	defer a.flushDispatchMu.Unlock()
	if a.flushDispatchGeneration == nil {
		return 0
	}
	return a.flushDispatchGeneration[threadID]
}

func (a *App) isFlushDispatchGenerationCurrent(threadID string, generation uint64) bool {
	return a.currentFlushDispatchGeneration(threadID) == generation
}

func (a *App) drainFlushDispatch(ctx context.Context, timeout time.Duration) error {
	if a == nil {
		return nil
	}
	drainCtx, cancel := contextWithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		a.flushDispatchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-drainCtx.Done():
		return drainCtx.Err()
	}
}

// restoreUnconfirmedQueueOnSessionDeath drains queued and unconfirmed
// flush state after a session death, restoring what it can to the
// composer draft. Items it REQUEUES instead (failed stale-row cleanup,
// failed draft restore) are also returned: a caller that runs a triage
// CleanupThread afterwards wipes the queue those items just re-entered
// and must re-register them once the cleanup has run (round-13,
// D13-1). Callers with no subsequent cleanup may ignore the return.
func (a *App) restoreUnconfirmedQueueOnSessionDeath(threadID string) []triage.UnconfirmedFlushItem {
	if a.triage == nil || strings.TrimSpace(threadID) == "" {
		return nil
	}

	dispatchItems := a.drainFlushDispatchForSessionEnd(threadID)

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	drained := a.triage.DrainUnconfirmedFlushItems(threadID)
	for _, item := range dispatchItems {
		payload := append(json.RawMessage(nil), item.Payload...)
		drained = append(drained, triage.UnconfirmedFlushItem{
			QueueItemID:     item.ID,
			Message:         item.Message,
			Payload:         payload,
			EnqueuedAt:      item.EnqueuedAt,
			StaleUserItemID: item.StaleUserItemID,
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
	for _, item := range restorable {
		part := a.restoredFlushDraftPart(threadID, item)
		if strings.TrimSpace(part.Content) != "" || len(part.AttachmentIDs) > 0 || part.SourceProposedPlan != nil {
			parts = append(parts, part)
		}
	}
	if len(parts) > 0 {
		if err := a.restoreFlushDraft(threadID, parts); err != nil {
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

	a.emitQueueStateChanged(threadID)
	a.emit("provider:queue_restored", QueueRestoredEvent{
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
		if err := a.restoreFlushDraft(threadID, parts); err != nil {
			log.Printf("flush queue: restore draft after failed resend for %s: %v", threadID, err)
			// The rows are already deleted — emit the removal so the
			// frontend drops them, then requeue so the messages keep a
			// delivery vehicle (their redispatch persists fresh rows).
			a.emit("provider:queue_restored", QueueRestoredEvent{
				ThreadID:    threadID,
				Reason:      "resend_failed",
				UserItemIDs: userItemIDs,
			})
			a.requeueEagerPersistedFlushes(threadID, restorable)
			return
		}
	}
	a.emit("provider:queue_restored", QueueRestoredEvent{
		ThreadID:     threadID,
		Reason:       "resend_failed",
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
		if unconfirmedFlushRestoreScore(item) > unconfirmedFlushRestoreScore(out[index]) {
			out[index] = item
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

func (a *App) restoreFlushDraft(threadID string, parts []composerdraft.Part) error {
	current, _, err := a.store.GetThreadDraft(threadID)
	if err != nil {
		return err
	}
	draft, err := composerdraft.MergeParts(threadID, current, parts, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	return a.store.UpsertThreadDraft(draft)
}

// flushQueuePayload is the local-scope alias for flushqueue.Payload.
// Kept as a local name so the compile-time drift guard in
// app_flush_queue_test.go and the in-place reads/writes from
// dispatchFlush + RegisterQueueItem don't need cosmetic churn.
type flushQueuePayload = flushqueue.Payload

// dispatchFlush is the triage.FlushDispatcher implementation: when
// triage observes a safe provider boundary, it hands queued user
// messages here for delivery to the provider.
//
// Per-item flow:
//
//  1. Decode QueuedFlushItem.Payload into flushQueuePayload.
//  2. Resolve attachments + source/revision plan refs (same shape
//     Send and Steer use).
//  3. Allocate the AO item id (`user:<turnIndex>:flush:<n>`).
//  4. Register the pending-send marker — provider-specific:
//     - Claude with active turn: EAGER persist. The user_text row is
//     persisted immediately at the active turn's index so it appears
//     in the timeline at the point it was dispatched (before any
//     response items the agent produces after). A non-deferred
//     pending send is registered at the response turn so
//     resolveTurnIndexOnStart opens a fresh turn for the response.
//     On echo, attachProviderItemIDToUserRow stamps provider_item_id.
//     provider:queue_flushed emits BEFORE PersistItem so the Zone 2
//     entry exists when the upsert clears it.
//     - Claude without active turn / Codex: DEFERRED persist. The
//     row is deferred via RegisterPendingFlushSend and persisted at
//     echo time via persistDeferredUserText at MAX+1 item_index.
//  5. Call the provider:
//     - Claude: sess.Send writes a fresh user envelope to stdin;
//     Claude's queue processor (queryGuard-gated) consumes it between
//     turns.
//     - Codex: sess.Steer pushes onto the active turn's
//     pending_input. Falls back to sess.Send when Steer returns
//     ErrNoActiveTurn.
//
// On any definite item error, the dispatcher persists a sibling `error`
// row, aborts the current batch, and requeues items not yet attempted.
//
// Invoked by the app-layer per-thread flush worker, after triage has released
// r.mu. The worker preserves FIFO order across multiple boundary drains and
// prevents concurrent sequence allocation for one thread.
func (a *App) dispatchFlush(threadID string, items []triage.QueuedFlushItem) {
	a.dispatchFlushWithGeneration(threadID, items, a.currentFlushDispatchGeneration(threadID))
}

func (a *App) dispatchFlushWithGeneration(threadID string, items []triage.QueuedFlushItem, generation uint64) {
	if len(items) == 0 {
		return
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
		return
	}

	for i, item := range items {
		if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
			return
		}
		flushedItem, flushedEmitted, requeue, err := a.dispatchFlushItem(threadID, item)
		if err != nil {
			log.Printf("flush dispatch: thread=%s item=%s: %v", threadID, item.ID, err)
			if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
				return
			}
			// The FAILING item requeues too, ahead of the unattempted
			// tail — dropping it would leave the message in no state at
			// all (round-13, CT13-1/C13-2). Its StaleUserItemID reflects
			// how far dispatch got: the original marker when cleanup
			// never ran or failed, cleared once cleanup succeeded, the
			// fresh row id once a quiet persist landed.
			a.triage.RegisterQueueItem(threadID, requeue)
			for _, unattempted := range items[i+1:] {
				a.triage.RegisterQueueItem(threadID, unattempted)
			}
			a.emitQueueStateChanged(threadID)
			return
		}
		if !flushedEmitted {
			a.emit("provider:queue_flushed", QueueFlushedEvent{
				ThreadID: threadID,
				Items:    []QueueFlushedItem{flushedItem},
			})
		}
	}
	a.emitQueueStateChanged(threadID)
}

// dispatchFlushItem dispatches one queued message. On error, the
// returned requeue value is the item to re-register (round-13,
// CT13-1): a copy of the input whose StaleUserItemID tracks the
// durable state left behind — unchanged until the stale-row cleanup
// runs, cleared once cleanup succeeds, and pointing at the fresh quiet
// row once an eager persist lands (so the redispatch cleans it up
// before persisting again). On success requeue is the zero value.
func (a *App) dispatchFlushItem(threadID string, item triage.QueuedFlushItem) (QueueFlushedItem, bool, triage.QueuedFlushItem, error) {
	requeue := item
	if a.shuttingDown.Load() {
		return QueueFlushedItem{}, false, requeue, ErrShuttingDown
	}

	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return QueueFlushedItem{}, false, requeue, fmt.Errorf("decode payload: %w", err)
		}
	}

	resolved, err := a.resolveUserMessageEnvelope(threadID, item.Message, userMessageInputs{
		attachmentIDs:                payload.AttachmentIDs,
		sourceProposedPlan:           payload.SourceProposedPlan,
		revisionSourceProposedPlan:   payload.RevisionSourceProposedPlan,
		revisionSourceCommentIDs:     payload.RevisionSourceCommentIDs,
		revisionSourceDiffReview:     payload.RevisionSourceDiffReview,
		revisionSourceDiffCommentIDs: payload.RevisionSourceDiffCommentIDs,
	})
	if err != nil {
		return QueueFlushedItem{}, false, requeue, err
	}
	content := resolved.content
	providerAttachments := resolved.providerAttachments
	userMeta := resolved.userMessageMeta

	a.ensureTriageRouter()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return QueueFlushedItem{}, false, requeue, fmt.Errorf("load thread: %w", err)
	}
	if err := a.ensureClaudeContextReadyForUserSendLocked(thread); err != nil {
		return QueueFlushedItem{}, false, requeue, err
	}
	sess, unlockAccount, err := a.lockProviderAccountForSendLocked(thread)
	if err != nil {
		return QueueFlushedItem{}, false, requeue, err
	}
	defer unlockAccount()

	responseTurnIndex, activeAtResolution, err := a.resolveFlushTurnPlacement(threadID, sess)
	if err != nil {
		return QueueFlushedItem{}, false, requeue, fmt.Errorf("resolve turn index: %w", err)
	}

	// Claude with an active turn: persist the user_text within the active turn
	// for timeline ordering (the message sorts alongside the ongoing response
	// at the point it was dispatched), but register the pending send at the
	// response turn so resolveTurnIndexOnStart opens a fresh turn for the
	// response. Codex steers into the active turn's pending_input, so both
	// indices stay the same.
	persistTurnIndex := responseTurnIndex
	eagerPersist := false
	if sess.codex == nil {
		if active, found, lookupErr := a.store.GetActiveTurn(threadID); lookupErr == nil && found {
			persistTurnIndex = active.TurnIndex
			eagerPersist = true
		} else if lookupErr != nil {
			return QueueFlushedItem{}, false, requeue, fmt.Errorf("resolve persist turn: %w", lookupErr)
		}
	}

	// Mint the queued message's wire id for Claude-family sessions, like
	// app_send.go does for direct sends: Claude honors a client-supplied
	// top-level uuid on QUEUED stdin messages too and echoes it verbatim
	// at turn pickup (verified 2.1.202, spike 2026-07-09 — claude-wire.md
	// §Outbound user message), and claudetui.Send uses a supplied
	// UserMessageUUID for its reconstructed echo. Registering the pending
	// send with this id makes the echo match identity-keyed, closing the
	// injected-envelope-during-queue-wait mispair (the entry waits in the
	// FIFO for the WHOLE remaining turn). The row meta is deliberately NOT
	// pre-stamped: the echo-time merge must produce a meta change so
	// attachProviderItemIDToUserRow emits the upsert that clears Zone 2.
	// Codex assigns its own item ids — sendUUID stays empty and the entry
	// keeps FIFO consumption.
	var sendUUID string
	if sess.codex == nil {
		sendUUID = uuid.NewString()
	}

	if item.StaleUserItemID != "" {
		// A previous dispatch of this message left a quiet row whose
		// session-death cleanup failed (see QueuedFlushItem doc). Retry
		// it here — AFTER every failure-prone resolution step above, so
		// an envelope/thread/session/placement error aborts the item
		// while the stale row (the message's only durable copy) is
		// still intact for the next retry (round-12, D12-1) — and
		// BEFORE nextFlushUserItemID, which allocates against the
		// turn's persisted rows. On cleanup failure, abort the item:
		// persisting a fresh row over the stale one would show the
		// message twice (round-11, R11-1). The remaining loss windows
		// (allocation, persist, send) are the same ones a first
		// dispatch of any message already has.
		if err := a.cleanupStaleFlushRow(threadID, item.StaleUserItemID); err != nil {
			return QueueFlushedItem{}, false, requeue, fmt.Errorf("cleanup stale flush row %s: %w", item.StaleUserItemID, err)
		}
		requeue.StaleUserItemID = ""
	}

	flushItemID, err := a.nextFlushUserItemID(threadID, persistTurnIndex)
	if err != nil {
		return QueueFlushedItem{}, false, requeue, fmt.Errorf("allocate item id: %w", err)
	}
	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        flushItemID,
		ThreadID:  threadID,
		TurnIndex: persistTurnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   content,
		Meta:      userMeta,
		CreatedAt: now,
		UpdatedAt: now,
	}

	flushedItem := QueueFlushedItem{QueueItemID: item.ID, UserItemID: userItem.ID, Message: item.Message}

	if eagerPersist {
		// Emit queue_flushed so the frontend creates the Zone 2 entry
		// (queued marker above the composer). Persist the row quietly —
		// no provider:item_event — so the item reserves its timeline
		// position in SQLite but stays as a queued marker in the UI
		// until the provider echo confirms it entered context.
		a.emit("provider:queue_flushed", QueueFlushedEvent{
			ThreadID: threadID,
			Items:    []QueueFlushedItem{flushedItem},
		})
		if persistErr := a.triage.PersistItemQuiet(userItem, nil); persistErr != nil {
			return QueueFlushedItem{}, true, requeue, fmt.Errorf("eager persist flush: %w", persistErr)
		}
		requeue.StaleUserItemID = userItem.ID
		// Non-deferred pending send at the response turn. The item is
		// already persisted at persistTurnIndex; on echo,
		// attachProviderItemIDToUserRow stamps provider_item_id and
		// emits the provider:item_event upsert that clears Zone 2.
		// resolveTurnIndexOnStart reads responseTurnIndex from the
		// FIFO to open a new turn for the response.
		a.triage.RegisterPendingQuietFlushSend(threadID, item.ID, userItem, responseTurnIndex, item.EnqueuedAt, sendUUID)
	} else {
		// Deferred: row persists at echo time via persistDeferredUserText.
		a.triage.RegisterPendingFlushSendWithEnqueuedAt(threadID, item.ID, userItem, item.EnqueuedAt, sendUUID)
	}
	if draftErr := a.store.DeleteThreadDraft(threadID); draftErr != nil {
		log.Printf("flush queue: delete draft for thread %s: %v", threadID, draftErr)
	}

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
		UserMessageUUID: sendUUID,
	}

	dispatchErr := a.dispatchFlushToProvider(sess, content, sendOpts)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s item=%s: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, item.ID)
			a.applyProposedPlanAcceptance(threadID, userItem, resolved)
			return flushedItem, eagerPersist, triage.QueuedFlushItem{}, nil
		}
		if sess.codex != nil && codex.IsNoActiveTurnRace(dispatchErr) {
			a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
			if activeAtResolution {
				responseTurnIndex++
			}
			freshFlushItemID, allocErr := a.nextFlushUserItemID(threadID, responseTurnIndex)
			if allocErr != nil {
				a.persistFlushDispatchError(threadID, responseTurnIndex, allocErr)
				return QueueFlushedItem{}, eagerPersist, requeue, allocErr
			}
			userItem.ID = freshFlushItemID
			userItem.TurnIndex = responseTurnIndex
			userItem.CreatedAt = time.Now().UnixMilli()
			userItem.UpdatedAt = userItem.CreatedAt
			// Codex-only branch (IsNoActiveTurnRace) — sendUUID is empty
			// here, so the re-registered entry keeps FIFO consumption.
			a.triage.RegisterPendingFlushSendWithEnqueuedAt(threadID, item.ID, userItem, item.EnqueuedAt, sendUUID)
			sess.liveness.bumpActivity(time.Now())
			if sendErr := sess.codex.Send(context.Background(), content, sendOpts); sendErr != nil {
				if codex.IsAmbiguousTurnStartTimeout(sendErr) {
					// Same ambiguity as the steer timeout above: the
					// turn/start was written and the echo may already be
					// coming. A requeue would double-send (round-14,
					// D14-2) — leave the pending entry for the echo.
					log.Printf("flush dispatch: thread=%s item=%s: codex turn/start timed out after write; leaving pending confirmation for provider echo", threadID, item.ID)
					a.applyProposedPlanAcceptance(threadID, userItem, resolved)
					flushedItem.UserItemID = userItem.ID
					return flushedItem, eagerPersist, triage.QueuedFlushItem{}, nil
				}
				a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
				a.persistFlushDispatchError(threadID, responseTurnIndex, sendErr)
				return QueueFlushedItem{}, eagerPersist, requeue, sendErr
			}
			a.applyProposedPlanAcceptance(threadID, userItem, resolved)
			flushedItem.UserItemID = userItem.ID
			return flushedItem, eagerPersist, triage.QueuedFlushItem{}, nil
		}
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, persistTurnIndex, dispatchErr)
		return QueueFlushedItem{}, eagerPersist, requeue, dispatchErr
	}
	a.applyProposedPlanAcceptance(threadID, userItem, resolved)
	return flushedItem, eagerPersist, triage.QueuedFlushItem{}, nil
}

// nextFlushSequenceForTurn returns the next available flush sequence
// number for (threadID, turnIndex). It considers both persisted rows
// and deferred pending rows waiting for provider echo, because queued
// sends no longer persist the user_text row optimistically.
func (a *App) nextFlushSequenceForTurn(threadID string, turnIndex int) (int, error) {
	next, err := a.nextSequenceForScope(threadID, turnIndex, "flush")
	if err != nil {
		return 0, err
	}
	if a.triage == nil {
		return next, nil
	}
	pendingNext := a.triage.MaxPendingFlushSequence(threadID, turnIndex) + 1
	if pendingNext > next {
		return pendingNext, nil
	}
	return next, nil
}

// resolveFlushTurnPlacement picks the turn index for a flush-dispatched
// message. Provider-specific because Claude and Codex handle queued
// messages differently:
//
//   - Codex: Steer injects into the active turn's pending_input. The
//     message is part of the current turn. Use the active turn's index.
//   - Claude: Send writes to stdin. Claude's useQueueProcessor only
//     dequeues between turns (queryGuard blocks while a query is
//     active), so the message always starts a NEW turn. Using the
//     active turn's index would cause setOpenTurn to reset
//     id-allocating counters for the already-running turn, producing
//     segment ID collisions (text:T:0 overwrites previous text:T:0).
//
// For the non-active-turn path (shared by both providers), in-flight
// pending sends are consulted via MaxPendingSendTurnIndex because
// deferred items don't land in items/turns until echo — two messages
// queued during the same active turn would otherwise both resolve to
// the same next index.
func (a *App) resolveFlushTurnPlacement(threadID string, sess session) (turnIndex int, activeAtResolution bool, err error) {
	if sess.codex != nil {
		if active, found, err := a.store.GetActiveTurn(threadID); err == nil && found {
			return active.TurnIndex, true, nil
		} else if err != nil {
			return 0, false, fmt.Errorf("lookup active turn: %w", err)
		}
	}
	turnIndex, err = a.nextSendTurnIndex(threadID)
	if err != nil {
		return 0, false, err
	}
	if a.triage != nil {
		if maxPending, ok := a.triage.MaxPendingSendTurnIndex(threadID); ok && maxPending+1 > turnIndex {
			turnIndex = maxPending + 1
		}
	}
	return turnIndex, false, nil
}

func (a *App) nextSendTurnIndex(threadID string) (int, error) {
	hasPriorItems, err := a.store.HasItems(threadID)
	if err != nil {
		return 0, fmt.Errorf("check prior items: %w", err)
	}
	last, err := a.store.LastTurnIndex(threadID)
	if err != nil {
		return 0, fmt.Errorf("get turn index: %w", err)
	}
	if hasPriorItems {
		return last + 1, nil
	}
	return last, nil
}

// dispatchFlushToProvider routes the actual provider call based on
// session type. Codex drains prefer Steer (mid-turn pending_input);
// the caller handles no-active-turn fallback after it can re-register
// the pending marker at the correct fresh-turn position. Claude has no
// Steer equivalent; sess.Send writes a user envelope that Claude's
// mid-loop drain (query.ts:1547) consumes at the next API iteration.
//
// Two distinct race shapes both trigger the fallback:
//
//  1. **Local-side race**: codex.Session's local activeTurnID is
//     empty when Steer enters. Returns the typed sentinel
//     codex.ErrNoActiveTurn — caught by errors.Is.
//  2. **Wire-side race**: the local activeTurnID was non-empty (so
//     Steer dispatched), but the upstream app-server had already
//     ended the turn. The wire reply carries the upstream's
//     "NoActiveTurn" error string, wrapped as a generic
//     transport error. We substring-match because the codex
//     package surfaces wire errors as `fmt.Errorf("codex: %s: %s
//     (code %d)", ...)` rather than a typed wrapper. Upstream's
//     error string is stable per codex-rs/core/src/session/mod.rs.
func (a *App) dispatchFlushToProvider(sess session, content string, opts provider.SendOptions) error {
	// Every branch below writes to provider stdin, so stamp activity
	// once up front. Matches the pre-Send bumps in sendToProvider /
	// steerMessageWithOptions so the idle reaper can't reap a session
	// in the middle of a flush dispatch.
	sess.liveness.bumpActivity(time.Now())
	if sess.codex != nil {
		return sess.codex.Steer(context.Background(), content, opts)
	}
	providerSess := sess.providerSession()
	if providerSess == nil {
		return fmt.Errorf("session has no provider")
	}
	return providerSess.Send(context.Background(), content, opts)
}

// persistFlushDispatchError persists a system `error` row sibling to
// the failed user_text. Rows allocate ids via the same per-turn error
// counter the EventError handler uses (NextErrorSequence) so a later
// provider error on the same turn doesn't collide on `error:<turn>:0`.
func (a *App) persistFlushDispatchError(threadID string, turnIndex int, dispatchErr error) {
	seq := a.triage.NextErrorSequence(threadID, turnIndex, "")
	now := time.Now().UnixMilli()
	errorItem := store.Item{
		ID:        triage.NewErrorID(turnIndex, "", seq),
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "error",
		Role:      "system",
		Status:    "completed",
		Summary:   fmt.Sprintf("Failed to deliver queued message: %v", dispatchErr),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := a.triage.PersistItem(errorItem, nil); err != nil {
		log.Printf("flush dispatch: persist error row: %v", err)
	}
}

// RegisterQueueItem appends a user message to the thread's pending-send
// queue. The composer keeps this item above the chat box immediately; if a
// provider session is live, the backend dispatches it as soon as possible and
// keeps it pending there until the provider-visible user-message echo creates
// the chat-history row.
//
// The wire-shape options carry attachment IDs and plan refs but NOT
// resolved attachments / plans — the dispatcher re-resolves at provider-write
// time so attachment validation reflects current store state. Validation
// establishes resource bounds (queue length,
// message size, attachment count) AND shape preconditions (existing
// thread, plan-ref shape).
//
// Returns the resolved QueuedItem with the assigned id and
// EnqueuedAt timestamp so the frontend can mirror the same row
// without an extra round-trip. Emits `provider:queue_state_changed`
// for any other client (remote `--connect` peers, additional
// webviews) that may be observing the same thread.
func (a *App) RegisterQueueItem(threadID string, message string, opts SendMessageOptions) (QueuedItem, error) {
	// A user queueing a message has just consumed their composer draft, so the
	// bound entry point clears it.
	return a.registerQueueItem(threadID, message, opts, false)
}

// registerQueueItem is RegisterQueueItem plus the one axis the wire does not
// carry: whether the durable composer draft belongs to this message.
//
// `preserveDraft` is set by the app-internal injectors (a workflow wake). Their
// text did not come from the composer, and clearing the draft would destroy
// text the user typed and has not sent — a silent data loss the user could not
// have anticipated from a run finishing in the background.
func (a *App) registerQueueItem(threadID string, message string, opts SendMessageOptions, preserveDraft bool) (QueuedItem, error) {
	if a.shuttingDown.Load() {
		return QueuedItem{}, ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return QueuedItem{}, fmt.Errorf("register queue item: empty thread id")
	}
	// Resource caps. The queue lives in router memory until it is
	// handed to the dispatch worker or the session is torn down —
	// without a length cap a misbehaving client (or a bug that
	// registers in a loop) wedges the backend by appending forever.
	// The per-message byte cap protects against an unbounded payload
	// riding the wire frame.
	if len(message) > maxQueueMessageBytes() {
		return QueuedItem{}, fmt.Errorf("register queue item: message too long: %d bytes (max %d)", len(message), maxQueueMessageBytes())
	}
	if len(opts.AttachmentIDs) > maxQueueAttachmentCount() {
		return QueuedItem{}, fmt.Errorf("register queue item: too many attachments: got %d, max %d", len(opts.AttachmentIDs), maxQueueAttachmentCount())
	}
	if opts.RevisionSourceProposedPlan == nil && len(opts.RevisionSourceCommentIDs) > 0 {
		return QueuedItem{}, fmt.Errorf("register queue item: revision comments require a source proposed plan")
	}
	if opts.RevisionSourceDiffReview == nil && len(opts.RevisionSourceDiffCommentIDs) > 0 {
		return QueuedItem{}, fmt.Errorf("register queue item: diff review comments require a source diff review")
	}
	// Thread-existence check: a stale or attacker-supplied threadID
	// would otherwise grow a permanent in-memory queue entry that
	// CleanupThread never sweeps (no session ever attached). Same
	// validation as Send / Steer.
	if _, err := a.store.GetThread(threadID); err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: %w", err)
	}

	// Defensive: production wires triage in initSubsystems. Mirrors
	// the lazy-init pattern on Send and Steer.
	a.ensureTriageRouter()

	// Hold flushHandoffMu across the queue append and the immediate flush
	// handoff below: the revert predicate reads the same queued / in-flight
	// counters under this mutex (pendingFlushWorkCount), so holding it here
	// keeps a Stop click from observing tryFlushQueue's handoff window and
	// discarding the turn-starting prompt. See the flushHandoffMu field doc
	// (app.go) for the window and why this isn't the per-thread action lock.
	//
	// Lock hierarchy (acyclic): threadLock -> flushHandoffMu -> {r.mu (triage),
	// flushDispatchMu, a.mu}. The only threadLock/flushHandoffMu edge is the
	// revert predicate's (InterruptAndRevertIfClean holds threadLock and reaches
	// flushHandoffMu via pendingFlushWorkCount). This span acquires r.mu,
	// flushDispatchMu (the synchronous enqueueFlushDispatch inflight bump), and
	// a.mu (sessionManager().get below) while holding flushHandoffMu, but never
	// the thread lock — enqueueFlushDispatch spawns the dispatch worker, which
	// takes threadLock asynchronously. And no path holds a.mu when entering
	// RegisterQueueItem or pendingFlushWorkCount, so a.mu is never inverted
	// against flushHandoffMu. No cycle, no deadlock.
	a.flushHandoffMu.Lock()
	defer a.flushHandoffMu.Unlock()

	totalQueued := a.triage.QueuedFlushItemCount(threadID) + a.triage.DeferredPendingFlushItemCount(threadID) + a.flushDispatchItemCount(threadID)
	if totalQueued >= maxQueueLength() {
		return QueuedItem{}, fmt.Errorf("register queue item: queue full (max %d items per thread)", maxQueueLength())
	}

	id := flushqueue.NewItemID()
	payload := flushQueuePayload{
		AttachmentIDs:                opts.AttachmentIDs,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: encode payload: %w", err)
	}

	enqueuedAt := a.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{
		ID:      id,
		Message: message,
		Payload: payloadBytes,
	})

	wireItem := QueuedItem{
		ID:                           id,
		ThreadID:                     threadID,
		Message:                      message,
		AttachmentIDs:                opts.AttachmentIDs,
		SourceProposedPlan:           opts.SourceProposedPlan,
		RevisionSourceProposedPlan:   opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:     opts.RevisionSourceCommentIDs,
		RevisionSourceDiffReview:     opts.RevisionSourceDiffReview,
		RevisionSourceDiffCommentIDs: opts.RevisionSourceDiffCommentIDs,
		EnqueuedAt:                   enqueuedAt,
	}
	if !preserveDraft {
		if draftErr := a.store.DeleteThreadDraft(threadID); draftErr != nil {
			log.Printf("register queue item: delete draft for thread %s: %v", threadID, draftErr)
		}
	}
	a.emitQueueStateChanged(threadID)
	if _, ok := a.sessionManager().get(threadID); ok {
		a.triage.FlushQueuedItems(threadID)
	}
	return wireItem, nil
}

// GetQueueState returns the current queue snapshot for the thread.
// Used by the frontend on bootstrap and thread-switch to seed its
// per-thread mirror; also by remote `--connect` clients attaching
// mid-session. Read-only — no emission.
//
// LocalOnly: the snapshot exposes the user's drafted-but-not-yet-sent
// prompts, attachment IDs, and plan refs. Same disclosure shape as
// the diff bindings, hence loopback-only at the transport layer.
func (a *App) GetQueueState(threadID string) ([]QueuedItem, error) {
	if strings.TrimSpace(threadID) == "" {
		return nil, fmt.Errorf("get queue state: empty thread id")
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return nil, fmt.Errorf("get queue state: %w", err)
	}
	if a.triage == nil {
		return nil, nil
	}
	items := a.triage.QueuedFlushItems(threadID)
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]QueuedItem, 0, len(items))
	for _, item := range items {
		out = append(out, flushqueue.ItemFromTriage(threadID, item))
	}
	return out, nil
}

// emitQueueStateChanged emits the post-mutation queue snapshot on
// `provider:queue_state_changed`. Always queries the current
// snapshot via the triage primitive so the wire payload is
// authoritative — observers don't have to combine deltas to get
// state.
func (a *App) emitQueueStateChanged(threadID string) {
	var items []QueuedItem
	if a.triage != nil {
		current := a.triage.QueuedFlushItems(threadID)
		items = make([]QueuedItem, 0, len(current))
		for _, item := range current {
			items = append(items, flushqueue.ItemFromTriage(threadID, item))
		}
	}
	a.emit("provider:queue_state_changed", QueueStateChangedEvent{
		ThreadID: threadID,
		Items:    items,
	})
}

// maxQueueAttachmentCount caps the per-item attachment count at the
// same limit the live send path enforces (attachmentstore.DefaultMaxCount).
func maxQueueAttachmentCount() int {
	return attachmentstore.DefaultMaxCount
}

// maxQueueLength caps the number of pending queue entries per thread.
// Bounded by user attention in normal operation (single-digit N); the
// cap exists to fail loudly rather than silently grow router memory
// when a client misbehaves or a bug puts RegisterQueueItem in a loop.
const queueMaxLength = 64

func maxQueueLength() int { return queueMaxLength }

// maxQueueMessageBytes caps the in-flight message text per queue
// entry. Chat-shaped messages comfortably fit; the cap protects
// against a 16 MiB-frame DoS vector. Attachments ride the existing
// UploadAttachment path and are not subject to this cap.
const queueMaxMessageBytes = 512 * 1024 // 512 KiB

func maxQueueMessageBytes() int { return queueMaxMessageBytes }

// nextFlushUserItemID is the flush-scope wrapper around
// nextSequencedUserItemID. Format: `user:<turnIndex>:flush:<n>`.
// Sortable; never collides with the seed `user:<turnIndex>` row or
// with `:steer:<n>` rows. Reads existing rows so a session reopen
// sees the right next sequence even after a restart.
func (a *App) nextFlushUserItemID(threadID string, turnIndex int) (string, error) {
	seq, err := a.nextFlushSequenceForTurn(threadID, turnIndex)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("user:%d:flush:%d", turnIndex, seq), nil
}
