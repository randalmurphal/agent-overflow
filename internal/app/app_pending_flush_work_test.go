package app

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/triage"
)

// A queued message is tracked by the triage queue, then by the App's
// in-flight count, and tryFlushQueue hands it from one to the other by
// deleting it from the queue before the dispatcher records it in flight.
// RegisterQueueItem holds flushDispatch.handoffMu across that window, and
// pendingFlushWorkCount reads its counters under the same mutex, so a count
// taken mid-handoff waits for the handoff and then counts the message. Idle
// eviction and transfer reservation rely on that to never treat a thread as
// idle while its follow-up is being delivered.
func TestPendingFlushWorkCountSeesAMessageMidHandoff(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "pending-work-handoff", "claude", t.TempDir())
	insertUserItem(t, app.store, thread.ID, "user:0", 0, "original prompt")
	// RegisterQueueItem only flushes immediately when a session is attached.
	app.sessionManager().put(thread.ID, session{Provider: "claude"})
	app.triage = triage.NewRouter(app.store, func(eventchan.Channel, any) {})

	inGap := make(chan struct{})
	release := make(chan struct{})
	// Interpose where tryFlushQueue invokes the dispatcher: the item is
	// already gone from the triage queue and nothing has counted it in
	// flight. Block there, then record it in flight the way
	// enqueueFlushDispatch does.
	app.triage.SetFlushDispatcher(func(threadID string, items []triage.QueuedFlushItem) {
		close(inGap)
		<-release
		app.flushDispatch.mu.Lock()
		app.ensureFlushDispatchMapsLocked()
		app.flushDispatch.inflightItems[threadID] += len(items)
		app.flushDispatch.mu.Unlock()
	})

	registered := make(chan error, 1)
	go func() {
		_, err := app.RegisterQueueItem(context.Background(), thread.ID, "follow-up", SendMessageOptions{})
		registered <- err
	}()
	select {
	case <-inGap:
	case <-time.After(5 * time.Second):
		t.Fatal("flush dispatcher never reached the handoff window")
	}

	counted := make(chan int, 1)
	go func() { counted <- app.pendingFlushWorkCount(thread.ID) }()
	select {
	case n := <-counted:
		close(release)
		t.Fatalf("pendingFlushWorkCount returned %d inside the handoff window; it must wait for the handoff", n)
	case <-time.After(500 * time.Millisecond):
	}

	close(release)
	if err := <-registered; err != nil {
		t.Fatalf("RegisterQueueItem: %v", err)
	}
	if n := <-counted; n != 1 {
		t.Fatalf("pendingFlushWorkCount after the handoff = %d, want the message counted in flight", n)
	}
}
