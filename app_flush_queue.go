package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"

	"github.com/google/uuid"
)

// QueuedItem is the wire-side projection of a triage QueuedFlushItem,
// used by the frontend to mirror the backend's per-thread queue. The
// wire shape mirrors SendMessageOptions's data fields plus the
// frontend-allocated id and stamped enqueuedAt — together they're
// enough for both the queue overlay rendering and the UP-arrow
// retract path that re-hydrates the composer draft.
//
// AttachmentIDs (not full Attachment records) ride the wire because
// the frontend already has the full records in its attachment store
// keyed by id; cross-wire transmission would duplicate bytes for no
// gain. Plan refs are passed by value because they're tiny and
// already used as plain JSON across the existing send path.
type QueuedItem struct {
	ID                           string              `json:"id"`
	ThreadID                     string              `json:"threadId"`
	Message                      string              `json:"message"`
	AttachmentIDs                []string            `json:"attachmentIds,omitempty"`
	SourceProposedPlan           *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string            `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *SourceDiffReview   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string            `json:"revisionSourceDiffCommentIds,omitempty"`
	EnqueuedAt                   int64               `json:"enqueuedAt"`
}

// QueueStateChangedEvent is the payload of `provider:queue_state_changed`,
// emitted whenever the per-thread queue mutates (register / undo /
// drained / dropped via session teardown). Carrying the full
// post-mutation snapshot — rather than a delta — lets the frontend
// reconcile state without keeping its own ordering log; the SvelteMap
// store assigns the `Items` slice to its per-thread entry and the
// reactive bindings update.
//
// Reason is intentionally sparse. The frontend does not infer dispatch
// lifecycle from an empty snapshot because undo and flush can both
// produce one; explicit flush reasons let it bridge the working indicator
// exactly like a normal user send while the provider write is in flight.
type QueueStateChangedEvent struct {
	ThreadID string       `json:"threadId"`
	Items    []QueuedItem `json:"items"`
	Reason   string       `json:"reason,omitempty"`
}

const (
	queueStateReasonFlushStarted = "flush_started"
	queueStateReasonFlushFailed  = "flush_failed"
)

type flushDispatchBatch struct {
	items []triage.QueuedFlushItem
	mode  triage.FlushDispatchMode
}

// QueueFlushedEvent is emitted by `dispatchFlush` at the start of a
// successful per-item provider dispatch. It carries
// the (queueItemId → userItemId) mapping the frontend uses to move
// the items from Zone 1 (retractable queue) to Zone 2 (flushed,
// awaiting wire-echo confirmation).
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

func (a *App) enqueueFlushDispatch(threadID string, items []triage.QueuedFlushItem, mode triage.FlushDispatchMode) {
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
		mode:  mode,
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

		a.dispatchFlush(threadID, batch.items, batch.mode)

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

// flushQueuePayload is the wire shape of QueuedFlushItem.Payload.
// The frontend serialises it via RegisterQueueItem; the dispatcher
// decodes it here. Mirrors the data fields on sendMessageOptions
// except RuntimeMode — by the time the flush trigger fires, the
// round's runtime mode is already established and a mid-round flip
// would defeat the whole point of in-flight queueing.
type flushQueuePayload struct {
	AttachmentIDs                []string            `json:"attachmentIds,omitempty"`
	SourceProposedPlan           *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan   *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs     []string            `json:"revisionSourceCommentIds,omitempty"`
	RevisionSourceDiffReview     *SourceDiffReview   `json:"revisionSourceDiffReview,omitempty"`
	RevisionSourceDiffCommentIDs []string            `json:"revisionSourceDiffCommentIds,omitempty"`
}

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
//  4. RegisterPendingSendWithDeferredItem so the wire echo (Claude
//     `--replay-user-messages` envelope or Codex `item/completed
//     userMessage`) creates the chat-history row with `provider_item_id`
//     already attached.
//  5. Call the provider:
//     - Claude: sess.Send writes a fresh user envelope to stdin;
//     Claude's mid-loop drain (query.ts:1547) consumes it on the
//     next API iteration.
//     - Codex boundary drains: sess.Steer pushes onto the active
//     turn's pending_input. Falls back to sess.Send when Steer returns
//     ErrNoActiveTurn — the active turn ended between trigger fire and
//     Steer arrival.
//     - Codex immediate drains (Send Now after interrupt): sess.Send
//     starts a fresh turn. This intentionally matches a manual
//     post-interrupt send instead of steering into the turn the user
//     just stopped.
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
// `provider:queue_state_changed` carries the post-flush snapshot
//
//	(empty in the common case) so any client mirroring the queue
//	(the local webview, remote `--connect` peers) sees Zone 1
//	drained.
//
// Each successful per-item provider write then emits
// `provider:queue_flushed` with the (queueItemId, userItemId, message)
// mapping. Failed or unattempted items never enter Zone 2, so the
// frontend cannot get stuck waiting for a provider confirmation that
// will never arrive.
func (a *App) dispatchFlush(threadID string, items []triage.QueuedFlushItem, mode triage.FlushDispatchMode) {
	if len(items) == 0 {
		return
	}

	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	turnIndex, err := a.resolveFlushTurnIndex(threadID, mode)
	if err != nil {
		log.Printf("flush dispatch: resolve turn index: %v", err)
		a.requeueFailedFlushBatch(threadID, items, mode)
		return
	}

	startSeq, err := a.firstFlushSequenceForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("flush dispatch: allocate id: %v", err)
		a.requeueFailedFlushBatch(threadID, items, mode)
		return
	}

	flushedItems := make([]QueueFlushedItem, len(items))
	for i, item := range items {
		flushedItems[i] = QueueFlushedItem{
			QueueItemID: item.ID,
			UserItemID:  fmt.Sprintf("user:%d:flush:%d", turnIndex, startSeq+i),
			Message:     item.Message,
		}
	}
	// tryFlushQueue already emptied the per-thread queue map
	// before it invoked us, so the snapshot here is empty in the
	// common case. Emit explicitly so any client mirroring the queue
	// observes Zone 1 collapse while the user_text rows wait for
	// provider confirmation.
	if mode == triage.FlushDispatchModeImmediate {
		a.emitQueueStateChangedWithReason(threadID, queueStateReasonFlushStarted)
	} else {
		a.emitQueueStateChanged(threadID)
	}

	for i, item := range items {
		if err := a.dispatchFlushItemWithID(threadID, item, turnIndex, flushedItems[i].UserItemID, mode); err != nil {
			log.Printf("flush dispatch: thread=%s item=%s: %v", threadID, item.ID, err)
			for _, unattempted := range items[i+1:] {
				a.triage.RegisterQueueItem(threadID, unattempted)
			}
			if mode == triage.FlushDispatchModeImmediate {
				a.emitQueueStateChangedWithReason(threadID, queueStateReasonFlushFailed)
			} else {
				a.emitQueueStateChanged(threadID)
			}
			return
		}
		a.emit("provider:queue_flushed", QueueFlushedEvent{
			ThreadID: threadID,
			Items:    []QueueFlushedItem{flushedItems[i]},
		})
	}
}

