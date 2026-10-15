package app

import (
	"context"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// interruptedWake is the wake a sender reads for a receipt's settlement.
func interruptedWake(t *testing.T, app *App, threadID, token string) string {
	t.Helper()
	receipt, found, err := app.store.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("receipt %s: found %v, %v", token, found, err)
	}
	if receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt %s settled %q, want interrupted", token, receipt.State)
	}
	body, err := app.threadWakeBody(store.ThreadRequest{
		Token: token, TargetThreadID: threadID, State: store.ThreadRequestInterrupted,
		Answer: receipt.Answer, AnswerKind: receipt.AnswerKind,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func runningReceipt(t *testing.T, app *App, threadID, token string, turnIndex int) {
	t.Helper()
	if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "d", Kind: "send", TargetThreadID: threadID, State: store.ThreadReceiptAccepted,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := app.store.BindThreadReceiptTurn(token, "u:"+token, threadRequestTurnKey(threadID, turnIndex), true); err != nil || !ok {
		t.Fatalf("mark %s running: %v %v", token, ok, err)
	}
	app.noteReceiptRunning(threadID, token)
}

// siblingThread is another thread in thread's project.
func siblingThread(t *testing.T, app *App, thread store.Thread, id string) store.Thread {
	t.Helper()
	sibling := thread
	sibling.ID = id
	if err := app.store.CreateThread(sibling); err != nil {
		t.Fatal(err)
	}
	return sibling
}

// Every interruption tells the sender why, from the reason its settlement
// recorded; only a restart says restart.
func TestEachInterruptionTellsTheSenderWhy(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "interrupt-reasons", "claude", t.TempDir())
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10}, store.Turn{TurnID: "t1", TurnIndex: 1, StartedAt: 20})
	runningReceipt(t, app, thread.ID, "tok-session", 0)
	runningReceipt(t, app, thread.ID, "tok-takeover", 1)
	if err := app.store.UpdateTurnCompleted("t1", 25, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedTurn(thread.ID)
	if err := app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedSession(thread.ID)

	moved := siblingThread(t, app, thread, "interrupt-reasons-moved")
	runningReceipt(t, app, moved.ID, "tok-moved", 0)
	if err := app.stopThreadRequestsForMovedThread(context.Background(), moved.ID, "Desk"); err != nil {
		t.Fatal(err)
	}

	restarted := siblingThread(t, app, thread, "interrupt-reasons-restart")
	runningReceipt(t, app, restarted.ID, "tok-restart", 0)
	app.sweepThreadRequestsAtBoot()

	for _, tc := range []struct {
		thread, token, want string
	}{
		{thread.ID, "tok-session", `[token tok-session]: The thread's session ended during the turn that read the request.`},
		{thread.ID, "tok-takeover", `[token tok-takeover]: The turn was interrupted before it answered.`},
		{moved.ID, "tok-moved", `[token tok-moved]: The thread was moved to Desk before it answered.`},
		{restarted.ID, "tok-restart", `[token tok-restart] was interrupted by a restart of this computer.`},
	} {
		body := interruptedWake(t, app, tc.thread, tc.token)
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s reaches the sender as %q, want %q", tc.token, body, tc.want)
		}
		if tc.token != "tok-restart" && strings.Contains(body, "restart") {
			t.Errorf("%s reaches the sender as a restart: %q", tc.token, body)
		}
	}
}

// Triage ends a turn its session took down with a synthetic truncated
// completion, and only that turn end reads as the session ending.
func TestOnlyASyntheticTurnEndIsTheSessionEnding(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		complete provider.TurnCompleteMeta
		want     bool
	}{
		"session ended": {&provider.TruncatedTurnCompleteMeta{Synthetic: true}, true},
		"send failed":   {&provider.TruncatedTurnCompleteMeta{}, false},
		"no completion": {nil, false},
	} {
		evt := provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: tc.complete}
		if got := turnEndedWithItsSession(evt); got != tc.want {
			t.Errorf("%s: turnEndedWithItsSession = %v, want %v", name, got, tc.want)
		}
	}
}
