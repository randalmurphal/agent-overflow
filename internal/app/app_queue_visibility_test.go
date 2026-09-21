package app

import (
	"context"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/triage"
)

// A reconnect must see the same accepted message while triage hands it to
// the worker, while the worker waits for the thread lock, and after dispatch.
func TestLiveQueueSnapshotCoversDispatchHandoff(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("live-queue-handoff")
	thread.WorkspacePath = t.TempDir()
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	queued, err := app.RegisterQueueItem(context.Background(), thread.ID, "keep visible", SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		t.Helper()
		live, err := app.GetThreadLiveState(thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(live.QueueItems) != 1 || live.QueueItems[0].ID != queued.ID {
			t.Fatalf("%s: live queue = %+v, want %s", stage, live.QueueItems, queued.ID)
		}
	}
	app.triage.SetFlushDispatcher(func(id string, items []triage.QueuedFlushItem) {
		check("claimed by triage")
		app.beginFlushDispatchVisibility(id, items)
	})
	defer app.endFlushDispatchVisibility(thread.ID)
	if !app.triage.FlushQueuedItems(thread.ID) {
		t.Fatal("queue did not drain")
	}
	check("held by worker")
}

func TestLiveQueueSnapshotHasOneHomeAcrossPendingRegistration(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	thread := testThread("live-queue-registration")
	if err := app.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	group := []triage.QueuedFlushItem{{ID: "q1", Message: "first"}, {ID: "q2", Message: "second"}}
	app.beginFlushDispatchVisibility(thread.ID, group)
	defer app.endFlushDispatchVisibility(thread.ID)
	app.triage.RegisterPendingFlushSendWithExpectation(thread.ID, "q1", store.Item{
		ID: "user:flush:joined", ThreadID: thread.ID, Kind: "user_text", Summary: "first and second",
	}, 1, triage.PendingSendExpectation{})
	before, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.QueueItems) != 2 || len(before.FlushedItems) != 0 {
		t.Fatalf("before publication: %+v", before)
	}
	app.publishFlushDispatch(thread.ID, group, []QueueFlushedItem{{QueueItemID: "q1", UserItemID: "user:flush:joined"}})
	after, err := app.GetThreadLiveState(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.QueueItems) != 0 || len(after.FlushedItems) != 1 {
		t.Fatalf("after publication: %+v", after)
	}
}

// Snapshot publication and removal from dispatch must share the lock. A
// delayed snapshot published outside it could reclaim an already-flushed
// message, which the next empty snapshot would then erase from the preview.
func TestQueuePublicationHoldsDispatchOwnership(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	group := []triage.QueuedFlushItem{{ID: "q", Message: "pending"}}
	app.beginFlushDispatchVisibility("thread", group)
	defer app.endFlushDispatchVisibility("thread")
	var channels []string
	app.testEmitHook = func(channel string, _ any) {
		channels = append(channels, channel)
		if app.flushDispatch.mu.TryLock() {
			app.flushDispatch.mu.Unlock()
			t.Errorf("%s published without dispatch ownership", channel)
		}
	}
	app.emitQueueStateChanged("thread")
	app.publishFlushDispatch("thread", group, []QueueFlushedItem{{QueueItemID: "q", UserItemID: "u"}})
	app.emitQueueStateChanged("thread")
	if len(channels) != 3 {
		t.Fatalf("publications = %v", channels)
	}
}

func TestDispatchReleasesTriageClaimBeforeWorkerCanSettle(t *testing.T) {
	app, _ := newAppForFlushQueueRPC(t)
	const threadID = "claim-transfer"
	// Stand in for an already-running worker; this admission only appends.
	app.flushDispatch.mu.Lock()
	app.ensureFlushDispatchMapsLocked()
	app.flushDispatch.running[threadID] = true
	app.flushDispatch.mu.Unlock()
	app.triage.SetFlushDispatcher(func(id string, items []triage.QueuedFlushItem) {
		app.enqueueFlushDispatch(id, items)
		// The callback has not returned yet, so tryFlushQueue's deferred release
		// has not run. App now owns the entire batch and may settle immediately.
		if claimed := app.triage.QueuedFlushItems(id); len(claimed) != 0 {
			t.Fatalf("claim survived transfer to worker: %+v", claimed)
		}
		if queued := app.queueSnapshotForThread(id); len(queued) != 1 || queued[0].ID != "q" {
			t.Fatalf("worker lost the transferred message: %+v", queued)
		}
	})
	app.triage.RegisterQueueItem(threadID, triage.QueuedFlushItem{ID: "q", Message: "pending"})
	if !app.triage.FlushQueuedItems(threadID) {
		t.Fatal("queue did not dispatch")
	}
}
