package app

// A decision that cannot be taken back reads rest from the threads' own
// sources, the threads below included, never from what the Agents rows'
// drain recorded: a held request settles `finished` only when no thread a
// spawn of its thread made has work, recorded or not, and the retention
// sweep clears no spawn whose thread works. A source that cannot be read
// holds either for the next pass.

import (
	"fmt"
	"path/filepath"
	"sync"
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
	row := store.ThreadRequest{Token: token, CallerThreadID: worker,
		Kind: store.ThreadRequestSpawn, TargetThreadID: child.ID, State: store.ThreadRequestRunning, CreatedAt: now, UpdatedAt: now}
	seedSpawnRun(f.app, &row)
	if err := f.app.store.InsertThreadRequest(row); err != nil {
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

// A spawn past the retention floor whose worker has a loop armed, which
// nothing reported to the Agents rows, keeps its answer and token on both
// sides: the sweep reads the worker's sources before it clears anything,
// and clears nothing while it cannot read them. Once the loop is cancelled
// a later sweep keeps only its lineage.
func TestTheRetentionSweepLeavesAWorkingSpawnWhole(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	f.holdCallerSession()
	installMockClaudeTurns(t, f.app, [][]string{reportTurn("Rebuilt the index.")})
	ack := f.spawnWorker(t, "rebuild the index")
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	waitUntil(t, 10*time.Second, func() bool {
		f.app.threadRequestsWG.Wait()
		return f.app.liveThreadRest(ack.ThreadID).unrest == ""
	})
	wake, err := f.app.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-worker", ThreadID: ack.ThreadID, Kind: store.AgentWakeLoop, Label: "index",
		IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.app.liveWorker(ack.ThreadID) {
		t.Fatalf("the record knows the loop (%+v); the test needs it not to", f.app.liveThreadRest(ack.ThreadID))
	}
	later := time.Now().AddDate(0, 0, store.ThreadRequestRetentionDays+1)
	whole := func(what string) {
		t.Helper()
		if row, found, err := f.app.store.GetThreadRequest(ack.Token); err != nil || !found || string(row.Answer) != "Rebuilt the index." {
			t.Fatalf("the spawn after %s: %+v, found %v, %v; want it whole", what, row, found, err)
		}
		if receipt, found, err := f.app.store.GetThreadRequestReceipt(ack.Token); err != nil || !found || string(receipt.Answer) != "Rebuilt the index." {
			t.Fatalf("the spawn's receipt after %s: %+v, found %v, %v; want it whole", what, receipt, found, err)
		}
	}
	// A sweep that cannot read the worker's sources clears nothing.
	restore := f.failRestSource(t)
	f.app.expireThreadRequests(later)
	whole("a sweep that could not read its worker")
	restore()
	f.app.expireThreadRequests(later)
	whole("the sweep")

	if _, found, err := f.app.store.CancelAgentWake(ack.ThreadID, wake.Label, store.AgentWakeLoop, agentWakeOutcomeCancelled); err != nil || !found {
		t.Fatalf("CancelAgentWake: %v %v", found, err)
	}
	passesUntil(t, func() { f.app.expireThreadRequests(later) }, func() error {
		if _, found, err := f.app.store.GetThreadRequest(ack.Token); err != nil || found {
			return fmt.Errorf("the spawn's token once its worker rests: found %v, %v; want unknown", found, err)
		}
		if _, found, err := f.app.store.GetThreadRequestReceipt(ack.Token); err != nil || found {
			return fmt.Errorf("the receipt once its worker rests: found %v, %v; want unknown", found, err)
		}
		return nil
	})
	if link, found, err := f.app.store.SpawnLinkOf(ack.ThreadID); err != nil || !found || link != ack.Token {
		t.Fatalf("the spawn's lineage %q, %v, %v; want it kept as the worker's link", link, found, err)
	}
}

// The record of a thread held with no spawn link, as one recorded before
// the spawn that names it did, is read again with its link when its
// durable sources change: its loop records it as a worker.
func TestTheRecordFindsAChildRecordedBeforeItsSpawnNamedIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	worker, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatal(err)
	}
	child := f.settledChild(t, worker.ID)
	if _, err := f.app.store.RegisterAgentWake(store.AgentWake{
		ID: "wake-child", ThreadID: child.ID, Kind: store.AgentWakeLoop, Label: "board",
		IntervalSeconds: 60, NextAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	f.app.threadRequestsWG.Wait()
	f.app.liveWorkers.mu.Lock()
	if f.app.liveWorkers.threads == nil {
		f.app.liveWorkers.threads = map[string]liveWorker{}
	}
	f.app.liveWorkers.threads[child.ID] = liveWorker{workerRest: workerRest{unrest: workerBlocked}}
	f.app.liveWorkers.mu.Unlock()
	if worker, err := f.app.reassessWorker(child.ID, true); err != nil || !worker {
		t.Fatalf("reassess the child: worker %v, %v; want its link read", worker, err)
	}
	if rest := f.app.liveWorkerUnrest(store.ThreadRequest{Token: "spawn-of-" + child.ID, TargetThreadID: child.ID}); rest.unrest != workerWaiting {
		t.Fatalf("the child's row reads %+v; want waiting on its loop", rest)
	}
}
