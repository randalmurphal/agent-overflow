package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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
// The userItemId is the opaque AO row identity allocated independently
// of placement for identified client sends. The frontend matches the
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
	SendID      string `json:"sendId,omitempty"`
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
	r.SetGeneratedImageImporter(a.importCodexGeneratedImage)
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
		// Under the SAME lock as the pop: the dispatch below can wait on
		// the thread lock for as long as a git operation takes, and the
		// batch must never be invisible to the queue snapshot in between.
		a.flushDispatch.dispatching[threadID] = batch.items
		a.flushDispatch.mu.Unlock()

		a.dispatchFlushWithGeneration(threadID, batch.items, batch.generation)

		a.flushDispatch.mu.Lock()
		delete(a.flushDispatch.current, threadID)
		delete(a.flushDispatch.dispatching, threadID)
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
	if a.flushDispatch.dispatching == nil {
		a.flushDispatch.dispatching = make(map[string][]triage.QueuedFlushItem)
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
	delete(a.flushDispatch.dispatching, threadID)
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
	delete(a.flushDispatch.dispatching, threadID)
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
// A drain is split into GROUPS (groupFlushDispatch) and each group becomes
// exactly one provider message. Codex and claude-tui take one group per item;
// headless Claude takes the whole drain as one group, because its own command
// queue merges a multi-message boundary drain into one transcript entry under
// the LAST uuid — see flushDispatchJoinsMessages and claude-wire.md
// §Queued-message consumption.
//
// Per-group flow:
//
//  1. Decode each member's QueuedFlushItem.Payload into flushQueuePayload
//     and resolve its attachments + source/revision plan refs (same shape
//     Send and Steer use). A failure here is BEFORE any durable or wire
//     effect, so nothing is sent and every member requeues in order.
//  2. Join the resolved members into one message: one content string, one
//     attachment list, one meta carrying every member's send id
//     (joinFlushMembers). A single-member group joins to itself.
//  3. Allocate a stable AO identity independently of its turn placement.
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
// On any definite group error, the dispatcher persists a sibling `error`
// row, aborts the current batch, and requeues the group plus every group
// not yet attempted.
//
// Invoked by the app-layer per-thread flush worker, after triage has released
// r.mu. The worker preserves FIFO order across multiple boundary drains and
// prevents concurrent sequence allocation for one thread.
func (a *App) dispatchFlush(threadID string, items []triage.QueuedFlushItem) {
	// The worker records the batch under the same lock as its pop; a
	// direct call records its own, so both entry paths keep the batch
	// visible to the queue snapshot for the whole dispatch.
	a.beginFlushDispatchVisibility(threadID, items)
	defer a.endFlushDispatchVisibility(threadID)
	a.dispatchFlushWithGeneration(threadID, items, a.currentFlushDispatchGeneration(threadID))
}

func (a *App) beginFlushDispatchVisibility(threadID string, items []triage.QueuedFlushItem) {
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatch.dispatching[threadID] = items
	a.flushDispatch.mu.Unlock()
}

func (a *App) endFlushDispatchVisibility(threadID string) {
	a.flushDispatch.mu.Lock()
	delete(a.flushDispatch.dispatching, threadID)
	a.flushDispatch.mu.Unlock()
}

// noteFlushDispatchItemSettled drops one item from the in-flight remainder.
// Called once its `queue_flushed` is about to be emitted (Zone 2 takes over)
// or once a failure has put it back on the triage queue (Zone 1 takes it
// back) — never in between, so no snapshot can miss it and none can show it
// twice.
func (a *App) noteFlushDispatchItemSettled(threadID, itemID string) {
	if itemID == "" {
		return
	}
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	remaining := a.flushDispatch.dispatching[threadID]
	for i, item := range remaining {
		if item.ID != itemID {
			continue
		}
		next := make([]triage.QueuedFlushItem, 0, len(remaining)-1)
		next = append(next, remaining[:i]...)
		next = append(next, remaining[i+1:]...)
		if len(next) == 0 {
			delete(a.flushDispatch.dispatching, threadID)
		} else {
			a.flushDispatch.dispatching[threadID] = next
		}
		return
	}
}

// noteFlushDispatchGroupSettled drops every member of a dispatch group: a
// joined group leaves the queue on one write, and an empty member settles
// with the group it dropped out of.
func (a *App) noteFlushDispatchGroupSettled(threadID string, group []triage.QueuedFlushItem) {
	for _, item := range group {
		a.noteFlushDispatchItemSettled(threadID, item.ID)
	}
}

// pendingFlushDispatchItems lists every queued message the App layer is
// holding for a thread: the remainder of the batch being dispatched, then
// the batches still waiting for the worker. Current generation only — a
// rollback's discarded batches are not queued for anyone.
func (a *App) pendingFlushDispatchItems(threadID string) []triage.QueuedFlushItem {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	generation := a.flushDispatch.generation[threadID]
	var out []triage.QueuedFlushItem
	out = append(out, a.flushDispatch.dispatching[threadID]...)
	for _, batch := range a.flushDispatch.queues[threadID] {
		if batch.generation != generation {
			continue
		}
		out = append(out, batch.items...)
	}
	return out
}

// queueSnapshotForThread is the ONE queue snapshot every wire surface
// publishes. A queued message is somewhere in the handoff chain at all
// times — triage queue → triage claim → App dispatch batch → in-flight
// item — and this reads the whole chain, oldest first, so no point in the
// chain can publish a snapshot that omits a message the user is still
// waiting on. The overlap between the App's batch and triage's claim (the
// instant the dispatcher is invoked, when both hold it) dedupes by id.
func (a *App) queueSnapshotForThread(threadID string) []QueuedItem {
	pending := a.pendingFlushDispatchItems(threadID)
	var queued []triage.QueuedFlushItem
	if a.triage != nil {
		queued = a.triage.QueuedFlushItems(threadID)
	}
	if len(pending)+len(queued) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(pending)+len(queued))
	out := make([]QueuedItem, 0, len(pending)+len(queued))
	for _, item := range append(pending, queued...) {
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		out = append(out, flushqueue.ItemFromTriage(threadID, item))
	}
	return out
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

	groups := a.groupFlushDispatch(threadID, items)
	for i, group := range groups {
		if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
			return
		}
		flushedItems, flushedEmitted, requeue, err := a.dispatchFlushGroup(threadID, group)
		if errors.Is(err, errEmptyUserMessage) {
			// Older clients admitted empty rows. They have no input to retry
			// or restore, and must not block the meaningful tail of the queue.
			a.noteFlushDispatchGroupSettled(threadID, group)
			settleFlushGroup(group)
			continue
		}
		if err != nil {
			log.Printf("flush dispatch: thread=%s items=[%s]: %v", threadID, flushGroupIDs(group), err)
			if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
				return
			}
			// The FAILING group requeues too, ahead of the unattempted
			// tail — dropping it would leave the message in no state at
			// all (round-13, CT13-1/C13-2). Its StaleUserItemID reflects
			// how far dispatch got: the original marker when cleanup
			// never ran or failed, cleared once cleanup succeeded, the
			// fresh row id once a quiet persist landed.
			// Requeue FIRST, then drop the in-flight record: the triage
			// queue has the item back before this layer stops vouching
			// for it, so a snapshot from another goroutine in between
			// still sees it.
			for _, item := range requeue {
				a.triage.RegisterQueueItem(threadID, item)
			}
			for _, unattempted := range groups[i+1:] {
				for _, item := range unattempted {
					a.triage.RegisterQueueItem(threadID, item)
				}
			}
			for _, settled := range groups[i:] {
				a.noteFlushDispatchGroupSettled(threadID, settled)
			}
			a.emitQueueStateChanged(threadID)
			return
		}
		if !flushedEmitted {
			// Zone 2 takes the message over here, so this layer stops
			// publishing it as queued in the same step.
			a.noteFlushDispatchGroupSettled(threadID, group)
			a.emit(eventchan.ProviderQueueFlushed, QueueFlushedEvent{
				ThreadID: threadID,
				Items:    flushedItems,
			})
		}
		// A successful provider write is one of the two settlement endpoints for
		// an injected message; session-death recovery into the composer is the
		// other. The shared settlement is exactly-once if those paths race, and
		// every member of a joined group settles on the one write that carried
		// it.
		settleFlushGroup(group)
	}
	a.emitQueueStateChanged(threadID)
}