func (a *App) requeueFailedFlushBatch(threadID string, items []triage.QueuedFlushItem, mode triage.FlushDispatchMode) {
	if a.triage != nil {
		for _, item := range items {
			a.triage.RegisterQueueItem(threadID, item)
		}
	}
	if mode == triage.FlushDispatchModeImmediate {
		a.emitQueueStateChangedWithReason(threadID, queueStateReasonFlushFailed)
	} else {
		a.emitQueueStateChanged(threadID)
	}
}

// dispatchFlushItemWithID is the per-item flush path with the
// userItemId pre-allocated. Splits id allocation from the dispatch
// body so the batch-level dispatchFlush can keep ids stable while
// emitting `provider:queue_flushed` only after provider write success.
func (a *App) dispatchFlushItemWithID(threadID string, item triage.QueuedFlushItem, turnIndex int, flushItemID string, mode triage.FlushDispatchMode) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return fmt.Errorf("decode payload: %w", err)
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
		return err
	}
	content := resolved.content
	providerAttachments := resolved.providerAttachments
	revisionSourceDiff := resolved.revisionSourceDiff
	revisionDiffCommentIDs := resolved.revisionDiffCommentIDs
	userMeta := resolved.userMessageMeta

	a.mu.Lock()
	sess, ok := a.sessions[threadID]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no active session for thread %s", threadID)
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
		return fmt.Errorf("load thread: %w", err)
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
	// Register the pending-send marker BEFORE the provider write so the
	// wire echo can't race ahead of the marker and miss the
	// pending-send-present branch in handleUserText. The row is deferred:
	// normal queued sends should not appear in chat history until the
	// provider confirms they entered context. Cleared on dispatch failure
	// below.
	a.triage.RegisterPendingFlushSend(threadID, item.ID, userItem)

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
	}

	dispatchErr := a.dispatchFlushToProvider(sess, content, sendOpts, mode)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s item=%s: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, item.ID)
			return nil
		}
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, turnIndex, dispatchErr)
		return dispatchErr
	}
	if revisionSourceDiff != nil && len(revisionDiffCommentIDs) > 0 {
		if err := a.store.MarkDiffReviewCommentsSent(threadID, revisionSourceDiff.Scope, revisionSourceDiff.SourceKey, revisionDiffCommentIDs, time.Now().UnixMilli(), userItem.ID); err != nil {
			log.Printf("flush queue: mark diff review comments sent: %v", err)
		}
	}

	return nil
}

