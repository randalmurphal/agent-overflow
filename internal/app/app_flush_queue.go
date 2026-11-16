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

	"agent-overflow/internal/agentmeta"
	attachmentstore "agent-overflow/internal/attachment"
	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/provider/claude"
	"agent-overflow/internal/provider/codex"
	"agent-overflow/internal/store"
	"agent-overflow/internal/transport"
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
	// items are the messages the batch has not finished writing: a group
	// leaves when it settles or goes back to the queue, and a message whose
	// row is gone at its group's commit point is dropped (commitFlushGroup).
	items      []triage.QueuedFlushItem
	generation uint64
	// writing are the ids of the group the worker committed to write and
	// has not settled or requeued yet: a withdrawal finds them dispatched.
	writing map[string]struct{}
}

// QueueFlushedEvent is emitted by `dispatchFlushWithGeneration` at the start of a
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
	// AgentMessage is agentmeta.IsAgentMessage of the row the message
	// became: an agent's message, which the queue preview leaves out.
	AgentMessage bool `json:"agentMessage,omitempty"`
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
	a.triage.SetFlushSendConsumedHook(a.confirmOwnedDeliveries)
	a.triage.SetSendDeliveredHook(a.onSendDelivered)
	a.triage.SetSendsPendingObserver(a.markSendsPendingDirty)
	a.triage.SetSendUndeliveredHook(func(threadID string) {
		// Off the provider event worker: the restore takes the thread
		// action lock, which an interrupt holds while it waits on this
		// worker for its ack.
		go a.restoreUndeliveredSends(threadID)
	})
}

// newTriageRouter constructs the triage router with every App-owned
// observer wired. ALL construction sites must route through this —
// a router built bare would silently drop the streaming observers.
func (a *App) newTriageRouter(st *store.Store) *triage.Router {
	r := triage.NewRouter(st, a.emitWithReplay())
	r.SetThreadAnnouncer(a.threadAnnouncer())
	r.SetAssistantTextObservers(a.highlightService().ObserveAssistantTextDelta, a.highlightService().EndAssistantText)
	r.SetDiffPayloadObserver(a.observeDiffPayloadPersisted)
	r.SetCommandSettledObserver(a.observeCommandSettled)
	r.SetCodeSpanEnricher(a.buildPersistedCodeSpans)
	r.SetGeneratedImageImporter(a.importCodexGeneratedImage)
	r.SetPromptRouter(a.routeWorkerPrompt)
	r.SetQuietTurnClassifier(a.agentTurnIsTick)
	r.SetUnsavedRowObserver(unsavedRowSink{app: a})
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
	if hook := a.flushHandoffForTest; hook != nil {
		hook(threadID)
	}
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	// The generation the queue handed the messages out in: a handoff a
	// teardown or a recovery overtook is not this session's to send.
	generation := items[0].DispatchGeneration
	stale := generation != a.currentFlushDispatchGeneration(threadID)
	if !stale {
		a.flushDispatch.queues[threadID] = append(a.flushDispatch.queues[threadID], flushDispatchBatch{
			items:      slices.Clone(items),
			generation: generation,
		})
		a.flushDispatch.inflightItems[threadID] += len(items)
		a.markSendsPendingDirty(threadID)
	}
	// The worker may finish before this callback returns. Retire triage's
	// overlapping claim before starting it, so a later snapshot cannot
	// resurrect a message whose dispatch has already settled.
	a.triage.ReleaseFlushClaim(threadID, items)
	switch {
	case stale && a.triage.FlushGenerationDropped(threadID, generation):
		// A Stop's teardown or a revert threw its generation's queue away,
		// and the handoff goes with it: its rows went with that drop.
	case stale:
		a.flushDispatch.wg.Add(1)
		go a.restoreStaleFlushHandoff(threadID, slices.Clone(items))
	default:
		a.startFlushDispatchWorkerLocked(threadID)
	}
	a.flushDispatch.mu.Unlock()
}

// restoreStaleFlushHandoff takes the messages of a handoff that a recovery
// overtook (enqueueFlushDispatch) as that recovery took the rest of the
// queue: back to the composer, or to their owners, while their rows stand.
// A withdrawal took the rows of the others, and they go nowhere
// (restoreFlushItemsToDraftLocked).
func (a *App) restoreStaleFlushHandoff(threadID string, items []triage.QueuedFlushItem) {
	defer a.flushDispatch.wg.Done()
	unlock := a.threadLocks().Lock(threadID)
	defer unlock()
	drained := make([]triage.UnconfirmedFlushItem, 0, len(items))
	for _, item := range items {
		drained = append(drained, unconfirmedFromQueued(item))
	}
	sortByEnqueuedAt(drained)
	a.restoreFlushItemsToDraftLocked(threadID, drained, queueRestoredReasonSessionDied)
}

// unconfirmedFromQueued is a queued message, never handed to the provider,
// in the form a restore takes.
func unconfirmedFromQueued(item triage.QueuedFlushItem) triage.UnconfirmedFlushItem {
	return triage.UnconfirmedFlushItem{
		QueueItemID:     item.ID,
		Message:         item.Message,
		Payload:         append(json.RawMessage(nil), item.Payload...),
		EnqueuedAt:      item.EnqueuedAt,
		StaleUserItemID: item.StaleUserItemID,
		Settlement:      item.Settlement,
		Queued:          true,
		NoRow:           item.NoRow,
	}
}

