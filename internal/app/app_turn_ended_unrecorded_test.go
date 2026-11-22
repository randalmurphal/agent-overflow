package app

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// Triggers on the turns table, each one refused write.
const (
	refuseEveryTurnEnd = `CREATE TRIGGER refuse_every_turn_end BEFORE UPDATE OF completed_at ON turns
		WHEN NEW.completed_at IS NOT NULL BEGIN SELECT RAISE(ABORT, 'injected settle failure'); END`
	allowEveryTurnEnd   = `DROP TRIGGER refuse_every_turn_end`
	refuseSecondTurnRow = `CREATE TRIGGER refuse_second_turn_row BEFORE INSERT ON turns
		WHEN NEW.turn_index = 1 BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`
)

// wantRestingNotRunning checks that thread_status and the rest a spawn's
// wait settles on both read threadID at rest, not running.
func wantRestingNotRunning(t *testing.T, a *App, threadID, when string) threadtools.ThreadState {
	t.Helper()
	status, err := a.threadTools().threadStateNow(t.Context(), threadID)
	if err != nil {
		t.Fatalf("threadStateNow %s: %v", when, err)
	}
	if status.State == threadtools.StateRunning || !status.Resting {
		t.Errorf("thread_status %s = %q, resting %v; want at rest, not running", when, status.State, status.Resting)
	}
	rest, err := a.threadRest(threadID)
	if err != nil || rest.unrest != "" {
		t.Errorf("rest %s = %+v, %v; want at rest", when, rest, err)
	}
	return status
}

// A turn whose end the store refused stays open under a later turn whose
// own row the store refused: the later turn runs on no row, and the thread
// reads running while it does. Once it ends nothing runs, and the thread
// rests, not running for good on the older open row.
func TestATurnThatRanWithoutARowLeavesTheThreadAtRest(t *testing.T) {
	q := newQueuedRequests(t)
	execOnStore(t, q.f.dbPath, refuseEveryTurnEnd)
	q.handle(t, "turn 0 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	execOnStore(t, q.f.dbPath, allowEveryTurnEnd)
	execOnStore(t, q.f.dbPath, refuseSecondTurnRow)
	q.handle(t, "turn 1 start", provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 1})
	if _, found, err := q.f.app.store.GetTurnByThreadIndex(q.target.ID, 1); err != nil || found {
		t.Fatalf("turn 1's row: found %v, %v; want the store to have refused it", found, err)
	}
	status, err := q.f.app.threadTools().threadStateNow(t.Context(), q.target.ID)
	if err != nil || status.State != threadtools.StateRunning || status.Resting {
		t.Fatalf("thread_status while turn 1 runs = %+v, %v; want running", status, err)
	}
	q.handle(t, "turn 1 complete", provider.ProviderEvent{
		Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"},
	})
	wantRestingNotRunning(t, q.f.app, q.target.ID, "once turn 1 ended")
}

// A turn start the session's replacement refuses after the turn's row was
// written opens no turn: nothing runs the row. A reassessment of the
// worker while the start was under way read it busy (queued: a start in
// progress reads as a pending send); the refusal
// reassesses it again, so its spawner's row rests, and thread_status and
// the rest read it at rest.
func TestARefusedTurnStartLeavesItsWorkerAtRest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.app.configureTriageQueueCallbacks()
	f.watchAgentRows()
	worker := f.settledSpawn(t, "refused-start-spawn", f.caller.ID)
	f.app.triage.MarkThreadActive(worker.ID, "session-1")
	var once sync.Once
	f.app.triage.SetTurnRowWrittenGate(func(threadID string) {
		if threadID != worker.ID {
			return
		}
		once.Do(func() {
			f.app.noteWorkerLiveness(worker.ID)
			f.awaitWorker(t, f.caller.ID, "refused-start-spawn", workerQueued)
			f.app.triage.MarkThreadActive(worker.ID, "session-2")
		})
	})
	if err := f.app.triage.Handle(provider.ProviderEvent{
		Kind: provider.EventTurnStart, ThreadID: worker.ID, SessionToken: "session-1", Timestamp: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, active, err := f.app.store.GetActiveTurn(worker.ID); err != nil || !active {
		t.Fatalf("the refused start's row: active %v, %v; want it written and open", active, err)
	}
	f.expectRested(t, "refused-start-spawn")
	wantRestingNotRunning(t, f.app, worker.ID, "after the refused start")
}

// A run that ends at its worker's rest keeps the worker's figures as the
// rest read them: a turn whose end the store refused counts to its end as
// the boot sweep will record it, the time the worker's own row shows, not
// to the moment the run ended.
func TestARunEndedOnAnUnrecordedTurnKeepsTheRowsRuntime(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.plainThread(t)
	f.acceptAgentRequest(t, "kept-runtime-spawn", f.caller.ID, store.ThreadRequestSpawn, worker.ID)
	started := time.Now().Add(-10 * time.Second)
	handle := func(evt provider.ProviderEvent, at time.Time) {
		t.Helper()
		evt.ThreadID, evt.Timestamp = worker.ID, at
		if err := f.app.triage.Handle(evt); err != nil {
			t.Fatalf("%s: %v", evt.Kind, err)
		}
	}
	handle(provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, started)
	f.awaitWorker(t, f.caller.ID, "kept-runtime-spawn", workerRunning)
	execOnStore(t, f.dbPath, refuseEveryTurnEnd)
	handle(provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}},
		started.Add(5*time.Second))
	f.settleAgentRequest(t, "kept-runtime-spawn")
	ended := f.awaitRunEnded(t, "kept-runtime-spawn")

	now := time.Now()
	tallies, err := f.app.store.ThreadTallies([]string{worker.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	live := f.app.liveTally(worker.ID, tallies[worker.ID], now)
	base := int64(0)
	if ended.RunBase != nil {
		base = ended.RunBase.TurnMs
	}
	if live.Running || ended.RunTotal.TurnMs != live.RuntimeMs-base {
		t.Fatalf("run kept %dms; the worker's tally reads %dms from the run's start (running %v); want the two equal",
			ended.RunTotal.TurnMs, live.RuntimeMs-base, live.Running)
	}
}