func settleFlushGroup(group []triage.QueuedFlushItem) {
	for _, item := range group {
		item.Settlement.Settle()
	}
}

func flushGroupIDs(group []triage.QueuedFlushItem) string {
	ids := make([]string, 0, len(group))
	for _, item := range group {
		ids = append(ids, item.ID)
	}
	return strings.Join(ids, " ")
}

// dispatchFlushGroup dispatches one group as a single provider message. On
// error the returned requeue value is the group to re-register in order
// (round-13, CT13-1): copies of the input whose StaleUserItemID tracks the
// durable state left behind — unchanged until the stale-row cleanup runs,
// cleared once cleanup succeeds, and, on the FIRST member only, pointing at
// the fresh quiet row once an eager persist lands (the group persists one row,
// so one member owns the obligation to clean it up before persisting again).
// On success requeue is nil.
func (a *App) dispatchFlushGroup(threadID string, group []triage.QueuedFlushItem) ([]QueueFlushedItem, bool, []triage.QueuedFlushItem, error) {
	requeue := slices.Clone(group)
	if a.shuttingDown.Load() {
		return nil, false, requeue, ErrShuttingDown
	}

	// Resolve EVERY member before anything durable or provider-visible
	// happens. A joined group is one message: a failure on its third member
	// must leave all three requeueable with nothing on the wire.
	members := make([]flushMember, 0, len(group))
	var staleRows []string
	for _, item := range group {
		var payload flushQueuePayload
		if len(item.Payload) > 0 {
			if err := json.Unmarshal(item.Payload, &payload); err != nil {
				return nil, false, requeue, fmt.Errorf("decode payload: %w", err)
			}
		}
		if item.StaleUserItemID != "" {
			staleRows = append(staleRows, item.StaleUserItemID)
		}
		if err := validateUserMessageInput(item.Message, payload.AttachmentIDs, payload.RevisionSourceCommentIDs, payload.RevisionSourceDiffCommentIDs); err != nil {
			// An empty member carries no input to send. It drops out of the
			// join and settles with the group; its stale row is still cleaned
			// up below with everyone else's.
			continue
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
			// its whole life (app_send_idempotency.go). A joined row carries
			// every member's id; joinFlushMeta owns that union.
			sendID: payload.SendID,
		})
		if err != nil {
			return nil, false, requeue, err
		}
		members = append(members, flushMember{item: item, payload: payload, resolved: resolved})
	}
	if len(members) == 0 {
		if err := a.cleanupStaleFlushRows(threadID, staleRows); err != nil {
			return nil, false, requeue, err
		}
		return nil, false, requeue, errEmptyUserMessage
	}

	a.ensureTriageRouter()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, false, requeue, fmt.Errorf("load thread: %w", err)
	}
	if err := a.ensureClaudeContextReadyForUserSendLocked(thread); err != nil {
		return nil, false, requeue, err
	}
	sess, unlockAccount, err := a.lockProviderAccountForSendLocked(thread)
	if err != nil {
		return nil, false, requeue, err
	}
	defer unlockAccount()

	placement, err := a.resolveUserMessagePlacement(thread, messageFlush)
	if err != nil {
		return nil, false, requeue, fmt.Errorf("resolve placement: %w", err)
	}
	responseTurnIndex := placement.responseTurn
	persistTurnIndex := placement.displayTurn
	eagerPersist := placement.persistence == messagePersistQuiet

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
	// ONE uuid per group: a joined message is one envelope, so a second
	// uuid would name a row the transcript never gets.
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

	if len(staleRows) > 0 {
		// A previous dispatch of this message left a quiet row whose
		// session-death cleanup failed (see QueuedFlushItem doc). Retry
		// it here — AFTER every failure-prone resolution step above, so
		// an envelope/thread/session/placement error aborts the group
		// while the stale row (the message's only durable copy) is
		// still intact for the next retry (round-12, D12-1) — and
		// BEFORE nextFlushUserItemID, which allocates against the
		// turn's persisted rows. On cleanup failure, abort the group:
		// persisting a fresh row over the stale one would show the
		// message twice (round-11, R11-1). The remaining loss windows
		// (allocation, persist, send) are the same ones a first
		// dispatch of any message already has.
		if err := a.cleanupStaleFlushRows(threadID, staleRows); err != nil {
			return nil, false, requeue, err
		}
		for i := range requeue {
			requeue[i].StaleUserItemID = ""
		}
	}

	joined, err := joinFlushMembers(members)
	if err != nil {
		return nil, false, requeue, fmt.Errorf("join queued messages: %w", err)
	}

	flushItemID, err := a.userMessageItemID(threadID, joined.sendID, persistTurnIndex, messageFlush)
	if err != nil {
		return nil, false, requeue, fmt.Errorf("allocate item id: %w", err)
	}
	now := time.Now().UnixMilli()
	userItem := store.Item{
		ID:        flushItemID,
		ThreadID:  threadID,
		TurnIndex: persistTurnIndex,
		Kind:      "user_text",
		Role:      "user",
		Status:    "completed",
		Summary:   joined.content,
		Meta:      joined.meta,
		CreatedAt: now,
		UpdatedAt: now,
	}

	// One acknowledgement per queued message the user is watching, all naming
	// the one row they became: markItemsFlushed drops each queue id from
	// Zone 1 and de-duplicates the additions by userItemId, so the overlay
	// shows the joined message once and its echo clears it once.
	flushedItems := make([]QueueFlushedItem, 0, len(members))
	for _, member := range members {
		flushedItems = append(flushedItems, QueueFlushedItem{
			QueueItemID: member.item.ID,
			UserItemID:  userItem.ID,
			Message:     joined.content,
			SendID:      member.payload.SendID,
		})
	}

	// The identity this dispatch will be recognised by — wire stamp and
	// registry expectation derived together (providerSendIdentity), so
	// the two cannot drift.
	clientUserMessageID, sendExpect := providerSendIdentity(sess, userItem.ID, sendUUID)

	// The group's queue identity for the pending-send entry is its first
	// member's: one entry per outbound message, and the session-death restore
	// reads the entry's own joined row rather than the queue ids.
	leadQueueItemID := members[0].item.ID
	leadEnqueuedAt := members[0].item.EnqueuedAt

	if eagerPersist {
		// Emit queue_flushed so the frontend creates the Zone 2 entry
		// (queued marker above the composer). Persist the row quietly —
		// no provider:item_event — so the item reserves its timeline
		// position in SQLite but stays as a queued marker in the UI
		// until the provider echo confirms it entered context.
		a.noteFlushDispatchGroupSettled(threadID, group)
		a.emit(eventchan.ProviderQueueFlushed, QueueFlushedEvent{
			ThreadID: threadID,
			Items:    flushedItems,
		})
		if persistErr := a.triage.PersistAndRegisterPendingQuietFlushSendWithExpectation(
			threadID, leadQueueItemID, userItem, responseTurnIndex, leadEnqueuedAt, sendExpect); persistErr != nil {
			return nil, true, requeue, fmt.Errorf("eager persist flush: %w", persistErr)
		}
		// One row, one cleanup obligation: the first member owns it.
		requeue[0].StaleUserItemID = userItem.ID

	} else {
		// Deferred: row persists at echo time via persistDeferredUserText.
		a.triage.RegisterPendingFlushSendWithExpectation(threadID, leadQueueItemID, userItem, leadEnqueuedAt, sendExpect)
	}

	sendOpts := provider.SendOptions{
		InteractionMode: provider.NormalizeInteractionMode(thread.Mode),
		Attachments:     joined.attachments,
		UserMessageUUID: sendUUID,
		// Codex's half of the same identity (empty for every other provider):
		// stamped on both `turn/steer` and the fresh-turn fallback, and echoed
		// back on the `userMessage` item's `clientId`.
		ClientUserMessageID: clientUserMessageID,
		// Agent Overflow's own expanded command must bypass Claude's local
		// router. Every other leading `/name` keeps Claude's native command
		// semantics, independent of discovery timing.
		GuardClaudeSlashCommand: joined.guardSlashCommand,
	}

	dispatchErr := a.dispatchFlushToProvider(sess, joined.providerContent, sendOpts)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s items=[%s]: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, flushGroupIDs(group))
			a.applyJoinedPlanAcceptance(threadID, userItem, members)
			return flushedItems, eagerPersist, nil, nil
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
			log.Printf("flush dispatch: thread=%s items=[%s]: the active codex turn cannot take input (%v); leaving the message queued for the next turn boundary",
				threadID, flushGroupIDs(group), dispatchErr)
			return nil, eagerPersist, requeue, dispatchErr
		}
		if sess.Codex != nil && codex.IsNoActiveTurnRace(dispatchErr) {
			a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
			fallback, allocErr := a.resolveUserMessagePlacement(thread, messageFlushFallback)
			if allocErr != nil {
				return nil, eagerPersist, requeue, allocErr
			}
			responseTurnIndex = fallback.responseTurn
			freshFlushItemID, allocErr := a.userMessageItemID(threadID, joined.sendID, responseTurnIndex, messageFlushFallback)
			if allocErr != nil {
				a.persistFlushDispatchError(threadID, responseTurnIndex, allocErr)
				return nil, eagerPersist, requeue, allocErr
			}
			userItem.ID = freshFlushItemID
			userItem.TurnIndex = responseTurnIndex
			userItem.CreatedAt = time.Now().UnixMilli()
			userItem.UpdatedAt = userItem.CreatedAt
			// Legacy callers may get a new row id. Derive both
			// wire stamp and expectation together; identified client sends
			// preserve both across this placement-only fallback.
			var refreshExpect triage.PendingSendExpectation
			sendOpts.ClientUserMessageID, refreshExpect = providerSendIdentity(sess, userItem.ID, "")
			a.triage.RegisterPendingFlushSendWithExpectation(
				threadID, leadQueueItemID, userItem, leadEnqueuedAt, refreshExpect)
			sess.Liveness.BumpActivity(time.Now())
			if sendErr := sess.Codex.Send(context.Background(), joined.providerContent, sendOpts); sendErr != nil {
				if codex.IsAmbiguousTurnStartTimeout(sendErr) {
					// Same ambiguity as the steer timeout above: the
					// turn/start was written and the echo may already be
					// coming. A requeue would double-send (round-14,
					// D14-2) — leave the pending entry for the echo.
					log.Printf("flush dispatch: thread=%s items=[%s]: codex turn/start timed out after write; leaving pending confirmation for provider echo", threadID, flushGroupIDs(group))
					a.applyJoinedPlanAcceptance(threadID, userItem, members)
					return retargetFlushedItems(flushedItems, userItem.ID), eagerPersist, nil, nil
				}
				a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
				a.persistFlushDispatchError(threadID, responseTurnIndex, sendErr)
				return nil, eagerPersist, requeue, sendErr
			}
			a.applyJoinedPlanAcceptance(threadID, userItem, members)
			return retargetFlushedItems(flushedItems, userItem.ID), eagerPersist, nil, nil
		}
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, persistTurnIndex, dispatchErr)
		return nil, eagerPersist, requeue, dispatchErr
	}
	a.applyJoinedPlanAcceptance(threadID, userItem, members)
	return flushedItems, eagerPersist, nil, nil
}

