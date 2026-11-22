package app

import (
	"testing"

	"agent-overflow/internal/eventchan"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A worker whose turn end the store refused has ended: its row rests with
// the turn's time up to its end, not a turn that runs on, and thread_status
// reads it as error at rest. The worker's next turn starts cleanly: the row
// runs for the new turn alone, not for the open row before it, and rests
// on the new turn's end with that turn's time; the refused turn's row stays
// open for the next boot's sweep, so its time is in no tally.
func TestAWorkerWhoseTurnEndIsUnrecordedRests(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "unrecorded-spawn", f.caller.ID)
	past := func() ThreadAgentRow {
		t.Helper()
		page, err := f.app.ListThreadPastAgentRows(t.Context(), f.caller.ID, store.PastAgentRowsCursor{}, 8)
		if err != nil {
			t.Fatal(err)
		}
		row, _ := agentRowByToken(page.Rows, "unrecorded-spawn")
		return row
	}

	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	f.awaitWorker(t, f.caller.ID, "unrecorded-spawn", workerRunning)
	first, active, err := f.app.store.GetActiveTurn(worker.ID)
	if err != nil || !active {
		t.Fatalf("first turn %+v, %v, %v", first, active, err)
	}
	execOnStore(t, f.dbPath, refuseTurnEnds)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	endedAt, unrecorded := f.app.triage.UnrecordedTurnEnd(worker.ID, first.TurnID)
	if !unrecorded {
		t.Fatal("the refused end is not held unrecorded")
	}
	f.expectRested(t, "unrecorded-spawn")
	ran := endedAt - first.StartedAt
	if row := past(); row.TurnOpen || row.TurnStartedAt != 0 || row.RuntimeMs != ran {
		t.Fatalf("row of the worker whose end is unrecorded %+v; want at rest with its turn's %dms", row, ran)
	}
	status, err := f.app.threadTools().threadStateNow(t.Context(), worker.ID)
	if err != nil || status.State != threadtools.StateError || !status.Resting {
		t.Fatalf("thread_status of the worker = %+v, %v; want error at rest", status, err)
	}

	execOnStore(t, f.dbPath, allowTurnEnds)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t2"}, nil)
	if still, found, err := f.app.store.GetTurn(first.TurnID); err != nil || !found || still.CompletedAt != nil {
		t.Fatalf("first turn once the next started %+v, %v, %v; want it left open", still, found, err)
	}
	second, active, err := f.app.store.GetActiveTurn(worker.ID)
	if err != nil || !active || second.TurnID == first.TurnID {
		t.Fatalf("second turn %+v, %v, %v; want a new open turn", second, active, err)
	}
	again := f.awaitWorker(t, f.caller.ID, "unrecorded-spawn", workerRunning)
	if !again.TurnOpen || again.TurnStartedAt != second.StartedAt {
		t.Fatalf("row in the second turn %+v; want the new turn running from %d", again, second.StartedAt)
	}
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.expectRested(t, "unrecorded-spawn")
	done, found, err := f.app.store.GetTurn(second.TurnID)
	if err != nil || !found || done.CompletedAt == nil {
		t.Fatalf("second turn %+v, %v, %v; want settled", done, found, err)
	}
	if row := past(); row.TurnOpen || row.RuntimeMs != *done.CompletedAt-done.StartedAt {
		t.Fatalf("row once the second turn ended %+v; want its %dms", row, *done.CompletedAt-done.StartedAt)
	}
	status, err = f.app.threadTools().threadStateNow(t.Context(), worker.ID)
	if err != nil || status.State != threadtools.StateIdle || !status.Resting {
		t.Fatalf("thread_status once the second turn ended = %+v, %v; want idle at rest", status, err)
	}
}

// A turn end triage stops holding can change with no turn frame (a retired
// session's turn start writes its row and opens no round): the change
// itself reassesses the worker, so the spawner's row of it is framed again,
// and a client re-reading it finds the open turn running.
func TestAWorkerIsReframedWhenItsUnrecordedEndIsForgotten(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "forgotten-spawn", f.caller.ID)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	f.awaitWorker(t, f.caller.ID, "forgotten-spawn", workerRunning)
	execOnStore(t, f.dbPath, refuseTurnEnds)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.expectRested(t, "forgotten-spawn")
	waitAgentEmitsIdle(t, f.app)

	framed := make(chan struct{}, 64)
	emit := f.app.testEmitHook
	f.app.testEmitHook = func(name string, data any) {
		emit(name, data)
		if change, ok := data.(ThreadAgentRowChange); ok && name == string(eventchan.ThreadAgentRowsChanged) && change.ID == "forgotten-spawn" {
			framed <- struct{}{}
		}
	}
	f.app.triage.ForgetUnrecordedTurnEnds(worker.ID)
	waitAgentEmitsIdle(t, f.app)
	if len(framed) == 0 {
		t.Fatal("no frame of the worker's row once its turn end was forgotten")
	}
	page, err := f.app.ListThreadPastAgentRows(t.Context(), f.caller.ID, store.PastAgentRowsCursor{}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if row, _ := agentRowByToken(page.Rows, "forgotten-spawn"); !row.TurnOpen {
		t.Fatalf("row once the turn end was forgotten %+v; want its open turn running", row)
	}
}

// A worker's completion ends its round before the frame that reassesses
// it goes out: a reassessment that runs while the frame is held, the
// liveness drain the frame's tap started, records the worker at rest, not
// running, and nothing later has to correct it.
func TestAWorkerReassessedOnItsCompletionReadsTheRoundEnded(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.watchAgentRows()
	worker := f.settledSpawn(t, "completing-spawn", f.caller.ID)
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnStart, TurnID: worker.ID + ":t1"}, nil)
	f.awaitWorker(t, f.caller.ID, "completing-spawn", workerRunning)

	held := false
	f.bus.mu.Lock()
	f.bus.tap = func(name eventchan.Channel, data any) {
		f.app.tapThreadAgentRows(name, data)
		if event, ok := data.(triage.TurnCompletedEvent); ok && !held && event.ThreadID == worker.ID {
			held = true
			// The frame's tap has queued the worker's reassessment; it
			// runs to its end before the frame is let go.
			waitAgentEmitsIdle(t, f.app)
		}
	}
	f.bus.mu.Unlock()
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	if !held {
		t.Fatal("the completion's frame was never held")
	}
	waitAgentEmitsIdle(t, f.app)
	f.app.liveWorkers.mu.Lock()
	recorded, live := f.app.liveWorkers.threads[worker.ID]
	f.app.liveWorkers.mu.Unlock()
	if live {
		t.Fatalf("worker recorded %+v after its completion; want it at rest", recorded)
	}
}
