package app

import (
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/entityid"
	"agent-overflow/internal/store"
	"agent-overflow/internal/workflow/wake"
)

// The dispatch of a live session's queue against a delete's claim and a
// withdrawal: a claimed thread's batch waits for the decision and is sent
// once the claim is released, and a message taken back before the dispatch
// committed to sending it never reaches the provider.

// queueTestWake queues a workflow run's wake saying marker for threadID, an
// app message whose queue write takes no lock a test holds.
func queueTestWake(t *testing.T, a *App, threadID, marker string) string {
	t.Helper()
	if err := a.queueWorkflowWake(threadID, "release", wake.Message{Notice: wake.DataNotice, Data: marker}, nil); err != nil {
		t.Fatalf("queue %s: %v", marker, err)
	}
	for _, row := range durableQueueRows(t, a, threadID) {
		if strings.Contains(row.Message, marker) {
			return row.SendID
		}
	}
	t.Fatalf("%s is not queued", marker)
	return ""
}

// heldAtTheActionLock waits until threadID's dispatch worker holds a batch it
// has not sent, waiting for the thread's action lock.
func heldAtTheActionLock(t *testing.T, a *App, threadID string) {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		a.flushDispatch.mu.Lock()
		defer a.flushDispatch.mu.Unlock()
		_, held := a.flushDispatch.current[threadID]
		return held
	})
}

// A refused delete's release flushes nothing of its own: a message queued
// as held, which waits for the idle thread's next turn boundary, stays
// queued, and no turn starts.
func TestAReleaseLeavesAHeldMessageForTheNextBoundary(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.StartSession(f.caller.ID); err != nil {
		t.Fatal(err)
	}
	held := "held-" + entityid.New()
	if _, err := f.app.registerQueueItem(f.caller.ID, held, SendMessageOptions{SendID: workflowWakeSendIDRoot + entityid.New()}, injectedQueueOptions{
		preserveDraft: true, held: true, appWritten: true,
	}); err != nil {
		t.Fatal(err)
	}
	refusedDelete(t, f.app, f.caller.ID, func() {})
	f.settle(t)
	if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot); len(ids) != 1 {
		t.Fatalf("queued after the release = %q, want the held message", ids)
	}
	if n := f.sent(t, held); n != 0 {
		t.Fatalf("the release sent the held message %d times", n)
	}
}

// A message withdrawn while triage hands its batch to the dispatch, as a
// turn boundary's drain does, is taken back: the withdrawal says it was
// removed, its durable row is gone, and the dispatch drops it at its
// group's commit point, so the provider never sees it.
func TestAWithdrawalReachesABatchInTriagesHandoff(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.StartSession(f.caller.ID); err != nil {
		t.Fatal(err)
	}
	withdrawn := "withdrawn-" + entityid.New()
	sendID := workflowWakeSendIDRoot + entityid.New()
	// Held, so queueing it does not flush it: the drain below does.
	if _, err := f.app.registerQueueItem(f.caller.ID, withdrawn, SendMessageOptions{SendID: sendID}, injectedQueueOptions{
		preserveDraft: true, held: true, appWritten: true,
	}); err != nil {
		t.Fatal(err)
	}
	at, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.app.flushHandoffForTest = func(string) {
		once.Do(func() {
			close(at)
			<-proceed
		})
	}
	drained := make(chan bool, 1)
	go func() { drained <- f.app.triage.FlushQueuedItems(f.caller.ID) }()
	within(t, "the handoff", at)
	removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, sendID)
	if err != nil || removal != queueRemovalRemoved {
		t.Errorf("the withdrawal = %v, %v; want it removed", removal, err)
	}
	if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot); len(ids) != 0 {
		t.Errorf("queued after the withdrawal = %q, want nothing", ids)
	}
	close(proceed)
	within(t, "the drain", drained)
	waitFor(t, "the dispatch to drop the withdrawn message", func() bool {
		return len(f.app.queueSnapshotForThread(f.caller.ID)) == 0
	})
	after := "after-" + entityid.New()
	queueTestWake(t, f.app, f.caller.ID, after)
	f.deliveredOnce(t, after)
	if n := f.sent(t, withdrawn); n != 0 {
		t.Fatalf("the withdrawn message reached the provider %d times", n)
	}
}

