package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/workflow/engine"
)

// A held request's decision on rest costs its subtree: the same statements
// and the same threads watched with nothing else on the computer as with
// 200 busy threads off it, each with open work, a loop, a call parked on
// its send, a running workflow run and a live record, 500 workflow reports
// owed to them, a stream of memory changes on them during the decision, and
// a reached thread that made hundreds of sends and asks. A third thread
// below is one more thread watched, and a done decision watches nothing.
// The rows the statements examine, which grow with the subtree, are held by
// TestASubtreeReadCostsItsSubtree.
func TestARestDecisionCostsItsSubtree(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	child := f.settledChild(t, worker.ID)
	f.settledChild(t, child.ID)
	var watched int
	f.app.restReadForTest = func([]string) { watched = f.app.restRevisions.watchedThreads() }
	measure := func(what string) (int, int, map[string]int) {
		t.Helper()
		f.app.threadRequestsWG.Wait()
		stop := readsUnder(f.app.store, "app.(*App).heldThreadAtRest")
		decision, err := f.app.heldThreadAtRest(worker.ID)
		total, by := stop()
		decision.done()
		if err != nil || !decision.rests(worker.ID) {
			t.Fatalf("%s: worker at rest %v, %v; want at rest", what, decision.rests(worker.ID), err)
		}
		if left := f.app.restRevisions.watchedThreads(); left != 0 {
			t.Fatalf("%s: %d threads watched once the decision is done; want none", what, left)
		}
		return total, watched, by
	}
	quiet, quietWatched, quietBy := measure("nothing else on the computer")

	now := time.Now().UnixMilli()
	busyThreads := make([]string, 0, 200)
	for i := range 200 {
		busy := f.settledChild(t, f.caller.ID)
		busyThreads = append(busyThreads, busy.ID)
		at := now + int64(10*i)
		f.armChildLoop(t, busy.ID, "board")
		for _, send := range []struct {
			token  string
			notify bool
		}{{"busy-send-" + busy.ID, true}, {"busy-parked-" + busy.ID, false}} {
			if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: send.token, CallerThreadID: busy.ID,
				Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: send.notify, State: store.ThreadRequestRunning,
				CreatedAt: at, UpdatedAt: at}); err != nil {
				t.Fatal(err)
			}
		}
		_, _, end := f.app.beginRequestWait(context.Background(), busy.ID, []string{"busy-parked-" + busy.ID})
		t.Cleanup(end)
		if err := f.app.store.CreateWorkItem(store.WorkItem{ID: "run-" + busy.ID, ProjectID: busy.ProjectID, Goal: "elsewhere",
			WorkflowID: "build", WorkflowScope: "project", State: "running", Source: "manual", OriginThreadID: busy.ID, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.reassessWorker(busy.ID, true); err != nil || !f.app.liveWorker(busy.ID) {
			t.Fatalf("busy thread %s is not recorded live: %v", busy.ID, err)
		}
		if i >= 20 {
			continue
		}
		for _, kind := range []string{store.ThreadRequestSend, store.ThreadRequestAsk} {
			hist := "hist-" + kind + "-" + busy.ID
			if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: hist, CallerThreadID: child.ID, Kind: kind,
				TargetThreadID: f.caller.ID, Notify: true, State: store.ThreadRequestRunning, CreatedAt: at + 1, UpdatedAt: at + 1}); err != nil {
				t.Fatal(err)
			}
			if settled, err := f.app.store.SettleThreadRequest(hist, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
				State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: at + 2,
			}); err != nil || !settled {
				t.Fatalf("settle %s: %v %v", hist, settled, err)
			}
			if _, err := f.app.store.MarkThreadRequestDelivered(hist, store.ThreadWakeInline, at+2, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	owed := f.oweReports(t, busyThreads, 500)

	stop := make(chan struct{})
	var stream sync.WaitGroup
	stream.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				f.app.noteThreadRestChange(busyThreads[i%len(busyThreads)])
			}
		}
	})
	busy, busyWatched, busyBy := measure("200 busy threads and 500 owed reports off the subtree, changing, and 40 settled sends and asks below")
	close(stop)
	stream.Wait()
	if busy != quiet || busyWatched != quietWatched {
		t.Fatalf("the decision reads %d statements and watches %d threads with busy threads off the subtree, %d and %d without; want the same\n%v\n%v",
			busy, busyWatched, quiet, quietWatched, busyBy, quietBy)
	}

	f.settledChild(t, child.ID)
	deeper, deeperWatched, deeperBy := measure("one more thread below")
	if deeper != quiet || deeperWatched != quietWatched+1 {
		t.Fatalf("one more thread below costs %d statements and %d threads watched, the decision before it %d and %d; want the same statements, one more thread\n%v\n%v",
			deeper, deeperWatched, quiet, quietWatched, deeperBy, quietBy)
	}
	owed()
	t.Logf("a held request's decision with two threads below: %d statements %v, %d threads watched; a third below: %d threads watched", quiet, quietBy, quietWatched, deeperWatched)
}

// oweReports owes count workflow reports to threads, round robin: runs
// bound to them that rested, whose wake jobs wait behind a held queue. The
// returned func unbinds the runs, so their wake jobs deliver nothing, lets
// the jobs run, and checks no report is left owed.
func (f *requestFixture) oweReports(t *testing.T, threads []string, count int) (settle func()) {
	t.Helper()
	service := f.app.workflowApplication()
	unblock := service.BlockWakeQueueForTesting()
	t.Cleanup(unblock)
	thread, err := f.app.store.GetThread(threads[0])
	if err != nil {
		t.Fatal(err)
	}
	items := make([]string, count)
	for i := range count {
		item := store.WorkItem{ID: fmt.Sprintf("owed-%03d", i), ProjectID: thread.ProjectID, Goal: "elsewhere", WorkflowID: "build",
			WorkflowScope: "project", State: string(engine.StateRunning), Source: "manual", OriginThreadID: threads[i%len(threads)],
			CreatedAt: time.Now().UnixMilli()}
		if err := f.app.store.CreateWorkItem(item); err != nil {
			t.Fatal(err)
		}
		if err := f.app.store.UpdateWorkItemState(item.ID, string(engine.StateCancelled), string(engine.ReasonInterrupted), time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		service.HandleEngineEvent(eventchan.WorkflowItemState, engine.StateEvent{
			ItemID: item.ID, ProjectID: item.ProjectID, From: engine.StateRunning, To: engine.StateCancelled,
		}, f.app.emit)
		items[i] = item.ID
	}
	for _, id := range threads {
		if src, err := f.app.store.ReadThreadRestSources(id); err != nil || !src.Workflow {
			t.Fatalf("no report owed to %s: %+v, %v", id, src, err)
		}
	}
	return func() {
		t.Helper()
		for _, id := range items {
			if _, err := f.app.store.UpdateWorkItemOriginThread(id, ""); err != nil {
				t.Fatal(err)
			}
		}
		unblock()
		service.WaitWake()
		// Bound to the caller, which no other run holds, a run whose report
		// is still owed would hold it.
		for _, id := range items {
			if _, err := f.app.store.UpdateWorkItemOriginThread(id, f.caller.ID); err != nil {
				t.Fatal(err)
			}
		}
		f.expectNoReportOwed(t, f.caller.ID)
	}
}
