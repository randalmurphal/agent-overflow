package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
	"agent-overflow/internal/triage"

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

	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	generation := a.flushDispatch.generation[threadID]
	a.flushDispatch.queues[threadID] = append(a.flushDispatch.queues[threadID], flushDispatchBatch{
		items:      batch,
		generation: generation,
	})
	a.flushDispatch.inflightItems[threadID] += len(batch)
	if a.flushDispatch.running[threadID] {
		a.flushDispatch.mu.Unlock()
		return
	}
	a.flushDispatch.running[threadID] = true
	a.flushDispatch.wg.Add(1)
	a.flushDispatch.mu.Unlock()

	go a.runFlushDispatchWorker(threadID)
}

func (a *App) runFlushDispatchWorker(threadID string) {
	defer a.flushDispatch.wg.Done()
	for {
		a.flushDispatch.mu.Lock()
		queue := a.flushDispatch.queues[threadID]
		if len(queue) == 0 {
			delete(a.flushDispatch.queues, threadID)
			delete(a.flushDispatch.running, threadID)
			a.flushDispatch.mu.Unlock()
			return
		}
		batch := queue[0]
		if len(queue) == 1 {
			delete(a.flushDispatch.queues, threadID)
		} else {
			a.flushDispatch.queues[threadID] = queue[1:]
		}
		a.flushDispatch.current[threadID] = batch
		a.flushDispatch.mu.Unlock()

		a.dispatchFlushWithGeneration(threadID, batch.items, batch.generation)

		a.flushDispatch.mu.Lock()
		delete(a.flushDispatch.current, threadID)
		if a.flushDispatch.generation[threadID] == batch.generation {
			a.flushDispatch.inflightItems[threadID] -= len(batch.items)
			if a.flushDispatch.inflightItems[threadID] <= 0 {
				delete(a.flushDispatch.inflightItems, threadID)
			}
		}
		a.flushDispatch.mu.Unlock()
	}
}

func (a *App) flushDispatchItemCount(threadID string) int {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	return a.flushDispatch.inflightItems[threadID]
}

func (a *App) ensureFlushDispatchMapsLocked() {
	if a.flushDispatch.queues == nil {
		a.flushDispatch.queues = make(map[string][]flushDispatchBatch)
	}
	if a.flushDispatch.current == nil {
		a.flushDispatch.current = make(map[string]flushDispatchBatch)
	}
	if a.flushDispatch.running == nil {
		a.flushDispatch.running = make(map[string]bool)
	}
	if a.flushDispatch.inflightItems == nil {
		a.flushDispatch.inflightItems = make(map[string]int)
	}
	if a.flushDispatch.generation == nil {
		a.flushDispatch.generation = make(map[string]uint64)
	}
}

func (a *App) clearFlushDispatchForRollback(threadID string) {
	// The rollback is deleting the history these messages were queued
	// against, so they are being thrown away rather than deferred: their
	// durable rows go with them, or the next boot restores messages the
	// user's revert already discarded.
	a.dropDurableFlushQueue(threadID)
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatch.generation[threadID]++
	delete(a.flushDispatch.queues, threadID)
	delete(a.flushDispatch.current, threadID)
	delete(a.flushDispatch.inflightItems, threadID)
	a.flushDispatch.mu.Unlock()
}

func (a *App) drainFlushDispatchForSessionEnd(threadID string) []triage.QueuedFlushItem {
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatch.generation[threadID]++
	var drained []triage.QueuedFlushItem
	if current, ok := a.flushDispatch.current[threadID]; ok {
		drained = append(drained, current.items...)
		delete(a.flushDispatch.current, threadID)
	}
	for _, batch := range a.flushDispatch.queues[threadID] {
		drained = append(drained, batch.items...)
	}
	delete(a.flushDispatch.queues, threadID)
	delete(a.flushDispatch.inflightItems, threadID)
	a.flushDispatch.mu.Unlock()
	return drained
}