// applyJoinedPlanAcceptance marks each member's plan / comment bookkeeping
// against the single row the group became. The refs are per-message even when
// the message is joined, so the marking loop is too; the joined row's meta
// carries only the refs it renders (joinFlushMeta).
func (a *App) applyJoinedPlanAcceptance(threadID string, userItem store.Item, members []flushMember) {
	for _, member := range members {
		a.applyProposedPlanAcceptance(threadID, userItem, member.resolved)
	}
}

// retargetFlushedItems re-points a group's acknowledgements at the row id a
// placement-only fallback re-allocated.
func retargetFlushedItems(items []QueueFlushedItem, userItemID string) []QueueFlushedItem {
	for i := range items {
		items[i].UserItemID = userItemID
	}
	return items
}

// cleanupStaleFlushRows retries the session-death cleanup for every quiet row
// a previous dispatch of this group left behind. Each delete is idempotent, so
// a group whose members were dispatched separately before being joined cleans
// up all of their rows.
func (a *App) cleanupStaleFlushRows(threadID string, userItemIDs []string) error {
	for _, id := range userItemIDs {
		if err := a.cleanupStaleFlushRow(threadID, id); err != nil {
			return fmt.Errorf("cleanup stale flush row %s: %w", id, err)
		}
	}
	return nil
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
	unlockAdmission, err := a.lockSendAdmission(ctx, threadID, opts.SendID)
	if err != nil {
		return QueuedItem{}, err
	}
	defer unlockAdmission()

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
	// persist atomically transfers an injector’s delivery responsibility to the
	// ordinary durable queue. Nil uses the standard queue insert.
	persist func(store.FlushQueueItem) error
}