// startFlushDispatchWorkerLocked starts threadID's worker unless one runs.
// The caller holds flushDispatch.mu.
func (a *App) startFlushDispatchWorkerLocked(threadID string) {
	if a.flushDispatch.running[threadID] {
		return
	}
	a.flushDispatch.running[threadID] = true
	a.flushDispatch.wg.Add(1)
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
		if hook := a.flushDispatchReturnedForTest; hook != nil {
			hook(threadID)
		}

		a.flushDispatch.mu.Lock()
		delete(a.flushDispatch.current, threadID)
		delete(a.flushDispatch.dispatching, threadID)
		a.markSendsPendingDirty(threadID)
		if a.currentFlushDispatchGeneration(threadID) == batch.generation {
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

// pendingFlushWorkCount sums every queued and in-flight follow-up message,
// the work idle eviction and a conversation transfer must not lose. It
// reads three counters that the flush handoff updates non-atomically
// (triage queue length, deferred pending count, App inflight count), so it
// holds a.flushDispatch.handoffMu across all three: the mutex
// RegisterQueueItem holds across its enqueue-to-flush handoff. A message
// mid-handoff is then counted as still queued or already in flight, never
// neither.
//
// The lock-free boundary drains do not hold handoffMu; for them the triage
// claim count (see tryFlushQueue) keeps a draining batch inside
// QueuedFlushItemCount until the App inflight count has it. That overlap
// closes the gap only if the triage counts are read first: a batch moving
// from claimed to in flight between the reads is double-counted, never
// missed. Do not reorder these reads.
//
// Callers hold the per-thread action lock, not handoffMu.
func (a *App) pendingFlushWorkCount(threadID string) int {
	a.flushDispatch.handoffMu.Lock()
	defer a.flushDispatch.handoffMu.Unlock()
	total := 0
	if a.triage != nil {
		total += a.triage.QueuedFlushItemCount(threadID)
		total += a.triage.DeferredPendingFlushItemCount(threadID)
	}
	total += a.flushDispatchItemCount(threadID)
	return total
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
	if a.flushDispatch.flushUnsettled == nil {
		a.flushDispatch.flushUnsettled = make(map[string]struct{})
	}
}

// dropFlushQueue throws away the thread's queue for a revert: the rollback
// is deleting the history these messages were queued against, so they are
// thrown away rather than deferred (dropFlushQueueLocked). Under the
// mutation lock, as a teardown's drop is (settleQueueForTeardown), so a
// message queued during it is in the queue it reads or queued after it.
func (a *App) dropFlushQueue(threadID string) {
	unlock := a.threadApplication().LockMutations(threadID)
	held, _ := a.dropFlushQueueLocked(threadID)
	// A revert that keeps its session (Codex's in-place cut) leaves triage's
	// queue alone, so a message still there is delivered after it, or
	// dropped and returned by the stop a cold cut makes next. What the
	// drop took is gone now and goes back to its owner.
	if a.triage != nil {
		for _, item := range a.triage.QueuedFlushItems(threadID) {
			held = slices.DeleteFunc(held, func(m ownedMessage) bool { return m.rowID == item.ID })
		}
	}
	unlock()
	a.giveBackToOwners(threadID, held)
}

// dropFlushQueueLocked throws away the thread's queued messages that have
// left triage for the dispatch worker, durable rows included: a rollback
// deleting the history they were queued against, and a teardown dropping the
// queue. A surviving row would make the next boot restore a message the
// user's revert or Stop discarded, and a surviving batch would be dispatched
// to the next session: the worker, once it has the action lock, finds its
// generation gone and sends nothing, and a handoff still on its way arrives
// stale and goes with them (enqueueFlushDispatch). The caller clears
// triage's own queue and holds the thread's mutation lock.
//
// It returns the answers, reminders and remote completions the durable drop
// kept (dropDurableFlushQueue), for the caller to give back to their owners
// once the rest of the queue is gone, and the batches it took from the
// worker, for a caller that must also retire the rows a requeued message
// still has (QueuedFlushItem.StaleUserItemID).
func (a *App) dropFlushQueueLocked(threadID string) ([]ownedMessage, []triage.QueuedFlushItem) {
	held := a.dropDurableFlushQueue(threadID)
	return held, a.takeFlushDispatch(threadID, true)
}

// takeFlushDispatch takes every message the dispatch holds for a thread and
// has not written, and starts the thread's next generation, so the worker
// sends none of them and a handoff still on its way arrives stale. dropped
// says the caller throws the queue away (a Stop's teardown, a revert), and
// such a handoff goes with it; a session end's restore takes them back
// (advanceFlushDispatchGenerationLocked).
func (a *App) takeFlushDispatch(threadID string, dropped bool) []triage.QueuedFlushItem {
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.advanceFlushDispatchGenerationLocked(threadID, dropped)
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
	a.markSendsPendingDirty(threadID)
	a.flushDispatch.mu.Unlock()
	return drained
}

// wantedFlushItems reads, in one statement, which of items are still
// wanted: those whose durable row stands, and those that never had one
// (triage.QueuedFlushItem.NoRow). A withdrawal deletes the row, so a copy
// of a withdrawn message fails it wherever it is. A failed read keeps every
// message: a group's commit point reads again before any is sent.
func (a *App) wantedFlushItems(threadID string, items []triage.QueuedFlushItem) func(triage.QueuedFlushItem) bool {
	present, err := a.store.FlushQueueItemsPresent(threadID, rowIDs(items))
	if err != nil {
		log.Printf("flush queue: read which queued messages of %s are wanted: %v", threadID, err)
		return func(triage.QueuedFlushItem) bool { return true }
	}
	return func(item triage.QueuedFlushItem) bool { return item.NoRow || present[item.ID] }
}

// rowIDs are the ids of items that have a durable row.
func rowIDs(items []triage.QueuedFlushItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !item.NoRow {
			ids = append(ids, item.ID)
		}
	}
	return ids
}

// requeueWantedFlushItems puts items, which a dispatch took and did not
// write, back on the queue for its next trigger, less those whose rows a
// withdrawal took meanwhile, under the mutation lock the withdrawal takes.
// The caller holds the thread's action lock.
func (a *App) requeueWantedFlushItems(threadID string, items []triage.QueuedFlushItem) {
	unlock := a.threadApplication().LockMutations(threadID)
	defer unlock()
	wanted := a.wantedFlushItems(threadID, items)
	for _, item := range items {
		if wanted(item) {
			a.triage.RegisterQueueItem(threadID, item)
		}
	}
}

// commitFlushGroup is a group's commit point, before anything of the group
// is persisted or written: under the thread's mutation lock, which a
// withdrawal takes, it reads in one statement which of the group's messages
// still have their rows, drops the others from the group and from the batch
// the worker holds, and marks the rest as being written, which a
// withdrawal from now on finds dispatched (flushItemWritten). A withdrawal
// and a send of one message are thereby ordered: whichever takes the lock
// first wins. The caller holds the thread's action lock.
func (a *App) commitFlushGroup(threadID string, generation uint64, group []triage.QueuedFlushItem) ([]triage.QueuedFlushItem, error) {
	unlock := a.threadApplication().LockMutations(threadID)
	defer unlock()
	present, err := a.store.FlushQueueItemsPresent(threadID, rowIDs(group))
	if err != nil {
		return nil, err
	}
	sending := make([]triage.QueuedFlushItem, 0, len(group))
	writing := make(map[string]struct{}, len(group))
	dropped := make(map[string]struct{})
	for _, item := range group {
		if item.NoRow || present[item.ID] {
			sending = append(sending, item)
			writing[item.ID] = struct{}{}
		} else {
			dropped[item.ID] = struct{}{}
		}
	}
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	if current, ok := a.flushDispatch.current[threadID]; ok && current.generation == generation {
		current.items = withoutIDs(current.items, dropped)
		current.writing = writing
		a.flushDispatch.current[threadID] = current
	}
	if len(dropped) > 0 {
		a.forgetDispatchingLocked(threadID, dropped)
	}
	return sending, nil
}

// finishFlushGroup takes a group the worker settled or put back on the
// queue out of the batch it holds, and ends its write.
func (a *App) finishFlushGroup(threadID string, generation uint64, group []triage.QueuedFlushItem) {
	done := make(map[string]struct{}, len(group))
	for _, item := range group {
		done[item.ID] = struct{}{}
	}
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	current, ok := a.flushDispatch.current[threadID]
	if !ok || current.generation != generation {
		return
	}
	current.items = withoutIDs(current.items, done)
	current.writing = nil
	a.flushDispatch.current[threadID] = current
}

// withoutIDs is items less those ids names, in a new slice.
func withoutIDs(items []triage.QueuedFlushItem, ids map[string]struct{}) []triage.QueuedFlushItem {
	return slices.DeleteFunc(slices.Clone(items), func(item triage.QueuedFlushItem) bool {
		_, gone := ids[item.ID]
		return gone
	})
}

// forgetDispatchingLocked takes ids out of the not-yet-delivered remainder
// the queue snapshot shows. The caller holds flushDispatch.mu.
func (a *App) forgetDispatchingLocked(threadID string, ids map[string]struct{}) {
	if remaining := withoutIDs(a.flushDispatch.dispatching[threadID], ids); len(remaining) == 0 {
		delete(a.flushDispatch.dispatching, threadID)
	} else {
		a.flushDispatch.dispatching[threadID] = remaining
	}
	a.markSendsPendingDirty(threadID)
}

// flushItemWritten reports whether a dispatch committed to write the queued
// message row is the row of, or wrote it and the provider has not confirmed
// it (an answer's row stands until its echo): a withdrawal finds it
// dispatched. The caller holds the thread's mutation lock, which a group's
// commit point takes, so no group commits meanwhile.
func (a *App) flushItemWritten(threadID string, row store.FlushQueueItem) bool {
	a.flushDispatch.mu.Lock()
	current, ok := a.flushDispatch.current[threadID]
	_, writing := current.writing[row.ID]
	writing = ok && writing && current.generation == a.currentFlushDispatchGeneration(threadID)
	a.flushDispatch.mu.Unlock()
	if writing {
		return true
	}
	if a.triage == nil {
		return false
	}
	for _, sent := range a.triage.UnconfirmedFlushSends(threadID) {
		if sent.QueueItemID == row.ID || (row.SendID != "" && slices.Contains(unconfirmedMembersOf(sent).ids, row.SendID)) {
			return true
		}
	}
	return false
}

// forgetUnsentCopies takes a withdrawn message's copies out of what the
// queue snapshot shows: triage's queue and the batches the dispatch holds.
// The message's row is the authority, so a copy this does not reach, as in
// triage's handoff, is dropped at the dispatch's commit point; this only
// keeps the snapshot from listing it until then.
func (a *App) forgetUnsentCopies(threadID, id string) {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	a.ensureFlushDispatchMapsLocked()
	if a.triage != nil {
		a.triage.RemoveQueuedFlushItem(threadID, id)
	}
	gone := map[string]struct{}{id: {}}
	queue := a.flushDispatch.queues[threadID]
	for i, batch := range queue {
		kept := withoutIDs(batch.items, gone)
		if len(kept) == len(batch.items) {
			continue
		}
		if len(kept) == 0 {
			queue = slices.Delete(queue, i, i+1)
		} else {
			batch.items = kept
			queue[i] = batch
		}
		a.flushDispatch.queues[threadID] = queue
		if a.flushDispatch.inflightItems[threadID]--; a.flushDispatch.inflightItems[threadID] <= 0 {
			delete(a.flushDispatch.inflightItems, threadID)
		}
		break
	}
	if current, ok := a.flushDispatch.current[threadID]; ok {
		current.items = withoutIDs(current.items, gone)
		a.flushDispatch.current[threadID] = current
	}
	a.forgetDispatchingLocked(threadID, gone)
}

// currentFlushDispatchGeneration is the thread's dispatch generation, kept
// where the queue stamps each handoff with it
// (triage.FlushDispatchGeneration).
func (a *App) currentFlushDispatchGeneration(threadID string) uint64 {
	if a.triage == nil {
		return 0
	}
	return a.triage.FlushDispatchGeneration(threadID)
}

// advanceFlushDispatchGenerationLocked starts the thread's next dispatch
// generation; dropped says the queue was thrown away, not recovered
// (triage.AdvanceFlushDispatchGeneration). The caller holds
// flushDispatch.mu, so a worker reading the generation under it sees the
// bump with the bookkeeping it went with.
func (a *App) advanceFlushDispatchGenerationLocked(threadID string, dropped bool) {
	if a.triage != nil {
		a.triage.AdvanceFlushDispatchGeneration(threadID, dropped)
	}
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
// the dispatch + RegisterQueueItem don't need cosmetic churn.
type flushQueuePayload = flushqueue.Payload

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
	a.noteFlushDispatchItemSettledLocked(threadID, itemID)
}

func (a *App) noteFlushDispatchItemSettledLocked(threadID, itemID string) {
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
		a.markSendsPendingDirty(threadID)
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

// pendingFlushDispatchItemsLocked lists every queued message the App layer is
// holding for a thread: the remainder of the batch being dispatched, then
// the batches still waiting for the worker. Current generation only — a
// rollback's discarded batches are not queued for anyone.
func (a *App) pendingFlushDispatchItemsLocked(threadID string) []triage.QueuedFlushItem {
	generation := a.currentFlushDispatchGeneration(threadID)
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

// pendingFlushDispatchItems is pendingFlushDispatchItemsLocked for a caller
// that does not hold the dispatch lock.
func (a *App) pendingFlushDispatchItems(threadID string) []triage.QueuedFlushItem {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	return a.pendingFlushDispatchItemsLocked(threadID)
}

// queueSnapshotForThread is the ONE queue snapshot every wire surface
// publishes. A queued message is somewhere in the handoff chain at all
// times — triage queue → triage claim → App dispatch batch → in-flight
// item — and this reads the whole chain, oldest first, so no point in the
// chain can publish a snapshot that omits a message the user is still
// waiting on. The overlap between the App's batch and triage's claim (the
// instant the dispatcher is invoked, when both hold it) dedupes by id.
func (a *App) queueSnapshotForThread(threadID string) []QueuedItem {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	return a.queueSnapshotForThreadLocked(threadID)
}

// Lock order is dispatch bookkeeping before triage. The triage dispatcher
// always runs outside the router lock. Holding both stops a snapshot from
// missing a message moving from triage into the worker.
func (a *App) queueSnapshotForThreadLocked(threadID string) []QueuedItem {
	pending := a.pendingFlushDispatchItemsLocked(threadID)
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

// dispatchFlushWithGeneration sends one batch under the thread's action
// lock, starting the thread's session first when it has none
// (lockFlushDispatch). Each group then has its commit point
// (commitFlushGroup), right before anything of it is persisted or written: a
// message withdrawn before it is not sent, and a withdrawal after it finds the
// message dispatched.
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
//     Publish queue_flushed after quiet persistence and before the
//     provider write. The pending registry then covers live-state reads.
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
// not yet attempted, less the messages withdrawn meanwhile
// (requeueWantedFlushItems).
func (a *App) dispatchFlushWithGeneration(threadID string, items []triage.QueuedFlushItem, generation uint64) {
	if len(items) == 0 {
		return
	}

	unlock, ready := a.lockFlushDispatch(threadID, items, generation)
	if !ready {
		return
	}
	defer unlock()

	groups := a.groupFlushDispatch(threadID, items)
	for i, group := range groups {
		if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
			return
		}
		if hook := a.flushGroupCommitForTest; hook != nil {
			hook(threadID, false)
		}
		committed, err := a.commitFlushGroup(threadID, generation, group)
		if err != nil {
			// Nothing of this group or the rest was sent, and without the
			// rows read none is known withdrawn: all of it goes back.
			log.Printf("flush dispatch: thread=%s items=[%s]: %v", threadID, flushGroupIDs(group), err)
			for _, unattempted := range groups[i:] {
				for _, item := range unattempted {
					a.triage.RegisterQueueItem(threadID, item)
				}
				a.noteFlushDispatchGroupSettled(threadID, unattempted)
				a.finishFlushGroup(threadID, generation, unattempted)
			}
			a.emitQueueStateChanged(threadID)
			return
		}
		if hook := a.flushGroupCommitForTest; hook != nil {
			hook(threadID, true)
		}
		if len(committed) == 0 {
			continue
		}
		group = committed
		flushedItems, flushedEmitted, requeue, written, err := a.dispatchFlushGroup(threadID, group)
		if errors.Is(err, errEmptyUserMessage) {
			// Older clients admitted empty rows. They have no input to retry
			// or restore, and must not block the meaningful tail of the queue.
			a.noteFlushDispatchGroupSettled(threadID, group)
			a.settleFlushGroup(threadID, group, "")
			a.finishFlushGroup(threadID, generation, group)
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
			// still sees it. The failed group's write ends before the
			// requeue reads its rows, so a withdrawal from then on removes
			// the message rather than finding it dispatched.
			back := slices.Clone(requeue)
			for _, unattempted := range groups[i+1:] {
				back = append(back, unattempted...)
			}
			a.finishFlushGroup(threadID, generation, back)
			a.requeueWantedFlushItems(threadID, back)
			a.noteFlushDispatchGroupSettled(threadID, back)
			a.emitQueueStateChanged(threadID)
			return
		}
		if !flushedEmitted {
			// Zone 2 takes the message over here, so this layer stops
			// publishing it as queued in the same step.
			a.publishFlushDispatch(threadID, group, flushedItems)
		}
		// A successful provider write is one of the two settlement endpoints for
		// an injected message; session-death recovery into the composer is the
		// other. The shared settlement is exactly-once if those paths race, and
		// every member of a joined group settles on the one write that carried
		// it.
		a.settleFlushGroup(threadID, group, written)
		a.finishFlushGroup(threadID, generation, group)
	}
	a.emitQueueStateChanged(threadID)
}

// lockFlushDispatch takes the thread's action lock for one batch's dispatch
// and readies the thread to take it. It reports whether the batch is to be
// sent; only then does the caller hold the lock and release it with the
// returned func.
//
// A thread a delete claimed is waited on, without the lock and with the
// batch still the worker's current one, until the delete decides
// (waitDeleteDecision), and the dispatch is then tried again from the top: a
// refused delete's batch is sent as if the claim had never stood, and a
// committed thread is sent nothing, its queue its teardown's. The check runs
// under the lock the delete's verify takes, so a batch either went before
// the verify read the thread or waits. A thread with no session has it
// started for the batch (ensureSessionForFlushLocked), and a claim that start
// meets is waited on the same way.
func (a *App) lockFlushDispatch(threadID string, items []triage.QueuedFlushItem, generation uint64) (func(), bool) {
	for {
		unlock := a.threadLocks().Lock(threadID)
		outcome := a.readyFlushDispatchLocked(threadID, items, generation)
		if outcome == flushSessionReady {
			return unlock, true
		}
		unlock()
		if outcome != flushSessionClaimed {
			return nil, false
		}
		if !a.waitDeleteDecision(a.lifeCtx(), threadID) {
			// Shut down, or a claim no delete decides: the rows are the next boot's.
			return nil, false
		}
	}
}

// readyFlushDispatchLocked is one try of lockFlushDispatch under the
// thread's action lock.
func (a *App) readyFlushDispatchLocked(threadID string, items []triage.QueuedFlushItem, generation uint64) flushSession {
	if !a.isFlushDispatchGenerationCurrent(threadID, generation) {
		return flushSessionNone
	}
	err := a.store.CheckThreadNotDeleting(threadID)
	switch {
	case errors.Is(err, store.ErrThreadDeleteClaimed):
		return flushSessionClaimed
	case errors.Is(err, store.ErrThreadDeleting):
		return flushSessionNone
	case err != nil:
		// Nothing was sent: the batch, less what was withdrawn, goes back on
		// the queue for the next trigger, as a failed group does.
		log.Printf("flush dispatch: thread=%s items=[%s]: %v", threadID, flushGroupIDs(items), err)
		a.requeueFlushBatch(threadID, items, generation)
		return flushSessionNone
	}
	return a.ensureSessionForFlushLocked(threadID, items, generation)
}

// flushSession is what readying a thread for a batch came to.
type flushSession int

const (
	// flushSessionReady: a session runs, or the batch has nothing to send
	// (dispatchFlushGroup settles empty items without one).
	flushSessionReady flushSession = iota
	// flushSessionClaimed: a delete claimed the thread; the dispatch waits
	// for its decision and tries again.
	flushSessionClaimed
	// flushSessionNone: nothing is sent now. The batch went back to the
	// queue or the composer, or is its teardown's.
	flushSessionNone
)

// ensureSessionForFlushLocked starts the thread's session for a batch queued
// while it had none: a message accepted while the thread was stopped (its
// session died, its worktree was removed, it was reconnecting) is sent the way
// a direct send is, by starting the session. When the session cannot start,
// the batch goes back to the composer with the reason on the thread, so the
// message is never left queued with nothing to send it. Caller holds the
// thread action lock.
//
// The start is a door like any other, so it respects the fences a notice's
// start does (startAgentNoticeSession): a thread a Stop is bringing to rest
// and an archived thread start nothing. The batch goes back to the queue
// unsent, for the rest or the archive to take back, or for the thread's next
// start. That refusal is not a failed start: no composer restore, no error
// row, no failure streak. Nor is a start that succeeded and left no session:
// the batch is still the queue's, unless the teardown that ended that session
// took it (the generation moved). A start a delete's claim refused waits for
// the delete's decision (lockFlushDispatch), and a thread a delete committed
// is sent nothing.
func (a *App) ensureSessionForFlushLocked(threadID string, items []triage.QueuedFlushItem, generation uint64) flushSession {
	if _, live := a.sessionManager().get(threadID); live || !flushBatchCarriesInput(items) {
		return flushSessionReady
	}
	fenced, err := a.flushStartFenced(threadID)
	if err == nil && fenced {
		a.requeueFlushBatch(threadID, items, generation)
		return flushSessionNone
	}
	if err == nil {
		err = a.startSession(a.lifeCtx(), threadID)
		switch {
		case err == nil:
			if _, live := a.sessionManager().get(threadID); live {
				return flushSessionReady
			}
			if a.isFlushDispatchGenerationCurrent(threadID, generation) {
				a.requeueFlushBatch(threadID, items, generation)
			}
			return flushSessionNone
		case errors.Is(err, store.ErrThreadDeleteClaimed):
			return flushSessionClaimed
		case errors.Is(err, store.ErrThreadDeleting):
			return flushSessionNone
		}
	}
	if errors.Is(err, ErrShuttingDown) || a.lifeCtx().Err() != nil {
		// The durable rows stay for the next boot's restore.
		return flushSessionNone
	}
	log.Printf("flush dispatch: thread=%s items=[%s]: start session: %v", threadID, flushGroupIDs(items), err)
	restored := make([]triage.UnconfirmedFlushItem, 0, len(items))
	for _, item := range items {
		restored = append(restored, unconfirmedFromQueued(item))
	}
	// The batch leaves the worker before the composer gets it, as a session
	// death drains the worker before its restore, so no read finds the
	// message queued and in the composer at once.
	a.noteFlushDispatchGroupSettled(threadID, items)
	a.finishFlushGroup(threadID, generation, items)
	// What the app wrote among them never enters the composer
	// (agentPartsNotTakenIn): an answer, a reminder or a completion goes back
	// to its owner, a wake's row records it was not delivered, and the
	// person's text alone is restored. Nobody may be watching a start an
	// app message asked for, so the thread's failure streak raises the
	// once-per-run notification as a notice's failed start does.
	if slices.ContainsFunc(restored, func(item triage.UnconfirmedFlushItem) bool {
		return slices.ContainsFunc(unconfirmedMembersOf(item).ids, appWrittenSendID)
	}) {
		a.noteAgentNoticeStartFailed(threadID)
	}
	a.restoreFlushItemsToDraftLocked(threadID, restored, queueRestoredReasonSessionStartFailed)
	a.emitErrorToThread(threadID, "Queued messages could not be sent and are back in the composer: "+err.Error())
	a.emitQueueStateChanged(threadID)
	return flushSessionNone
}

// flushBatchCarriesInput reports whether any item has input to send. An item
// whose payload does not decode counts, so its dispatch reports the error.
func flushBatchCarriesInput(items []triage.QueuedFlushItem) bool {
	for _, item := range items {
		var payload flushQueuePayload
		if len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) != nil {
			return true
		}
		if validateUserMessageInput(item.Message, payload.AttachmentIDs, payload.RevisionSourceCommentIDs, payload.RevisionSourceDiffCommentIDs) == nil {
			return true
		}
	}
	return false
}

// flushStartFenced reports whether the dispatcher may not start the thread's
// session: a Stop is bringing it to rest (threadRestFence), or it is
// archived. A delete's claim or commit is the start's own refusal
// (ensureSessionForFlushLocked). An error names the read.
func (a *App) flushStartFenced(threadID string) (bool, error) {
	if a.resting.holds(threadID) {
		return true, nil
	}
	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return false, fmt.Errorf("read the thread: %w", err)
	}
	return thread.Archived, nil
}

// requeueFlushBatch puts a batch the dispatcher sent none of back on the
// triage queue, less what was withdrawn meanwhile (requeueWantedFlushItems),
// ahead of anything queued since, without flushing it: the thread's next
// start or turn boundary sends it. Requeued before the worker stops vouching
// for it, so no snapshot misses it.
func (a *App) requeueFlushBatch(threadID string, items []triage.QueuedFlushItem, generation uint64) {
	a.requeueWantedFlushItems(threadID, items)
	a.noteFlushDispatchGroupSettled(threadID, items)
	a.finishFlushGroup(threadID, generation, items)
	a.emitQueueStateChanged(threadID)
}

// Publish the handoff in the same critical section as queue snapshots.
// Otherwise an older queue snapshot can arrive after queue_flushed and be
// mistaken for a failed dispatch that returned the message to the queue.
func (a *App) publishFlushDispatch(threadID string, group []triage.QueuedFlushItem, items []QueueFlushedItem) {
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	for _, item := range group {
		a.noteFlushDispatchItemSettledLocked(threadID, item.ID)
	}
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatch.flushUnsettled[threadID] = struct{}{}
	// The items just left dispatching, which marks the answer dirty only
	// when it found them there. The frame owed for this flush needs a
	// publish either way.
	a.markSendsPendingDirty(threadID)
	a.emit(eventchan.ProviderQueueFlushed, QueueFlushedEvent{ThreadID: threadID, Items: items})
}

// settleFlushGroup ends a group the provider write carried, or that had
// nothing to send: every member's durable queue row goes in one statement
// (store.DeleteDispatchedFlushQueueItems), so a joined message never keeps
// some members' rows and loses the others', then each member's injector
// bookkeeping runs. session is the token of the session the group was
// written to, empty when it was not written.
func (a *App) settleFlushGroup(threadID string, group []triage.QueuedFlushItem, session string) {
	ids := make([]string, 0, len(group))
	for _, item := range group {
		ids = append(ids, item.ID)
	}
	if err := a.store.DeleteDispatchedFlushQueueItems(ids); err != nil {
		log.Printf("flush queue: delete durable rows [%s] of %s: %v", strings.Join(ids, " "), threadID, err)
	}
	for _, item := range group {
		item.Settlement.Settle(session)
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
// On success requeue is nil, and the string is the token of the session the
// group was written to.
func (a *App) dispatchFlushGroup(threadID string, group []triage.QueuedFlushItem) ([]QueueFlushedItem, bool, []triage.QueuedFlushItem, string, error) {
	requeue := slices.Clone(group)
	if a.shuttingDown.Load() {
		return nil, false, requeue, "", ErrShuttingDown
	}

	// Resolve EVERY member before anything durable or provider-visible
	// happens. A joined group is one message: a failure on its third member
	// must leave all three requeueable with nothing on the wire.
	members := make([]flushMember, 0, len(group))
	// memberOf maps a group index to its member, or -1 for an empty item.
	memberOf := make([]int, len(group))
	var staleRows []string
	for i, item := range group {
		memberOf[i] = -1
		var payload flushQueuePayload
		if len(item.Payload) > 0 {
			if err := json.Unmarshal(item.Payload, &payload); err != nil {
				return nil, false, requeue, "", fmt.Errorf("decode payload: %w", err)
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
			// Attribution travels with the message, so a wake or a request
			// send that waited on the queue lands with the same chip it
			// would have had if the thread had been idle.
			origin:       payload.Origin,
			originThread: payload.OriginThread,
			originWake:   payload.OriginWake,
			joinedParts:  payload.JoinedParts,
			// The review drafts a previous dispatch's stale row still
			// holds are this message's; the cleanup below returns them
			// before the claim takes them again.
			heldBy: staleRowsOf(item),
			// A message rebuilt from its row renders its review comments
			// already; it claims them again without rendering them.
			textBeforeReviewComments: payload.TextBeforeReviewComments,
			renderedReviewComments:   payload.RenderedReviewComments,
		})
		if err != nil {
			return nil, false, requeue, "", err
		}
		memberOf[i] = len(members)
		members = append(members, flushMember{item: item, payload: payload, resolved: resolved})
	}
	if len(members) == 0 {
		if err := a.cleanupStaleFlushRows(threadID, staleRows); err != nil {
			return nil, false, requeue, "", err
		}
		return nil, false, requeue, "", errEmptyUserMessage
	}

	a.ensureTriageRouter()

	thread, err := a.store.GetThread(threadID)
	if err != nil {
		return nil, false, requeue, "", fmt.Errorf("load thread: %w", err)
	}
	sess, unlockAccount, err := a.lockProviderAccountForSendLocked(thread)
	if err != nil {
		return nil, false, requeue, "", err
	}
	defer unlockAccount()

	// Placement through registration runs under the turn allocation lock,
	// so a turn the provider starts on its own can neither take the index
	// chosen here nor miss the registered send (LockTurnAllocation).
	unlockAllocation := a.triage.LockTurnAllocation(threadID)
	allocationHeld := true
	releaseAllocation := func() {
		if allocationHeld {
			allocationHeld = false
			unlockAllocation()
		}
	}
	defer releaseAllocation()
	placement, err := a.resolveUserMessagePlacement(thread, messageFlush)
	if err != nil {
		return nil, false, requeue, "", fmt.Errorf("resolve placement: %w", err)
	}
	responseTurnIndex := placement.responseTurn
	persistTurnIndex := placement.displayTurn
	eagerPersist := placement.persistence == messagePersistQuiet
	allWakes := flushGroupIsAgentWakes(members)
	a.noteAgentWakeDispatch(threadID, allWakes, !placement.joinsActiveTurn)

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
		//
		// A message whose text renders its review comments claims again
		// the ones its stale row returns, which hold the bodies it renders.
		// Its requeue copy carries them from here on: once the row is gone,
		// nothing else records them.
		for i := range requeue {
			if requeue[i].StaleUserItemID == "" {
				continue
			}
			returned, err := a.cleanupStaleFlushRow(threadID, requeue[i].StaleUserItemID)
			if err != nil {
				return nil, false, requeue, "", fmt.Errorf("cleanup stale flush row %s: %w", requeue[i].StaleUserItemID, err)
			}
			requeue[i].StaleUserItemID = ""
			m := memberOf[i]
			if m < 0 || !addReturnedReviewComments(&members[m].payload, returned) {
				continue
			}
			encoded, err := json.Marshal(members[m].payload)
			if err != nil {
				return nil, false, requeue, "", fmt.Errorf("encode payload: %w", err)
			}
			requeue[i].Payload = encoded
			members[m].resolved.reviewDeliveries, err = a.renderedReviewDeliveries(threadID, members[m].payload.RenderedReviewComments)
			if err != nil {
				return nil, false, requeue, "", err
			}
		}
	}

	joined, err := joinFlushMembers(members)
	if err != nil {
		return nil, false, requeue, "", fmt.Errorf("join queued messages: %w", err)
	}

	flushItemID, err := a.userMessageItemID(threadID, joined.sendID, persistTurnIndex, messageFlush)
	if err != nil {
		return nil, false, requeue, "", fmt.Errorf("allocate item id: %w", err)
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
	joinedMeta, err := usermessage.FromItem(userItem)
	if err != nil {
		return nil, false, requeue, "", err
	}
	agentMessage := agentmeta.IsAgentMessage(joinedMeta.Agent())
	flushedItems := make([]QueueFlushedItem, 0, len(members))
	for _, member := range members {
		flushedItems = append(flushedItems, QueueFlushedItem{
			QueueItemID:  member.item.ID,
			UserItemID:   userItem.ID,
			Message:      joined.content,
			SendID:       member.payload.SendID,
			AgentMessage: agentMessage,
		})
	}

	// The identity this dispatch will be recognised by — wire stamp and
	// registry expectation derived together (providerSendIdentity), so
	// the two cannot drift.
	clientUserMessageID, sendExpect := providerSendIdentity(sess, userItem.ID, userItem.Meta, sendUUID)
	sendExpect.JoinedParts = joined.parts
	// What this message puts on the wire, block by block. Headless Claude can
	// acknowledge a queued message whose content is this message CONCATENATED
	// onto earlier ones the CLI merged with it at a turn boundary, and the
	// flattened echo text cannot be told from one longer message. Registering
	// the outbound block fingerprints is what lets triage recognise that and
	// fold AO's rows to match (claude_merge_fold.go). Derived from the real
	// block builder, so the expectation is the bytes Send writes.
	if sess.Claude != nil {
		digest, digestErr := claude.UserMessageBlockDigest(joined.providerContent, joined.attachments, joined.guardSlashCommand)
		if digestErr != nil {
			return nil, false, requeue, "", fmt.Errorf("fingerprint queued message content: %w", digestErr)
		}
		sendExpect.ContentBlockDigest = digest
	}

	// The group's queue identity for the pending-send entry is its first
	// member's: one entry per outbound message, and the session-death restore
	// reads the entry's own joined row rather than the queue ids.
	leadQueueItemID := members[0].item.ID
	leadEnqueuedAt := members[0].item.EnqueuedAt

	// The review drafts each member carries are claimed for the row's turn
	// before anything reaches the provider, so two threads dispatching the
	// same drafts cannot both deliver them (claimDiffReviewDelivery). Every
	// path below that requeues the group hands them back (unclaim). The
	// claims settle when the provider confirms the message, not when its
	// quiet row lands, and a session death that takes the message back
	// returns them with it (cleanupStaleFlushRow).
	var claims diffReviewClaims
	for _, member := range members {
		claim, claimErr := a.claimDiffReviewDelivery(threadID, persistTurnIndex, userItem.ID, member.resolved, true)
		if claimErr != nil {
			return nil, false, requeue, "", errors.Join(claimErr, claims.release(a))
		}
		claims = append(claims, claim...)
	}
	unclaim := func(err error) error { return errors.Join(err, claims.release(a)) }

	if eagerPersist {
		// Emit queue_flushed so the frontend creates the Zone 2 entry
		// (queued marker above the composer). Persist the row quietly —
		// no provider:item_event — so the item reserves its timeline
		// position in SQLite but stays as a queued marker in the UI
		// until the provider echo confirms it entered context.
		if persistErr := a.triage.PersistAndRegisterPendingQuietFlushSendWithExpectation(
			threadID, leadQueueItemID, userItem, responseTurnIndex, leadEnqueuedAt, sendExpect); persistErr != nil {
			return nil, false, requeue, "", unclaim(fmt.Errorf("eager persist flush: %w", persistErr))
		}
		a.publishFlushDispatch(threadID, group, flushedItems)
		// One row, one cleanup obligation: the first member owns it.
		requeue[0].StaleUserItemID = userItem.ID

	} else {
		// Deferred: row persists at echo time via persistDeferredUserText.
		a.triage.RegisterPendingFlushSendWithExpectation(threadID, leadQueueItemID, userItem, leadEnqueuedAt, sendExpect)
	}
	releaseAllocation()

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

	// Requests the message carries name this session before it can report
	// anything about them (receiptGate).
	a.noteRequestsWriting(threadID, sess.Token, usermessage.MemberSendIDs(joinedMeta))
	dispatchErr := a.dispatchFlushToProvider(sess, joined.providerContent, sendOpts)
	if dispatchErr != nil {
		if codex.IsAmbiguousSteerTimeout(dispatchErr) {
			log.Printf("flush dispatch: thread=%s items=[%s]: codex steer timed out after write; leaving pending confirmation for provider echo", threadID, flushGroupIDs(group))
			a.applyJoinedPlanAcceptance(threadID, userItem, members, claims)
			return flushedItems, eagerPersist, nil, sess.Token, nil
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
			return nil, eagerPersist, requeue, "", unclaim(dispatchErr)
		}
		if sess.Codex != nil && codex.IsNoActiveTurnRace(dispatchErr) {
			a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
			fallback, allocErr := a.resolveUserMessagePlacement(thread, messageFlushFallback)
			if allocErr != nil {
				return nil, eagerPersist, requeue, "", unclaim(allocErr)
			}
			responseTurnIndex = fallback.responseTurn
			a.noteAgentWakeDispatch(threadID, allWakes, true)
			freshFlushItemID, allocErr := a.userMessageItemID(threadID, joined.sendID, responseTurnIndex, messageFlushFallback)
			if allocErr != nil {
				a.persistFlushDispatchError(threadID, responseTurnIndex, allocErr)
				return nil, eagerPersist, requeue, "", unclaim(allocErr)
			}
			userItem.ID = freshFlushItemID
			userItem.TurnIndex = responseTurnIndex
			userItem.CreatedAt = time.Now().UnixMilli()
			userItem.UpdatedAt = userItem.CreatedAt
			// The claims follow the row before it can land, so its insert
			// settles them.
			if retargetErr := claims.retarget(a, userItem.TurnIndex, userItem.ID); retargetErr != nil {
				a.persistFlushDispatchError(threadID, responseTurnIndex, retargetErr)
				return nil, eagerPersist, requeue, "", unclaim(retargetErr)
			}
			// Legacy callers may get a new row id. Derive both
			// wire stamp and expectation together; identified client sends
			// preserve both across this placement-only fallback.
			var refreshExpect triage.PendingSendExpectation
			sendOpts.ClientUserMessageID, refreshExpect = providerSendIdentity(sess, userItem.ID, userItem.Meta, "")
			refreshExpect.JoinedParts = joined.parts
			// The message is now the fresh turn's input, which a Stop does
			// not discard, so the Stop does not send it again.
			a.triage.RegisterPendingFlushTurnStartWithExpectation(
				threadID, leadQueueItemID, userItem, leadEnqueuedAt, refreshExpect)
			sess.Liveness.BumpActivity(time.Now())
			if sendErr := sess.Codex.Send(context.Background(), joined.providerContent, sendOpts); sendErr != nil {
				if codex.IsAmbiguousTurnStartTimeout(sendErr) {
					// Same ambiguity as the steer timeout above: the
					// turn/start was written and the echo may already be
					// coming. A requeue would double-send (round-14,
					// D14-2) — leave the pending entry for the echo.
					log.Printf("flush dispatch: thread=%s items=[%s]: codex turn/start timed out after write; leaving pending confirmation for provider echo", threadID, flushGroupIDs(group))
					a.applyJoinedPlanAcceptance(threadID, userItem, members, claims)
					return retargetFlushedItems(flushedItems, userItem.ID), eagerPersist, nil, sess.Token, nil
				}
				a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
				a.persistFlushDispatchError(threadID, responseTurnIndex, sendErr)
				return nil, eagerPersist, requeue, "", unclaim(sendErr)
			}
			a.applyJoinedPlanAcceptance(threadID, userItem, members, claims)
			return retargetFlushedItems(flushedItems, userItem.ID), eagerPersist, nil, sess.Token, nil
		}
		a.triage.ClearPendingSendForFailure(threadID, userItem.ID)
		a.persistFlushDispatchError(threadID, persistTurnIndex, dispatchErr)
		return nil, eagerPersist, requeue, "", unclaim(dispatchErr)
	}
	a.applyJoinedPlanAcceptance(threadID, userItem, members, claims)
	return flushedItems, eagerPersist, nil, sess.Token, nil
}

// applyJoinedPlanAcceptance marks each member's plan / comment bookkeeping
// against the single row the group became. The refs are per-message even when
// the message is joined, so the marking loop is too; the joined row's meta
// carries only the refs it renders (joinFlushMeta). The members' review
// drafts were claimed for that row before dispatch.
func (a *App) applyJoinedPlanAcceptance(threadID string, userItem store.Item, members []flushMember, claims diffReviewClaims) {
	for _, member := range members {
		a.applyProposedPlanAcceptance(threadID, userItem, member.resolved)
	}
	claims.announce(a)
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
		if _, err := a.cleanupStaleFlushRow(threadID, id); err != nil {
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
// the failed user_text.
func (a *App) persistFlushDispatchError(threadID string, turnIndex int, dispatchErr error) {
	summary := fmt.Sprintf("Failed to deliver queued message: %v", dispatchErr)
	if err := a.triage.PersistTurnError(threadID, turnIndex, summary); err != nil {
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
// without an extra round-trip, and the thread's sends-pending answer
// once the message is queued, stamped with the provider:sends_pending
// sequence it reflects: the reply can arrive ahead of earlier frames. Emits
// `provider:queue_state_changed` for any other client (remote
// `--connect` peers, additional webviews) that may be observing the
// same thread.
//
//ao:scope threads:operate
func (a *App) RegisterQueueItem(ctx context.Context, threadID string, message string, opts SendMessageOptions) (RegisteredQueueItem, error) {
	if err := a.requireAutonomyForThread(ctx, threadID, opts.RuntimeMode); err != nil {
		return RegisteredQueueItem{}, err
	}
	if err := refusePRDraftSource(opts); err != nil {
		return RegisteredQueueItem{}, err
	}
	unlockAdmission, err := a.lockClientSendAdmission(ctx, threadID, opts.SendID)
	if err != nil {
		return RegisteredQueueItem{}, err
	}
	defer unlockAdmission()

	// A user queueing a message has just consumed their composer draft, so the
	// bound entry point clears it, and nothing is waiting on the dispatch.
	item, err := a.registerQueueItem(threadID, message, opts, injectedQueueOptions{
		expandComposerCommands: true,
	})
	if err != nil {
		return RegisteredQueueItem{}, err
	}
	return RegisteredQueueItem{QueuedItem: item, SendsPending: a.publishSendsPending(threadID)}, nil
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
	// session is the token of the session the message was written to, empty
	// when it was restored instead.
	onDurable func(session string)
	// persist atomically transfers an injector’s delivery responsibility to the
	// ordinary durable queue. Nil uses the standard queue insert.
	persist func(store.FlushQueueItem) error
	// held queues the message without flushing it into a live session: the
	// thread's next turn boundary or session start sends it. A message given
	// back by a Stop or a session end sets it, so the giving back does not
	// start the turn the Stop ended or resend into a session that failed it.
	held bool
	// appWritten marks a message the app writes for the thread, as against
	// a person's: it is taken by a thread a delete claimed
	// (threadapp.LockDeliverable), which refuses a person's.
	appWritten bool
	// heldWhileArchived refuses the message with errThreadArchived when the
	// thread is archived, before persist runs, and with errThreadResting
	// while a Stop brings it to rest. queueAgentNotice sets it for every
	// notice but the person's own answers. A person's message is taken by an
	// archived thread they opened, and a workflow run's wake is the engine's
	// to settle (queueWorkflowWake).
	heldWhileArchived bool
	// noticeStarts marks a notice whose own caller starts the thread's
	// session when it has none (queueAgentNotice), so the queue write does
	// not hand it to the dispatcher, which would start one too.
	noticeStarts bool
	// origin and originThread attribute the queued message to the agent
	// thread that asked for it. They land on the payload so the dispatch
	// that persists the row later stamps the same attribution an immediate
	// send would have.
	origin       string
	originThread *agentmeta.OriginThread
	originWake   *agentmeta.OriginWake
}

// dropDurableFlushQueue deletes every durable queue row of a thread. Its
// callers are the WHOLESALE DROPS — a teardown whose triage cleanup discards
// the in-memory queue without restoring it, and the Codex rollback purge —
// where a surviving row would resurrect at the next boot the very messages
// the user's Stop or revert threw away.
//
// The answers, reminders and remote completions among them are kept and
// returned, and the caller gives them back to their owners
// (giveBackToOwners) once the rest of the queue, in memory and dispatch, is
// gone too.
func (a *App) dropDurableFlushQueue(threadID string) (held []ownedMessage) {
	held = a.noteAgentWakesDropped(threadID)
	keep := make([]string, 0, len(held))
	for _, m := range held {
		keep = append(keep, m.rowID)
	}
	if a.queueDropReadForTest != nil {
		a.queueDropReadForTest(threadID)
	}
	// An outside send's row goes with its notice (noteAgentSendsCleared).
	if err := a.store.DeleteFlushQueueItemsForThread(threadID, agentSendIDRoot, keep...); err != nil {
		log.Printf("flush queue: drop durable rows for thread %s: %v", threadID, err)
	}
	if a.queueDroppedForTest != nil {
		a.queueDroppedForTest(threadID)
	}
	a.emit(eventchan.ProviderAsyncQuestionsChanged, map[string]string{"threadId": threadID})
	return held
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
	lock := a.threadApplication().LockMutable
	if injected.appWritten {
		lock = a.threadApplication().LockDeliverable
	}
	unlock, err := lock(context.Background(), threadID)
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
	if !injected.preserveDraft {
		if err := a.refuseSendOfMovedDraft(threadID, opts.ConsumeDraft); err != nil {
			return QueuedItem{}, err
		}
	}
	// Both archive doors flip the row under this lock, so a held message
	// lands wholly before the flip, where the archive takes it back, or is
	// refused here.
	if injected.heldWhileArchived && thread.Archived {
		return QueuedItem{}, errThreadArchived
	}
	// A Stop's rest fences the thread under this lock the same way.
	if injected.heldWhileArchived && a.resting.holds(threadID) {
		return QueuedItem{}, errThreadResting
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
	// handoff below: idle eviction and transfer reservation read the same
	// queued / in-flight counters under this mutex (pendingFlushWorkCount),
	// so holding it here keeps them from observing tryFlushQueue's handoff
	// window and treating the thread as having no queued work. See the
	// a.flushDispatch.handoffMu field doc (app_state.go) for the window and
	// why this isn't the per-thread action lock.
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

	// Every message between its admission and the boundary that resolves
	// it counts, whether queued, in flight, or handed to the provider and
	// not yet echoed: triage keeps merge evidence for each of those.
	totalQueued := a.triage.QueuedFlushItemCount(threadID) + a.triage.OutstandingFlushSendCount(threadID) + a.flushDispatchItemCount(threadID)
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
		Origin:                       injected.origin,
		OriginThread:                 injected.originThread,
		OriginWake:                   injected.originWake,
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
		// The durable row leaves with the dispatch that carries the message
		// (settleFlushGroup) or the restore that takes it back
		// (store.RestoreFlushQueueToDraft); the settlement is the injector's
		// bookkeeping, once after either.
		Settlement: triage.NewFlushSettlement(injected.onDurable),
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
	// A message given back by a Stop or a session end (held) waits for the
	// thread's next turn boundary or session start. A notice starts its own
	// session (queueAgentNotice), which flushes the queue as it comes up, so
	// the dispatcher does not start a second one for it. Any other message
	// is sent now: with no session the dispatch starts one
	// (ensureSessionForFlushLocked), as a direct send does, so the message
	// never waits for a later send.
	if _, live := a.sessionManager().get(threadID); !injected.held && (live || !injected.noticeStarts) {
		a.triage.FlushQueuedItems(threadID)
	}
	return wireItem, nil
}

// removeQueuedItem takes one still-queued message back out of a thread's
// queue by its send id, durable row and in-memory entry together. It is the
// withdrawal path for a message the app itself queued on someone's behalf —
// an agent request cancelled before the target's turn boundary drained it.
//
// The message's durable row is the authority: deleting it withdraws the
// message wherever a copy of it waits unsent, because every dispatch commit
// point and every put-back reads the rows first and drops a message whose row
// is gone (commitFlushGroup, requeueWantedFlushItems,
// restoreFlushItemsToDraftLocked). It refuses once a dispatch committed to
// writing the message, or wrote it and awaits its echo (flushItemWritten):
// the caller is told so and cancels the turn instead. The two refusals are
// told apart, because they mean different things to a caller that has to say
// what it stopped: queueRemovalAbsent is a send id nothing on this queue
// carries, queueRemovalDispatched is an entry the dispatch is sending or
// sent. An error is a failure to carry out a removal that should have
// applied.
//
// Lock order is registerQueueItem's: send admission, then the thread mutation
// lock, which every commit point and put-back takes, then the dispatch
// handoff mutex, then the dispatch bookkeeping lock.
func (a *App) removeQueuedItem(ctx context.Context, threadID, sendID string) (queueRemoval, error) {
	return a.removeQueuedItemWith(ctx, threadID, sendID, func(row store.FlushQueueItem) error {
		return a.store.DeleteFlushQueueItem(row.ID)
	})
}

// removeQueuedItemWith is removeQueuedItem with the durable delete supplied
// by the caller, for a message whose removal writes more than its row in the
// same transaction.
func (a *App) removeQueuedItemWith(ctx context.Context, threadID, sendID string, deleteRow func(store.FlushQueueItem) error) (queueRemoval, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(sendID) == "" {
		return queueRemovalAbsent, fmt.Errorf("remove queued item: thread id and send id are required")
	}
	unlockAdmission, err := a.lockSendAdmission(ctx, threadID, sendID)
	if err != nil {
		return queueRemovalAbsent, err
	}
	defer unlockAdmission()
	// What the app queued it may take back from a thread a delete claimed.
	unlock, err := a.threadApplication().LockDeliverable(ctx, threadID)
	if err != nil {
		return queueRemovalAbsent, err
	}
	defer unlock()
	a.flushDispatch.handoffMu.Lock()
	defer a.flushDispatch.handoffMu.Unlock()

	row, found, err := a.store.FindFlushQueueItemBySendID(threadID, sendID)
	if err != nil {
		return queueRemovalAbsent, fmt.Errorf("remove queued item: %w", err)
	}
	if !found {
		// Either the message was dispatched (its row is gone and the
		// `user_text` row carries the id) or it never existed. Nothing on
		// this queue carries the id either way; the caller that needs to
		// tell the two apart reads the message's own record.
		return queueRemovalAbsent, nil
	}
	if a.flushItemWritten(threadID, row) {
		// The durable row stays: its settlement deletes it when the
		// dispatch finishes, or the echo when an answer's write lands.
		return queueRemovalDispatched, nil
	}
	if err := deleteRow(row); err != nil {
		return queueRemovalAbsent, fmt.Errorf("remove queued item: %w", err)
	}
	a.forgetUnsentCopies(threadID, row.ID)
	a.emitQueueStateChanged(threadID)
	return queueRemovalRemoved, nil
}

// queueRemoval is what removeQueuedItem found under one send id.
type queueRemoval int

const (
	// queueRemovalAbsent: no queued message carries that send id. It was
	// dispatched and settled, or it never existed.
	queueRemovalAbsent queueRemoval = iota
	// queueRemovalRemoved: the message was taken back off the queue, durable
	// row and in-memory entry together.
	queueRemovalRemoved
	// queueRemovalDispatched: a dispatch committed to writing the entry, or
	// wrote it and awaits its echo. The turn it starts is what a caller must
	// stop instead.
	queueRemovalDispatched
)

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
	a.flushDispatch.mu.Lock()
	defer a.flushDispatch.mu.Unlock()
	items := a.queueSnapshotForThreadLocked(threadID)
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

// maxQueueLength caps the follow-up messages a thread has between their
// admission and the boundary that resolves them (triage's
// MaxOutstandingFlushSends, the bound its merge evidence is kept to).
// Bounded by user attention in normal operation (single-digit N); the
// cap exists to fail loudly rather than silently grow router memory
// when a client misbehaves or a bug puts RegisterQueueItem in a loop.
const queueMaxLength = triage.MaxOutstandingFlushSends

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

// staleRowsOf is the stale row a requeued item's earlier dispatch left, if
// any (QueuedFlushItem.StaleUserItemID).
func staleRowsOf(item triage.QueuedFlushItem) []string {
	if item.StaleUserItemID == "" {
		return nil
	}
	return []string{item.StaleUserItemID}
}
