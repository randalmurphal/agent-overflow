package app

// A decision on rest that cannot be taken back reads the threads' own
// sources (readRest) and is written only if no work came to a thread it
// read before the write (restDecision.guard): work admitted during the
// read holds it, a stale record of work that ended does not, a read that
// fails is retried by the sweep with no new event, and changes elsewhere
// on the computer neither hold it nor add to what it reads.

import (
	"bytes"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
)

// passesUntil runs pass, one retention pass, until cleared returns nil, and
// fails t with what cleared last returned once 10 seconds have passed. A
// pass that reads a thread at rest keeps its spawns whole when what memory
// holds of that thread changes before the write, as a session's late
// events or a queue write can after its turn ends, and the product's next
// pass reads again (sweepThreadRequestsBefore): some pass clears a spawn
// whose worker rests, not always the first.
func passesUntil(t *testing.T, pass func(), cleared func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for passes := 1; ; passes++ {
		pass()
		err := cleared()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after %d retention passes: %v", passes, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// armChildLoop arms a loop on threadID as registerAgentWake does: the row,
// then its frame.
func (f *requestFixture) armChildLoop(t *testing.T, threadID, label string) store.AgentWake {
	t.Helper()
	wake, err := f.app.store.RegisterAgentWake(store.AgentWake{
		ID: newAgentWakeID(), ThreadID: threadID, Kind: store.AgentWakeLoop, Label: label,
		IntervalSeconds: 3600, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Error(err)
		return store.AgentWake{}
	}
	f.app.emitAgentWakeChanged(threadID, wake.ID)
	return wake
}

func (f *requestFixture) cancelChildLoop(t *testing.T, wake store.AgentWake) {
	t.Helper()
	if _, found, err := f.app.cancelAgentWake(wake.ThreadID, wake.Label, store.AgentWakeLoop); err != nil || !found {
		t.Fatalf("cancelAgentWake: %v %v", found, err)
	}
}

// endWorkerTurn releases the worker's gated turn and waits until its turn
// end's settlement has read the worker's requests.
func (f *requestFixture) endWorkerTurn(t *testing.T, worker, gate string) {
	t.Helper()
	ended := f.awaitTurnEndRead(t, worker)
	release(t, gate)
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker's turn end was never read")
	}
}

// heldWorker spawns a worker whose turn holds open, with children settled
// children spawns of it made earlier.
func (f *requestFixture) heldWorker(t *testing.T, children int) (worker, token, gate string, below []store.Thread) {
	t.Helper()
	f.watchAgentRows()
	f.holdCallerSession()
	gate = filepath.Join(t.TempDir(), "worker-turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Handed it on.")})
	w := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, w.Token)
	for range children {
		below = append(below, f.settledChild(t, w.ThreadID))
	}
	return w.ThreadID, w.Token, gate, below
}

func (f *requestFixture) expectHeld(t *testing.T, worker, token, why string) {
	t.Helper()
	if row := f.request(t, token); row.State != store.ThreadRequestRunning || !f.app.restHeld(worker, token) {
		t.Fatalf("request %+v settled %s", row, why)
	}
}

func (f *requestFixture) expectFinished(t *testing.T, token string) {
	t.Helper()
	row := f.awaitRequestState(t, token, store.ThreadRequestFinished)
	if string(row.Answer) != "Handed it on." || row.Revision != 1 {
		t.Fatalf("settled %+v; want finished once with the worker's message", row)
	}
}

// Two settled child spawns; the first takes a loop once the worker's turn
// end has read the threads and before the settlement is written, with the
// Agents rows' drain held so nothing records it. The request stays held,
// through the turn end and a pass, and settles once the loop is cancelled.
func TestWorkAdmittedDuringARestReadHoldsTheRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, below := f.heldWorker(t, 2)
	letGo := f.holdAgentRowsDrain(t)
	var loop atomic.Pointer[store.AgentWake]
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, worker) || loop.Load() != nil {
			return
		}
		wake := f.armChildLoop(t, below[0].ID, "board")
		loop.Store(&wake)
	}
	f.endWorkerTurn(t, worker, gate)
	if loop.Load() == nil {
		t.Fatal("the turn end never read the worker's rest")
	}
	f.expectHeld(t, worker, token, "though a thread below took work during its read")
	f.app.settleRestedReceipts()
	f.expectHeld(t, worker, token, "on a pass with a thread below working")

	letGo()
	f.cancelChildLoop(t, *loop.Load())
	f.expectFinished(t, token)
}

// A child whose loop is cancelled with the drain held, so the record still
// says it waits: the worker's request settles at its turn end, read from
// the child's sources.
func TestAStaleBusyRecordBelowDoesNotHoldASettlement(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, below := f.heldWorker(t, 1)
	child := below[0]
	wake := f.armChildLoop(t, child.ID, "board")
	waitUntil(t, 10*time.Second, func() bool { return f.app.liveWorker(child.ID) })
	f.holdAgentRowsDrain(t)
	f.cancelChildLoop(t, wake)
	if !f.app.liveWorker(child.ID) {
		t.Fatal("the drain recorded the cancel; the test needs the record stale")
	}
	f.endWorkerTurn(t, worker, gate)
	if row := f.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "Handed it on." {
		t.Fatalf("request %+v with the thread below at rest by its sources; want finished", row)
	}
	if !f.app.liveWorker(child.ID) {
		t.Fatal("the settlement wrote the record")
	}
}

