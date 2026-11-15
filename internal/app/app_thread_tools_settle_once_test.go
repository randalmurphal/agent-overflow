package app

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// A request between two threads of this computer has two rows, the caller's
// request and the target's receipt, and they settle together or not at all.
// A settlement that wrote one and not the other left the request open with
// nothing left to settle it: every wait timed out, a cancel found nothing to
// stop, and the conversation could never be copied.

// refuseRequestWrite makes every update of one request's column fail until
// the returned func drops the trigger.
func (f *requestFixture) refuseRequestWrite(t *testing.T, token, column string) (allow func()) {
	t.Helper()
	f.execWaiting(t, `CREATE TRIGGER refuse_request_write BEFORE UPDATE OF `+column+` ON thread_requests
		WHEN NEW.token = '`+token+`' BEGIN SELECT RAISE(ABORT, 'injected request write failure'); END`)
	return func() { f.execWaiting(t, `DROP TRIGGER refuse_request_write`) }
}

// execWaiting runs statement on the fixture's database through its own
// connection, waiting out a write the app has in flight.
func (f *requestFixture) execWaiting(t *testing.T, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+f.dbPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestALocalSettlementThatCannotWriteTheRequestWritesNeitherRow(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "keep going")
	allow := f.refuseRequestWrite(t, ack.Token, "state")

	err := f.app.settleThreadReceipt(ack.Token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptErrored, Answer: []byte("the turn failed"), AnswerKind: store.ThreadAnswerError,
	})
	if err == nil || !strings.Contains(err.Error(), "injected request write failure") {
		t.Fatalf("settle = %v, want the request's write failure returned", err)
	}
	if receipt := f.receipt(t, ack.Token); receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("receipt = %q after the request could not be written, want it still running", receipt.State)
	}
	allow()

	// The restart finds the receipt still open, which is what lets its sweep
	// settle both rows.
	after := f.restart(t)
	after.app.sweepThreadRequestsAtBoot()
	row := after.request(t, ack.Token)
	if row.State != store.ThreadRequestInterrupted || string(row.Answer) != threadReceiptRestarted {
		t.Fatalf("request = %q %q, want it interrupted by the restart", row.State, row.Answer)
	}
	receipt := after.receipt(t, ack.Token)
	if receipt.State != store.ThreadReceiptInterrupted || receipt.CollectedRevision != receipt.Revision {
		t.Fatalf("receipt = %q collected %d of %d, want it interrupted and collected", receipt.State, receipt.CollectedRevision, receipt.Revision)
	}
}

