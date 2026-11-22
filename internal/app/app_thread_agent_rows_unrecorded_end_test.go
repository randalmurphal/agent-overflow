package app

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A worker whose turn end the store refused has ended: its row rests with
// the turn's time up to its end, not a turn that runs on, and thread_status
// reads it as error at rest. Once the store takes writes, the worker's next
// turn writes that end first, with its own outcome, and starts cleanly: the
// row runs for the new turn alone and rests on its end with both turns'
// time.
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
	endedAt, unrecorded := f.app.triage.UnrecordedTurnEnd(first.TurnID)
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
	settled, found, err := f.app.store.GetTurn(first.TurnID)
	if err != nil || !found || settled.CompletedAt == nil || *settled.CompletedAt != endedAt || settled.StopReason != "end_turn" {
		t.Fatalf("first turn once the next started %+v, %v, %v; want settled end_turn at %d", settled, found, err, endedAt)
	}
	second, active, err := f.app.store.GetActiveTurn(worker.ID)
	if err != nil || !active || second.TurnID == first.TurnID {
		t.Fatalf("second turn %+v, %v, %v; want a new open turn", second, active, err)
	}
	again := f.awaitWorker(t, f.caller.ID, "unrecorded-spawn", workerRunning)
	if !again.TurnOpen || again.TurnStartedAt != second.StartedAt || again.RuntimeMs < ran {
		t.Fatalf("row in the second turn %+v; want the new turn running from %d over the first's %dms", again, second.StartedAt, ran)
	}
	workerEvent(t, f.app, worker.ID, provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}}, nil)
	f.expectRested(t, "unrecorded-spawn")
	done, found, err := f.app.store.GetTurn(second.TurnID)
	if err != nil || !found || done.CompletedAt == nil {
		t.Fatalf("second turn %+v, %v, %v; want settled", done, found, err)
	}
	if row := past(); row.TurnOpen || row.RuntimeMs != ran+(*done.CompletedAt-done.StartedAt) {
		t.Fatalf("row once both turns ended %+v; want %dms", row, ran+(*done.CompletedAt-done.StartedAt))
	}
}