// lockedBuffer is a log destination several goroutines write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A source of the worker's subtree cannot be read at its turn end: the
// request stays held and the failure is logged naming the thread; the
// sweep sleeps no longer than its retry while the read is owed, and once
// the source reads again the sweep's next pass settles it, with no new
// event. Serial: it captures the global logger.
func TestAFailedRestReadIsRetriedByTheSweep(t *testing.T) {
	logs := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	f := newRequestFixture(t)
	worker, token, gate, _ := f.heldWorker(t, 1)
	f.app.threadRequestsWG.Wait()
	restore := f.failRestSource(t)
	f.endWorkerTurn(t, worker, gate)
	f.app.threadRequestsWG.Wait()
	f.expectHeld(t, worker, token, "with its sources unread")
	if got := logs.String(); !strings.Contains(got, worker) || !strings.Contains(got, "remote_watches") {
		t.Fatalf("the failed read was not logged naming %s:\n%s", worker, got)
	}
	if delay := f.app.threadRequestSweepDelay(time.Now()); delay > restRecoveryRetry {
		t.Fatalf("the sweep sleeps %v with a failed read of rest owed; want at most %v", delay, restRecoveryRetry)
	}
	restore()
	f.app.threadRequestsWG.Wait()
	f.expectHeld(t, worker, token, "with no retry")
	f.app.retryFailedRestReads()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, token); row.State != store.ThreadRequestFinished {
		t.Fatalf("request %+v after the sweep's retry; want finished", row)
	}
	if f.app.restReadsFailed() {
		t.Fatal("the read is still owed once it read")
	}
}

// A spawn past the floor whose worker takes a loop once the sweep has read
// it, with the drain held, is left whole; once the loop is cancelled a
// later sweep keeps only its lineage.
func TestWorkAdmittedDuringTheSweepsReadKeepsTheSpawnWhole(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	installMockClaudeTurns(t, f.app, [][]string{reportTurn("Rebuilt the index.")})
	ack := f.spawnWorker(t, "rebuild the index")
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	f.app.threadRequestsWG.Wait()
	f.holdAgentRowsDrain(t)
	var loop atomic.Pointer[store.AgentWake]
	f.app.restReadForTest = func(roots []string) {
		if slices.Contains(roots, ack.ThreadID) && loop.Load() == nil {
			wake := f.armChildLoop(t, ack.ThreadID, "index")
			loop.Store(&wake)
		}
	}
	later := time.Now().AddDate(0, 0, store.ThreadRequestRetentionDays+1)
	f.app.expireThreadRequests(later)
	if loop.Load() == nil {
		t.Fatal("the sweep never read the worker's rest")
	}
	if row, found, err := f.app.store.GetThreadRequest(ack.Token); err != nil || !found || string(row.Answer) != "Rebuilt the index." {
		t.Fatalf("the spawn after the sweep: %+v, found %v, %v; want it whole", row, found, err)
	}
	f.app.restReadForTest = nil
	f.cancelChildLoop(t, *loop.Load())
	passesUntil(t, func() { f.app.expireThreadRequests(later) }, func() error {
		if _, found, err := f.app.store.GetThreadRequest(ack.Token); err != nil || found {
			return fmt.Errorf("the spawn's token once its worker rests: found %v, %v; want unknown", found, err)
		}
		return nil
	})
}

// Work admitted on threads outside the worker's subtree while its turn end
// reads it, a loop, a request and a workflow run of another thread, with
// more such changes running all along, does not hold the request: it
// settles at that turn end.
func TestSteadyChangesOffTheTreeDoNotStarveASettlement(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, token, gate, _ := f.heldWorker(t, 1)
	off := f.settledChild(t, f.caller.ID)
	run := store.WorkItem{ID: "off-run", ProjectID: off.ProjectID, Goal: "elsewhere", WorkflowID: "build", WorkflowScope: "project",
		State: "running", Source: "manual", OriginThreadID: off.ID, CreatedAt: time.Now().UnixMilli()}
	if err := f.app.store.CreateWorkItem(run); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var steady sync.WaitGroup
	steady.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				f.app.noteWorkerLiveness(off.ID)
				f.app.noteWorkflowRunChanged(run.ID)
			}
		}
	})
	var changed atomic.Bool
	f.app.restReadForTest = func(roots []string) {
		if !slices.Contains(roots, worker) || !changed.CompareAndSwap(false, true) {
			return
		}
		f.armChildLoop(t, off.ID, "elsewhere")
		now := time.Now().UnixMilli()
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "off-send", CallerThreadID: off.ID,
			Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: true, State: store.ThreadRequestRunning,
			CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Error(err)
		}
	}
	f.endWorkerTurn(t, worker, gate)
	close(stop)
	steady.Wait()
	if !changed.Load() {
		t.Fatal("the turn end never read the worker's rest")
	}
	if row := f.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "Handed it on." {
		t.Fatalf("request %+v with work changing only off its subtree; want finished at its turn end", row)
	}
}
