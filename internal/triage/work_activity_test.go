package triage

import (
	"testing"
	"time"
)

func TestUnfinishedWorkKeepsEchoVisibleAcrossConsumptionAndCleanup(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.RegisterPendingSendWithExpectation("thread", "user:1", 1, PendingSendExpectation{})
	release := r.beginPendingEcho("thread")
	// Teardown may sweep pending sends while an echo is already processing.
	r.CleanupThread("thread")
	if !r.HasPendingWork("thread") || !r.AnyUnfinishedWork(time.Minute) {
		t.Fatal("in-flight echo became invisible during cleanup")
	}
	release()
	if r.HasPendingWork("thread") || r.AnyUnfinishedWork(time.Minute) {
		t.Fatal("finished echo left an admission claim")
	}
}

func TestUnfinishedWorkIncludesFlushHandoffAfterCleanup(t *testing.T) {
	r, _, _ := newTestRouter(t)
	r.SetFlushDispatcher(func(threadID string, items []QueuedFlushItem) {
		r.CleanupThread(threadID)
		if !r.AnyUnfinishedWork(time.Minute) {
			t.Fatal("batch mid-handoff became invisible")
		}
	})
	r.RegisterQueueItem("thread", makeQueueItem("queue:0", "send"))
	r.tryFlushQueue("thread")
	if r.AnyUnfinishedWork(time.Minute) {
		t.Fatal("settled dispatch left an admission claim")
	}
}

func TestUnfinishedWorkUsesExistingOwners(t *testing.T) {
	cases := map[string]func(*threadState){
		"turn":       func(s *threadState) { s.openTurnSet = true },
		"round":      func(s *threadState) { s.currentRoundOpen = true },
		"settlement": func(s *threadState) { s.streamingItemCount = 1 },
		"approval":   func(s *threadState) { s.pendingApprovalOrder = []string{"request"} },
		"question":   func(s *threadState) { s.pendingUserInputOrder = []string{"request"} },
		"wakeup": func(s *threadState) {
			s.pendingWakeupSet = true
			s.pendingWakeupAt = time.Now().Add(time.Hour).UnixMilli()
		},
	}
	for name, active := range cases {
		t.Run(name, func(t *testing.T) {
			r, _, _ := newTestRouter(t)
			r.mu.Lock()
			active(r.state("thread"))
			r.mu.Unlock()
			if !r.AnyUnfinishedWork(time.Minute) {
				t.Fatal("active work was ignored")
			}
		})
	}
	t.Run("expired wakeup", func(t *testing.T) {
		r, _, _ := newTestRouter(t)
		r.mu.Lock()
		s := r.state("thread")
		s.pendingWakeupSet = true
		s.pendingWakeupAt = time.Now().Add(-time.Hour).UnixMilli()
		r.mu.Unlock()
		if r.AnyUnfinishedWork(time.Minute) {
			t.Fatal("stale wakeup blocked maintenance")
		}
	})
}
