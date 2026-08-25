package triage

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestFlushClaimSurvivesCleanupMidHandoff pins the claimedFlushItems
// lifetime: the mid-handoff claim lives on the never-deleted threadIdentity,
// not on threadState, so a session teardown sweeping the thread's state while
// the dispatcher holds a batch must NOT make that batch invisible to
// QueuedFlushItemCount (the revert-on-interrupt predicate reads it — C14-1).
//
// The T1 threadState split briefly moved the claim onto the deletable state
// (Codex cross-review finding 1, 2026-08-25). Two behaviors regressed, both
// asserted here:
//
//  1. CleanupThread mid-handoff dropped the claim, so the in-flight batch
//     read as zero.
//  2. A successor session's own claim was then eaten by the old dispatch's
//     deferred decrement (+1 gen1, sweep, +2 gen2, −1 → 1 instead of 2 while
//     gen2's batch was still in flight — with the clamp hiding the mismatch).
func TestFlushClaimSurvivesCleanupMidHandoff(t *testing.T) {
	router, _, _ := newTestRouter(t)

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var call atomic.Int32
	router.SetFlushDispatcher(func(threadID string, items []QueuedFlushItem) {
		if call.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
	})

	waitForCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if got := router.QueuedFlushItemCount("t1"); got == want {
				return
			} else if time.Now().After(deadline) {
				t.Fatalf("QueuedFlushItemCount: got %d, want %d", got, want)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	// Generation 1: one item, dispatcher blocks holding the claim.
	router.RegisterQueueItem("t1", makeQueueItem("queue:0", "first"))
	go router.tryFlushQueue("t1")
	<-firstEntered
	if got := router.QueuedFlushItemCount("t1"); got != 1 {
		t.Fatalf("claim before sweep: count = %d, want 1", got)
	}

	// Session teardown sweeps threadState while the batch is mid-handoff.
	router.CleanupThread("t1")
	if got := router.QueuedFlushItemCount("t1"); got != 1 {
		t.Fatalf("claim after sweep: count = %d, want 1 (in-flight batch went invisible)", got)
	}

	// Generation 2: successor session queues two items; its dispatch
	// returns immediately, so only its own claim (+2, −2) settles.
	router.MarkThreadActive("t1")
	router.RegisterQueueItem("t1", makeQueueItem("queue:1", "second"))
	router.RegisterQueueItem("t1", makeQueueItem("queue:2", "third"))
	if got := router.QueuedFlushItemCount("t1"); got != 3 {
		t.Fatalf("queued+claimed: count = %d, want 3", got)
	}
	if !router.tryFlushQueue("t1") {
		t.Fatalf("generation-2 flush did not dispatch")
	}
	// Gen-2's decrement has settled (its dispatch was synchronous); gen-1's
	// claim must still stand — a threadState-scoped claim would read 0 here.
	if got := router.QueuedFlushItemCount("t1"); got != 1 {
		t.Fatalf("after generation-2 settle: count = %d, want 1 (old decrement ate the claim)", got)
	}

	close(releaseFirst)
	waitForCount(0)
}
