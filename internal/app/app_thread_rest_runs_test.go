package app

// A thread's rest waits only on what its own runs started
// (docs/specs/standing-agents.md, "At rest"): a worker its spawner's run
// left at rest, which the person or another thread continues, holds that
// spawner from rest no longer, and nothing above it either.

import (
	"path/filepath"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// The person, or another thread's send, continues a worker its spawner's
// run left at rest, and the spawner itself was spawned. The spawner's turn
// ends while the worker runs: the spawner reads at rest, as thread_status
// reads it, its spawn's request settles finished and that spawn's run
// ends, all while the worker still runs.
func TestAWorkersLaterRunHoldsNoSpawnerAboveIt(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"the person", "another thread's send"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f := newRequestFixture(t)
			f.watchAgentRows()
			f.holdCallerSession()
			gate := filepath.Join(t.TempDir(), "middle-turn")
			installMockClaudeTurns(t, f.app, [][]string{gatedTurn(gate, "Handed it on.")})
			middle := f.spawnWorker(t, "run the board")
			f.awaitTurnRunning(t, middle.Token)

			worker := f.plainThread(t)
			f.acceptAgentRequest(t, "middle-worker", middle.ThreadID, store.ThreadRequestSpawn, worker.ID)
			workerTurn(t, f.app, worker.ID, "t0", 1, 10)
			f.settleAgentRequest(t, "middle-worker")
			// Its answer is taken, so nothing is on its way to the middle thread.
			if _, err := f.app.store.SetThreadRequestNotify("middle-worker", false); err != nil {
				t.Fatal(err)
			}
			f.awaitRunEnded(t, "middle-worker")

			if by == "another thread's send" {
				other := f.plainThread(t)
				f.acceptAgentRequest(t, "other-send", other.ID, store.ThreadRequestSend, worker.ID)
			}
			workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
			workerToolCall(t, f.app, worker.ID, "t1-a", "make test")
			// What the worker starts in that run, an open request, is that
			// run's too.
			f.acceptAgentRequest(t, "later-send", worker.ID, store.ThreadRequestSend, f.plainThread(t).ID)
			waitUntil(t, 10*time.Second, func() bool { return f.app.liveWorker(worker.ID) })

			release(t, gate)
			f.awaitRequestState(t, middle.Token, store.ThreadRequestFinished)
			f.awaitRunEnded(t, middle.Token)
			rest, err := f.app.threadRest(middle.ThreadID)
			if err != nil || rest.unrest != "" {
				t.Fatalf("the middle thread reads %+v, %v; want it at rest while %s runs its worker", rest, err, by)
			}
			if unrest, err := f.app.workerUnrest(worker.ID); err != nil || unrest.unrest != workerRunning {
				t.Fatalf("the worker reads %+v, %v; the test needs it running throughout", unrest, err)
			}
		})
	}
}

// A worker its spawner's run is still open on holds that spawner as before:
// background work it left running keeps the spawner from rest and is what
// thread_status says the spawner waits on.
func TestAWorkerInItsSpawnersRunStillHoldsIt(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	middle, worker := f.plainThread(t), f.plainThread(t)
	f.acceptAgentRequest(t, "hold-middle", f.caller.ID, store.ThreadRequestSpawn, middle.ID)
	f.acceptAgentRequest(t, "hold-worker", middle.ID, store.ThreadRequestSpawn, worker.ID)
	launchWorkerShell(t, f.app, worker.ID, "bg-shell")
	f.settleAgentRequest(t, "hold-worker")
	if _, err := f.app.store.SetThreadRequestNotify("hold-worker", false); err != nil {
		t.Fatal(err)
	}
	f.app.noteRestWaitsMayChange()
	f.app.threadRequestsWG.Wait()
	if row := f.request(t, "hold-worker"); !row.RunOpen() {
		t.Fatalf("spawn %+v; want its run open while its worker's background work runs", row)
	}
	rest, err := f.app.threadRest(middle.ID)
	if err != nil || rest.unrest != workerWaiting || rest.waitingLine() != "threads it started" {
		t.Fatalf("the middle thread reads %+v, %v; want it waiting on the thread it started", rest, err)
	}
	read, err := f.app.readRest([]string{middle.ID})
	defer read.done()
	if err != nil || read.rests(middle.ID) {
		t.Fatalf("the decision reads the middle thread at rest (%v); want it held by its worker's run", err)
	}
}
