package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// interruptedWake is the wake a sender reads for a receipt's settlement,
// and the facts its row shows.
func interruptedWake(t *testing.T, app *App, threadID, token string) (string, threadtools.MessageFacts) {
	t.Helper()
	receipt, found, err := app.store.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("receipt %s: found %v, %v", token, found, err)
	}
	if receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt %s settled %q, want interrupted", token, receipt.State)
	}
	message, err := soloThreadWakeMessage(store.ThreadRequest{
		Token: token, TargetThreadID: threadID, State: store.ThreadRequestInterrupted,
		Answer: receipt.Answer, AnswerKind: receipt.AnswerKind, Cause: receipt.Cause, CauseArg: receipt.CauseArg,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return app.threadWakeFor(message).Message()
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
	app.noteReceiptRunning(threadID, token, testSessionToken(app, threadID))
}

// testSessionToken is the session a test's own gate note or session end
// names: the thread's live session, or a stand-in for a thread with none.
func testSessionToken(app *App, threadID string) string {
	if sess, ok := app.sessionManager().get(threadID); ok && sess.Token != "" {
		return sess.Token
	}
	return "test-session"
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

// Every interruption tells the sender why, from the cause its settlement
// recorded as data: each producer here records its code, the wake says the
// sentence it said before the cause was data, and the row names the same
// stopper. Only a restart says restart.
func TestEachInterruptionTellsTheSenderWhy(t *testing.T) {
	t.Parallel()
	app := newTestApp(t)
	thread := createAppTestThread(t, app, "interrupt-reasons", "claude", t.TempDir())
	stops := map[int]turnStop{
		2: stopByWorkflow(),
		3: stopByThread("Lead", "lead-id", "Studio"),
		4: stopFromAgentsTray(),
		5: stopForTakeover(),
		6: assistantsOffStop().turn,
	}
	insertTurns(t, app, thread.ID, store.Turn{TurnID: "t0", TurnIndex: 0, StartedAt: 10}, store.Turn{TurnID: "t1", TurnIndex: 1, StartedAt: 20})
	runningReceipt(t, app, thread.ID, "tok-session", 0)
	runningReceipt(t, app, thread.ID, "tok-takeover", 1)
	if err := app.store.UpdateTurnCompleted("t1", 25, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedTurn(thread.ID)
	for index := 2; index <= 6; index++ {
		turnID := fmt.Sprintf("t%d", index)
		insertTurns(t, app, thread.ID, store.Turn{TurnID: turnID, TurnIndex: index, StartedAt: int64(10 * (index + 1))})
		runningReceipt(t, app, thread.ID, fmt.Sprintf("tok-stop-%d", index), index)
		app.recordTurnStop(thread.ID, stoppedTurn{index: index}, stops[index])
		if err := app.store.UpdateTurnCompleted(turnID, int64(10*(index+1)+5), "interrupted", "", "", ""); err != nil {
			t.Fatal(err)
		}
		app.settleReceiptsForEndedTurn(thread.ID)
	}
	if err := app.store.UpdateTurnCompleted("t0", 15, "interrupted", "", "", ""); err != nil {
		t.Fatal(err)
	}
	app.settleReceiptsForEndedSession(thread.ID, testSessionToken(app, thread.ID))

	moved := siblingThread(t, app, thread, "interrupt-reasons-moved")
	runningReceipt(t, app, moved.ID, "tok-moved", 0)
	if undelivered, err := app.stopThreadRequestsForMovedThread(context.Background(), moved.ID, "Desk"); undelivered != nil || err != nil {
		t.Fatal(undelivered, err)
	}

	handedOff := siblingThread(t, app, thread, "interrupt-reasons-handed-off")
	runningReceipt(t, app, handedOff.ID, "tok-handed-off", 0)
	if settled := app.settleReadRequestsForHandOff(handedOff.ID); len(settled) != 1 {
		t.Fatalf("the handoff settled %v", settled)
	}

	unread := siblingThread(t, app, thread, "interrupt-reasons-unread")
	for _, token := range []string{"tok-unread", "tok-not-taken-in", "tok-to-composer"} {
		if _, _, err := app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "d", Kind: "send", TargetThreadID: unread.ID, State: store.ThreadReceiptAccepted,
		}); err != nil {
			t.Fatal(err)
		}
		if marked, err := app.store.MarkThreadReceiptDispatched(token, "u:"+token); err != nil || !marked {
			t.Fatalf("dispatch %s: %v %v", token, marked, err)
		}
	}
	for token, why := range map[string]string{
		"tok-unread": threadtools.CauseSessionEndedUnread, "tok-not-taken-in": threadtools.CauseNotTakenIn,
		"tok-to-composer": threadtools.CauseNotTakenInToComposer,
	} {
		app.runThreadSettlementWork(app.settleReceiptNotRead(unread.ID, token, store.ThreadReceiptRunning, why))
	}

	restarted := siblingThread(t, app, thread, "interrupt-reasons-restart")
	runningReceipt(t, app, restarted.ID, "tok-restart", 0)
	app.sweepThreadRequestsAtBoot()

	for _, tc := range []struct {
		thread, token string
		cause         threadtools.Cause
		want          string
		stoppedBy     string
	}{
		{thread.ID, "tok-session", threadtools.Cause{Code: threadtools.CauseSessionEndedMidTurn},
			`[token tok-session]: The thread's session ended during the turn that read the request.`, "the session ended during the turn that read the request"},
		{thread.ID, "tok-takeover", threadtools.Cause{Code: threadtools.CauseTurn},
			`[token tok-takeover]: The turn was interrupted before it answered.`, "an interrupt"},
		{thread.ID, "tok-stop-2", threadtools.Cause{Code: threadtools.CauseWorkflow},
			`[token tok-stop-2]: Its workflow stopped the turn before it answered.`, ""},
		{thread.ID, "tok-stop-3", threadtools.Cause{Code: threadtools.CauseThreadCancel, Arg: `"Lead" (lead-id, on Studio)`},
			`[token tok-stop-3]: "Lead" (lead-id, on Studio) stopped its turn with thread_cancel before it answered.`, ""},
		{thread.ID, "tok-stop-4", threadtools.Cause{Code: threadtools.CauseTrayStop},
			`[token tok-stop-4]: The user stopped its turn from the Agents tray before it answered.`, ""},
		{thread.ID, "tok-stop-5", threadtools.Cause{Code: threadtools.CauseTakeover},
			`[token tok-stop-5]: The user took the thread over from its workflow before it answered.`, ""},
		{thread.ID, "tok-stop-6", threadtools.Cause{Code: threadtools.CauseAssistantsOff},
			`[token tok-stop-6]: Assistants were turned off in Settings before it answered.`, ""},
		{moved.ID, "tok-moved", threadtools.Cause{Code: threadtools.CauseMoved, Arg: "Desk"},
			`[token tok-moved]: The thread was moved to Desk before it answered.`, "a move to Desk"},
		{handedOff.ID, "tok-handed-off", threadtools.Cause{Code: threadtools.CauseHandedOff, Arg: handedOff.ID},
			`[token tok-handed-off]: The assistant moved to a new thread before it answered. Its previous thread is ` + handedOff.ID +
				`; read it with thread_show to see what was done, or ask the assistant again by name.`, "the assistant moving to a new thread"},
		{unread.ID, "tok-unread", threadtools.Cause{Code: threadtools.CauseSessionEndedUnread},
			`[token tok-unread]: The thread's session ended before it read the request.`, "the session ended before it read the request"},
		{unread.ID, "tok-not-taken-in", threadtools.Cause{Code: threadtools.CauseNotTakenIn},
			`[token tok-not-taken-in]: The thread did not take the request in.`, "the thread did not take it in"},
		{unread.ID, "tok-to-composer", threadtools.Cause{Code: threadtools.CauseNotTakenInToComposer},
			`[token tok-to-composer]: The thread did not take the request in, and its message went back to that thread's composer.`,
			"the thread did not take it in, and its message went back to that thread's composer"},
		{restarted.ID, "tok-restart", threadtools.Cause{Code: threadtools.CauseRestart},
			`[token tok-restart] was interrupted by a restart of this computer.`, "a restart of this computer"},
	} {
		receipt, _, err := app.store.GetThreadRequestReceipt(tc.token)
		if err != nil || (threadtools.Cause{Code: receipt.Cause, Arg: receipt.CauseArg}) != tc.cause || len(receipt.Answer) != 0 {
			t.Errorf("%s recorded cause %q arg %q answer %q, want %+v and no text", tc.token, receipt.Cause, receipt.CauseArg, receipt.Answer, tc.cause)
			continue
		}
		body, facts := interruptedWake(t, app, tc.thread, tc.token)
		if !strings.HasSuffix(body, tc.want) {
			t.Errorf("%s reaches the sender as %q, want %q", tc.token, body, tc.want)
		}
		if facts.StoppedBy != tc.stoppedBy {
			t.Errorf("%s names %q as its stopper, want %q", tc.token, facts.StoppedBy, tc.stoppedBy)
		}
		if tc.token != "tok-restart" && strings.Contains(body, "restart") {
			t.Errorf("%s reaches the sender as a restart: %q", tc.token, body)
		}
	}
}