// firstFlushSequenceForTurn returns the next available flush
// sequence number for (threadID, turnIndex). Distinct from
// nextFlushUserItemID which formats the full id; the batch
// dispatchFlush uses the bare integer to allocate a contiguous range
// of ids before any persist runs.
//
// Returns 1 for an empty turn (highest+1 with highest=0).
func (a *App) firstFlushSequenceForTurn(threadID string, turnIndex int) (int, error) {
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

// resolveFlushTurnIndex picks the turn index to attribute the
// dispatched user_text row to. Boundary flushes prefer the active turn
// because they are same-round queue drains. Immediate flushes come from
// Send Now after an interrupt and intentionally use the same next-turn
// calculation as a normal user send.
//
// The fallback exists because between trigger fire and dispatcher
// invocation, the active wire round can settle (turn/completed lands
// on the wire bus before the dispatcher's mu acquisition). A flush
// item that resolves AFTER turn settle should attribute to the next
// logical turn, not the (now closed) prior one — otherwise the
// deferred user_text row would later land on a settled turn whose
// wire echo is no longer associated with an open turn.
func (a *App) resolveFlushTurnIndex(threadID string, mode triage.FlushDispatchMode) (int, error) {
	if mode == triage.FlushDispatchModeImmediate {
		return a.nextSendTurnIndex(threadID)
	}
	if active, found, err := a.store.GetActiveTurn(threadID); err == nil && found {
		return active.TurnIndex, nil
	} else if err != nil {
		return 0, fmt.Errorf("lookup active turn: %w", err)
	}
	// No active turn: derive next index using the same shape Send does.
	return a.nextSendTurnIndex(threadID)
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
// session type and flush mode. Codex boundary drains prefer Steer
// (mid-turn pending_input) and fall back to Send on the no-active-turn
// race — the active turn ended between trigger fire and Steer arrival,
// and a fresh Send starts a new turn carrying the queued message.
// Immediate drains come from explicit Send Now / interrupt and always
// use Send so the queued message behaves like a normal user message
// submitted after stopping the turn. Claude has no Steer equivalent;
// sess.Send writes a user envelope that Claude's mid-loop drain
// (query.ts:1547) consumes at the next API iteration.
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
func (a *App) dispatchFlushToProvider(sess session, content string, opts provider.SendOptions, mode triage.FlushDispatchMode) error {
	// Every branch below writes to provider stdin, so stamp activity
	// once up front. Matches the pre-Send bumps in sendToProvider /
	// steerMessageWithOptions so the idle reaper can't reap a session
	// in the middle of a flush dispatch.
	sess.liveness.bumpActivity(time.Now())
	if sess.codex != nil {
		if mode == triage.FlushDispatchModeImmediate {
			return sess.codex.Send(context.Background(), content, opts)
		}
		err := sess.codex.Steer(context.Background(), content, opts)
		if err == nil {
			return nil
		}
		if codex.IsNoActiveTurnRace(err) {
			return sess.codex.Send(context.Background(), content, opts)
		}
		return err
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

// RegisterQueueItem appends a queued user message to the thread's
// in-flight queue. Called by the composer when the user submits while
// a wire round is still active — the message waits in the queue until
// the next safe provider boundary (see triage flush_queue.go).
//
// The wire-shape options carry attachment IDs and plan refs but NOT
// resolved attachments / plans — the dispatcher re-resolves at
// trigger-fire time so attachment validation reflects current store
// state. Validation establishes resource bounds (queue length,
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
	// Resource caps. The queue lives in router memory until either a
	// flush trigger fires or the session is torn down — without a
	// length cap a misbehaving client (or a bug that registers in a
	// loop) wedges the backend by appending forever. The per-message
	// byte cap protects against an unbounded payload riding the wire
	// frame.
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

	id := newQueueItemID()
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
	return wireItem, nil
}

// UndoQueuedItems drops every queued item for the thread and returns
// them so the composer can combine them into a single editable draft
// (matching Claude TUI's popAllEditable). Called by the UP-arrow
// retract handler.
//
// Emits `provider:queue_state_changed` after the drop so other clients
// observing the same thread see the empty queue.
func (a *App) UndoQueuedItems(threadID string) ([]QueuedItem, error) {
	if a.shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	if strings.TrimSpace(threadID) == "" {
		return nil, fmt.Errorf("undo queued items: empty thread id")
	}
	if _, err := a.store.GetThread(threadID); err != nil {
		return nil, fmt.Errorf("undo queued items: %w", err)
	}
	if a.triage == nil {
		return nil, nil
	}

	dropped := a.triage.DropAllQueuedItems(threadID)
	if len(dropped) == 0 {
		return nil, nil
	}

	out := make([]QueuedItem, 0, len(dropped))
	for _, item := range dropped {
		out = append(out, queuedItemFromTriage(threadID, item))
	}
	a.emitQueueStateChanged(threadID)
	return out, nil
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
		out = append(out, queuedItemFromTriage(threadID, item))
	}
	return out, nil
}

// emitQueueStateChanged emits the post-mutation queue snapshot on
// `provider:queue_state_changed`. Always queries the current
// snapshot via the triage primitive so the wire payload is
// authoritative — observers don't have to combine deltas to get
// state.
func (a *App) emitQueueStateChanged(threadID string) {
	a.emitQueueStateChangedWithReason(threadID, "")
}

func (a *App) emitQueueStateChangedWithReason(threadID string, reason string) {
	var items []QueuedItem
	if a.triage != nil {
		current := a.triage.QueuedFlushItems(threadID)
		items = make([]QueuedItem, 0, len(current))
		for _, item := range current {
			items = append(items, queuedItemFromTriage(threadID, item))
		}
	}
	a.emit("provider:queue_state_changed", QueueStateChangedEvent{
		ThreadID: threadID,
		Items:    items,
		Reason:   reason,
	})
}

// queuedItemFromTriage decodes a triage QueuedFlushItem back into the
// wire-side QueuedItem. The Payload is opaque app-layer JSON; on
// decode failure we still return a partially-populated wire item so
// the frontend can render the message text and offer retract — losing
// attachment refs on a corrupt payload is preferable to the wire
// dropping the item entirely.
func queuedItemFromTriage(threadID string, item triage.QueuedFlushItem) QueuedItem {
	out := QueuedItem{
		ID:         item.ID,
		ThreadID:   threadID,
		Message:    item.Message,
		EnqueuedAt: item.EnqueuedAt,
	}
	if len(item.Payload) == 0 {
		return out
	}
	var payload flushQueuePayload
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		log.Printf("decode queued item payload thread=%s item=%s: %v", threadID, item.ID, err)
		return out
	}
	out.AttachmentIDs = payload.AttachmentIDs
	out.SourceProposedPlan = payload.SourceProposedPlan
	out.RevisionSourceProposedPlan = payload.RevisionSourceProposedPlan
	out.RevisionSourceCommentIDs = payload.RevisionSourceCommentIDs
	out.RevisionSourceDiffReview = payload.RevisionSourceDiffReview
	out.RevisionSourceDiffCommentIDs = payload.RevisionSourceDiffCommentIDs
	return out
}

// newQueueItemID allocates a new opaque queue-item id. The
// `queue:` prefix matches the frontend's draft-id convention so the
// id is recognisable in logs / traces. The uuid suffix carries the
// uniqueness — collision against another concurrent register on the
// same thread is statistically impossible.
func newQueueItemID() string {
	return "queue:" + uuid.NewString()
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
	return a.nextSequencedUserItemID(threadID, turnIndex, "flush")
}
