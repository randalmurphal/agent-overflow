package app

import (
	"testing"

	"agent-overflow/internal/store"
)

func insertTurns(t *testing.T, app *App, threadID string, turns ...store.Turn) {
	t.Helper()
	for _, turn := range turns {
		turn.ThreadID = threadID
		if err := app.store.InsertTurn(turn); err != nil {
			t.Fatal(err)
		}
	}
}

// A second Stop before the first stopped turn settles keeps the first turn's
// mark: each turn's requests read the person's Stop.
func TestEveryStoppedTurnKeepsItsPersonStopMark(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stop-marks-two-turns", "claude", t.TempDir())
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10}, store.Turn{TurnID: "t1", TurnIndex: 1, StartedAt: 20})
	app.noteReceiptRunning(thread.ID, "tok-0")
	app.noteReceiptRunning(thread.ID, "tok-1")
	app.notePersonStop(thread.ID, 0)
	app.notePersonStop(thread.ID, 1)
	for index, startedAt := range map[int]int64{0: 10, 1: 20} {
		turn := store.Turn{ThreadID: thread.ID, TurnIndex: index, StartedAt: startedAt, StopReason: "interrupted"}
		settlement, err := app.turnSettlement(thread.ID, turn, false)
		if err != nil {
			t.Fatal(err)
		}
		if settlement.AnswerKind != store.ThreadAnswerFinal {
			t.Errorf("turn %d, stopped by the person, settles as %q/%q", index, settlement.State, settlement.AnswerKind)
		}
	}
}

// Two threads stopped at once keep their own marks.
func TestTwoThreadsKeepTheirOwnPersonStopMarks(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	a := createAppTestThread(t, app, "stop-marks-a", "claude", t.TempDir())
	b := siblingThread(t, app, a, "stop-marks-b")
	for _, id := range []string{a.ID, b.ID} {
		insertTurns(t, app, id, store.Turn{TurnID: id + "-t0", TurnIndex: 0, StartedAt: 10})
		app.noteReceiptRunning(id, "tok-"+id)
		app.notePersonStop(id, 0)
	}
	for _, id := range []string{a.ID, b.ID} {
		if !app.personStopped(id, store.Turn{ThreadID: id, TurnIndex: 0, StartedAt: 10}) {
			t.Fatalf("thread %s lost its mark", id)
		}
	}
}

// A turn's mark goes once its requests settle, and only that turn's: a later
// stopped turn still running keeps its own.
func TestASettledTurnTakesOnlyItsOwnPersonStopMark(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "stop-marks-settle", "claude", t.TempDir())
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10}, store.Turn{TurnID: "t1", TurnIndex: 1, StartedAt: 20})
	for index, token := range []string{"tok-0", "tok-1"} {
		if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "d", Kind: "send", TargetThreadID: thread.ID, State: store.ThreadReceiptAccepted,
		}); err != nil {
			t.Fatal(err)
		}
		if ok, err := app.store.MarkThreadReceiptRunning(token, "u:"+token, threadRequestTurnKey(thread.ID, index)); err != nil || !ok {
			t.Fatalf("mark %s running: %v %v", token, ok, err)
		}
		app.noteReceiptRunning(thread.ID, token)
	}
	app.notePersonStop(thread.ID, 0)
	app.notePersonStop(thread.ID, 1)

	if err := app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedTurn(thread.ID, false)

	if receipt, _, _ := app.store.GetThreadRequestReceipt("tok-0"); receipt.State != store.ThreadReceiptInterrupted || receipt.AnswerKind != store.ThreadAnswerFinal {
		t.Fatalf("turn 0's request settled %q/%q, want the person's Stop", receipt.State, receipt.AnswerKind)
	}
	if app.personStopped(thread.ID, store.Turn{TurnIndex: 0, StartedAt: 10}) {
		t.Error("turn 0's mark outlived its settled request")
	}
	if !app.personStopped(thread.ID, store.Turn{TurnIndex: 1, StartedAt: 20}) {
		t.Error("turn 0's settlement took turn 1's mark")
	}
}
