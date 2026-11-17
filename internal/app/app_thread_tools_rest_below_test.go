package app

// A held request settles `finished` from a reading of its thread's rest that
// is fresh at that moment, the threads below it included: a thread a spawn
// of it made that started work since the Agents rows' drain last ran keeps
// it held, and a source that cannot be read holds it for the next pass.

import (
	"bytes"
	"database/sql"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/threadtools"
)

// holdAgentRowsDrain makes the Agents rows' drain wait before its next
// batch until the returned func runs, so what liveWorkers records stays
// behind every change made meanwhile.
func (f *requestFixture) holdAgentRowsDrain(t *testing.T) (letGo func()) {
	t.Helper()
	f.app.threadRequestsWG.Wait()
	gate := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(letGo)
	f.app.threadAgentDrainForTest = func() { <-gate }
	return letGo
}

// settledChild makes a thread the worker spawned earlier, its spawn
// settled and its answer delivered: at rest, and nothing below the worker
// is open.
func (f *requestFixture) settledChild(t *testing.T, worker string) store.Thread {
	t.Helper()
	child, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	token := "spawn-of-" + child.ID
	now := time.Now().UnixMilli()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: token, CallerThreadID: worker,
		Kind: store.ThreadRequestSpawn, TargetThreadID: child.ID, State: store.ThreadRequestRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if settled, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadRequestFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal, SettledAt: now,
	}); err != nil || !settled {
		t.Fatalf("settle the child's spawn: %v %v", settled, err)
	}
	return child
}

// awaitTurnEndRead returns a channel closed once the settlement pass of
// threadID's turn end has read its requests, after its first turn ended.
func (f *requestFixture) awaitTurnEndRead(t *testing.T, threadID string) <-chan struct{} {
	t.Helper()
	read := make(chan struct{})
	var once sync.Once
	f.app.turnEndSettledForTest = func(id string) {
		if id != threadID {
			return
		}
		if turn, found, err := f.app.store.GetTurnByThreadIndex(threadID, 0); err == nil && found && turn.CompletedAt != nil {
			once.Do(func() { close(read) })
		}
	}
	return read
}

// A worker's turn ends while a thread it spawned earlier has work the
// drain has not recorded: a send with no wake the worker made, a workflow
// run bound to it, a message the person typed there. The worker's request
// stays held, through the turn end and a pass, and settles once that
// thread rests.
func TestAHeldRequestReadsTheThreadsBelowItFresh(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// busy gives the child work and returns what brings it to rest.
		busy func(t *testing.T, f *requestFixture, worker, child string) (rest func(t *testing.T))
	}{
		{"a send with no wake", func(t *testing.T, f *requestFixture, worker, child string) func(*testing.T) {
			gate := filepath.Join(t.TempDir(), "child-turn")
			installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Tailing it.")})
			if _, err := f.adapter().Send(t.Context(), threadtools.Caller{ThreadID: worker}, threadtools.SendCall{
				ThreadID: child, Message: "tail the deploy", Notify: false,
			}); err != nil {
				t.Fatalf("Send: %v", err)
			}
			return func(t *testing.T) { release(t, gate) }
		}},
		{"a workflow bound to it", func(t *testing.T, f *requestFixture, worker, child string) func(*testing.T) {
			item := f.reportingRuns(t, child).start(t, child, "ship it")
			return func(t *testing.T) {
				if _, err := f.app.WorkflowUnbindThread(item.ID); err != nil {
					t.Fatalf("WorkflowUnbindThread: %v", err)
				}
			}
		}},
		{"a message typed there", func(t *testing.T, f *requestFixture, worker, child string) func(*testing.T) {
			gate := filepath.Join(t.TempDir(), "child-turn")
			installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Looking.")})
			if err := f.app.SendMessage(t.Context(), child, "how is it going?", nil); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			return func(t *testing.T) { release(t, gate) }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerSession()
			gate := filepath.Join(t.TempDir(), "worker-turn")
			installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Handed it on.")})
			w := f.spawnWorker(t, "run the board")
			f.awaitTurnRunning(t, w.Token)
			child := f.settledChild(t, w.ThreadID)
			letGo := f.holdAgentRowsDrain(t)

			rest := c.busy(t, f, w.ThreadID, child.ID)
			waitUntil(t, 20*time.Second, func() bool {
				unrest, err := f.app.workerUnrest(child.ID)
				return err == nil && unrest.unrest != ""
			})
			if f.app.liveWorker(child.ID) {
				t.Fatal("the drain recorded the child's work; the test needs it unrecorded")
			}
			ended := f.awaitTurnEndRead(t, w.ThreadID)
			release(t, gate)
			select {
			case <-ended:
			case <-time.After(20 * time.Second):
				t.Fatal("the worker's turn end was never read")
			}
			f.app.settleRestedReceipts()
			if row := f.request(t, w.Token); row.State != store.ThreadRequestRunning || !f.app.restHeld(w.ThreadID, w.Token) {
				t.Fatalf("request %+v settled while a thread below its worker works", row)
			}

			letGo()
			rest(t)
			row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
			if string(row.Answer) != "Handed it on." || row.Revision != 1 {
				t.Fatalf("settled %+v; want finished once with the worker's message", row)
			}
		})
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

// A source of a thread below the worker cannot be read while the worker's
// turn end reads its rest: the request stays held, the failure is logged
// naming that thread, and a pass once the source reads again settles it.
// Serial: it captures the global logger.
func TestAFailedReadBelowAHeldRequestHoldsIt(t *testing.T) {
	logs := &lockedBuffer{}
	previous := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	gate := filepath.Join(t.TempDir(), "worker-turn")
	installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Handed it on.")})
	w := f.spawnWorker(t, "run the board")
	f.awaitTurnRunning(t, w.Token)
	child := f.settledChild(t, w.ThreadID)

	db, err := sql.Open("sqlite", "file:"+f.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var broken atomic.Bool
	f.app.threadRequestsWG.Wait()
	f.app.restBelowReadForTest = func(threadID string) {
		if threadID != w.ThreadID || !broken.CompareAndSwap(false, true) {
			return
		}
		// The worker's own sources read it at rest; the child's background
		// inventory reads remote_watches, now unreadable.
		if _, err := db.Exec(`ALTER TABLE remote_watches RENAME TO remote_watches_unreadable`); err != nil {
			t.Error(err)
		}
	}
	ended := f.awaitTurnEndRead(t, w.ThreadID)
	release(t, gate)
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker's turn end was never read")
	}
	if !broken.Load() {
		t.Fatal("the turn end never read below the worker")
	}
	if row := f.request(t, w.Token); row.State != store.ThreadRequestRunning || !f.app.restHeld(w.ThreadID, w.Token) {
		t.Fatalf("request %+v settled with a source below it unread", row)
	}
	if got := logs.String(); !strings.Contains(got, child.ID) {
		t.Fatalf("the failed read below the worker was not logged naming %s:\n%s", child.ID, got)
	}

	if _, err := db.Exec(`ALTER TABLE remote_watches_unreadable RENAME TO remote_watches`); err != nil {
		t.Fatal(err)
	}
	f.app.settleRestedReceipts()
	row := f.awaitRequestState(t, w.Token, store.ThreadRequestFinished)
	if string(row.Answer) != "Handed it on." {
		t.Fatalf("settled %+v; want finished with the worker's message", row)
	}
}

// A thread below the worker that the record holds with no spawn link, as
// one recorded before the spawn that names it did, is read again with its
// link: its loop keeps the worker from rest, and its cancel lets it rest.
func TestARestReadFindsAChildRecordedBeforeItsSpawnNamedIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	child := f.settledChild(t, worker.ID)
	wake, err := f.app.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-child", ThreadID: child.ID, Kind: store.AgentWakeLoop, Label: "board",
		IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	f.app.liveWorkers.mu.Lock()
	if f.app.liveWorkers.threads == nil {
		f.app.liveWorkers.threads = map[string]liveWorker{}
	}
	f.app.liveWorkers.threads[child.ID] = liveWorker{workerRest: workerRest{unrest: workerBlocked}}
	f.app.liveWorkers.mu.Unlock()
	if atRest, err := f.app.heldThreadAtRest(worker.ID); err != nil || atRest {
		t.Fatalf("worker at rest %v, %v with its child's loop armed; want not", atRest, err)
	}
	if _, found, err := f.app.store.CancelAgentWake(child.ID, wake.Label, store.AgentWakeLoop, agentWakeOutcomeCancelled); err != nil || !found {
		t.Fatalf("CancelAgentWake: %v %v", found, err)
	}
	// The cancel's frame reaches the drain, which records the child at rest.
	f.app.noteWorkerWaiting(child.ID)
	f.app.threadRequestsWG.Wait()
	if atRest, err := f.app.heldThreadAtRest(worker.ID); err != nil || !atRest {
		t.Fatalf("worker at rest %v, %v with its child's loop cancelled; want at rest", atRest, err)
	}
}