func (a *App) currentFlushDispatchGeneration(threadID string) uint64 {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	if a.flushDispatch.generation == nil {
		return 0
	}
	return a.flushDispatch.generation[threadID]
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
		a.flushDispatch.wg.Wait()
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
			a.emit(eventchan.ProviderQueueFlushed, QueueFlushedEvent{
				ThreadID: threadID,
				Items:    []QueueFlushedItem{flushedItem},
			})
		}
		// A successful provider write is one of the two durable endpoints for
		// an injected message; session-death recovery into the composer is the
		// other. The shared settlement is exactly-once if those paths race.
		item.Settlement.Settle()
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
		// Resolve composer commands HERE, at dispatch, not at enqueue: the
		// block names the runs live when the message reaches the provider.
		// App-injected wake prose keeps this false so a leading slash reaches
		// the model rather than Claude's local router.
		expandComposerCommands: payload.ExpandComposerCommands,
		// The send id moves from the durable queue row onto the row this
		// dispatch persists, so the message keeps one idempotency record for
		// its whole life (app_send_idempotency.go).
		sendID: payload.SendID,
	})
	if err != nil {
		return QueueFlushedItem{}, false, requeue, err
	}
	content := resolved.content
	providerContent := resolved.providerContent
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

	// Two placements, one per dispatch shape (resolveFlushTurnPlacement
	// picks the response turn; this picks where the row is PERSISTED):
	//
	//   - Claude with an active turn: persist the user_text within the active
	//     turn for timeline ordering (the message sorts alongside the ongoing
	//     response at the point it was dispatched), but register the pending
	//     send at the response turn so resolveTurnIndexOnStart opens a fresh
	//     turn for the response. This is the only eager-persist case.
	//   - Codex on `turn/steer`: the message joins the RUNNING turn's
	//     pending_input, so resolveFlushTurnPlacement already returned the
	//     active turn's index and both indices are the same one.
	persistTurnIndex := responseTurnIndex
	eagerPersist := false
	if sess.Codex == nil {
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
	//
	// Codex assigns its own item ids, so it names the message the other way
	// round: AO passes the row id as `clientUserMessageId` on `turn/steer`
	// (and on the fresh-turn fallback below), and the dispatched
	// `userMessage` echoes it back as `clientId`. Both halves — the stamp on
	// the wire and the ByClientID registration — are one decision: an entry
	// registered by client id is invisible to an id-less echo, so a codex
	// send that stamps but registers FIFO (or the reverse) reintroduces the
	// 2026-08-24 mispop.
	var sendUUID string
	if sess.Codex == nil {
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

	// The identity this dispatch will be recognised by — wire stamp and
	// registry expectation derived together (providerSendIdentity), so
	// the two cannot drift.
	clientUserMessageID, sendExpect := providerSendIdentity(sess, userItem.ID, sendUUID)

	if eagerPersist {
		// Emit queue_flushed so the frontend creates the Zone 2 entry
		// (queued marker above the composer). Persist the row quietly —
		// no provider:item_event — so the item reserves its timeline
		// position in SQLite but stays as a queued marker in the UI
		// until the provider echo confirms it entered context.
		a.emit(eventchan.ProviderQueueFlushed, QueueFlushedEvent{
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
		a.triage.RegisterPendingQuietFlushSendWithExpectation(
			threadID, item.ID, userItem, responseTurnIndex, item.EnqueuedAt, sendExpect)
	} else {
		// Deferred: row persists at echo time via persistDeferredUserText.
		a.triage.RegisterPendingFlushSendWithExpectation(threadID, item.ID, userItem, item.EnqueuedAt, sendExpect)
	}
	if draftErr := a.removeThreadDraft(transport.ClientIdentity{}, threadID); draftErr != nil {
		log.Printf("flush queue: delete draft for thread %s: %v", threadID, draftErr)
	}

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     providerAttachments,
		UserMessageUUID: sendUUID,
		// Codex's half of the same identity (empty for every other provider):
		// stamped on both `turn/steer` and the fresh-turn fallback, and echoed
		// back on the `userMessage` item's `clientId`.
		ClientUserMessageID: clientUserMessageID,
		// Agent Overflow's own expanded command must bypass Claude's local
		// router. Every other leading `/name` keeps Claude's native command
		// semantics, independent of discovery timing.
		GuardClaudeSlashCommand: !payload.ExpandComposerCommands || resolved.command != "",
	}

	dispatchErr := a.dispatchFlushToProvider(sess, providerContent, sendOpts)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s item=%s: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, item.ID)
			a.applyProposedPlanAcceptance(threadID, userItem, resolved)
			return flushedItem, eagerPersist, triage.QueuedFlushItem{}, nil
		}
		// A turn IS running and simply cannot take input — Codex is running a
		// review or a compaction (codex.ErrTurnNotSteerable). Nothing is sent:
		// re-dispatching as a fresh `turn/start` would interleave the user's
		// message with the running review, and `thread/queue/add` is not an
		// option AO has any more. The item goes back on AO's own flush queue,
		// where the next boundary drain (maybeFlushQueueAtBoundary, which the
		// review's own turn completion raises) retries it. Deliberately NOT
		// routed through persistFlushDispatchError: "the queue is waiting for
		// the review to finish" is the queue working, not a failure to show
		// the user an error row for.
		if sess.Codex != nil && codex.IsTurnNotSteerable(dispatchErr) {
			a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
			log.Printf("flush dispatch: thread=%s item=%s: the active codex turn cannot take input (%v); leaving the message queued for the next turn boundary",
				threadID, item.ID, dispatchErr)
			return QueueFlushedItem{}, eagerPersist, requeue, dispatchErr
		}
		if sess.Codex != nil && codex.IsNoActiveTurnRace(dispatchErr) {
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
			// The row id changed, and the client id IS the row id — so both
			// the wire stamp and the expectation are re-derived from the new
			// one. Re-registering with the old id would leave the entry
			// waiting on a `clientId` nothing will ever echo.
			var refreshExpect triage.PendingSendExpectation
			sendOpts.ClientUserMessageID, refreshExpect = providerSendIdentity(sess, userItem.ID, "")
			a.triage.RegisterPendingFlushSendWithExpectation(
				threadID, item.ID, userItem, item.EnqueuedAt, refreshExpect)
			sess.Liveness.BumpActivity(time.Now())
			if sendErr := sess.Codex.Send(context.Background(), providerContent, sendOpts); sendErr != nil {
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
	if a.triage != nil {
		// Both reads (persisted rows + deferred registry) run under the
		// thread's flush anchor: an unanchored pair here could land
		// inside the echo path's pop->persist section and re-issue a
		// consumed message's sequence (triage.NextFlushSequence doc).
		return a.triage.NextFlushSequence(threadID, turnIndex)
	}
	return a.nextSequenceForScope(threadID, turnIndex, "flush")
}

// resolveFlushTurnPlacement picks the turn index for a flush-dispatched
// message. Provider-specific because Claude and Codex handle queued
// messages differently:
//
//   - Codex: Steer injects into the active turn's pending_input. The
//     message is part of the current turn. Use the active turn's index.
//   - Claude: Send writes to stdin, and the CLI may drain it either
//     MID-loop (folded into the running wire round as a
//     `queued_command` attachment, no new system.init) or at the next
//     turn pickup. AO gives it a NEW logical turn index either way. That
//     is a deliberate AO choice, not a reading of the CLI: reusing the
//     active turn's index would make setOpenTurn reset id-allocating
//     counters for the already-running turn and collide segment ids
//     (text:T:0 overwriting the previous text:T:0). Triage owns the
//     mid-loop case explicitly — openQueuedEchoTurn opens the logical
//     turn when the echo arrives with no init behind it (see
//     queue_dispatch_turn_test.go
//     TestDeferredFlushEcho_MidLoopConsumption_OpensLogicalTurn).
//
// For the non-active-turn path (shared by both providers), in-flight
// pending sends are consulted via MaxPendingSendTurnIndex because
// deferred items don't land in items/turns until echo — two messages
// queued during the same active turn would otherwise both resolve to
// the same next index.
func (a *App) resolveFlushTurnPlacement(threadID string, sess session) (turnIndex int, activeAtResolution bool, err error) {
	// Codex STEERS a queued message into the running turn, so its row belongs
	// in that turn.
	if sess.Codex != nil {
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
// session type. A Codex drain STEERS (mid-turn pending_input); the caller
// handles the no-active-turn fallback after it can re-register the pending
// marker at the correct fresh-turn position, and the not-steerable refusal by
// leaving the message queued. It never writes to the app-server's own
// `thread/queue/*`: that queue dispatches on ITS clock, which means AO's
// queue and the provider's would both own the same message. Claude needs no
// second call: sess.Send writes the user envelope to stdin, which IS the
// steer — the CLI's queue processor drains it into the running turn at
// the next API iteration (query.ts:1547) whenever that turn still has
// tool iterations left, and otherwise runs it as the next turn. The
// message is never dropped in either case. See app_steer.go's doc for
// the verified behaviour and claude-wire.md §command_lifecycle for the
// per-message ack that reports which path it took.
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
	sess.Liveness.BumpActivity(time.Now())
	if sess.Codex != nil {
		return sess.Codex.Steer(context.Background(), content, opts)
	}
	providerSess := sess.ProviderSession()
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
		Kind:      triage.ItemKindError,
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
//
//ao:scope threads:operate
func (a *App) RegisterQueueItem(ctx context.Context, threadID string, message string, opts SendMessageOptions) (QueuedItem, error) {
	if err := a.requireAutonomyForThread(ctx, threadID, opts.RuntimeMode); err != nil {
		return QueuedItem{}, err
	}
	// A user queueing a message has just consumed their composer draft, so the
	// bound entry point clears it, and nothing is waiting on the dispatch.
	return a.registerQueueItem(threadID, message, opts, injectedQueueOptions{
		expandComposerCommands: true,
	})
}

// injectedQueueOptions carries the two axes the wire does not, both of them
// only meaningful for the app-internal injectors (a workflow wake) whose text
// did not come from a person typing into the composer.
type injectedQueueOptions struct {
	// expandComposerCommands is true only for the public composer entry.
	// App-injected wake text is prose even when its first word starts with `/`.
	expandComposerCommands bool
	// preserveDraft keeps the thread's durable composer draft. Clearing it
	// would destroy text the user typed and has not sent — a silent data loss
	// the user could not have anticipated from a run finishing in the
	// background.
	preserveDraft bool
	// onDurable runs once the message has either been written to the provider
	// or restored into the durable composer after a session death. An
	// injector whose bookkeeping outlives the message must settle here rather
	// than at register time because the queues in between are process memory.
	onDurable func()
}

// flushQueueSettlement is the durable-endpoint hook every queued message
// carries: it deletes the message's durable row, then runs whatever
// bookkeeping an injector added.
//
// The row's whole life is "registered, not yet anywhere else", so the two
// moments that end it are exactly the two a FlushSettlement already models —
// a successful provider write, and a session-death restore into the composer
// draft. Composing here rather than deleting at those two call sites is what
// makes the delete exactly-once when they race, and what keeps a future third
// endpoint from having to remember this table.
//
// A delete that fails is logged and not surfaced: the message has already
// arrived somewhere durable, so the only cost is that the boot sweep may
// restore it into the composer once, which is recoverable in a way that
// failing a delivered send is not.
func (a *App) flushQueueSettlement(threadID, id string, onDurable func()) *triage.FlushSettlement {
	return triage.NewFlushSettlement(func() {
		if err := a.store.DeleteFlushQueueItem(id); err != nil {
			log.Printf("flush queue: delete durable row %s/%s: %v", threadID, id, err)
		}
		if onDurable != nil {
			onDurable()
		}
	})
}

// dropDurableFlushQueue deletes every durable queue row of a thread. Its
// callers are the WHOLESALE DROPS — a teardown whose triage cleanup discards
// the in-memory queue without restoring it, and the Codex rollback purge —
// where a surviving row would resurrect at the next boot the very messages
// the user's Stop or revert threw away.
func (a *App) dropDurableFlushQueue(threadID string) {
	if err := a.store.DeleteFlushQueueItemsForThread(threadID); err != nil {
		log.Printf("flush queue: drop durable rows for thread %s: %v", threadID, err)
	}
}

// registerQueueItem is RegisterQueueItem plus the injected-message axes.
func (a *App) registerQueueItem(
	threadID string, message string, opts SendMessageOptions, injected injectedQueueOptions,
) (QueuedItem, error) {
	endAdmission, admitErr := a.workAdmission.begin(a.lifeCtx())
	if admitErr != nil {
		return QueuedItem{}, admitErr
	}
	defer endAdmission()

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
	unlock, err := a.threadApplication().LockMutable(context.Background(), threadID)
	if err != nil {
		return QueuedItem{}, err
	}
	defer unlock()
	// Thread-existence check: a stale or attacker-supplied threadID
	// would otherwise grow a permanent in-memory queue entry that
	// CleanupThread never sweeps (no session ever attached). Same
	// validation as Send / Steer.
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: %w", err)
	}
	if injected.expandComposerCommands && thread.Provider == string(provider.Codex) {
		_, isReview, parseErr := codexReviewCommandTarget(message)
		if parseErr != nil {
			return QueuedItem{}, fmt.Errorf("register queue item: /review: %w", parseErr)
		}
		if isReview {
			return QueuedItem{}, fmt.Errorf("register queue item: /review needs an idle thread; wait for the current turn to finish")
		}
	}

	// Defensive: production wires triage in initSubsystems. Mirrors
	// the lazy-init pattern on Send and Steer.
	a.ensureTriageRouter()

	// Hold a.flushDispatch.handoffMu across the queue append and the immediate flush
	// handoff below: the revert predicate reads the same queued / in-flight
	// counters under this mutex (pendingFlushWorkCount), so holding it here
	// keeps a Stop click from observing tryFlushQueue's handoff window and
	// discarding the turn-starting prompt. See the a.flushDispatch.handoffMu field doc
	// (app.go) for the window and why this isn't the per-thread action lock.
	//
	// Lock order: action -> ordinary mutation -> handoff -> triage/runtime.
	// Queue admission holds only the mutation and handoff locks, so a slow
	// send/revert never blocks typing into the queue. Transfer reservation
	// takes action then mutation before checking queues and recording its
	// fence. Dispatch workers acquire action asynchronously after admission.
	a.flushDispatch.handoffMu.Lock()
	defer a.flushDispatch.handoffMu.Unlock()

	// Idempotency, before the length cap and before anything is appended: a
	// repeated frame is answered with what the first one produced, and a
	// duplicate must not be able to report "queue full" either. This mutex
	// is the queue path's serialization point, so two frames carrying one id
	// cannot both pass. See app_send_idempotency.go.
	if record, found, err := a.findRecordedSend(threadID, opts.SendID); err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: %w", err)
	} else if found {
		if !record.dispatched {
			return flushqueue.ItemFromStore(record.queued), nil
		}
		// The queue already handed this message to the provider between the
		// first frame and this one, so there is no queue row left to project.
		// The answer names the row the message became: Zone 1 is driven by
		// `provider:queue_state_changed`, which has already removed the
		// entry, so what matters here is that the caller reads a success and
		// does not send a second copy.
		return QueuedItem{
			ID:         record.item.ID,
			ThreadID:   threadID,
			Message:    record.item.Summary,
			EnqueuedAt: record.item.CreatedAt,
		}, nil
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
		ExpandComposerCommands:       injected.expandComposerCommands,
		SendID:                       opts.SendID,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: encode payload: %w", err)
	}

	// DURABLE FIRST, then memory. The composer clears the moment this
	// returns, so between the register and the provider write the queue row
	// is the message's only copy — and it lived in process memory, which a
	// crash or an ungraceful restart threw away with no trace on screen that
	// a message had ever existed. Writing it first means the failure mode is
	// a visible refusal to queue rather than a message that quietly is not
	// there tomorrow morning.
	enqueuedAt := time.Now().UnixMilli()
	if err := a.store.InsertFlushQueueItem(store.FlushQueueItem{
		ID:         id,
		ThreadID:   threadID,
		SendID:     opts.SendID,
		Message:    message,
		Payload:    payloadBytes,
		EnqueuedAt: enqueuedAt,
	}); err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: %w", err)
	}

	a.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{
		ID:         id,
		Message:    message,
		Payload:    payloadBytes,
		EnqueuedAt: enqueuedAt,
		Settlement: a.flushQueueSettlement(threadID, id, injected.onDurable),
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
	if !injected.preserveDraft {
		if draftErr := a.removeThreadDraft(transport.ClientIdentity{}, threadID); draftErr != nil {
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
// threads:operate rather than threads:read: the snapshot exposes the
// user's drafted-but-not-yet-sent prompts, attachment IDs, and plan
// refs, which is what a session driving the thread sees and not what a
// read-only observer signed up for.
//
//ao:scope threads:operate
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
	a.emit(eventchan.ProviderQueueStateChanged, QueueStateChangedEvent{
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
// against a 16 MiB-frame DoS vector. Attachments never reach this cap
// because they never reach this socket: they cross on their own HTTP
// route, and a queue entry carries only their ids.
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
