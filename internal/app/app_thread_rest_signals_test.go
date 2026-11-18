package app

// What a decision on rest takes from memory (a call parked on a send) and
// the revisions of what memory holds, which the decision's write compares
// against: a change on a thread it reached vetoes the write, a change
// anywhere else does not.

import (
	"context"
	"slices"
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
	// The drain's own reassessments bump revisions too; held, only the
	// changes a test makes do.
	f.holdAgentRowsDrain(t)
	return worker, child
}

// holds reports whether decision's write for root would happen now: what
// its guard reads inside the write's transaction, read here outside one.
func (f *requestFixture) holds(t *testing.T, decision *restDecision, root string) bool {
	t.Helper()
	subtree, err := f.app.store.ReadSubtreeRest(decision.guard(root).Roots)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(decision.stillAtRest(subtree), root)
}

// readAtRest decides root's rest, failing unless it reads at rest and its
// write would happen.
func (f *requestFixture) readAtRest(t *testing.T, root string) *restDecision {
	t.Helper()
	decision, err := f.app.readRest([]string{root})
	t.Cleanup(decision.done)
	if err != nil || !decision.rests(root) {
		t.Fatalf("read %s at rest %v, %v; want at rest", root, decision.rests(root), err)
	}
	if !f.holds(t, decision, root) {
		t.Fatal("a decision with no change since does not hold")
	}
	return decision
}

// A send a thread below made with no wake armed is open work of it while a
// call is parked on it: the read says not at rest until the call leaves,
// and a decision made before the call parked does not hold once it has.
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
	before := f.readAtRest(t, worker.ID)
	_, _, end := f.app.beginRequestWait(context.Background(), child.ID, []string{"parked-send"})
	if f.holds(t, before, worker.ID) {
		t.Fatal("a decision made before a call parked on a send below still holds")
	}
	read, err := f.app.readRest([]string{worker.ID})
	read.done()
	if err != nil || read.rests(worker.ID) {
		t.Fatalf("read with a call parked on a send below: at rest %v, %v; want not", read.rests(worker.ID), err)
	}
	end()
	f.readAtRest(t, worker.ID)
}

// An open send with no wake that the decision did not see, made below after
// it, keeps the write from happening: no read checked it for a parked call.
func TestASendTheDecisionDidNotCheckHoldsTheWrite(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	decision := f.readAtRest(t, worker.ID)
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "later-send", CallerThreadID: child.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: false, State: store.ThreadRequestRunning,
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if f.holds(t, decision, worker.ID) {
		t.Fatal("a decision holds with an open send below it never checked for a parked call")
	}
}

// A thread spawned below after the decision read the subtree keeps the
// write from happening: the decision noted no revision of it.
func TestAThreadReachedOnlyAtTheWriteHoldsIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	decision := f.readAtRest(t, worker.ID)
	f.settledChild(t, child.ID)
	if f.holds(t, decision, worker.ID) {
		t.Fatal("a decision holds over a thread it never read")
	}
}

// A change to what memory holds of a reached thread, its revision bumped,
// vetoes the write; the same change on a thread off the subtree does not,
// and once the decision is done nothing is watched.
func TestAMemoryChangeVetoesOnlyItsOwnSubtree(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	off := f.settledChild(t, f.caller.ID)
	decision, err := f.app.readRest([]string{worker.ID})
	if err != nil || !decision.rests(worker.ID) {
		t.Fatalf("read at rest %v, %v", decision.rests(worker.ID), err)
	}
	if watched := f.app.restRevisions.watchedThreads(); watched != 2 {
		t.Fatalf("%d threads watched during a decision on two; want 2", watched)
	}
	f.app.noteThreadRestChange(off.ID)
	f.app.noteThreadRestChange(f.caller.ID)
	if !f.holds(t, decision, worker.ID) {
		t.Fatal("a change off the subtree vetoed the write")
	}
	f.app.noteThreadRestChange(child.ID)
	if f.holds(t, decision, worker.ID) {
		t.Fatal("a change to a reached thread's memory did not veto the write")
	}
	decision.done()
	decision.done()
	if watched := f.app.restRevisions.watchedThreads(); watched != 0 {
		t.Fatalf("%d threads watched after the decision; want none", watched)
	}
}

// A message queued for a thread below vetoes a decision under way as the
// queue changes, before the queue's worker publishes it, which here never
// runs.
func TestAQueuedMessageBelowVetoesADecisionAtOnce(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	decision := f.readAtRest(t, worker.ID)
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
	if f.holds(t, decision, worker.ID) {
		t.Fatal("a decision holds after a message was queued below")
	}
}

// A background tray frame the record skips, a launch running on a thread
// the record says waits on background work, still vetoes a decision under
// way: Codex's background work is memory, and the launch new work.
func TestATrayFrameTheRecordSkipsVetoesADecision(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	decision := f.readAtRest(t, worker.ID)
	w := &f.app.liveWorkers
	w.mu.Lock()
	if w.threads == nil {
		w.threads = map[string]liveWorker{}
	}
	w.threads[child.ID] = liveWorker{workerRest: workerRest{unrest: workerWaiting}, background: true}
	w.mu.Unlock()
	f.app.noteBackgroundTrayLiveness(triage.BackgroundTrayEvent{ThreadID: child.ID, LaunchIDs: []string{"launch-1"},
		Rows: []store.Item{{ThreadID: child.ID, Status: "running"}}})
	if f.holds(t, decision, worker.ID) {
		t.Fatal("a decision holds after a launch started below")
	}
}
