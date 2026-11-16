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
// carrying that token was minted there. A sender's cancel disarms the wake in
// the statement that settles the request.
func TestSettleLocalThreadRequestWritesBothRowsOrOnlyTheReceipt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedLocalThreadRequest(t, s, "tok-local", "t-caller", "t-target", "")
	seedLocalThreadRequest(t, s, "tok-outbound", "t-caller", "t-target", "backend-2")

	receipt := ThreadRequestSettlement{State: ThreadReceiptCancelled, Answer: []byte("cancelled"), AnswerKind: ThreadAnswerError, DisarmWake: true}
	for token, want := range map[string]LocalSettlement{
		"tok-local":    {Settled: true, Collected: true},
		"tok-outbound": {Settled: true, Outbound: true},
	} {
		got, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), receipt, ThreadRequestCancelled)
		if err != nil || got.Settled != want.Settled || got.Collected != want.Collected || got.Outbound != want.Outbound || got.Notify {
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
	if again, err := s.SettleLocalThreadRequest("tok-local", ThreadReceiptOpenStates(), receipt, ThreadRequestCancelled); err != nil || again.Settled {
		t.Fatalf("second settle = %+v, %v; want a lost race", again, err)
	}

	// A request write that fails takes the receipt's settlement with it.
	seedLocalThreadRequest(t, s, "tok-both", "t-caller", "t-target", "")
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_request BEFORE UPDATE OF state ON thread_requests
		WHEN NEW.token = 'tok-both' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SettleLocalThreadRequest("tok-both", ThreadReceiptOpenStates(), receipt, ThreadRequestCancelled); err == nil {
		t.Fatal("a settlement whose request could not be written was accepted")
	}
	if stored, _, _ := s.GetThreadRequestReceipt("tok-both"); stored.State != ThreadReceiptRunning {
		t.Fatalf("receipt = %q after its request was refused, want it still running", stored.State)
	}
	// The wake is the request's: a receipt's own settlement refuses it, and a
	// settlement cannot both arm and disarm it.
	if _, _, err := settleThreadReceiptRow(s.db, "tok-both", ThreadReceiptOpenStates(),
		ThreadRequestSettlement{State: ThreadReceiptCancelled, AnswerKind: ThreadAnswerError, DisarmWake: true}); err == nil {
		t.Fatal("a receipt settlement disarming a wake was accepted")
	}
	if _, err := s.SettleLocalThreadRequest("tok-both", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptReplied, Answer: []byte("x"), AnswerKind: ThreadAnswerReply, ArmWake: true, DisarmWake: true,
	}, ThreadRequestReplied); err == nil {
		t.Fatal("a settlement that both arms and disarms the wake was accepted")
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
		if _, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
		}, ThreadRequestFinished); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for token, want := range map[string]ThreadRequestCollected{
		"tok-local":    {LateReply: true},
		"tok-outbound": {},
	} {
		stored, got, err := s.StoreLocalThreadRequestLateReply(token, []byte("and one more thing"), 0, &ThreadAnswerAgent{Role: "Reviewer"}, true)
		if err != nil || !stored || got != want {
			t.Fatalf("late reply %s = %v %+v, %v; want stored and %+v", token, stored, got, err, want)
		}
	}
	if row := mustGetThreadRequest(t, s, "tok-local"); string(row.LateReply) != "and one more thing" ||
		row.LateReplyAgent == nil || row.LateReplyAgent.Role != "Reviewer" || !row.Notify {
		t.Fatalf("local late reply = %q by %+v, notify %v; want the reply by its agent, armed", row.LateReply, row.LateReplyAgent, row.Notify)
	}
	// A retry of the same text is the same reply and stores nothing.
	if stored, got, err := s.StoreLocalThreadRequestLateReply("tok-local", []byte("and one more thing"), 0, nil, true); err != nil || stored || got.Any() {
		t.Fatalf("repeated late reply = %v %+v, %v; want nothing stored", stored, got, err)
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

// seedOrphanedReceipt is a local request left open beside a receipt that
// settled without it, with the wake disarmed so a collection's arming shows.
// A non-empty `late` is a late reply stored on the receipt alone.
func seedOrphanedReceipt(t *testing.T, s *Store, token string, settlement ThreadRequestSettlement, late string) ThreadRequestReceipt {
	t.Helper()
	seedLocalThreadRequest(t, s, token, "t-caller", "t-target", "")
	if _, err := s.SetThreadRequestNotify(token, false); err != nil {
		t.Fatal(err)
	}
	if _, settled, err := settleThreadReceiptRow(s.db, token, ThreadReceiptOpenStates(), settlement); err != nil || !settled {
		t.Fatalf("settle receipt %s: settled=%v err=%v", token, settled, err)
	}
	if late != "" {
		if stored, err := s.StoreThreadReceiptLateReply(token, []byte(late), 0, nil); err != nil || !stored {
			t.Fatalf("late reply on receipt %s: stored=%v err=%v", token, stored, err)
		}
	}
	return mustGetReceipt(t, s, token)
}

func mustGetReceipt(t *testing.T, s *Store, token string) ThreadRequestReceipt {
	t.Helper()
	receipt, found, err := s.GetThreadRequestReceipt(token)
	if err != nil || !found {
		t.Fatalf("receipt %s: found=%v err=%v", token, found, err)
	}
	return receipt
}

// A collection carries everything the receipt holds up to the revision it
// acknowledges: the answer, and the late reply with its time and its wake.
func TestCollectLocalThreadReceiptTakesTheWholeReceiptAtTheRevisionItAcknowledges(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	final := ThreadRequestSettlement{State: ThreadReceiptFinished, Answer: []byte("original final"), AnswerKind: ThreadAnswerFinal}
	reply := ThreadRequestSettlement{State: ThreadReceiptReplied, Answer: []byte("the reply"), AnswerKind: ThreadAnswerReply}

	cases := []struct {
		token     string
		receipt   ThreadRequestSettlement
		late      string
		arms      bool
		want      ThreadRequestCollected
		state     string
		notify    bool
		firstOwed bool
		lateOwed  bool
	}{
		{"tok-final-late", final, "actual requested reply", true, ThreadRequestCollected{Settled: true, LateReply: true}, ThreadRequestFinished, true, false, true},
		{"tok-final", final, "", true, ThreadRequestCollected{Settled: true}, ThreadRequestFinished, false, true, false},
		{"tok-reply", reply, "", true, ThreadRequestCollected{Settled: true}, ThreadRequestReplied, true, true, false},
		{"tok-reply-unarmed", reply, "", false, ThreadRequestCollected{Settled: true}, ThreadRequestReplied, false, true, false},
	}
	for _, c := range cases {
		receipt := seedOrphanedReceipt(t, s, c.token, c.receipt, c.late)
		got, err := s.CollectLocalThreadReceipt(c.token, ThreadRequestWake{ReplyArms: c.arms})
		if err != nil || got != c.want {
			t.Fatalf("%s: collect = %+v, %v; want %+v", c.token, got, err, c.want)
		}
		row := mustGetThreadRequest(t, s, c.token)
		if row.State != c.state || string(row.Answer) != string(receipt.Answer) || row.AnswerKind != receipt.AnswerKind ||
			row.SettledAt != receipt.SettledAt || row.ExpiresAt != receipt.ExpiresAt {
			t.Fatalf("%s: request = %+v, want the receipt's answer and moments", c.token, row)
		}
		if string(row.LateReply) != c.late || row.LateReplyAt != receipt.LateReplyAt {
			t.Fatalf("%s: request late reply = %q at %d, want %q at %d", c.token, row.LateReply, row.LateReplyAt, c.late, receipt.LateReplyAt)
		}
		if row.Revision != receipt.Revision || row.Notify != c.notify ||
			row.WakeOwed(false) != c.firstOwed || row.WakeOwed(true) != c.lateOwed {
			t.Fatalf("%s: request revision %d notify %v owed first %v late %v; want revision %d notify %v owed first %v late %v",
				c.token, row.Revision, row.Notify, row.WakeOwed(false), row.WakeOwed(true),
				receipt.Revision, c.notify, c.firstOwed, c.lateOwed)
		}
		if after := mustGetReceipt(t, s, c.token); after.CollectedRevision != receipt.Revision {
			t.Fatalf("%s: receipt collected %d of %d, want the revision the request holds", c.token, after.CollectedRevision, receipt.Revision)
		}

		// Collecting again is a no-op.
		again, err := s.CollectLocalThreadReceipt(c.token, ThreadRequestWake{ReplyArms: c.arms})
		if err != nil || again.Any() {
			t.Fatalf("%s: second collect = %+v, %v; want nothing written", c.token, again, err)
		}
		if repeat := mustGetThreadRequest(t, s, c.token); repeat.Revision != row.Revision || repeat.UpdatedAt != row.UpdatedAt {
			t.Fatalf("%s: second collect rewrote the request: %+v", c.token, repeat)
		}
	}
}

// An open receipt has nothing to collect, and nothing is acknowledged.
func TestCollectLocalThreadReceiptLeavesAnOpenReceiptAlone(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedLocalThreadRequest(t, s, "tok-open", "t-caller", "t-target", "")
	got, err := s.CollectLocalThreadReceipt("tok-open", ThreadRequestWake{ReplyArms: true})
	if err != nil || got.Any() {
		t.Fatalf("collect = %+v, %v; want nothing", got, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-open"); row.State != ThreadRequestRunning {
		t.Fatalf("request = %q, want it still running", row.State)
	}
}

// The copy and the acknowledgement are one transaction: a late reply the
// request cannot take leaves it open with the receipt unacknowledged, and the
// next collection takes all of it.
func TestCollectLocalThreadReceiptWritesNothingWhenAnyPartFails(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedOrphanedReceipt(t, s, "tok-torn", ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("original final"), AnswerKind: ThreadAnswerFinal,
	}, "actual requested reply")
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_late BEFORE UPDATE OF late_reply ON thread_requests
		WHEN NEW.token = 'tok-torn' BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollectLocalThreadReceipt("tok-torn", ThreadRequestWake{}); err == nil {
		t.Fatal("a collection whose late reply could not be written was accepted")
	}
	if row := mustGetThreadRequest(t, s, "tok-torn"); row.State != ThreadRequestRunning || len(row.Answer) != 0 {
		t.Fatalf("request = %q %q, want it untouched", row.State, row.Answer)
	}
	if receipt := mustGetReceipt(t, s, "tok-torn"); receipt.CollectedRevision != 0 {
		t.Fatalf("receipt collected %d, want nothing acknowledged", receipt.CollectedRevision)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_late`); err != nil {
		t.Fatal(err)
	}
	got, err := s.CollectLocalThreadReceipt("tok-torn", ThreadRequestWake{})
	if err != nil || got != (ThreadRequestCollected{Settled: true, LateReply: true}) {
		t.Fatalf("retried collect = %+v, %v; want all of it", got, err)
	}
}

// A late reply stored against a request still open beside its settled
// receipt collects the receipt's answer with it.
func TestALocalLateReplyCollectsTheAnswerItFollows(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedOrphanedReceipt(t, s, "tok-late", ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("original final"), AnswerKind: ThreadAnswerFinal,
	}, "")
	stored, got, err := s.StoreLocalThreadRequestLateReply("tok-late", []byte("actual requested reply"), 0, nil, true)
	if err != nil || !stored || got != (ThreadRequestCollected{Settled: true, LateReply: true}) {
		t.Fatalf("late reply = %v %+v, %v; want both parts collected", stored, got, err)
	}
	row := mustGetThreadRequest(t, s, "tok-late")
	if string(row.Answer) != "original final" || string(row.LateReply) != "actual requested reply" || !row.Notify {
		t.Fatalf("request = %q late %q notify %v, want the final, the reply and the wake armed", row.Answer, row.LateReply, row.Notify)
	}
	if receipt := mustGetReceipt(t, s, "tok-late"); receipt.CollectedRevision != receipt.Revision {
		t.Fatalf("receipt collected %d of %d", receipt.CollectedRevision, receipt.Revision)
	}
}

// An answer from another computer is collected whole too: a late reply the
// request cannot take leaves the settlement unwritten.
func TestCollectThreadRequestWritesTheSettlementAndLateReplyTogether(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remote", CallerThreadID: "t-caller", Kind: ThreadRequestSend, TargetComputerID: "backend-2",
		TargetThreadID: "t-there", State: ThreadRequestRunning,
	}); err != nil {
		t.Fatal(err)
	}
	answer := ThreadReceiptAnswer{
		Revision: 2, State: ThreadReceiptFinished, Answer: []byte("original final"), AnswerKind: ThreadAnswerFinal,
		SettledAt: 100, ExpiresAt: 200, Agent: &ThreadAnswerAgent{Role: "Reviewer"},
		LateReply: []byte("actual requested reply"), LateReplyAt: 150, LateReplyAgent: &ThreadAnswerAgent{},
	}
	if _, err := s.db.Exec(`CREATE TRIGGER refuse_late BEFORE UPDATE OF late_reply ON thread_requests
		BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CollectThreadRequest("tok-remote", answer, ThreadRequestWake{}); err == nil {
		t.Fatal("a collection whose late reply could not be written was accepted")
	}
	if row := mustGetThreadRequest(t, s, "tok-remote"); row.State != ThreadRequestRunning {
		t.Fatalf("request = %q, want it still running", row.State)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_late`); err != nil {
		t.Fatal(err)
	}
	got, err := s.CollectThreadRequest("tok-remote", answer, ThreadRequestWake{})
	if err != nil || got != (ThreadRequestCollected{Settled: true, LateReply: true}) {
		t.Fatalf("collect = %+v, %v; want both parts", got, err)
	}
	row := mustGetThreadRequest(t, s, "tok-remote")
	if row.State != ThreadRequestFinished || string(row.LateReply) != "actual requested reply" || row.LateReplyAt != 150 || row.Revision != 2 {
		t.Fatalf("request = %+v, want the final and the late reply at revision 2", row)
	}
	if row.AnswerAgent == nil || row.AnswerAgent.Role != "Reviewer" || row.LateReplyAgent == nil || row.LateReplyAgent.Role != "" {
		t.Fatalf("request agents = %+v and %+v, want each answer's own", row.AnswerAgent, row.LateReplyAgent)
	}
	if again, err := s.CollectThreadRequest("tok-remote", answer, ThreadRequestWake{}); err != nil || again.Any() {
		t.Fatalf("second collect = %+v, %v; want nothing written", again, err)
	}
}

// A source cannot acknowledge a revision the receipt does not hold yet.
func TestAckThreadRequestReceiptStopsAtTheReceiptsRevision(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedOrphanedReceipt(t, s, "tok-ack", ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
	}, "")
	if acked, err := s.AckThreadRequestReceipt("tok-ack", 5, 0); err != nil || !acked {
		t.Fatalf("ack = %v, %v", acked, err)
	}
	if receipt := mustGetReceipt(t, s, "tok-ack"); receipt.CollectedRevision != 1 {
		t.Fatalf("receipt collected %d, want its own revision 1", receipt.CollectedRevision)
	}
	if acked, err := s.AckThreadRequestReceipt("tok-ack", 5, 0); err != nil || acked {
		t.Fatalf("repeated ack = %v, %v; want nothing changed", acked, err)
	}
}

// A receipt the retention floor reduced to its lineage holds no answer, and
// a request beside it collects nothing from it.
func TestCollectLocalThreadReceiptSkipsALineageOnlyReceipt(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-target")
	seedOrphanedReceipt(t, s, "tok-lineage", ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
	}, "")
	if _, err := s.db.Exec(`UPDATE thread_request_receipts SET lineage_only = 1, answer = NULL WHERE token = 'tok-lineage'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CollectLocalThreadReceipt("tok-lineage", ThreadRequestWake{}); err != nil || got.Any() {
		t.Fatalf("collect = %+v, %v; want nothing", got, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-lineage"); row.State != ThreadRequestRunning {
		t.Fatalf("request = %q, want it still running", row.State)
	}
}
