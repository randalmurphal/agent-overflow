package app

// What a read of rest takes from memory (a call parked on a send) and the
// owners' change signals a decision compares against at its write.

import (
	"context"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// restTree is a thread with one settled child spawn of it, both at rest,
// with the Agents rows' drain held.
func (f *requestFixture) restTree(t *testing.T) (worker, child store.Thread) {
	t.Helper()
	worker, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	child = f.settledChild(t, worker.ID)
	// The drain's own reassessments signal too; held, only the changes a
	// test makes do.
	f.holdAgentRowsDrain(t)
	return worker, child
}

// readAtRest reads root's rest and returns the read, failing unless it
// reads at rest.
func (f *requestFixture) readAtRest(t *testing.T, root string) *restRead {
	t.Helper()
	read, err := f.app.readRest([]string{root})
	t.Cleanup(read.done)
	if err != nil || !read.rests(root) {
		t.Fatalf("read %s at rest %v, %v; want at rest", root, read.rests(root), err)
	}
	if still, err := read.unchanged(root); err != nil || !still {
		t.Fatalf("a read with no change since: unchanged %v, %v", still, err)
	}
	return read
}

// A send a thread below made with no wake armed is open work of it while a
// call is parked on it: the read says not at rest until the call leaves.
func TestACallParkedOnASendBelowHoldsRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "parked-send", CallerThreadID: child.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: false, State: store.ThreadRequestRunning,
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	f.readAtRest(t, worker.ID)
	_, _, end := f.app.beginRequestWait(context.Background(), child.ID, []string{"parked-send"})
	read, err := f.app.readRest([]string{worker.ID})
	read.done()
	if err != nil || read.rests(worker.ID) {
		t.Fatalf("read with a call parked on a send below: at rest %v, %v; want not", read.rests(worker.ID), err)
	}
	end()
	f.readAtRest(t, worker.ID)
}

// A message queued for a thread below changes a decision under way as the
// queue changes, before the queue's worker publishes it, which here never
// runs.
func TestAQueuedMessageBelowChangesADecisionAtOnce(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	read := f.readAtRest(t, worker.ID)
	s := &f.app.sendsPending
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		s.running = false
		clear(s.dirty)
		s.mu.Unlock()
	})
	f.app.markSendsPendingDirty(child.ID)
	if still, err := read.unchanged(worker.ID); err != nil || still {
		t.Fatalf("a decision after a message was queued below: unchanged %v, %v; want changed", still, err)
	}
}

// A background tray frame the record skips, a launch running on a thread
// the record says waits on background work, still changes a decision under
// way: the record may be stale, and the launch new work.
func TestATrayFrameTheRecordSkipsChangesADecision(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	read := f.readAtRest(t, worker.ID)
	w := &f.app.liveWorkers
	w.mu.Lock()
	if w.threads == nil {
		w.threads = map[string]liveWorker{}
	}
	w.threads[child.ID] = liveWorker{workerRest: workerRest{unrest: workerWaiting}, background: true}
	w.mu.Unlock()
	f.app.noteBackgroundTrayLiveness(triage.BackgroundTrayEvent{ThreadID: child.ID, LaunchIDs: []string{"launch-1"},
		Rows: []store.Item{{ThreadID: child.ID, Status: "running"}}})
	if still, err := read.unchanged(worker.ID); err != nil || still {
		t.Fatalf("a decision after a launch started below: unchanged %v, %v; want changed", still, err)
	}
}
