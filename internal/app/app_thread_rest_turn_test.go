package app

// A thread's open turn holds the threads above it from rest: the row a turn
// start commits is read inside the decision's write, whether or not its
// frame has been published, and the opening triage marks before that row
// moves the thread's revision at the mark.

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A provider-started turn on a settled child commits its open turn row once
// the worker's turn end has read the subtree, and is held there before its
// frame: the worker's request stays held.
func TestATurnOpeningBelowHoldsTheRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	worker, token, gate, children := f.heldWorker(t, 1)
	child := children[0].ID
	f.holdAgentRowsDrain(t)
	opened, letStartGo := make(chan struct{}), make(chan struct{})
	var openOnce, goOnce sync.Once
	letGo := func() { goOnce.Do(func() { close(letStartGo) }) }
	t.Cleanup(letGo)
	f.app.triage.SetTurnRowWrittenGate(func(id string) {
		if id == child {
			openOnce.Do(func() { close(opened) })
			<-letStartGo
		}
	})
	t.Cleanup(func() { f.app.triage.SetTurnRowWrittenGate(nil) })
	started := make(chan error, 1)
	var staged atomic.Bool
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, worker) || !staged.CompareAndSwap(false, true) {
			return
		}
		go func() {
			started <- f.app.triage.Handle(provider.ProviderEvent{
				Kind: provider.EventTurnStart, ThreadID: child, TurnID: child + ":below", TurnIndex: 1, Timestamp: time.Now(),
			})
		}()
		select {
		case <-opened:
		case err := <-started:
			t.Errorf("the child's turn start ended before its row: %v", err)
		case <-time.After(10 * time.Second):
			t.Error("the child's turn start never committed its row")
		}
	}
	f.endWorkerTurn(t, worker, gate)
	if !staged.Load() {
		t.Fatal("the turn end never read the worker's rest")
	}
	if _, open, err := f.app.store.GetActiveTurn(child); err != nil || !open {
		t.Fatalf("the child has no committed open turn: %v, %v", open, err)
	}
	f.expectHeld(t, worker, token, "though a thread below committed an open turn before the write")
	letGo()
	if err := <-started; err != nil {
		t.Fatal(err)
	}
}

// An open turn row committed on a thread below once the decision has read
// the subtree, with nothing in memory marking it: the request stays held.
func TestACommittedOpenTurnBelowHoldsTheRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, children := f.heldWorker(t, 1)
	child := children[0].ID
	f.holdAgentRowsDrain(t)
	var staged atomic.Bool
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, worker) || !staged.CompareAndSwap(false, true) {
			return
		}
		if err := f.app.store.InsertTurn(store.Turn{TurnID: child + ":1", ThreadID: child, TurnIndex: 1, StartedAt: time.Now().UnixMilli()}); err != nil {
			t.Error(err)
		}
	}
	f.endWorkerTurn(t, worker, gate)
	if !staged.Load() {
		t.Fatal("the turn end never read the worker's rest")
	}
	f.expectHeld(t, worker, token, "though a thread below has an open turn row")
}

// A thread whose latest turn row is open with nothing in this process
// running its turn has ended (triage.Router.TurnEnded): it reads at rest,
// not running for good, and the first read of a rest decision reads the
// thread above it at rest.
func TestAnOpenTurnRowNothingRunsReadsAtRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, child := f.restTree(t)
	if err := f.app.store.InsertTurn(store.Turn{TurnID: child.ID + ":1", ThreadID: child.ID, TurnIndex: 1, StartedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if rest, err := f.app.threadRest(child.ID); err != nil || rest.unrest != "" {
		t.Fatalf("a thread with an open turn row nothing runs reads %+v, %v; want at rest", rest, err)
	}
	decision, err := f.app.readRest([]string{worker.ID})
	defer decision.done()
	if err != nil || !decision.rests(worker.ID) {
		t.Fatalf("the thread above an open turn row nothing runs reads at rest %v, %v; want at rest", decision.rests(worker.ID), err)
	}
}

// A turn opening on a thread a decision watches has moved the thread's
// revision by the time its row is written, before its frame.
func TestATurnOpeningMovesItsRevisionBeforeItsFrame(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	child := f.settledChild(t, f.caller.ID)
	noted := f.app.restRevisions.watch([]string{child.ID})
	t.Cleanup(func() { f.app.restRevisions.unwatch(noted) })
	var atRow, reached atomic.Bool
	f.app.triage.SetTurnRowWrittenGate(func(id string) {
		if id == child.ID {
			reached.Store(true)
			atRow.Store(f.app.restRevisions.moved(noted)[child.ID])
		}
	})
	t.Cleanup(func() { f.app.triage.SetTurnRowWrittenGate(nil) })
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: child.ID, TurnID: child.ID + ":t", TurnIndex: 1, Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if !reached.Load() {
		t.Fatal("the turn start never wrote its row")
	}
	if !atRow.Load() {
		t.Fatal("the thread's revision had not moved when its turn row was written")
	}
}