// A message withdrawn while the dispatch worker holds it, waiting for the
// thread's action lock, is taken back: the withdrawal says it was removed,
// its durable row is gone, and the provider never sees it once the lock is
// free.
func TestAWithdrawalReachesABatchTheDispatchHoldsUnsent(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	if err := f.app.StartSession(f.caller.ID); err != nil {
		t.Fatal(err)
	}
	unlock := f.app.threadLocks().Lock(f.caller.ID)
	withdrawn := "withdrawn-" + entityid.New()
	sendID := queueTestWake(t, f.app, f.caller.ID, withdrawn)
	heldAtTheActionLock(t, f.app, f.caller.ID)
	removal, err := f.app.removeQueuedItem(t.Context(), f.caller.ID, sendID)
	if err != nil || removal != queueRemovalRemoved {
		t.Errorf("the withdrawal = %v, %v; want it removed", removal, err)
	}
	if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workflowWakeSendIDRoot); len(ids) != 0 {
		t.Errorf("queued after the withdrawal = %q, want nothing", ids)
	}
	unlock()
	after := "after-" + entityid.New()
	queueTestWake(t, f.app, f.caller.ID, after)
	f.deliveredOnce(t, after)
	if n := f.sent(t, withdrawn); n != 0 {
		t.Fatalf("the withdrawn message reached the provider %d times", n)
	}
}

// S r6 finding 2: a live thread a delete claimed is handed a sidebar
// worker's approval wake, and its dispatch worker holds the batch at the
// action lock when the person answers the approval in the worker. The
// answer takes the wake back, and once the delete is refused the provider
// is sent what came later, never the answered approval.
func TestAnApprovalAnsweredWhileTheDispatchHoldsItNeverReachesTheProvider(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	worker := f.plainThread(t)
	recordSpawn(t, f.app, "spawn-held", f.caller.ID, worker.ID)
	if _, err := f.app.store.SetThreadPlacement([]string{worker.ID}, store.PlacementStandalone); err != nil {
		t.Fatal(err)
	}
	if err := f.app.StartSession(f.caller.ID); err != nil {
		t.Fatal(err)
	}
	refusedDelete(t, f.app, f.caller.ID, func() {
		unlock := f.app.threadLocks().Lock(f.caller.ID)
		askApproval(t, f.app, worker.ID, "approval-held", "make release")
		waitUntil(t, 10*time.Second, func() bool {
			_ = f.app.drainWorkerPrompts(t.Context(), 10*time.Second)
			return len(queuedSendIDsOf(t, f.app, f.caller.ID, workerPromptSendIDRoot)) == 1
		})
		heldAtTheActionLock(t, f.app, f.caller.ID)
		resolveApprovalOn(t, f.app, worker.ID, "approval-held")
		if err := f.app.drainWorkerPrompts(t.Context(), 10*time.Second); err != nil {
			t.Fatal(err)
		}
		if ids := queuedSendIDsOf(t, f.app, f.caller.ID, workerPromptSendIDRoot); len(ids) != 0 {
			t.Errorf("the answered approval's wake is still queued: %q", ids)
		}
		unlock()
		quietAtGates(t, f.app)
		// The dispatch waits for the decision with nothing left to send.
		if queued := f.app.queueSnapshotForThread(f.caller.ID); len(queued) != 0 {
			t.Errorf("queued while the dispatch waits = %+v, want nothing", queued)
		}
	})
	after := "after-" + entityid.New()
	queueTestWake(t, f.app, f.caller.ID, after)
	f.deliveredOnce(t, after)
	if n := f.sent(t, "make release"); n != 0 {
		t.Fatalf("the answered approval reached the provider %d times", n)
	}
}