// A late reply is the same two rows: one that reached the receipt and not
// the request was lost, because a retry of the same text is the same reply
// and stores nothing.
func TestALateReplyThatCannotWriteTheRequestIsNotStoredAndCanBeRetried(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	release := f.mockClaudeAfterTheAck(t, "looking into it")
	f.holdCallerTurn(t)
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{
		Prompt: "what did the profiler say?", WaitSeconds: 0, Notify: true,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	release(t, ack)
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	responder := f.targetIdentity(t, ack.ThreadID)
	reply := threadtools.ReplyCall{Token: ack.Token, Text: "the profiler blames the JSON decode"}
	allow := f.refuseRequestWrite(t, ack.Token, "late_reply")

	if _, err := f.adapter().Reply(t.Context(), responder, reply); err == nil || !strings.Contains(err.Error(), "injected request write failure") {
		t.Fatalf("late Reply = %v, want the request's write failure returned", err)
	}
	if receipt := f.receipt(t, ack.Token); len(receipt.LateReply) != 0 {
		t.Fatalf("receipt late reply = %q after the request could not be written, want none", receipt.LateReply)
	}
	allow()

	late, err := f.adapter().Reply(t.Context(), responder, reply)
	if err != nil || !late.Accepted || !late.Late {
		t.Fatalf("retried late Reply = %+v, %v; want it accepted", late, err)
	}
	if row := f.request(t, ack.Token); string(row.LateReply) != reply.Text {
		t.Fatalf("request late reply = %q, want the retried reply", row.LateReply)
	}
}

// A cancel turns "tell me when it's done" off only once it is accepted: one
// that fails leaves the work running, and the wake armed for its answer.
func TestALocalCancelThatFailsLeavesTheWakeArmed(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	ack := f.runningRequest(t, "keep going")
	f.execWaiting(t, `CREATE TRIGGER refuse_receipt_cancel BEFORE UPDATE OF state ON thread_request_receipts
		WHEN NEW.state = 'cancelled' BEGIN SELECT RAISE(ABORT, 'injected cancel failure'); END`)

	if _, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token}); err == nil ||
		!strings.Contains(err.Error(), "injected cancel failure") {
		t.Fatalf("Cancel = %v, want the settlement's failure", err)
	}
	if row := f.request(t, ack.Token); !row.Notify || row.State != store.ThreadRequestRunning {
		t.Fatalf("request after a failed cancel = notify %v, state %q; want it armed and running", row.Notify, row.State)
	}
	f.execWaiting(t, `DROP TRIGGER refuse_receipt_cancel`)

	report, err := f.adapter().Cancel(t.Context(), f.callerIdentity(), threadtools.CancelCall{Token: ack.Token})
	if err != nil || report.State != threadtools.RequestCancelled {
		t.Fatalf("Cancel = %+v, %v; want it cancelled", report, err)
	}
	if row := f.request(t, ack.Token); row.Notify {
		t.Fatalf("request after the cancel = notify %v, want it disarmed", row.Notify)
	}
	f.nothingOwed(t)
}

// A request this computer made of its own threads that is still open when it
// boots can never settle by itself: its receipt was settled by the sweep, or
// settled by an older build that never wrote the request, or never written.
func TestBootSettlesALocalRequestNothingElseCanSettle(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.busyThread(t, "")

	// Accepted by an older build that settled the receipt and lost the
	// request's write.
	answered := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: answered, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		TargetThreadID: target.ID, State: store.ThreadRequestRunning,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: answered, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.caller.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: target.ID, State: store.ThreadReceiptRunning,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	if settled, err := f.app.store.SettleThreadRequestReceipt(answered, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptFinished, Answer: []byte("the backfill touched 40 rows"), AnswerKind: store.ThreadAnswerFinal,
	}); err != nil || !settled {
		t.Fatalf("SettleThreadRequestReceipt: settled=%v err=%v", settled, err)
	}
	// Written, and the process died before its receipt was.
	unaccepted := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: unaccepted, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		State: store.ThreadRequestUnconfirmed,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	// A reminder has no receipt, and the clock settles it.
	reminder := newThreadRequestToken()
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: reminder, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestRemind,
		DueAt: time.Now().Add(time.Hour).UnixMilli(), State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}

	after := f.restart(t)
	after.app.sweepThreadRequestsAtBoot()

	if row := after.request(t, answered); row.State != store.ThreadRequestFinished || string(row.Answer) != "the backfill touched 40 rows" {
		t.Errorf("answered request = %q %q, want the receipt's answer", row.State, row.Answer)
	}
	if receipt := after.receipt(t, answered); receipt.CollectedRevision != receipt.Revision {
		t.Errorf("receipt collected %d of %d, want its answer collected", receipt.CollectedRevision, receipt.Revision)
	}
	if row := after.request(t, unaccepted); row.State != store.ThreadRequestInterrupted || string(row.Answer) != threadReceiptRestarted {
		t.Errorf("unaccepted request = %q %q, want it interrupted by the restart", row.State, row.Answer)
	}
	if row := after.request(t, reminder); row.State != store.ThreadRequestAccepted {
		t.Errorf("reminder = %q, want it left for its clock", row.State)
	}
	if open, err := after.app.store.HasOpenThreadRequests(f.caller.ID); err != nil {
		t.Fatalf("HasOpenThreadRequests: %v", err)
	} else if !open {
		t.Error("the reminder no longer counts as open")
	}
}
