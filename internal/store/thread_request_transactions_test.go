package store

import (
	"testing"
)

// commitsOf runs fn and returns how many writer transactions it committed.
func commitsOf(t *testing.T, s *Store, fn func()) uint64 {
	t.Helper()
	before := s.CommitCount()
	fn()
	return s.CommitCount() - before
}

// A local send is written whole in one transaction: the source row already
// accepted and naming its target, and its receipt. Dispatching it and
// binding its turn each move the receipt and the source row together.
func TestALocalSendIsAcceptedInOneTransaction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"caller", "target"} {
		mustCreateThread(t, s, id)
	}
	var changes []ThreadRequestChange
	s.OnThreadRequestChanged(func(c ThreadRequestChange) { changes = append(changes, c) })
	request := ThreadRequest{Token: "tok", CallerThreadID: "caller", Kind: ThreadRequestSend, TargetThreadID: "target", Notify: true, State: ThreadRequestAccepted}
	receipt := ThreadRequestReceipt{Token: "tok", OwnerDeviceID: "local", SourceThreadID: "caller", Kind: ThreadRequestSend, TargetThreadID: "target"}
	if n := commitsOf(t, s, func() {
		got, err := s.AcceptLocalThreadRequest(request, receipt)
		if err != nil || got.State != ThreadReceiptAccepted || got.TargetThreadID != "target" {
			t.Fatalf("accept = %+v, %v", got, err)
		}
	}); n != 1 {
		t.Fatalf("accepting a local send took %d transactions, want 1", n)
	}
	row, found, err := s.GetThreadRequest("tok")
	if err != nil || !found || row.State != ThreadRequestAccepted || row.TargetThreadID != "target" || !row.Notify {
		t.Fatalf("source row = %+v, %v %v", row, found, err)
	}
	if len(changes) != 1 || changes[0].CallerThreadID != "caller" {
		t.Fatalf("changes = %+v, want the insert reported with its caller", changes)
	}
	if _, err := s.AcceptLocalThreadRequest(request, receipt); err == nil {
		t.Fatal("a second accept of the same token was not refused")
	}

	if n := commitsOf(t, s, func() {
		if marked, err := s.MarkThreadReceiptDispatched("tok", "item-1"); err != nil || !marked {
			t.Fatalf("mark = %v, %v", marked, err)
		}
	}); n != 1 {
		t.Fatalf("the dispatch mark took %d transactions, want 1", n)
	}
	if row, _, _ := s.GetThreadRequest("tok"); row.State != ThreadRequestRunning {
		t.Fatalf("source after the dispatch = %q, want running", row.State)
	}
	if bound, err := s.BindThreadReceiptTurn("tok", "item-1", "target#0", true); err != nil || !bound {
		t.Fatalf("bind = %v, %v", bound, err)
	}
	if got, _, _ := s.GetThreadRequestReceipt("tok"); got.TurnID != "target#0" || !got.OwnTurn {
		t.Fatalf("receipt after the bind = %+v", got)
	}
}

// A delivery can bind the receipt before the dispatch mark lands: the bind
// moves the source row, and the mark that no longer applies still leaves it
// running.
func TestABindBeforeTheMarkRunsTheSource(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"caller", "target"} {
		mustCreateThread(t, s, id)
	}
	if _, err := s.AcceptLocalThreadRequest(
		ThreadRequest{Token: "tok", CallerThreadID: "caller", Kind: ThreadRequestSend, TargetThreadID: "target", State: ThreadRequestAccepted},
		ThreadRequestReceipt{Token: "tok", OwnerDeviceID: "local", Kind: ThreadRequestSend, TargetThreadID: "target"}); err != nil {
		t.Fatal(err)
	}
	if bound, err := s.BindThreadReceiptTurn("tok", "item-1", "target#1", true); err != nil || !bound {
		t.Fatalf("bind = %v, %v", bound, err)
	}
	if row, _, _ := s.GetThreadRequest("tok"); row.State != ThreadRequestRunning {
		t.Fatalf("source after the bind = %q, want running", row.State)
	}
	if marked, err := s.MarkThreadReceiptDispatched("tok", "item-1"); err != nil || marked {
		t.Fatalf("a mark after the bind = %v, %v; want it not to apply", marked, err)
	}
}

