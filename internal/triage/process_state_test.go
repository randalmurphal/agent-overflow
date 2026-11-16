package triage

import (
	"reflect"
	"testing"

	"agent-overflow/internal/provider"
)

// A session commit replaces the state of the process it retires and keeps
// the rest: the queue, the thread's epoch, which only grows, and its locks.
// A replacement reaches the commit with no cleanup; a restart after a stop
// reaches it after one.
func TestASessionCommitReplacesOnlyItsProcessState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cleanup bool
	}{{"replacement", false}, {"restart after cleanup", true}} {
		t.Run(tc.name, func(t *testing.T) {
			const threadID = "thread-commit"
			r, st, _ := newTestRouter(t)
			createTestThread(t, st, threadID)
			r.MarkThreadActive(threadID, "session-a")
			r.RegisterQueueItem(threadID, makeQueueItem("queue:0", "held for the next session"))
			r.mu.Lock()
			s := r.state(threadID)
			s.softClosedTurn, s.softClosedTurnSet = 2, true
			s.settledTurns = map[int]bool{1: true}
			s.lastSettled, s.lastSettledSet = settledTurnOutcome{turn: 1, cut: true}, true
			s.waitingStop, s.waitingStopSet = waitingStop{turn: 2, summary: StoppedByUser}, true
			s.effectiveModel, s.effectiveModelSet = "claude-opus-4-8", true
			s.announcedDeath = "session-died:1"
			s.deliveriesAwaitingTurn = []SendDelivery{{UserItemID: "user:1:flush:1"}}
			s.pendingWakeupAt, s.pendingWakeupSet = 1, true
			s.subagentProgress = map[string]provider.SubagentProgressMeta{"launch-1": {}}
			s.compactingSince, s.compactingSinceSet = 1, true
			s.commandLifecycle = map[string]commandLifecycleEntry{"command-1": {}}
			s.workspacePath, s.workspacePathSet = "/moved", true
			r.mu.Unlock()
			anchor, epoch := r.flushAnchor(threadID), r.ThreadEpoch(threadID)

			if tc.cleanup {
				r.CleanupThread(threadID)
			}
			r.MarkThreadActive(threadID, "session-b")

			r.mu.Lock()
			var bound processBound
			if s := r.threadStateIfPresent(threadID); s != nil {
				bound = s.processBound
			}
			r.mu.Unlock()
			if !reflect.DeepEqual(bound, processBound{}) {
				t.Fatalf("the commit kept the retired process's state: %+v", bound)
			}
			// A held Stop is host state, dropped by name: the replacement
			// reuses turn indexes.
			r.mu.Lock()
			held := false
			if s := r.threadStateIfPresent(threadID); s != nil {
				held = s.waitingStopSet
			}
			r.mu.Unlock()
			if held {
				t.Fatal("the commit kept a Stop held for the retired process's turn")
			}
			if !tc.cleanup && !r.HasQueuedFlushItems(threadID) {
				t.Fatal("a replacement dropped the queue the host owns")
			}
			if r.flushAnchor(threadID) != anchor {
				t.Fatal("the commit replaced the thread's flush anchor")
			}
			if got := r.ThreadEpoch(threadID); got <= epoch {
				t.Fatalf("epoch = %d after %d, want it to grow", got, epoch)
			}
		})
	}
}