// A held request's read of rest costs the reads of its own thread's
// sources, one read of the links below it, a reassessment of each thread
// there and the listing's state twice: the same with nothing else on the
// computer as with busy threads outside the subtree, each with open work
// and a loop and recorded live. One more thread below costs one more
// reassessment.
func TestARestReadBelowCostsItsSubtree(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	child := f.settledChild(t, worker.ID)
	f.settledChild(t, child.ID)
	measure := func(what string) (int, map[string]int) {
		t.Helper()
		f.app.threadRequestsWG.Wait()
		stop := readsUnder(f.app.store, "app.(*App).heldThreadAtRest")
		atRest, err := f.app.heldThreadAtRest(worker.ID)
		total, by := stop()
		if err != nil || !atRest {
			t.Fatalf("%s: worker at rest %v, %v; want at rest", what, atRest, err)
		}
		return total, by
	}
	quiet, quietBy := measure("nothing else on the computer")

	now := time.Now().UnixMilli()
	for i := range 20 {
		busy := f.settledChild(t, f.caller.ID)
		if _, err := f.app.store.RegisterAgentWake(store.AgentWake{
			ID: "wake-" + busy.ID, ThreadID: busy.ID, Kind: store.AgentWakeLoop, Label: "board",
			IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{Token: "busy-send-" + busy.ID, CallerThreadID: busy.ID,
			Kind: store.ThreadRequestSend, TargetThreadID: f.caller.ID, Notify: true, State: store.ThreadRequestRunning,
			CreatedAt: now + int64(i), UpdatedAt: now + int64(i)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.app.reassessWorker(busy.ID, true); err != nil {
			t.Fatal(err)
		}
		if !f.app.liveWorker(busy.ID) {
			t.Fatalf("busy thread %s is not recorded live", busy.ID)
		}
	}
	busy, busyBy := measure("20 busy threads outside the subtree")
	if busy != quiet {
		t.Fatalf("the read costs %d statements with busy threads outside the subtree, %d without; want the same\n%v\n%v", busy, quiet, busyBy, quietBy)
	}

	f.settledChild(t, child.ID)
	deeper, deeperBy := measure("one more thread below")
	if deeper <= quiet {
		t.Fatalf("one more thread below costs %d statements, the read before it %d; want more\n%v\n%v", deeper, quiet, deeperBy, quietBy)
	}
	t.Logf("a held request's read of rest with two threads below: %d statements %v; a third below: %d", quiet, quietBy, deeper)
}
