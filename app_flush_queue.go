package main

import (
	"context"
	"encoding/json"
	"errors"
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
	ID                         string              `json:"id"`
	ThreadID                   string              `json:"threadId"`
	Message                    string              `json:"message"`
	AttachmentIDs              []string            `json:"attachmentIds,omitempty"`
	SourceProposedPlan         *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs   []string            `json:"revisionSourceCommentIds,omitempty"`
	EnqueuedAt                 int64               `json:"enqueuedAt"`
}

// QueueStateChangedEvent is the payload of `provider:queue_state_changed`,
// emitted whenever the per-thread queue mutates (register / undo /
// drained / dropped via session teardown). Carrying the full
// post-mutation snapshot — rather than a delta — lets the frontend
// reconcile state without keeping its own ordering log; the SvelteMap
// store assigns the `Items` slice to its per-thread entry and the
// reactive bindings update.
type QueueStateChangedEvent struct {
	ThreadID string       `json:"threadId"`
	Items    []QueuedItem `json:"items"`
}

// QueueFlushedEvent is emitted by `dispatchFlush` at the start of a
// trigger fire, before the per-item provider dispatch runs. It carries
// the (queueItemId → userItemId) mapping the frontend uses to move
// the items from Zone 1 (retractable queue) to Zone 2 (flushed,
// awaiting wire-echo confirmation).
//
// The userItemId is the deterministic AO row id the dispatcher will
// allocate (`user:<turnIndex>:flush:<n>`). The frontend matches the
// id against incoming `provider:item_event` upserts: when the
// corresponding row's Meta gains a `provider_item_id`, the wire echo
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

// flushQueuePayload is the wire shape of QueuedFlushItem.Payload.
// The frontend serialises it via RegisterQueueItem; the dispatcher
// decodes it here. Mirrors the data fields on sendMessageOptions
// except RuntimeMode — by the time the flush trigger fires, the
// round's runtime mode is already established and a mid-round flip
// would defeat the whole point of in-flight queueing.
type flushQueuePayload struct {
	AttachmentIDs              []string            `json:"attachmentIds,omitempty"`
	SourceProposedPlan         *SourceProposedPlan `json:"sourceProposedPlan,omitempty"`
	RevisionSourceProposedPlan *SourceProposedPlan `json:"revisionSourceProposedPlan,omitempty"`
	RevisionSourceCommentIDs   []string            `json:"revisionSourceCommentIds,omitempty"`
}

