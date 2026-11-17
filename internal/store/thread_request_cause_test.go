package store

import "testing"

// An interruption's cause is written with its settlement on the receipt and
// carried to the request beside it, by a local settlement, a collection of
// the receipt and a collection of another computer's answer, with what the
// turn wrote as the answer.
func TestAnInterruptionsCauseTravelsWithItsAnswer(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	interrupted := ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, Answer: []byte("Half done."), AnswerKind: ThreadAnswerError,
		Cause: "session_ended_mid_turn",
	}
	want := func(name string, cause, arg, answer, gotCause, gotArg string, gotAnswer []byte) {
		t.Helper()
		if gotCause != cause || gotArg != arg || string(gotAnswer) != answer {
			t.Errorf("%s = cause %q arg %q answer %q, want %q %q %q", name, gotCause, gotArg, gotAnswer, cause, arg, answer)
		}
	}

	seedLocalThreadRequest(t, s, "tok-local", "t-caller", "t-target", "")
	if got, err := s.SettleLocalThreadRequest("tok-local", ThreadReceiptOpenStates(), interrupted, ThreadRequestInterrupted); err != nil || !got.Collected {
		t.Fatalf("settle = %+v, %v", got, err)
	}
	receipt := mustGetReceipt(t, s, "tok-local")
	want("receipt", "session_ended_mid_turn", "", "Half done.", receipt.Cause, receipt.CauseArg, receipt.Answer)
	request := mustGetThreadRequest(t, s, "tok-local")
	want("request", "session_ended_mid_turn", "", "Half done.", request.Cause, request.CauseArg, request.Answer)
	if snapshot := receipt.Snapshot(); snapshot.Cause != receipt.Cause || snapshot.CauseArg != receipt.CauseArg {
		t.Errorf("snapshot = %+v, want the receipt's cause", snapshot)
	}

	// A receipt whose request collects it later.
	seedOrphanedReceipt(t, s, "tok-later", ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, Cause: "moved", CauseArg: "Desk",
	}, "")
	if got, err := s.CollectLocalThreadReceipt("tok-later", ThreadRequestWake{}); err != nil || !got.Settled {
		t.Fatalf("collect = %+v, %v", got, err)
	}
	later := mustGetThreadRequest(t, s, "tok-later")
	want("collected request", "moved", "Desk", "", later.Cause, later.CauseArg, later.Answer)

	// Another computer's answer.
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remote", CallerThreadID: "t-caller", Kind: ThreadRequestSend, TargetComputerID: "backend-2",
		TargetThreadID: "t-there", State: ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CollectThreadRequest("tok-remote", ThreadReceiptAnswer{
		Revision: 1, State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, Cause: "handed_off", CauseArg: "t-old", SettledAt: 5,
	}, ThreadRequestWake{}); err != nil || !got.Settled {
		t.Fatalf("collect remote = %+v, %v", got, err)
	}
	remote := mustGetThreadRequest(t, s, "tok-remote")
	want("remote request", "handed_off", "t-old", "", remote.Cause, remote.CauseArg, remote.Answer)

	// The Agents segment reads it with its answer head.
	if _, err := s.db.Exec(`UPDATE thread_requests SET notify = 1`); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListThreadPastAgentRequests("t-caller", PastAgentRowsCursor{SettledAt: 1 << 62}, 10, 64, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	causes := map[string]string{}
	for _, row := range page.Requests {
		causes[row.Token] = row.Cause + "|" + row.CauseArg
	}
	if causes["tok-local"] != "session_ended_mid_turn|" || causes["tok-later"] != "moved|Desk" || causes["tok-remote"] != "handed_off|t-old" {
		t.Errorf("past agent rows read causes %v", causes)
	}
}

// Only an interruption has a cause, and an argument needs one.
func TestASettlementRefusesACauseItCannotHave(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedLocalThreadRequest(t, s, "tok", "t-caller", "t-target", "")
	for name, settlement := range map[string]ThreadRequestSettlement{
		"a cause on a finished answer": {State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, Cause: "restart"},
		"an argument with no cause":    {State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, CauseArg: "Desk"},
	} {
		if _, err := s.SettleLocalThreadRequest("tok", ThreadReceiptOpenStates(), settlement, ThreadRequestStateForReceipt(settlement.State)); err == nil {
			t.Errorf("%s was accepted", name)
		}
		if _, err := s.SettleThreadRequest("tok", ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestStateForReceipt(settlement.State), Answer: settlement.Answer, AnswerKind: settlement.AnswerKind,
			Cause: settlement.Cause, CauseArg: settlement.CauseArg,
		}); err == nil {
			t.Errorf("%s was accepted on the request", name)
		}
	}
	if receipt := mustGetReceipt(t, s, "tok"); receipt.State != ThreadReceiptRunning {
		t.Fatalf("receipt = %q, want it still running", receipt.State)
	}
}

// An answer the hold dropped, and a row the retention floor keeps as
// lineage, keep no cause with no answer.
func TestADroppedAnswerTakesItsCauseWithIt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedOrphanedReceipt(t, s, "tok-expired", ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, Cause: "moved", CauseArg: "Desk", SettledAt: 10, ExpiresAt: 20,
	}, "")
	if _, err := s.ExpireThreadRequestAnswers(30); err != nil {
		t.Fatal(err)
	}
	if receipt := mustGetReceipt(t, s, "tok-expired"); receipt.State != ThreadReceiptExpired || receipt.Cause != "" || receipt.CauseArg != "" {
		t.Fatalf("expired receipt = %q cause %q arg %q, want no cause", receipt.State, receipt.Cause, receipt.CauseArg)
	}

	seedLocalThreadRequest(t, s, "tok-old", "t-caller", "t-target", "")
	if _, err := s.SettleLocalThreadRequest("tok-old", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerError, Cause: "thread_cancel", CauseArg: `"Lead" (t-lead)`, SettledAt: 10,
	}, ThreadRequestInterrupted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(clearLineagePayloadSQL + ` AND token = 'tok-old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(clearReceiptLineageSQL + ` token = 'tok-old'`); err != nil {
		t.Fatal(err)
	}
	var requestCause, requestArg, receiptCause, receiptArg string
	if err := s.db.QueryRow(`SELECT answer_cause, answer_cause_arg FROM thread_requests WHERE token = 'tok-old'`).Scan(&requestCause, &requestArg); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT answer_cause, answer_cause_arg FROM thread_request_receipts WHERE token = 'tok-old'`).Scan(&receiptCause, &receiptArg); err != nil {
		t.Fatal(err)
	}
	if requestCause+requestArg+receiptCause+receiptArg != "" {
		t.Fatalf("lineage keeps a cause: request %q %q, receipt %q %q", requestCause, requestArg, receiptCause, receiptArg)
	}
}