// flushQueueSettlement is the dispatch-or-restore hook every queued message
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
// been dispatched or restored, so the boot sweep may
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
			SendID:     opts.SendID,
			ThreadID:   threadID,
			Message:    record.item.Summary,
			EnqueuedAt: record.item.CreatedAt,
		}, nil
	}

	if err := validateUserMessageInput(message, opts.AttachmentIDs, opts.RevisionSourceCommentIDs, opts.RevisionSourceDiffCommentIDs); err != nil {
		return QueuedItem{}, fmt.Errorf("register queue item: %w", err)
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
	persist := injected.persist
	if persist == nil {
		persist = a.store.InsertFlushQueueItem
	}
	if err := persist(store.FlushQueueItem{
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
		SendID:                       opts.SendID,
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
		if draftErr := a.removeThreadDraft(transport.ClientIdentity{}, threadID, opts.ConsumeDraft); draftErr != nil {
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
	return a.queueSnapshotForThread(threadID), nil
}

// emitQueueStateChanged emits the post-mutation queue snapshot on
// `provider:queue_state_changed`. Always re-reads the whole handoff
// chain (`queueSnapshotForThread`) so the wire payload is
// authoritative — observers don't have to combine deltas to get
// state, and a snapshot raised while another message is mid-dispatch
// cannot tell them to drop it.
func (a *App) emitQueueStateChanged(threadID string) {
	items := a.queueSnapshotForThread(threadID)
	if items == nil {
		items = []QueuedItem{}
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