// dispatchFlush is the triage.FlushDispatcher implementation: when
// the first non-subagent tool_use of a wire round fires, triage
// hands the queued user messages here for delivery to the provider.
//
// Per-item flow:
//
//  1. Decode QueuedFlushItem.Payload into flushQueuePayload.
//  2. Resolve attachments + source/revision plan refs (same shape
//     Send and Steer use).
//  3. Allocate the AO item id (`user:<turnIndex>:flush:<n>`) — never
//     collides with the seed `user:<turnIndex>` row or with
//     `:steer:<n>` rows.
//  4. Persist the user_text row optimistically through the triage
//     chokepoint so parent_id validation and ItemsPersisted metric
//     stay consistent with provider-sourced items.
//  5. RegisterPendingSend so the wire echo (Claude `--replay-user-
//     messages` envelope or Codex `item/completed userMessage`)
//     correlates and stamps `provider_item_id` onto our optimistic
//     row instead of writing a "wire-only" twin.
//  6. Call the provider:
//     - Claude: sess.Send writes a fresh user envelope to stdin;
//     Claude's mid-loop drain (query.ts:1547) consumes it on the
//     next API iteration.
//     - Codex: sess.Steer pushes onto the active turn's
//     pending_input. Falls back to sess.Send when Steer returns
//     ErrNoActiveTurn — the active turn ended between trigger
//     fire and Steer arrival.
//
// On any item error, the dispatcher persists a sibling `error` row
// and aborts the rest of the batch. Items not yet attempted are
// dropped from this batch — preserving wire ordering for the
// SUCCEEDED prefix matters more than best-effort delivery for the
// failing tail (a partial batch with a wire-visible gap would be
// more confusing than "two of three messages were not delivered, see
// error row"). The user can retype the dropped items.
//
// Synchronously invoked from triage.fireFlushTriggerOnce after r.mu
// is released — the dispatcher MAY call back into the router
// (RegisterPendingSend, PersistItem, NextErrorSequence) without
// re-entrancy.
//
// Two emissions fire BEFORE the per-item dispatch loop so the
// frontend can move items from Zone 1 (retractable queue) to Zone 2
// (flushed, awaiting wire echo) before the dispatcher's optimistic
// user_text row emissions arrive:
//
//   - `provider:queue_flushed` carries the (queueItemId, userItemId,
//     message) mapping. The userItemIds are pre-allocated against
//     the resolved turn index so the frontend can correlate against
//     the wire echo's Meta-stamped row.
//   - `provider:queue_state_changed` carries the post-flush snapshot
//     (empty in the common case) so any client mirroring the queue
//     (the local webview, remote `--connect` peers) sees Zone 1
//     drained.
func (a *App) dispatchFlush(threadID string, items []triage.QueuedFlushItem) {
	if len(items) == 0 {
		return
	}

	turnIndex, err := a.resolveFlushTurnIndex(threadID)
	if err != nil {
		log.Printf("flush dispatch: resolve turn index: %v", err)
		return
	}

	startSeq, err := a.firstFlushSequenceForTurn(threadID, turnIndex)
	if err != nil {
		log.Printf("flush dispatch: allocate id: %v", err)
		return
	}

	flushed := make([]QueueFlushedItem, 0, len(items))
	for i, item := range items {
		flushed = append(flushed, QueueFlushedItem{
			QueueItemID: item.ID,
			UserItemID:  fmt.Sprintf("user:%d:flush:%d", turnIndex, startSeq+i),
			Message:     item.Message,
		})
	}

	a.emit("provider:queue_flushed", QueueFlushedEvent{
		ThreadID: threadID,
		Items:    flushed,
	})
	// fireFlushTriggerOnce already emptied the per-thread queue map
	// before it invoked us, so the snapshot here is empty in the
	// common case. Emit explicitly so any client mirroring the queue
	// observes Zone 1 collapse before the user_text row upserts
	// arrive.
	a.emitQueueStateChanged(threadID)

	for i, item := range items {
		if err := a.dispatchFlushItemWithID(threadID, item, turnIndex, flushed[i].UserItemID); err != nil {
			log.Printf("flush dispatch: thread=%s item=%s: %v", threadID, item.ID, err)
			return
		}
	}
}

// dispatchFlushItemWithID is the per-item flush path with the
// userItemId pre-allocated. Splits id allocation from the dispatch
// body so the batch-level dispatchFlush can emit the
// `provider:queue_flushed` mapping before any persistence runs.
func (a *App) dispatchFlushItemWithID(threadID string, item triage.QueuedFlushItem, turnIndex int, flushItemID string) error {
	if a.shuttingDown.Load() {
		return ErrShuttingDown
	}

	var payload flushQueuePayload
	if len(item.Payload) > 0 {
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
	}

	providerAttachments, persistedAttachments, err := a.resolveSendMessageAttachments(threadID, payload.AttachmentIDs)
	if err != nil {
		return fmt.Errorf("attachments: %w", err)
	}

	// Per-thread critical section: matches the Send/Steer locks so a
	// concurrent direct send and an in-flight flush dispatch cannot
	// interleave pending-send registration ordering for the same
	// thread.
	unlock := sendThreadMuRegistry.lockFor(threadID)
	defer unlock()

	sourcePlan, err := a.resolveSourceProposedPlan(threadID, payload.SourceProposedPlan, true)
	if err != nil {
		return fmt.Errorf("source proposed plan: %w", err)
	}
	revisionSourcePlan, err := a.resolveSourceProposedPlan(threadID, payload.RevisionSourceProposedPlan, false)
	if err != nil {
		return fmt.Errorf("revision source proposed plan: %w", err)
	}
	if revisionSourcePlan == nil && len(payload.RevisionSourceCommentIDs) > 0 {
		return fmt.Errorf("revision comments require a source proposed plan")
	}

	content := item.Message
	revisionCommentIDs := payload.RevisionSourceCommentIDs
	if revisionSourcePlan != nil && len(revisionCommentIDs) > 0 {
		nextContent, commentIDs, err := a.appendPlanRevisionCommentsToContent(threadID, content, revisionSourcePlan.ItemID, revisionCommentIDs)
		if err != nil {
			return fmt.Errorf("revision comments: %w", err)
		}
		content = nextContent
		revisionCommentIDs = commentIDs
	}

	userMeta, err := marshalUserMessageMeta(persistedAttachments, sourcePlan, revisionSourcePlan, revisionCommentIDs)
	if err != nil {
		return fmt.Errorf("user meta: %w", err)
	}

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
	if err := a.triage.PersistItem(userItem, nil); err != nil {
		return fmt.Errorf("persist user message: %w", err)
	}
	a.captureMessageCheckpoint(thread, userItem)

	// Register the pending-send marker BEFORE the provider write so the
	// wire echo can't race ahead of the marker and miss the
	// pending-send-present branch in handleUserText. Cleared on
	// dispatch failure below.
	a.triage.RegisterPendingSend(threadID, userItem.ID, turnIndex)

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
	}

	dispatchErr := a.dispatchFlushToProvider(sess, content, sendOpts)
	if dispatchErr != nil {
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, turnIndex, dispatchErr)
		return dispatchErr
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
	return a.nextSequenceForScope(threadID, turnIndex, "flush")
}