// A local settlement writes the receipt, the source row and the
// acknowledgement in one transaction; the receipt keeps its answer.
func TestALocalSettlementIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"caller", "target"} {
		mustCreateThread(t, s, id)
	}
	if _, err := s.AcceptLocalThreadRequest(
		ThreadRequest{Token: "tok", CallerThreadID: "caller", Kind: ThreadRequestSend, TargetThreadID: "target", Notify: true, State: ThreadRequestAccepted},
		ThreadRequestReceipt{Token: "tok", OwnerDeviceID: "local", Kind: ThreadRequestSend, TargetThreadID: "target"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindThreadReceiptTurn("tok", "item", "target#0", true); err != nil {
		t.Fatal(err)
	}
	settlement := ThreadRequestSettlement{State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 50, Agent: &ThreadAnswerAgent{Role: "Reviewer"}}
	var result LocalSettlement
	if n := commitsOf(t, s, func() {
		var err error
		result, err = s.SettleLocalThreadRequest("tok", []string{ThreadReceiptRunning}, settlement, ThreadRequestFinished)
		if err != nil {
			t.Fatal(err)
		}
	}); n != 1 {
		t.Fatalf("a local settlement took %d transactions, want 1", n)
	}
	if !result.Settled || !result.Collected || !result.Notify || result.Outbound {
		t.Fatalf("result = %+v", result)
	}
	row, _, _ := s.GetThreadRequest("tok")
	if row.State != ThreadRequestFinished || string(row.Answer) != "done" || row.SettledAt != 50 ||
		row.ExpiresAt != 50+threadAnswerHoldMillis || row.AnswerAgent == nil || row.AnswerAgent.Role != "Reviewer" || row.Revision != 1 {
		t.Fatalf("source = %+v", row)
	}
	receipt, _, _ := s.GetThreadRequestReceipt("tok")
	if receipt.State != ThreadReceiptFinished || string(receipt.Answer) != "done" || receipt.CollectedRevision != receipt.Revision || receipt.Revision != 1 {
		t.Fatalf("receipt = %+v, want it settled, acknowledged and keeping its answer", receipt)
	}
	again, err := s.SettleLocalThreadRequest("tok", []string{ThreadReceiptRunning}, settlement, ThreadRequestFinished)
	if err != nil || again.Settled {
		t.Fatalf("a second settlement = %+v, %v; want a lost race", again, err)
	}
}

// A receipt another computer's request holds has no source row here: the
// receipt settles alone and waits for that computer's poller. A token that
// names this computer's own outbound request is not collected into it.
func TestALocalSettlementLeavesOtherComputersRows(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	for _, id := range []string{"caller", "target"} {
		mustCreateThread(t, s, id)
	}
	for _, token := range []string{"remote", "outbound"} {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{Token: token, OwnerDeviceID: "device", Kind: ThreadRequestSend, TargetThreadID: "target"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.InsertThreadRequest(ThreadRequest{Token: "outbound", CallerThreadID: "caller", Kind: ThreadRequestSend,
		TargetComputerID: "pc", TargetThreadID: "far", State: ThreadRequestRunning}); err != nil {
		t.Fatal(err)
	}
	settlement := ThreadRequestSettlement{State: ThreadReceiptErrored, Answer: []byte("x"), AnswerKind: ThreadAnswerError}
	for token, outbound := range map[string]bool{"remote": false, "outbound": true} {
		result, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), settlement, ThreadRequestErrored)
		if err != nil || !result.Settled || result.Collected || result.Outbound != outbound {
			t.Fatalf("%s: %+v, %v", token, result, err)
		}
		if receipt, _, _ := s.GetThreadRequestReceipt(token); receipt.CollectedRevision != 0 {
			t.Fatalf("%s: acknowledged %+v", token, receipt)
		}
	}
	if row, _, _ := s.GetThreadRequest("outbound"); row.State != ThreadRequestRunning {
		t.Fatalf("the outbound request took another computer's answer: %+v", row)
	}
}
