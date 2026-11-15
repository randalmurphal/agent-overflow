package store

import "testing"

// seedLocalThreadRequest is a request with its receipt on this store, both
// open. `computer` names the request's destination; empty is this computer.
func seedLocalThreadRequest(t *testing.T, s *Store, token, caller, target, computer string) {
	t.Helper()
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: ThreadRequestSend, TargetComputerID: computer,
		TargetThreadID: target, Notify: true, State: ThreadRequestRunning,
	}); err != nil {
		t.Fatalf("insert request %s: %v", token, err)
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "local", SourceThreadID: caller,
		Kind: ThreadRequestSend, TargetThreadID: target, State: ThreadReceiptRunning,
	}); err != nil {
		t.Fatalf("accept %s: %v", token, err)
	}
}

// A local settlement writes the receipt, the request and the collection
// together, and touches no request that names another computer: a receipt
// carrying that token was minted there.
func TestSettleLocalThreadRequestWritesBothRowsOrOnlyTheReceipt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedLocalThreadRequest(t, s, "tok-local", "t-caller", "t-target", "")
	seedLocalThreadRequest(t, s, "tok-outbound", "t-caller", "t-target", "backend-2")

	receipt := ThreadRequestSettlement{State: ThreadReceiptCancelled, Answer: []byte("cancelled"), AnswerKind: ThreadAnswerError}
	request := ThreadRequestSettlement{State: ThreadRequestCancelled, DisarmWake: true}
	for token, want := range map[string]LocalThreadRequestSettlement{
		"tok-local":    {Receipt: true, Request: true},
		"tok-outbound": {Receipt: true},
	} {
		got, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), receipt, request)
		if err != nil || got != want {
			t.Fatalf("settle %s = %+v, %v; want %+v", token, got, err, want)
		}
	}
	local := mustGetThreadRequest(t, s, "tok-local")
	stored, _, err := s.GetThreadRequestReceipt("tok-local")
	if err != nil {
		t.Fatal(err)
	}
	if local.State != ThreadRequestCancelled || string(local.Answer) != "cancelled" || local.Notify ||
		local.SettledAt != stored.SettledAt || local.ExpiresAt != stored.ExpiresAt {
		t.Fatalf("local request = %+v, want the receipt's answer and moments with its wake disarmed", local)
	}
	if stored.CollectedRevision != stored.Revision {
		t.Fatalf("receipt collected %d of %d, want it collected", stored.CollectedRevision, stored.Revision)
	}
	if outbound := mustGetThreadRequest(t, s, "tok-outbound"); outbound.State != ThreadRequestRunning || !outbound.Notify {
		t.Fatalf("outbound request = %q notify %v, want it untouched", outbound.State, outbound.Notify)
	}
	if again, err := s.SettleLocalThreadRequest("tok-local", ThreadReceiptOpenStates(), receipt, request); err != nil || again.Receipt {
		t.Fatalf("second settle = %+v, %v; want a lost race", again, err)
	}

	seedLocalThreadRequest(t, s, "tok-both", "t-caller", "t-target", "")
	if _, err := s.SettleLocalThreadRequest("tok-both", ThreadReceiptOpenStates(), receipt,
		ThreadRequestSettlement{State: ThreadRequestCancelled, ArmWake: true, DisarmWake: true}); err == nil {
		t.Fatal("a settlement that both arms and disarms the wake was accepted")
	}
	if stored, _, _ := s.GetThreadRequestReceipt("tok-both"); stored.State != ThreadReceiptRunning {
		t.Fatalf("receipt = %q after its request was refused, want it still running", stored.State)
	}
	if _, err := s.SettleThreadRequestReceipt("tok-both", ThreadReceiptOpenStates(),
		ThreadRequestSettlement{State: ThreadReceiptCancelled, DisarmWake: true}); err == nil {
		t.Fatal("a receipt settlement disarming a wake was accepted")
	}
}

// A late reply is stored on both rows of a local request together, and on
// the receipt alone for a request that names another computer.
func TestStoreLocalThreadRequestLateReplyWritesBothRowsOrOnlyTheReceipt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	for token, computer := range map[string]string{"tok-local": "", "tok-outbound": "backend-2"} {
		seedLocalThreadRequest(t, s, token, "t-caller", "t-target", computer)
		if _, err := s.SettleThreadRequestReceipt(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for token, want := range map[string]LocalThreadRequestSettlement{
		"tok-local":    {Receipt: true, Request: true},
		"tok-outbound": {Receipt: true},
	} {
		got, err := s.StoreLocalThreadRequestLateReply(token, []byte("and one more thing"), 0, true)
		if err != nil || got != want {
			t.Fatalf("late reply %s = %+v, %v; want %+v", token, got, err, want)
		}
	}
	if row := mustGetThreadRequest(t, s, "tok-local"); string(row.LateReply) != "and one more thing" {
		t.Fatalf("local late reply = %q", row.LateReply)
	}
	if receipt, _, _ := s.GetThreadRequestReceipt("tok-local"); receipt.CollectedRevision != receipt.Revision {
		t.Fatalf("receipt collected %d of %d, want the late reply collected", receipt.CollectedRevision, receipt.Revision)
	}
	if row := mustGetThreadRequest(t, s, "tok-outbound"); len(row.LateReply) != 0 {
		t.Fatalf("outbound late reply = %q, want none", row.LateReply)
	}
}

// The boot reads the open requests this computer answers itself: not one
// sent to another computer, not a reminder, not one already settled.
func TestListOpenLocalThreadRequestsIsWhatOnlyAReceiptHereCanSettle(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-open", "t-caller", ThreadRequestSend, 100)
	seedSettledThreadRequest(t, s, "tok-settled", "t-caller", ThreadRequestSend, 200, "done")
	for _, row := range []ThreadRequest{
		{Token: "tok-remote", CallerThreadID: "t-caller", Kind: ThreadRequestSend, TargetComputerID: "backend-2", State: ThreadRequestRunning},
		{Token: "tok-remind", CallerThreadID: "t-caller", Kind: ThreadRequestRemind, DueAt: 500, State: ThreadRequestAccepted},
	} {
		if err := s.InsertThreadRequest(row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListOpenLocalThreadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Token != "tok-open" {
		t.Fatalf("open local requests = %+v, want tok-open alone", rows)
	}
}