// resolveFlushTurnIndex picks the turn index to attribute the
// dispatched user_text row to. Prefers the active turn (the trigger
// fired mid-round, so it's present in the common case); falls back
// to the next-turn-index calculation Send uses when the active turn
// has just ended.
//
// The fallback exists because between trigger fire and dispatcher
// invocation, the active wire round can settle (turn/completed lands
// on the wire bus before the dispatcher's mu acquisition). A flush
// item that resolves AFTER turn settle should attribute to the next
// logical turn, not the (now closed) prior one — otherwise the
// optimistic user_text row lands on a settled turn and the wire echo
// for it would never arrive (no turn is open).
func (a *App) resolveFlushTurnIndex(threadID string) (int, error) {
	if active, found, err := a.store.GetActiveTurn(threadID); err == nil && found {
		return active.TurnIndex, nil
	} else if err != nil {
		return 0, fmt.Errorf("lookup active turn: %w", err)
	}
	// No active turn: derive next index using the same shape Send does.
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
// session type. Codex prefers Steer (mid-turn pending_input) and
// falls back to Send on the no-active-turn race — the active turn
// ended between trigger fire and Steer arrival, and a fresh Send
// starts a new turn carrying the queued message. Claude has no
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
	if sess.codex != nil {
		err := sess.codex.Steer(context.Background(), content, opts)
		if err == nil {
			return nil
		}
		if isNoActiveTurnRace(err) {
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

// isNoActiveTurnRace reports whether err is one of the two
// no-active-turn race shapes that warrant a Send fallback. See
// dispatchFlushToProvider for the full taxonomy. Substring matching
// against "NoActiveTurn" catches the wire-level case because the
// codex package surfaces wire errors as opaque strings (not typed
// wrappers); the substring is stable per upstream
// codex-rs/core/src/session/mod.rs.
func isNoActiveTurnRace(err error) bool {
	if errors.Is(err, codex.ErrNoActiveTurn) {
		return true
	}
	return strings.Contains(err.Error(), "NoActiveTurn")
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
// the next first-tool-use trigger fires (see triage flush_queue.go).
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
		a.triage.SetFlushDispatcher(a.dispatchFlush)
	}

	if a.triage.QueuedFlushItemCount(threadID) >= maxQueueLength() {
		return QueuedItem{}, fmt.Errorf("register queue item: queue full (max %d items per thread)", maxQueueLength())
	}

	id := newQueueItemID()
	payload := flushQueuePayload{
		AttachmentIDs:              opts.AttachmentIDs,
		SourceProposedPlan:         opts.SourceProposedPlan,
		RevisionSourceProposedPlan: opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:   opts.RevisionSourceCommentIDs,
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
		ID:                         id,
		ThreadID:                   threadID,
		Message:                    message,
		AttachmentIDs:              opts.AttachmentIDs,
		SourceProposedPlan:         opts.SourceProposedPlan,
		RevisionSourceProposedPlan: opts.RevisionSourceProposedPlan,
		RevisionSourceCommentIDs:   opts.RevisionSourceCommentIDs,
		EnqueuedAt:                 enqueuedAt,
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
