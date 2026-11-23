package app

// The write that cannot be taken back carries its own precondition: what
// a decision on rest reads of the store is read again inside the write's
// transaction, on that transaction's connection, so work a thread below
// takes before the write holds it, a workflow run's hold has no gap
// between its rest and its report, and the write needs no second
// connection.

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/workflow/engine"
)

// newOneConnectionRequestFixture is newRequestFixture on a store with one
// connection, as the rollback-journal fallback and a quiesced read pool
// leave it: every read waits for the writer's connection.
func newOneConnectionRequestFixture(t *testing.T) *requestFixture {
	t.Helper()
	app, bus := setupE2EAppOn(t, ":memory:", t.TempDir())
	app.configDir = t.TempDir()
	caller, err := createTestThread(t, app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create caller thread: %v", err)
	}
	app.installThreadRequestObserver()
	return &requestFixture{app: app, bus: bus, caller: caller}
}

// completes runs fn and fails unless it returns within a bound no
// healthy run comes near: a write that waits for its own connection never
// returns. It ends the test binary rather than fail the test, whose
// cleanup, the app's shutdown, would wait on that write: a panic off the
// test's goroutine is not recovered.
func completes(what string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		go func() { panic(what + " did not complete on a store with one connection") }()
		select {}
	}
}

// On a store with one connection, a held request settles at rest and the
// retention sweep clears a spawn past the floor, with a request elsewhere
// changing during the sweep's read: neither write asks for a second
// connection while its own is held.
func TestRestDecisionsCompleteOnOneConnection(t *testing.T) {
	t.Parallel()
	f := newOneConnectionRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	installMockClaudeTurns(t, f.app, [][]string{reportTurn("Rebuilt the index.")})
	var ack threadtools.RequestAck
	completes("the spawn and its settlement at rest", func() {
		ack = f.spawnWorker(t, "rebuild the index")
		f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
		f.app.threadRequestsWG.Wait()
	})

	var changed atomic.Bool
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, ack.ThreadID) || !changed.CompareAndSwap(false, true) {
			return
		}
		now := time.Now().UnixMilli()
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "elsewhere", CallerThreadID: f.caller.ID,
			Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: true, State: store.ThreadRequestRunning,
			CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Error(err)
		}
	}
	later := time.Now().AddDate(0, 0, store.ThreadRequestRetentionDays+1)
	passesUntil(t, func() { completes("the retention sweep", func() { f.app.expireThreadRequests(later) }) }, func() error {
		if !changed.Load() {
			t.Fatal("the sweep never read the worker's rest")
		}
		if _, found, err := f.app.store.GetThreadRequest(ack.Token); err != nil || found {
			return fmt.Errorf("the spawn's token after the sweep: found %v, %v; want it cleared to its lineage", found, err)
		}
		return nil
	})
}

// A change to what memory holds of a thread below, made once the turn end
// has read the subtree, vetoes the settlement's write: the request stays
// held until a pass reads again. The same change off the subtree does not
// (TestSteadyChangesOffTheTreeDoNotStarveASettlement).
func TestAMemoryChangeBelowVetoesTheSettlement(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, below := f.heldWorker(t, 1)
	letGo := f.holdAgentRowsDrain(t)
	var changed atomic.Bool
	// The change only bumps the revision, so the pass the veto starts reads
	// the subtree at rest and settles; that read waits until the request
	// has been seen held.
	seen := make(chan struct{})
	var seenOnce sync.Once
	sawHeld := func() { seenOnce.Do(func() { close(seen) }) }
	t.Cleanup(sawHeld)
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, worker) {
			return
		}
		if changed.CompareAndSwap(false, true) {
			f.app.noteThreadRestChange(below[0].ID)
			return
		}
		<-seen
	}
	f.endWorkerTurn(t, worker, gate)
	if !changed.Load() {
		t.Fatal("the turn end never read the worker's rest")
	}
	f.expectHeld(t, worker, token, "though memory of a thread below changed before the write")
	sawHeld()
	letGo()
	f.expectFinished(t, token)
}

// A child's loop committed once the settlement has read the subtree, at
// the settlement's own lookup of the answering agent just before its
// write: the request stays held.
func TestWorkCommittedBeforeTheWriteHoldsTheRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, children := f.heldWorker(t, 1)
	f.holdAgentRowsDrain(t)
	var armed atomic.Bool
	stop := f.app.store.ObserveReads(func() {
		if armed.Load() {
			return
		}
		var pcs [64]uintptr
		frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
		var answer, settlement bool
		for {
			frame, more := frames.Next()
			answer = answer || strings.HasSuffix(frame.Function, ".answerAgent")
			settlement = settlement || strings.HasSuffix(frame.Function, ".settleReadReceiptLocked")
			if !more {
				break
			}
		}
		if answer && settlement && armed.CompareAndSwap(false, true) {
			f.armChildLoop(t, children[0].ID, "after the read")
		}
	})
	defer stop()
	f.endWorkerTurn(t, worker, gate)
	if !armed.Load() {
		t.Fatal("the settlement never looked up its answering agent")
	}
	f.expectHeld(t, worker, token, "though a thread below took work before the write")
}

// A run bound to a thread below the worker comes to rest in the store, its
// event not yet emitted, as the worker's turn ends: the worker's request
// stays held.
func TestARunBelowBetweenItsRestAndItsEventHoldsTheRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, children := f.heldWorker(t, 1)
	item := f.reportingRuns(t, children[0].ID).start(t, children[0].ID, "finish the check")
	before, err := f.app.heldThreadAtRest(worker)
	before.done()
	if err != nil || before.rests(worker) {
		t.Fatalf("a running run below reads at rest %v, %v; want not", before.rests(worker), err)
	}
	f.holdAgentRowsDrain(t)
	// The engine's write of the rest; its event comes once the check's
	// gate opens, at the test's end.
	if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.endWorkerTurn(t, worker, gate)
	f.expectHeld(t, worker, token, "between a run's rest below and its event")
}

// A run bound to the worker comes to rest in the store, the write owing
// its report, before its event, with the report's delivery held: the
// worker's request stays held through the rest's write, its event and the
// held delivery, and settles once the worker has read the report.
func TestARunsRestHoldsItsThreadUntilTheReportIsDelivered(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	turns := filepath.Join(t.TempDir(), "turns")
	installMockClaudeTurns(t, f.app, [][]string{
		gatedTurn(turns, "Started the run."),
		reportTurn("The run is done."),
	})
	w := f.spawnWorker(t, "run the checks")
	f.awaitTurnRunning(t, w.Token)
	item := f.reportingRuns(t, w.ThreadID).start(t, w.ThreadID, "ship it")
	release(t, turns)
	f.awaitRestHold(t, w)

	unblock := f.app.workflowApplication().BlockWakeQueueForTesting()
	t.Cleanup(unblock)
	// The engine's write of the rest, before its event.
	if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateDone), "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	f.expectHeld(t, w.ThreadID, w.Token, "between the run's rest and its event")

	f.app.workflowApplication().HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
		ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateDone,
	}, f.app.emit)
	f.app.threadRequestsWG.Wait()
	f.app.settleRestedReceipts()
	f.app.threadRequestsWG.Wait()
	f.expectHeld(t, w.ThreadID, w.Token, "with the run's report held")

	unblock()
	row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "The run is done." {
		t.Fatalf("settled %+v; want finished with the turn that read the report", row)
	}
}
