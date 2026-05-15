package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
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
	items []triage.QueuedFlushItem
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

func (a *App) configureTriageQueueCallbacks() {
	if a.triage == nil {
		return
	}
	a.triage.SetFlushDispatcher(a.enqueueFlushDispatch)
	a.triage.SetDeferredUserTextConfirmedHook(func(threadID string, item store.Item) {
		thread, err := a.store.GetThread(threadID)
		if err != nil {
			log.Printf("flush queue: load thread for deferred checkpoint %s/%s: %v", threadID, item.ID, err)
			return
		}
		a.captureMessageCheckpoint(thread, item)
	})
}

func (a *App) enqueueFlushDispatch(threadID string, items []triage.QueuedFlushItem) {
	if threadID == "" || len(items) == 0 {
		return
	}
	batch := make([]triage.QueuedFlushItem, len(items))
	copy(batch, items)

	a.flushDispatchMu.Lock()
	if a.flushDispatchQueues == nil {
		a.flushDispatchQueues = make(map[string][]flushDispatchBatch)
		a.flushDispatchRunning = make(map[string]bool)
		a.flushDispatchInflightItems = make(map[string]int)
	}
	a.flushDispatchQueues[threadID] = append(a.flushDispatchQueues[threadID], flushDispatchBatch{
		items: batch,
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
		a.flushDispatchMu.Unlock()

		a.dispatchFlush(threadID, batch.items)

		a.flushDispatchMu.Lock()
		a.flushDispatchInflightItems[threadID] -= len(batch.items)
		if a.flushDispatchInflightItems[threadID] <= 0 {
			delete(a.flushDispatchInflightItems, threadID)
		}
		a.flushDispatchMu.Unlock()
	}
}

func (a *App) flushDispatchItemCount(threadID string) int {
	a.flushDispatchMu.Lock()
	defer a.flushDispatchMu.Unlock()
	return a.flushDispatchInflightItems[threadID]
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
//  3. Allocate the AO item id (`user:<turnIndex>:flush:<n>`) — never
//     collides with the seed `user:<turnIndex>` row or with
//     `:steer:<n>` rows.
//  4. RegisterPendingFlushSendAtIndex so the wire echo (Claude
//     `--replay-user-messages` envelope or Codex `item/completed
//     userMessage`) creates the chat-history row at the position
//     captured when the message was written, with `provider_item_id`
//     already attached.
//  5. Call the provider:
//     - Claude: sess.Send writes a fresh user envelope to stdin;
//     Claude's mid-loop drain (query.ts:1547) consumes it on the
//     next API iteration.
//     - Codex drains: sess.Steer pushes onto the active turn's
//     pending_input. Falls back to sess.Send when Steer returns
//     ErrNoActiveTurn — the active turn ended between trigger fire and
//     Steer arrival.
//
// On any definite item error, the dispatcher persists a sibling `error`
// row, aborts the current batch, and requeues items not yet attempted. The
// failed item itself never enters Zone 2 because no provider confirmation
// can arrive for it. Codex turn/steer timeouts are different: once the
// request has been written, timeout means the ACK is missing, not that the
// provider rejected the message. Those stay pending for the provider echo.
//
// Invoked by the app-layer per-thread flush worker, after triage has released
// r.mu. The worker preserves FIFO order across multiple boundary drains and
// prevents concurrent sequence allocation for one thread.
//
// Each successful per-item provider write emits `provider:queue_flushed`
// with the (queueItemId, userItemId, message) mapping before the empty
// `provider:queue_state_changed` snapshot. That order keeps the above-composer
// pending row visible until the client can move it from queued to provider-sent.
// Failed or unattempted items never enter Zone 2, so the frontend cannot get
// stuck waiting for a provider confirmation that will never arrive.
func (a *App) dispatchFlush(threadID string, items []triage.QueuedFlushItem) {
	if len(items) == 0 {
		return
	}

	unlock := a.threadLocks().Lock(threadID)
	defer unlock()

	for i, item := range items {
		flushedItem, err := a.dispatchFlushItem(threadID, item)
		if err != nil {
			log.Printf("flush dispatch: thread=%s item=%s: %v", threadID, item.ID, err)
			for _, unattempted := range items[i+1:] {
				a.triage.RegisterQueueItem(threadID, unattempted)
			}
			a.emitQueueStateChanged(threadID)
			return
		}
		a.emit("provider:queue_flushed", QueueFlushedEvent{
			ThreadID: threadID,
			Items:    []QueueFlushedItem{flushedItem},
		})
	}
	a.emitQueueStateChanged(threadID)
}

func (a *App) dispatchFlushItem(threadID string, item triage.QueuedFlushItem) (QueueFlushedItem, error) {
	if a.shuttingDown.Load() {
		return QueueFlushedItem{}, ErrShuttingDown
	}

	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return QueueFlushedItem{}, fmt.Errorf("decode payload: %w", err)
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
		return QueueFlushedItem{}, err
	}
	content := resolved.content
	providerAttachments := resolved.providerAttachments
	revisionSourceDiff := resolved.revisionSourceDiff
	revisionDiffCommentIDs := resolved.revisionDiffCommentIDs
	userMeta := resolved.userMessageMeta

	sess, ok := a.sessionManager().get(threadID)
	if !ok {
		return QueueFlushedItem{}, fmt.Errorf("no active session for thread %s", threadID)
	}

	if a.triage == nil {
		// Defensive: production wires triage in initSubsystems
		// (app.go:332) before any provider events flow. The lazy init
		// here matches the pattern used by sendMessageWithOptions and
		// steerMessageWithOptions for tests that build a partial App.
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
		a.configureTriageQueueCallbacks()
	}

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return QueueFlushedItem{}, fmt.Errorf("load thread: %w", err)
	}

	turnIndex, activeAtResolution, err := a.resolveFlushTurnPlacement(threadID)
	if err != nil {
		return QueueFlushedItem{}, fmt.Errorf("resolve turn index: %w", err)
	}
	flushItemID, err := a.nextFlushUserItemID(threadID, turnIndex)
	if err != nil {
		return QueueFlushedItem{}, fmt.Errorf("allocate item id: %w", err)
	}
	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        flushItemID,
		ThreadID:  threadID,
		TurnIndex: turnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   content,
		Meta:      userMeta,
		CreatedAt: now,
		UpdatedAt: now,
	}
	insertItemIndex, err := a.nextFlushInsertItemIndex(threadID, turnIndex)
	if err != nil {
		return QueueFlushedItem{}, fmt.Errorf("allocate insert index: %w", err)
	}
	// Register the pending-send marker BEFORE the provider write so the
	// wire echo can't race ahead of the marker and miss the
	// pending-send-present branch in handleUserText. The row is deferred:
	// normal queued sends should not appear in chat history until the
	// provider confirms they entered context. Cleared on dispatch failure
	// below.
	a.triage.RegisterPendingFlushSendAtIndex(threadID, item.ID, userItem, insertItemIndex)

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
	}

	dispatchErr := a.dispatchFlushToProvider(sess, content, sendOpts)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s item=%s: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, item.ID)
			return QueueFlushedItem{QueueItemID: item.ID, UserItemID: userItem.ID, Message: item.Message}, nil
		}
		if sess.codex != nil && codex.IsNoActiveTurnRace(dispatchErr) {
			a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
			if activeAtResolution {
				turnIndex++
			}
			freshFlushItemID, allocErr := a.nextFlushUserItemID(threadID, turnIndex)
			if allocErr != nil {
				a.persistFlushDispatchError(threadID, turnIndex, allocErr)
				return QueueFlushedItem{}, allocErr
			}
			userItem.ID = freshFlushItemID
			userItem.TurnIndex = turnIndex
			userItem.CreatedAt = time.Now().UnixMilli()
			userItem.UpdatedAt = userItem.CreatedAt
			insertItemIndex, indexErr := a.nextFlushInsertItemIndex(threadID, turnIndex)
			if indexErr != nil {
				a.persistFlushDispatchError(threadID, turnIndex, indexErr)
				return QueueFlushedItem{}, indexErr
			}
			a.triage.RegisterPendingFlushSendAtIndex(threadID, item.ID, userItem, insertItemIndex)
			sess.liveness.bumpActivity(time.Now())
			if sendErr := sess.codex.Send(context.Background(), content, sendOpts); sendErr != nil {
				a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
				a.persistFlushDispatchError(threadID, turnIndex, sendErr)
				return QueueFlushedItem{}, sendErr
			}
			if revisionSourceDiff != nil && len(revisionDiffCommentIDs) > 0 {
				if err := a.store.MarkDiffReviewCommentsSent(threadID, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs, time.Now().UnixMilli(), userItem.ID); err != nil {
					log.Printf("flush queue: mark diff review comments sent: %v", err)
				}
			}
			return QueueFlushedItem{QueueItemID: item.ID, UserItemID: userItem.ID, Message: item.Message}, nil
		}
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, turnIndex, dispatchErr)
		return QueueFlushedItem{}, dispatchErr
	}
	if revisionSourceDiff != nil && len(revisionDiffCommentIDs) > 0 {
		if err := a.store.MarkDiffReviewCommentsSent(threadID, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs, time.Now().UnixMilli(), userItem.ID); err != nil {
			log.Printf("flush queue: mark diff review comments sent: %v", err)
		}
	}

	return QueueFlushedItem{QueueItemID: item.ID, UserItemID: userItem.ID, Message: item.Message}, nil
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

func (a *App) nextFlushInsertItemIndex(threadID string, turnIndex int) (int, error) {
	itemIndex, err := a.store.NextItemIndex(threadID, turnIndex)
	if err != nil {
		return 0, err
	}
	if a.triage == nil {
		return itemIndex, nil
	}
	return itemIndex + a.triage.PendingDeferredInsertCount(threadID, turnIndex), nil
}

func (a *App) resolveFlushTurnPlacement(threadID string) (turnIndex int, activeAtResolution bool, err error) {
	if active, found, err := a.store.GetActiveTurn(threadID); err == nil && found {
		return active.TurnIndex, true, nil
	} else if err != nil {
		return 0, false, fmt.Errorf("lookup active turn: %w", err)
	}
	// No active turn: derive next index using the same shape Send does.
	turnIndex, err = a.nextSendTurnIndex(threadID)
	return turnIndex, false, err
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

	if a.triage == nil {
		// Defensive: production wires triage in initSubsystems.
		// Mirrors the lazy-init pattern on Send and Steer.
		a.triage = triage.NewRouter(a.store, a.emitWithReplay())
		a.configureTriageQueueCallbacks()
	}

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
