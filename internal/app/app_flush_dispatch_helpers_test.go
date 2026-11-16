package app

import (
	"slices"
	"time"

	"agent-overflow/internal/triage"
)

// dispatchThroughWorker hands items to threadID's dispatch worker, as
// triage's flush does (enqueueFlushDispatch), and returns once the worker
// has finished every batch it holds for the thread.
func dispatchThroughWorker(a *App, threadID string, items []triage.QueuedFlushItem) {
	items = handedOut(a, threadID, items)
	a.enqueueFlushDispatch(threadID, items)
	for {
		a.flushDispatch.mu.Lock()
		busy := a.flushDispatch.running[threadID]
		a.flushDispatch.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// handedOut is items as triage's flush hands them out: stamped with the
// thread's dispatch generation, and those a test made without a durable row
// marked so (triage.QueuedFlushItem.NoRow), as the dispatch sends only
// messages whose rows stand.
func handedOut(a *App, threadID string, items []triage.QueuedFlushItem) []triage.QueuedFlushItem {
	out := slices.Clone(items)
	ids := make([]string, 0, len(out))
	for _, item := range out {
		ids = append(ids, item.ID)
	}
	present, err := a.store.FlushQueueItemsPresent(threadID, ids)
	if err != nil {
		panic(err)
	}
	generation := a.currentFlushDispatchGeneration(threadID)
	for i := range out {
		out[i].DispatchGeneration = generation
		out[i].NoRow = out[i].NoRow || !present[out[i].ID]
	}
	return out
}

// beginFlushDispatchVisibility shows items as the batch threadID's worker
// is dispatching, as the worker records it, for a test that reads the queue
// snapshot mid-dispatch.
func (a *App) beginFlushDispatchVisibility(threadID string, items []triage.QueuedFlushItem) {
	a.flushDispatch.mu.Lock()
	a.ensureFlushDispatchMapsLocked()
	a.flushDispatch.dispatching[threadID] = items
	a.markSendsPendingDirty(threadID)
	a.flushDispatch.mu.Unlock()
}

// endFlushDispatchVisibility clears what beginFlushDispatchVisibility showed.
func (a *App) endFlushDispatchVisibility(threadID string) {
	a.flushDispatch.mu.Lock()
	delete(a.flushDispatch.dispatching, threadID)
	a.markSendsPendingDirty(threadID)
	a.flushDispatch.mu.Unlock()
}
