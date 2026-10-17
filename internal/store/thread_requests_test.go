package store

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func seedThreadRequest(t *testing.T, s *Store, token, caller, kind string, createdAt int64) {
	t.Helper()
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: token, CallerThreadID: caller, Kind: kind,
		State: ThreadRequestUnconfirmed, CreatedAt: createdAt, UpdatedAt: createdAt,
	}); err != nil {
		t.Fatalf("insert request %s: %v", token, err)
	}
}

// rawThreadRequest reads a row whatever it is, lineage-only included,
// which the ledger reads skip.
func rawThreadRequest(t *testing.T, s *Store, token string) ThreadRequest {
	t.Helper()
	row, err := scanThreadRequest(s.db.QueryRow(`SELECT `+threadRequestColumns+` FROM thread_requests WHERE token = ?`, token))
	if err != nil {
		t.Fatalf("read thread request %s: %v", token, err)
	}
	return row
}

// seedSettledThreadRequest is a request that has already settled with an
// answer. Only a settled request can be delivered, so every delivery test
// starts from one.
func seedSettledThreadRequest(t *testing.T, s *Store, token, caller, kind string, createdAt int64, answer string) {
	t.Helper()
	seedThreadRequest(t, s, token, caller, kind, createdAt)
	settled, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte(answer), AnswerKind: ThreadAnswerFinal, SettledAt: createdAt + 1,
	})
	if err != nil || !settled {
		t.Fatalf("settle request %s: settled=%v err=%v", token, settled, err)
	}
}

func mustGetThreadRequest(t *testing.T, s *Store, token string) ThreadRequest {
	t.Helper()
	row, found, err := s.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("get request %s: found=%v err=%v", token, found, err)
	}
	return row
}

// The settlement records the answering thread as a standing agent, an agent
// with no role line apart from an ordinary thread; a late reply replaces it
// with the agent it was written under; a settlement with no agent records
// none. A receipt records it the same way, and a new row is never read as
// written before v150.
func TestThreadRequestRecordsTheAnsweringAgent(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	settle := func(token string, agent *ThreadAnswerAgent) ThreadRequest {
		t.Helper()
		seedThreadRequest(t, s, token, "t-caller", ThreadRequestSend, 100)
		settled, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 200, Agent: agent,
		})
		if err != nil || !settled {
			t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
		}
		return mustGetThreadRequest(t, s, token)
	}
	seedThreadRequest(t, s, "tok-open", "t-caller", ThreadRequestSend, 100)
	if row := mustGetThreadRequest(t, s, "tok-open"); row.AnswerAgentUnrecorded || row.AnswerAgent != nil {
		t.Errorf("open row = unrecorded %v agent %+v, want no agent recorded", row.AnswerAgentUnrecorded, row.AnswerAgent)
	}
	if row := settle("tok-agent", &ThreadAnswerAgent{Role: "Reviewer"}); row.AnswerAgent == nil || row.AnswerAgent.Role != "Reviewer" {
		t.Errorf("agent with a role = %+v", row.AnswerAgent)
	}
	if row := settle("tok-roleless", &ThreadAnswerAgent{}); row.AnswerAgent == nil || row.AnswerAgent.Role != "" {
		t.Errorf("agent with no role line = %+v, want an agent with an empty role", row.AnswerAgent)
	}
	if row := settle("tok-ordinary", nil); row.AnswerAgent != nil {
		t.Errorf("ordinary thread = %+v, want no agent", row.AnswerAgent)
	}

	if stored, err := s.StoreThreadRequestLateReply("tok-agent", []byte("more"), 300, &ThreadAnswerAgent{Role: "Second role"}); err != nil || !stored {
		t.Fatalf("late reply: stored=%v err=%v", stored, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-agent"); row.AnswerAgent == nil || row.AnswerAgent.Role != "Second role" {
		t.Errorf("agent after the late reply = %+v", row.AnswerAgent)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-roleless", []byte("more"), 300, nil); err != nil || !stored {
		t.Fatalf("late reply: stored=%v err=%v", stored, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-roleless"); row.AnswerAgent != nil {
		t.Errorf("agent after a late reply from an ordinary thread = %+v", row.AnswerAgent)
	}

	for _, token := range []string{"tok-agent", "tok-roleless", "tok-ordinary"} {
		if row := mustGetThreadRequest(t, s, token); row.AnswerAgentUnrecorded {
			t.Errorf("%s reads as written before v150", token)
		}
	}

	receipt := func(token string) ThreadRequestReceipt {
		t.Helper()
		row, found, err := s.GetThreadRequestReceipt(token)
		if err != nil || !found {
			t.Fatalf("receipt %s: found=%v err=%v", token, found, err)
		}
		return row
	}
	for token, agent := range map[string]*ThreadAnswerAgent{"rcpt-agent": {Role: "Reviewer"}, "rcpt-roleless": {}, "rcpt-ordinary": nil} {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "device-1", Kind: ThreadRequestSend, TargetThreadID: "t-caller", CreatedAt: 100, UpdatedAt: 100,
		}); err != nil {
			t.Fatal(err)
		}
		if row := receipt(token); row.AnswerAgent != nil {
			t.Errorf("open receipt %s records agent %+v", token, row.AnswerAgent)
		}
		if result, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, Agent: agent,
		}, ThreadRequestFinished); err != nil || !result.Settled {
			t.Fatalf("settle receipt %s: settled=%v err=%v", token, result.Settled, err)
		}
		got := receipt(token).AnswerAgent
		if (got == nil) != (agent == nil) || (got != nil && got.Role != agent.Role) {
			t.Errorf("receipt %s agent = %+v, want %+v", token, got, agent)
		}
	}
	if stored, err := s.StoreThreadReceiptLateReply("rcpt-agent", []byte("more"), 0, &ThreadAnswerAgent{Role: "Second role"}); err != nil || !stored {
		t.Fatalf("receipt late reply: stored=%v err=%v", stored, err)
	}
	if got := receipt("rcpt-agent").AnswerAgent; got == nil || got.Role != "Second role" {
		t.Errorf("receipt agent after the late reply = %+v", got)
	}
	if stored, err := s.StoreThreadReceiptLateReply("rcpt-roleless", []byte("more"), 0, nil); err != nil || !stored {
		t.Fatalf("receipt late reply: stored=%v err=%v", stored, err)
	}
	if got := receipt("rcpt-roleless").AnswerAgent; got != nil {
		t.Errorf("receipt agent after an ordinary thread's late reply = %+v", got)
	}
}

// Every state change on a request is conditional on the state it expects to
// find. A change that does not apply is a lost race with another observer,
// which is a normal outcome and must not read as an error or as a second
// settlement.
func TestThreadRequestStateTransitionsAreConditional(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-1", "t-caller", ThreadRequestSend, 100)

	applied, err := s.AdvanceThreadRequestState("tok-1", ThreadRequestUnconfirmed, ThreadRequestAccepted)
	if err != nil || !applied {
		t.Fatalf("accept: applied=%v err=%v", applied, err)
	}
	applied, err = s.AdvanceThreadRequestState("tok-1", ThreadRequestUnconfirmed, ThreadRequestAccepted)
	if err != nil {
		t.Fatalf("repeat accept: %v", err)
	}
	if applied {
		t.Error("a transition from a state the row has left must not apply")
	}
	if _, err := s.AdvanceThreadRequestState("tok-1", ThreadRequestAccepted, "nonsense"); err == nil {
		t.Error("an unknown target state must be refused")
	}
	applied, err = s.AdvanceThreadRequestState("tok-missing", ThreadRequestUnconfirmed, ThreadRequestAccepted)
	if err != nil || applied {
		t.Fatalf("unknown token: applied=%v err=%v", applied, err)
	}

	answer := bytes.Repeat([]byte("answer "), 5000)
	settled, err := s.SettleThreadRequest("tok-1", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: answer, AnswerKind: ThreadAnswerFinal,
		SettledAt: 900, ExpiresAt: 1900,
	})
	if err != nil || !settled {
		t.Fatalf("settle: applied=%v err=%v", settled, err)
	}
	row := mustGetThreadRequest(t, s, "tok-1")
	if !bytes.Equal(row.Answer, answer) {
		t.Errorf("answer round trip lost %d bytes", len(answer)-len(row.Answer))
	}
	if row.State != ThreadRequestFinished || row.Revision != 1 || row.SettledAt != 900 || row.ExpiresAt != 1900 {
		t.Errorf("settled row = %+v", row)
	}

	// A second collector settling the same request finds it closed.
	settled, err = s.SettleThreadRequest("tok-1", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestErrored, AnswerKind: ThreadAnswerError, SettledAt: 950,
	})
	if err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if settled {
		t.Error("a settled request must not settle twice")
	}
	if got := mustGetThreadRequest(t, s, "tok-1"); got.Revision != 1 {
		t.Errorf("revision after the lost race = %d, want 1", got.Revision)
	}

	// Delivery is recorded once, so a repeated observation cannot inject a
	// second wake.
	marked, err := s.MarkThreadRequestDelivered("tok-1", ThreadWakeInline, 1000, false)
	if err != nil || !marked {
		t.Fatalf("mark delivered: marked=%v err=%v", marked, err)
	}
	marked, err = s.MarkThreadRequestDelivered("tok-1", ThreadWakeQueued, 1100, false)
	if err != nil {
		t.Fatalf("repeat mark delivered: %v", err)
	}
	if marked {
		t.Error("a delivered request must not be delivered twice")
	}
	if _, err := s.MarkThreadRequestDelivered("tok-1", "carrier-pigeon", 1200, false); err == nil {
		t.Error("an unknown delivery route must be refused")
	}
	if got := mustGetThreadRequest(t, s, "tok-1"); got.DeliveredAt != 1000 || got.DeliveredHow != ThreadWakeInline {
		t.Errorf("delivery = %d/%s, want 1000/inline", got.DeliveredAt, got.DeliveredHow)
	}
}

// A late reply is a new revision on both sides: that is what makes a waiting
// thread_status return again and a second wake land.
func TestThreadRequestLateReplyIsANewRevision(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-late", "t-caller", ThreadRequestAsk, 100)
	if _, err := s.SettleThreadRequest("tok-late", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("first"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}

	stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("second thoughts"), 300, nil)
	if err != nil || !stored {
		t.Fatalf("store late reply: stored=%v err=%v", stored, err)
	}
	row := mustGetThreadRequest(t, s, "tok-late")
	if string(row.LateReply) != "second thoughts" || row.LateReplyAt != 300 || row.Revision != 2 {
		t.Fatalf("late reply row = %+v", row)
	}
	if string(row.Answer) != "first" {
		t.Errorf("late reply overwrote the answer: %q", row.Answer)
	}

	stored, err = s.StoreThreadRequestLateReply("tok-late", []byte("third"), 400, nil)
	if err != nil {
		t.Fatalf("second late reply: %v", err)
	}
	if stored {
		t.Error("only the first late reply is stored")
	}

	// The first wake is already spent; the late reply gets its own.
	if _, err := s.MarkThreadRequestDelivered("tok-late", ThreadWakeQueued, 350, false); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	marked, err := s.MarkThreadRequestDelivered("tok-late", ThreadWakeQueued, 450, true)
	if err != nil || !marked {
		t.Fatalf("mark late delivered: marked=%v err=%v", marked, err)
	}
	if got := mustGetThreadRequest(t, s, "tok-late"); got.LateDeliveredAt != 450 {
		t.Errorf("late delivery = %d, want 450", got.LateDeliveredAt)
	}

	// A request that never settled has nothing to reply late to.
	seedThreadRequest(t, s, "tok-open", "t-caller", ThreadRequestAsk, 100)
	stored, err = s.StoreThreadRequestLateReply("tok-open", []byte("early"), 500, nil)
	if err != nil {
		t.Fatalf("late reply on an open request: %v", err)
	}
	if stored {
		t.Error("an unsettled request must not take a late reply")
	}
}

// The caller's list is what an agent reads to recover its tokens: open
// requests first, newest first within each group.
func TestListThreadRequestsByCallerOrdersOpenFirst(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-other")
	seedThreadRequest(t, s, "tok-old-settled", "t-caller", ThreadRequestSend, 100)
	seedThreadRequest(t, s, "tok-new-settled", "t-caller", ThreadRequestSend, 200)
	seedThreadRequest(t, s, "tok-old-open", "t-caller", ThreadRequestSend, 300)
	seedThreadRequest(t, s, "tok-new-open", "t-caller", ThreadRequestSend, 400)
	seedThreadRequest(t, s, "tok-elsewhere", "t-other", ThreadRequestSend, 500)
	for _, token := range []string{"tok-old-settled", "tok-new-settled"} {
		if _, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, AnswerKind: ThreadAnswerFinal, SettledAt: 600,
		}); err != nil {
			t.Fatalf("settle %s: %v", token, err)
		}
	}

	listed, err := s.ListThreadRequestsByCaller("t-caller", 10, 0)
	if err != nil {
		t.Fatalf("list requests: %v", err)
	}
	var order []string
	for _, row := range listed {
		order = append(order, row.Token)
	}
	want := "tok-new-open,tok-old-open,tok-new-settled,tok-old-settled"
	if strings.Join(order, ",") != want {
		t.Errorf("order = %s, want %s", strings.Join(order, ","), want)
	}

	page, err := s.ListThreadRequestsByCaller("t-caller", 2, 2)
	if err != nil {
		t.Fatalf("page requests: %v", err)
	}
	if len(page) != 2 || page[0].Token != "tok-new-settled" {
		t.Errorf("second page = %+v", page)
	}

	open, err := s.HasOpenThreadRequests("t-caller")
	if err != nil || !open {
		t.Fatalf("has open requests: open=%v err=%v", open, err)
	}
	for _, token := range []string{"tok-old-open", "tok-new-open"} {
		if _, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestCancelled, AnswerKind: ThreadAnswerNote, SettledAt: 700,
		}); err != nil {
			t.Fatalf("settle %s: %v", token, err)
		}
	}
	open, err = s.HasOpenThreadRequests("t-caller")
	if err != nil || open {
		t.Fatalf("has open requests after settling: open=%v err=%v", open, err)
	}
}

// The reminder clock and the remote poller each read a partial index; both
// predicates are restated in the query, so both have to answer exactly.
func TestThreadRequestDueQueries(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remind", CallerThreadID: "t-caller", Kind: ThreadRequestRemind,
		DueAt: 1000, State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-later", CallerThreadID: "t-caller", Kind: ThreadRequestRemind,
		DueAt: 5000, State: ThreadRequestAccepted, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert later reminder: %v", err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remote", CallerThreadID: "t-caller", Kind: ThreadRequestSend,
		TargetComputerID: "backend-2", State: ThreadRequestRunning, NextCheck: 900,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("insert remote request: %v", err)
	}

	due, err := s.DueThreadReminders(1000, 10)
	if err != nil {
		t.Fatalf("due reminders: %v", err)
	}
	if len(due) != 1 || due[0].Token != "tok-remind" {
		t.Fatalf("due reminders = %+v", due)
	}

	polls, err := s.DueThreadRequestPolls(1000, 10)
	if err != nil {
		t.Fatalf("due polls: %v", err)
	}
	if len(polls) != 1 || polls[0].Token != "tok-remote" {
		t.Fatalf("due polls = %+v", polls)
	}
	if err := s.RescheduleThreadRequest("tok-remote", 4000, 3, "backend unreachable"); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	polls, err = s.DueThreadRequestPolls(1000, 10)
	if err != nil {
		t.Fatalf("due polls after reschedule: %v", err)
	}
	if len(polls) != 0 {
		t.Errorf("rescheduled request still due: %+v", polls)
	}
	if got := mustGetThreadRequest(t, s, "tok-remote"); got.Attempts != 3 || got.Issue != "backend unreachable" {
		t.Errorf("reschedule did not record the attempt: %+v", got)
	}

	// Cancelling a reminder drops the row; every other kind keeps its
	// lineage until the retention floor.
	deleted, err := s.DeleteThreadRequest("tok-later")
	if err != nil || !deleted {
		t.Fatalf("delete reminder: deleted=%v err=%v", deleted, err)
	}
	deleted, err = s.DeleteThreadRequest("tok-later")
	if err != nil || deleted {
		t.Fatalf("repeat delete: deleted=%v err=%v", deleted, err)
	}
}

// The notify flag is what a settlement checks before waking a caller, so
// arming and disarming it must report whether anything changed.
func TestThreadRequestNotifyFlag(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-notify", "t-caller", ThreadRequestSend, 100)
	seedThreadRequest(t, s, "tok-quiet", "t-caller", ThreadRequestSend, 200)

	changed, err := s.SetThreadRequestNotify("tok-notify", true)
	if err != nil || !changed {
		t.Fatalf("arm notify: changed=%v err=%v", changed, err)
	}
	changed, err = s.SetThreadRequestNotify("tok-notify", true)
	if err != nil {
		t.Fatalf("repeat arm: %v", err)
	}
	if changed {
		t.Error("arming an armed request must report no change")
	}

	armed, err := s.SetThreadRequestsNotifyForCaller("t-caller", true)
	if err != nil {
		t.Fatalf("arm caller: %v", err)
	}
	if armed != 1 {
		t.Errorf("armed rows = %d, want the one that was not armed", armed)
	}
	disarmed, err := s.SetThreadRequestsNotifyForCaller("t-caller", false)
	if err != nil {
		t.Fatalf("disarm caller: %v", err)
	}
	if disarmed != 2 {
		t.Errorf("disarmed rows = %d, want 2", disarmed)
	}
}

// A settled receipt is the answer a paired computer has not collected yet,
// so it must outlive the thread it ran in: an ask's scratch fork is deleted
// as soon as it answers, and the thread binding cascades.
func TestThreadRequestReceiptSurvivesTheThreadItRanIn(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-answered")
	mustCreateThread(t, s, "t-open")
	for token, thread := range map[string]string{"tok-answered": "t-answered", "tok-open": "t-open"} {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "device-1", SourceComputerID: "backend-2",
			SourceThreadID: "t-remote", Kind: ThreadRequestSend, TargetThreadID: thread,
		}); err != nil {
			t.Fatalf("accept %s: %v", token, err)
		}
	}
	if _, err := s.SettleLocalThreadRequest("tok-answered", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("the answer"), AnswerKind: ThreadAnswerFinal,
	}, ThreadRequestFinished); err != nil {
		t.Fatalf("settle: %v", err)
	}

	detached, err := s.DetachThreadReceiptsFromThread("t-answered")
	if err != nil || detached != 1 {
		t.Fatalf("detach settled receipts = %d, %v", detached, err)
	}
	// An open receipt keeps its thread: its own caller settles it first.
	if detached, err := s.DetachThreadReceiptsFromThread("t-open"); err != nil || detached != 0 {
		t.Fatalf("detach open receipts = %d, %v", detached, err)
	}
	if err := s.DeleteThread("t-answered"); err != nil {
		t.Fatalf("delete the thread the request ran in: %v", err)
	}

	receipt, found, err := s.GetThreadRequestReceipt("tok-answered")
	if err != nil || !found {
		t.Fatalf("the answer went with the thread: found=%v err=%v", found, err)
	}
	if string(receipt.Answer) != "the answer" || receipt.TargetThreadID != "" {
		t.Fatalf("detached receipt = %+v", receipt)
	}
}

// The destination row is keyed by the token, so a retried delivery of the
// same request finds the acceptance it already made and spawns nothing.
func TestThreadRequestReceiptAcceptanceIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	receipt := ThreadRequestReceipt{
		Token: "tok-r", OwnerDeviceID: "device-1", SourceComputerID: "backend-2",
		SourceComputerName: "Studio", SourceThreadID: "t-remote", SourceThreadTitle: "Remote",
		Kind: ThreadRequestSend, TargetThreadID: "t-target", CreatedAt: 100, UpdatedAt: 100,
	}
	stored, created, err := s.AcceptThreadRequestReceipt(receipt)
	if err != nil || !created {
		t.Fatalf("accept: created=%v err=%v", created, err)
	}
	if stored.State != ThreadReceiptAccepted || stored.OwnerDeviceID != "device-1" {
		t.Fatalf("accepted receipt = %+v", stored)
	}

	retry := receipt
	retry.OwnerDeviceID = "device-9"
	retry.SourceThreadTitle = "Renamed"
	again, created, err := s.AcceptThreadRequestReceipt(retry)
	if err != nil {
		t.Fatalf("retry accept: %v", err)
	}
	if created {
		t.Error("a retried acceptance must not create a second row")
	}
	if again.OwnerDeviceID != "device-1" || again.SourceThreadTitle != "Remote" {
		t.Errorf("retry overwrote the acceptance: %+v", again)
	}

	dispatched, err := s.MarkThreadReceiptDispatched("tok-r", "item-1")
	if err != nil || !dispatched {
		t.Fatalf("mark dispatched: dispatched=%v err=%v", dispatched, err)
	}
	if row, _, _ := s.GetThreadRequestReceipt("tok-r"); row.State != ThreadReceiptRunning || row.TurnID != "" || row.MessageItemID != "item-1" {
		t.Fatalf("a dispatched receipt is running with no turn: %+v", row)
	}
	if again, err := s.MarkThreadReceiptDispatched("tok-r", "item-2"); err != nil || again {
		t.Fatalf("a second dispatch mark applied: %v %v", again, err)
	}
	if _, err := s.BindThreadReceiptTurn("tok-r", "item-1", "", true); err == nil {
		t.Error("a receipt cannot be bound to no turn")
	}
	bound, err := s.BindThreadReceiptTurn("tok-r", "item-1", "turn-1", true)
	if err != nil || !bound {
		t.Fatalf("bind turn: bound=%v err=%v", bound, err)
	}
	bound, err = s.BindThreadReceiptTurn("tok-r", "item-2", "turn-2", false)
	if err != nil {
		t.Fatalf("repeat bind: %v", err)
	}
	if bound {
		t.Error("a bound receipt must not be re-bound to another turn")
	}
	if row, _, _ := s.GetThreadRequestReceipt("tok-r"); row.TurnID != "turn-1" || !row.OwnTurn {
		t.Errorf("receipt = %+v, want the first delivery's turn, its own", row)
	}
	if again, err := s.MarkThreadReceiptDispatched("tok-r", "item-3"); err != nil || again {
		t.Fatalf("a dispatch mark after the delivery applied: %v %v", again, err)
	}
	if row, _, _ := s.GetThreadRequestReceipt("tok-r"); row.TurnID != "turn-1" || row.MessageItemID != "item-1" {
		t.Fatalf("receipt = %+v, want the first delivery's binding", row)
	}

	open, err := s.ListOpenThreadRequestReceipts()
	if err != nil {
		t.Fatalf("list open receipts: %v", err)
	}
	if len(open) != 1 || open[0].Token != "tok-r" {
		t.Fatalf("open receipts = %+v", open)
	}

	result, err := s.SettleLocalThreadRequest("tok-r", []string{ThreadReceiptRunning}, ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 500,
	}, ThreadRequestFinished)
	if err != nil || !result.Settled {
		t.Fatalf("settle receipt: settled=%v err=%v", result.Settled, err)
	}
	row, found, err := s.GetThreadRequestReceipt("tok-r")
	if err != nil || !found {
		t.Fatalf("get receipt: found=%v err=%v", found, err)
	}
	if row.Revision != 1 || row.MessageItemID != "item-1" || row.TurnID != "turn-1" {
		t.Errorf("settled receipt = %+v", row)
	}
	if row.ExpiresAt != 500+threadAnswerHoldMillis {
		t.Errorf("expiry = %d, want the settle time plus the hold", row.ExpiresAt)
	}

	byThread, err := s.ListThreadRequestReceiptsForThread("t-target")
	if err != nil {
		t.Fatalf("list receipts for thread: %v", err)
	}
	if len(byThread) != 1 || byThread[0].Token != "tok-r" {
		t.Fatalf("receipts for thread = %+v", byThread)
	}
	if _, found, err := s.GetThreadRequestReceipt("tok-unknown"); err != nil || found {
		t.Fatalf("unknown token: found=%v err=%v", found, err)
	}
}

// A delivery can land before the dispatch mark: it binds an accepted
// receipt directly, the late mark changes nothing, and a settled receipt is
// never bound.
func TestADeliveryBindsAReceiptAheadOfItsDispatchMark(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-early", OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
		TargetThreadID: "t-target", CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if bound, err := s.BindThreadReceiptTurn("tok-early", "item-e", "turn-e", false); err != nil || !bound {
		t.Fatalf("bind an accepted receipt: %v %v", bound, err)
	}
	if again, err := s.MarkThreadReceiptDispatched("tok-early", "item-e"); err != nil || again {
		t.Fatalf("a dispatch mark after the delivery applied: %v %v", again, err)
	}
	if row, _, _ := s.GetThreadRequestReceipt("tok-early"); row.State != ThreadReceiptRunning || row.TurnID != "turn-e" || row.OwnTurn {
		t.Fatalf("receipt = %+v, want it running on the delivered turn", row)
	}
	if _, err := s.SettleLocalThreadRequest("tok-early", []string{ThreadReceiptRunning}, ThreadRequestSettlement{
		State: ThreadReceiptCancelled, Answer: []byte("x"), AnswerKind: ThreadAnswerError,
	}, ThreadRequestCancelled); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if bound, err := s.BindThreadReceiptTurn("tok-early", "item-e", "turn-later", true); err != nil || bound {
		t.Fatalf("a settled receipt was bound: %v %v", bound, err)
	}
}

// A rebind moves a running receipt only from the turn it is bound to, and only
// while that turn holds no rows; a completion on the empty turn does not pin
// it. A settled receipt is never moved.
func TestARebindMovesOnlyFromAnEmptyBoundTurn(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-a", OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
		TargetThreadID: "t-target", CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if bound, err := s.BindThreadReceiptTurn("tok-a", "item-a", "t-target#1", true); err != nil || !bound {
		t.Fatalf("bind: %v %v", bound, err)
	}
	if err := s.InsertTurn(Turn{TurnID: "turn-1", ThreadID: "t-target", TurnIndex: 1, StartedAt: 200}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if _, err := s.UpsertItem(Item{
		ID: "item-a", ThreadID: "t-target", TurnIndex: 1, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "check the changelog", CreatedAt: 200, UpdatedAt: 200,
	}, nil); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	rebind := func(from string) bool {
		t.Helper()
		moved, err := s.RebindThreadReceiptTurn("tok-a", "item-b", "t-target", 1, from, "t-target#2", true)
		if err != nil {
			t.Fatalf("rebind: %v", err)
		}
		return moved
	}
	if rebind("t-target#1") {
		t.Fatal("moved from a turn that still holds a row")
	}
	if err := s.DeleteThreadItem("t-target", "item-a"); err != nil {
		t.Fatalf("DeleteThreadItem: %v", err)
	}
	if err := s.UpdateTurnCompleted("turn-1", 300, "end_turn", "", "", ""); err != nil {
		t.Fatalf("UpdateTurnCompleted: %v", err)
	}
	if rebind("t-target#0") {
		t.Fatal("moved from a turn it is not bound to")
	}
	if !rebind("t-target#1") {
		t.Fatal("not moved from its empty, completed turn")
	}
	if row, _, _ := s.GetThreadRequestReceipt("tok-a"); row.TurnID != "t-target#2" || row.MessageItemID != "item-b" || !row.OwnTurn {
		t.Fatalf("receipt = %+v, want it on turn 2 under the survivor's row", row)
	}
	if _, err := s.SettleLocalThreadRequest("tok-a", []string{ThreadReceiptRunning}, ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("x"), AnswerKind: ThreadAnswerFinal,
	}, ThreadRequestFinished); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if moved, err := s.RebindThreadReceiptTurn("tok-a", "item-c", "t-target", 2, "t-target#2", "t-target#3", true); err != nil || moved {
		t.Fatalf("a settled receipt was moved: %v %v", moved, err)
	}
}

// An answer nobody collected is dropped once its hold runs out; a collected
// one is left alone, because the acknowledgement is what proves the caller
// has it durably.
func TestExpireThreadRequestAnswersOnlyDropsUncollected(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	for _, token := range []string{"tok-collected", "tok-uncollected", "tok-open"} {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "device-1", Kind: ThreadRequestAsk,
			TargetThreadID: "t-target", CreatedAt: 100, UpdatedAt: 100,
		}); err != nil {
			t.Fatalf("accept %s: %v", token, err)
		}
	}
	for _, token := range []string{"tok-collected", "tok-uncollected"} {
		if _, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("answer"), AnswerKind: ThreadAnswerFinal,
			SettledAt: 1000, ExpiresAt: 2000,
		}, ThreadRequestFinished); err != nil {
			t.Fatalf("settle %s: %v", token, err)
		}
	}
	acked, err := s.AckThreadRequestReceipt("tok-collected", 1, 1500)
	if err != nil || !acked {
		t.Fatalf("ack: acked=%v err=%v", acked, err)
	}
	acked, err = s.AckThreadRequestReceipt("tok-collected", 1, 1600)
	if err != nil {
		t.Fatalf("repeat ack: %v", err)
	}
	if acked {
		t.Error("acknowledging a revision already collected must report no change")
	}

	dropped, err := s.ExpireThreadRequestAnswers(1999)
	if err != nil {
		t.Fatalf("early expiry sweep: %v", err)
	}
	if dropped != 0 {
		t.Errorf("the sweep dropped %d answers before their hold ran out", dropped)
	}

	dropped, err = s.ExpireThreadRequestAnswers(2000)
	if err != nil {
		t.Fatalf("expiry sweep: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want only the uncollected answer", dropped)
	}
	expired, _, err := s.GetThreadRequestReceipt("tok-uncollected")
	if err != nil {
		t.Fatalf("read expired receipt: %v", err)
	}
	if expired.State != ThreadReceiptExpired || len(expired.Answer) != 0 {
		t.Errorf("expired receipt = %+v", expired)
	}
	kept, _, err := s.GetThreadRequestReceipt("tok-collected")
	if err != nil {
		t.Fatalf("read collected receipt: %v", err)
	}
	if kept.State != ThreadReceiptFinished || string(kept.Answer) != "answer" {
		t.Errorf("collected receipt was swept: %+v", kept)
	}
	stillOpen, _, err := s.GetThreadRequestReceipt("tok-open")
	if err != nil {
		t.Fatalf("read open receipt: %v", err)
	}
	if stillOpen.State != ThreadReceiptAccepted {
		t.Errorf("an unsettled receipt was expired: %+v", stillOpen)
	}

	// A late reply inside the hold is a new uncollected revision with its
	// own hold.
	stored, err := s.StoreThreadReceiptLateReply("tok-collected", []byte("more"), 1900, nil)
	if err != nil || !stored {
		t.Fatalf("store receipt late reply: stored=%v err=%v", stored, err)
	}
	dropped, err = s.ExpireThreadRequestAnswers(1900 + threadAnswerHoldMillis)
	if err != nil {
		t.Fatalf("late expiry sweep: %v", err)
	}
	if dropped != 1 {
		t.Errorf("late reply sweep dropped %d, want 1", dropped)
	}
}

// A late reply is only worth storing while the source still polls for it,
// which is the hold on the first answer: at or past `expires_at` the reply is
// refused rather than parked where nothing will ever collect it.
func TestStoreThreadReceiptLateReplyRefusesPastTheHold(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-stale", OwnerDeviceID: "device-1", Kind: ThreadRequestAsk,
		TargetThreadID: "t-target", CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := s.SettleLocalThreadRequest("tok-stale", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptFinished, AnswerKind: ThreadAnswerNote, SettledAt: 1000, ExpiresAt: 2000,
	}, ThreadRequestFinished); err != nil {
		t.Fatalf("settle: %v", err)
	}
	for _, at := range []int64{2000, 2001} {
		stored, err := s.StoreThreadReceiptLateReply("tok-stale", []byte("too late"), at, nil)
		if err != nil {
			t.Fatalf("late reply at %d: %v", at, err)
		}
		if stored {
			t.Fatalf("a late reply at %d was stored past the hold ending at 2000", at)
		}
	}
	stored, err := s.StoreThreadReceiptLateReply("tok-stale", []byte("in time"), 1999, nil)
	if err != nil || !stored {
		t.Fatalf("late reply inside the hold: stored=%v err=%v", stored, err)
	}
	row, _, err := s.GetThreadRequestReceipt("tok-stale")
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if string(row.LateReply) != "in time" || row.ExpiresAt != 1999+threadAnswerHoldMillis {
		t.Errorf("late reply row = %+v", row)
	}
}

// Past the retention floor a token stops answering on both sides, so a late
// reply to it is unknown rather than a resurrected month-old exchange.
//
// The floor is measured from the moment a request finished, and it applies
// to finished requests alone: an open row, a reminder still to fire and an
// answer written yesterday all outlive the age of the row that carries them.
func TestThreadRequestRetentionSweepClearsSettledRowsOnly(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")

	settle := func(token string, at int64) {
		t.Helper()
		applied, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
			State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: at,
		})
		if err != nil || !applied {
			t.Fatalf("settle %s: applied=%v err=%v", token, applied, err)
		}
	}

	// Old and settled long ago: the only row the floor takes.
	seedThreadRequest(t, s, "tok-settled-old", "t-caller", ThreadRequestSend, 100)
	settle("tok-settled-old", 200)
	// Old, but answered after the floor: the answer is still readable.
	seedThreadRequest(t, s, "tok-settled-recent", "t-caller", ThreadRequestSend, 100)
	settle("tok-settled-recent", 5000)
	// Old and still open: nothing else can settle or cancel it once it is
	// gone, so it waits for its own settlement.
	seedThreadRequest(t, s, "tok-open-old", "t-caller", ThreadRequestSend, 100)
	// Old, settled, and the record of which thread spawned t-child: its
	// lineage lives as long as that thread does, its answer does not. A
	// spawn whose thread is gone, or one made on another computer, is an
	// ordinary settled row.
	mustCreateThread(t, s, "t-child")
	for token, target := range map[string][2]string{
		"tok-spawn-lineage": {"", "t-child"}, "tok-spawn-gone": {"", "t-deleted"}, "tok-spawn-remote": {"laptop", "t-child"},
	} {
		seedThreadRequest(t, s, token, "t-caller", ThreadRequestSpawn, 100)
		if _, err := s.SetThreadRequestTarget(token, target[0], target[1], "", ""); err != nil {
			t.Fatalf("target %s: %v", token, err)
		}
		settle(token, 200)
	}
	// The lineage row also owes a wake it never handed over, and holds a
	// late reply.
	if _, err := s.SetThreadRequestNotify("tok-spawn-lineage", true); err != nil {
		t.Fatal(err)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-spawn-lineage", []byte("one more thing"), 300, &ThreadAnswerAgent{Role: "Reviewer"}); err != nil || !stored {
		t.Fatalf("late reply: stored=%v err=%v", stored, err)
	}
	owesWake := func() bool {
		t.Helper()
		wakes, err := s.UndeliveredThreadRequestWakes(1<<62, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range wakes {
			if row.Token == "tok-spawn-lineage" {
				return true
			}
		}
		return false
	}
	if !owesWake() {
		t.Fatal("the lineage row owes no wake before the sweep")
	}
	// And the Agents columns a remote answer writes.
	if _, err := s.db.Exec(`UPDATE thread_requests SET nested_open = 1, target_provider = 'codex' WHERE token = ?`, "tok-spawn-lineage"); err != nil {
		t.Fatal(err)
	}
	// A reminder armed past the floor. No ceiling: next week is a valid
	// reminder, and so is next month.
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remind-later", CallerThreadID: "t-caller", Kind: ThreadRequestRemind,
		DueAt: 9000, State: ThreadRequestAccepted, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}

	for token, created := range map[string]int64{
		"tok-receipt-settled-old": 100, "tok-receipt-settled-recent": 100, "tok-receipt-open-old": 100,
	} {
		if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
			Token: token, OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
			TargetThreadID: "t-caller", CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			t.Fatalf("accept %s: %v", token, err)
		}
	}
	for token, at := range map[string]int64{"tok-receipt-settled-old": 200, "tok-receipt-settled-recent": 5000} {
		result, err := s.SettleLocalThreadRequest(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: at,
		}, ThreadRequestFinished)
		if err != nil || !result.Settled {
			t.Fatalf("settle receipt %s: applied=%v err=%v", token, result.Settled, err)
		}
	}

	swept, err := s.DeleteThreadRequestsBefore(1000)
	if err != nil {
		t.Fatalf("retention sweep: %v", err)
	}
	if swept != 5 {
		t.Errorf("swept = %d, want three settled requests and one receipt deleted and one lineage row cleared", swept)
	}
	for _, token := range []string{"tok-settled-recent", "tok-open-old", "tok-remind-later"} {
		if _, found, err := s.GetThreadRequest(token); err != nil || !found {
			t.Errorf("%s was swept: found=%v err=%v", token, found, err)
		}
	}
	for _, token := range []string{"tok-settled-old", "tok-spawn-gone", "tok-spawn-remote"} {
		if _, found, err := s.GetThreadRequest(token); err != nil || found {
			t.Errorf("%s, settled before the floor, survived: found=%v err=%v", token, found, err)
		}
	}
	// The lineage row keeps what names the spawn and nothing it answered,
	// and the ledger no longer knows its token.
	if _, found, err := s.GetThreadRequest("tok-spawn-lineage"); err != nil || found {
		t.Errorf("the ledger still reads the lineage row: found=%v err=%v", found, err)
	}
	if rows, err := s.GetThreadRequests([]string{"tok-spawn-lineage", "tok-open-old"}); err != nil || len(rows) != 1 || rows[0].Token != "tok-open-old" {
		t.Errorf("GetThreadRequests with the lineage token = %+v, %v; want tok-open-old alone", rows, err)
	}
	if rows, err := s.ListThreadRequestsByCaller("t-caller", 50, 0); err != nil {
		t.Fatal(err)
	} else {
		for _, row := range rows {
			if row.Token == "tok-spawn-lineage" {
				t.Errorf("the caller's listing still shows the lineage row: %+v", row)
			}
		}
	}
	lineage := rawThreadRequest(t, s, "tok-spawn-lineage")
	if !lineage.LineageOnly {
		t.Errorf("the kept spawn row is not marked lineage-only: %+v", lineage)
	}
	if len(lineage.Answer) != 0 || lineage.AnswerKind != "" || len(lineage.LateReply) != 0 || lineage.LateReplyAt != 0 || lineage.Notify ||
		lineage.AnswerAgent != nil || lineage.NestedOpen || lineage.TargetProvider != "" {
		t.Errorf("lineage row past the floor = answer %q kind %q late reply %q at %d notify %v agent %+v; want its payload cleared",
			lineage.Answer, lineage.AnswerKind, lineage.LateReply, lineage.LateReplyAt, lineage.Notify, lineage.AnswerAgent)
	}
	var agentRole string
	if err := s.db.QueryRow(`SELECT answer_agent_role FROM thread_requests WHERE token = ?`, "tok-spawn-lineage").Scan(&agentRole); err != nil || agentRole != "" {
		t.Errorf("lineage row kept its answer agent role %q (%v)", agentRole, err)
	}
	if lineage.CallerThreadID != "t-caller" || lineage.Kind != ThreadRequestSpawn || lineage.TargetThreadID != "t-child" ||
		lineage.State != ThreadRequestFinished || lineage.SettledAt != 200 || lineage.CreatedAt != 100 {
		t.Errorf("lineage row past the floor lost its lineage: %+v", lineage)
	}
	if owesWake() {
		t.Error("the cleared lineage row still owes a wake")
	}
	if child, err := s.GetThread("t-child"); err != nil || child.SpawnedByThreadID != "t-caller" {
		t.Errorf("t-child after the sweep = spawnedBy %q, %v; want t-caller", child.SpawnedByThreadID, err)
	}
	if ids, err := s.ListSpawnedThreadIDs("t-caller"); err != nil || !slices.Equal(ids, []string{"t-child"}) {
		t.Errorf("threads spawned by t-caller after the sweep = %v, %v; want t-child", ids, err)
	}
	// A second sweep finds nothing left to take.
	if swept, err := s.DeleteThreadRequestsBefore(1000); err != nil || swept != 0 {
		t.Errorf("second sweep: swept=%d err=%v", swept, err)
	}
	// Nothing writes an answer or a wake back onto the lineage row.
	if stored, err := s.StoreThreadRequestLateReply("tok-spawn-lineage", []byte("much later"), 2000, nil); err != nil || stored {
		t.Errorf("late reply on the lineage row: stored=%v err=%v", stored, err)
	}
	if armed, err := s.SetThreadRequestNotify("tok-spawn-lineage", true); err != nil || armed {
		t.Errorf("arm the lineage row's wake: armed=%v err=%v", armed, err)
	}
	if _, err := s.SetThreadRequestsNotifyForCaller("t-caller", true); err != nil {
		t.Fatal(err)
	}
	if row := rawThreadRequest(t, s, "tok-spawn-lineage"); row.Notify || len(row.LateReply) != 0 {
		t.Errorf("lineage row after the writers = notify %v late reply %q; want neither", row.Notify, row.LateReply)
	}
	// Once the spawned thread is gone the lineage row is one more old
	// settled request.
	if err := s.DeleteThread("t-child"); err != nil {
		t.Fatal(err)
	}
	if swept, err := s.DeleteThreadRequestsBefore(1000); err != nil || swept != 1 {
		t.Errorf("sweep after the child was deleted: swept=%d err=%v", swept, err)
	}
	for _, token := range []string{"tok-receipt-settled-recent", "tok-receipt-open-old"} {
		if _, found, err := s.GetThreadRequestReceipt(token); err != nil || !found {
			t.Errorf("receipt %s was swept: found=%v err=%v", token, found, err)
		}
	}
	if _, found, err := s.GetThreadRequestReceipt("tok-receipt-settled-old"); err != nil || found {
		t.Errorf("a receipt settled before the floor survived: found=%v err=%v", found, err)
	}
}

// Both tables hang off the thread they belong to, so deleting the thread
// takes its requests and receipts with it.
func TestThreadRequestRowsFollowTheirThread(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-gone")
	seedThreadRequest(t, s, "tok-gone", "t-gone", ThreadRequestSend, 100)
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-receipt-gone", OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
		TargetThreadID: "t-gone", CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("accept receipt: %v", err)
	}
	if err := s.DeleteThread("t-gone"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	if _, found, err := s.GetThreadRequest("tok-gone"); err != nil || found {
		t.Errorf("request outlived its caller: found=%v err=%v", found, err)
	}
	if _, found, err := s.GetThreadRequestReceipt("tok-receipt-gone"); err != nil || found {
		t.Errorf("receipt outlived its target: found=%v err=%v", found, err)
	}
}

// Delivery is marked on the caller's transaction so the queue insert and the
// mark commit together.
func TestMarkThreadRequestDeliveredTxSharesTheCallerTransaction(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-tx", "t-caller", ThreadRequestSend, 100, "the answer")

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	marked, err := MarkThreadRequestDeliveredTx(tx, "tok-tx", ThreadWakeQueued, 700, false)
	if err != nil {
		t.Fatalf("mark delivered in tx: %v", err)
	}
	if !marked {
		t.Fatal("the first delivery must apply")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := mustGetThreadRequest(t, s, "tok-tx"); got.DeliveredAt != 0 {
		t.Errorf("a rolled-back delivery was recorded anyway: %+v", got)
	}
}

// An armed reminder carries its note as the answer while it is still open,
// and a request that is only running can carry a partial reply. Reading one
// is not a delivery: the wake its settlement owes has not happened yet, so
// the mark is refused until the row settles.
func TestAnOpenThreadRequestCannotBeDelivered(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-armed", CallerThreadID: "t-caller", Kind: ThreadRequestRemind,
		DueAt: 5000, Answer: []byte("check the deploy"), AnswerKind: ThreadAnswerNote,
		Notify: true, State: ThreadRequestAccepted, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("insert armed reminder: %v", err)
	}

	marked, err := s.MarkThreadRequestDelivered("tok-armed", ThreadWakeInline, 200, false)
	if err != nil {
		t.Fatalf("mark an open request delivered: %v", err)
	}
	if marked {
		t.Error("an open request was marked delivered, which disarms the wake it still owes")
	}
	item := FlushQueueItem{ID: "queue:armed", ThreadID: "t-caller", SendID: "thread-wake:tok-armed", Message: "the note", EnqueuedAt: 300}
	if err := s.QueueThreadWake("tok-armed", false, item); err == nil {
		t.Error("a wake was queued for a request that has not settled")
	}
	if got := mustGetThreadRequest(t, s, "tok-armed"); got.DeliveredAt != 0 {
		t.Errorf("delivery recorded on an open request: %+v", got)
	}

	// The same delivery applies once the clock settles it.
	if settled, err := s.SettleThreadRequest("tok-armed", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("check the deploy"), AnswerKind: ThreadAnswerNote, SettledAt: 400,
	}); err != nil || !settled {
		t.Fatalf("settle the fired reminder: settled=%v err=%v", settled, err)
	}
	if err := s.QueueThreadWake("tok-armed", false, item); err != nil {
		t.Fatalf("QueueThreadWake after settling: %v", err)
	}
	if got := mustGetThreadRequest(t, s, "tok-armed"); got.DeliveredAt != 300 || got.DeliveredHow != ThreadWakeQueued {
		t.Errorf("delivery = %d/%s, want the queued wake", got.DeliveredAt, got.DeliveredHow)
	}
}

// A spawn or ask is accepted before the thread it will answer in exists: the
// token has to be durable first, or a retried call forks a second time. The
// target is named once, afterwards, and the naming is conditional so a retry
// that raced the first attempt cannot repoint the receipt.
func TestThreadReceiptTargetIsNamedAfterAcceptance(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-spawned")
	mustCreateThread(t, s, "t-other")

	stored, created, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-spawn", OwnerDeviceID: "device-1", Kind: ThreadRequestSpawn,
		CreatedAt: 100, UpdatedAt: 100,
	})
	if err != nil || !created {
		t.Fatalf("accept untargeted spawn: created=%v err=%v", created, err)
	}
	if stored.TargetThreadID != "" {
		t.Errorf("target = %q, want empty until the thread exists", stored.TargetThreadID)
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-ask", OwnerDeviceID: "device-1", Kind: ThreadRequestAsk,
		CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("accept untargeted ask: %v", err)
	}
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-send", OwnerDeviceID: "device-1", Kind: ThreadRequestSend,
		CreatedAt: 100, UpdatedAt: 100,
	}); err == nil {
		t.Error("a send must name the thread it is addressed to")
	}

	// Boot can still settle an acceptance whose thread was never created.
	open, err := s.ListOpenThreadRequestReceipts()
	if err != nil {
		t.Fatalf("list open receipts: %v", err)
	}
	if len(open) != 2 {
		t.Fatalf("open receipts = %+v, want both untargeted rows", open)
	}
	result, err := s.SettleLocalThreadRequest("tok-ask", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerNote, SettledAt: 200,
	}, ThreadRequestInterrupted)
	if err != nil || !result.Settled {
		t.Fatalf("settle untargeted receipt: settled=%v err=%v", result.Settled, err)
	}

	// An untargeted receipt belongs to no thread's list.
	listed, err := s.ListThreadRequestReceiptsForThread("t-spawned")
	if err != nil {
		t.Fatalf("list receipts for thread: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("receipts for an unnamed target = %+v", listed)
	}

	named, err := s.SetThreadReceiptTarget("tok-spawn", "t-spawned")
	if err != nil || !named {
		t.Fatalf("set target: named=%v err=%v", named, err)
	}
	named, err = s.SetThreadReceiptTarget("tok-spawn", "t-other")
	if err != nil {
		t.Fatalf("second set target: %v", err)
	}
	if named {
		t.Error("a receipt that already names its thread must not be repointed")
	}
	if _, err := s.SetThreadReceiptTarget("tok-spawn", ""); err == nil {
		t.Error("an empty thread id must be refused")
	}
	named, err = s.SetThreadReceiptTarget("tok-unknown", "t-spawned")
	if err != nil || named {
		t.Fatalf("unknown token: named=%v err=%v", named, err)
	}

	row, found, err := s.GetThreadRequestReceipt("tok-spawn")
	if err != nil || !found {
		t.Fatalf("get receipt: found=%v err=%v", found, err)
	}
	if row.TargetThreadID != "t-spawned" {
		t.Errorf("target = %q, want t-spawned", row.TargetThreadID)
	}
	listed, err = s.ListThreadRequestReceiptsForThread("t-spawned")
	if err != nil {
		t.Fatalf("list receipts after naming: %v", err)
	}
	if len(listed) != 1 || listed[0].Token != "tok-spawn" {
		t.Fatalf("receipts for t-spawned = %+v", listed)
	}

	// Once named, the receipt follows its thread.
	if err := s.DeleteThread("t-spawned"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	if _, found, err := s.GetThreadRequestReceipt("tok-spawn"); err != nil || found {
		t.Errorf("named receipt outlived its thread: found=%v err=%v", found, err)
	}
	if _, found, err := s.GetThreadRequestReceipt("tok-ask"); err != nil || !found {
		t.Errorf("an untargeted receipt was cascaded away: found=%v err=%v", found, err)
	}
}

// The source row learns where its request is being answered after dispatch,
// and learns it again when the target moves to another computer.
func TestSetThreadRequestTargetRecordsWhereItRuns(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-target", "t-caller", ThreadRequestAsk, 100)

	set, err := s.SetThreadRequestTarget("tok-target", "", "t-scratch-fork", "", "")
	if err != nil || !set {
		t.Fatalf("set target: set=%v err=%v", set, err)
	}
	row := mustGetThreadRequest(t, s, "tok-target")
	if row.TargetThreadID != "t-scratch-fork" || row.TargetComputerID != "" {
		t.Fatalf("target = %+v", row)
	}

	// The conversation moved: the same request is now answered elsewhere,
	// whatever state it is in.
	if _, err := s.AdvanceThreadRequestState("tok-target", ThreadRequestUnconfirmed, ThreadRequestRunning); err != nil {
		t.Fatalf("advance state: %v", err)
	}
	set, err = s.SetThreadRequestTarget("tok-target", "backend-2", "t-moved", "Release notes", "codex")
	if err != nil || !set {
		t.Fatalf("repoint target: set=%v err=%v", set, err)
	}
	row = mustGetThreadRequest(t, s, "tok-target")
	if row.TargetComputerID != "backend-2" || row.TargetThreadID != "t-moved" || row.State != ThreadRequestRunning {
		t.Fatalf("repointed row = %+v", row)
	}
	// The answering thread is on another computer, so its name has no local
	// row to read after a restart. The dispatch records it here instead.
	if row.TargetThreadTitle != "Release notes" || row.TargetProvider != "codex" {
		t.Fatalf("target title %q provider %q", row.TargetThreadTitle, row.TargetProvider)
	}
	// The provider alone changing is a change.
	if set, err := s.SetThreadRequestTarget("tok-target", "backend-2", "t-moved", "Release notes", "claude"); err != nil || !set {
		t.Fatalf("provider change: set=%v err=%v", set, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-target"); row.TargetProvider != "claude" {
		t.Fatalf("target provider = %q, want claude", row.TargetProvider)
	}
	// It is now a remote request, so the poller must see it.
	polls, err := s.DueThreadRequestPolls(nowMillis(), 10)
	if err != nil {
		t.Fatalf("due polls: %v", err)
	}
	if len(polls) != 1 || polls[0].Token != "tok-target" {
		t.Fatalf("due polls = %+v", polls)
	}

	set, err = s.SetThreadRequestTarget("tok-unknown", "backend-2", "t-moved", "", "")
	if err != nil || set {
		t.Fatalf("unknown token: set=%v err=%v", set, err)
	}
}

// QueueThreadWake is one transaction over two tables: the queue row that
// carries the answer into the caller's thread, and the delivery mark that
// keeps a second observation of the same settlement from queueing it again.
// Neither may land without the other.
func TestQueueThreadWakeWritesTheRowAndTheMarkTogether(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-wake", "t-caller", ThreadRequestSend, 100, "the answer")

	item := FlushQueueItem{ID: "queue:wake", ThreadID: "t-caller", SendID: "thread-wake:tok-wake", Message: "the answer", EnqueuedAt: 900}
	if err := s.QueueThreadWake("tok-wake", false, item); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	row := mustGetThreadRequest(t, s, "tok-wake")
	if row.DeliveredAt != 900 || row.DeliveredHow != ThreadWakeQueued {
		t.Fatalf("delivery = %q at %d, want a queued delivery", row.DeliveredHow, row.DeliveredAt)
	}
	rows, err := s.ListFlushQueueItems("t-caller")
	if err != nil {
		t.Fatalf("ListFlushQueueItems: %v", err)
	}
	if len(rows) != 1 || rows[0].SendID != item.SendID {
		t.Fatalf("queued rows = %+v, want the wake", rows)
	}

	// A second wake for the same answer is refused, and refusing it leaves
	// no second message behind.
	second := item
	second.ID = "queue:wake-2"
	if err := s.QueueThreadWake("tok-wake", false, second); err == nil {
		t.Fatal("a second wake for the same answer was accepted")
	}
	if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 1 {
		t.Fatalf("the refused wake left a row behind: %+v err=%v", rows, err)
	}

	// The late reply is a delivery of its own and is not blocked by the
	// first one. It is owed only once the late text exists.
	late := item
	late.ID, late.SendID = "queue:wake-too-early", "thread-wake-late:tok-wake"
	if err := s.QueueThreadWake("tok-wake", true, late); err == nil {
		t.Fatal("a late wake was queued before there was a late reply")
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-wake", []byte("on reflection"), 940, nil); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	late = item
	late.ID, late.SendID, late.EnqueuedAt = "queue:wake-late", "thread-wake-late:tok-wake", 950
	if err := s.QueueThreadWake("tok-wake", true, late); err != nil {
		t.Fatalf("QueueThreadWake(late): %v", err)
	}
	if got := mustGetThreadRequest(t, s, "tok-wake"); got.LateDeliveredAt != 950 {
		t.Fatalf("late delivery = %d, want the late mark", got.LateDeliveredAt)
	}
}

// WithdrawThreadWake is QueueThreadWake undone for an archive: the queue row,
// its delivery mark and the request's notify go in one transaction, so a
// retry never sees the answer owed and armed. A wake clears only its own
// answer's mark and route, the late reply's or the first answer's, and a
// row already gone changes nothing.
func TestWithdrawThreadWakeOwesTheAnswerAgainDisarmed(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-back", "t-caller", ThreadRequestSend, 100, "the answer")
	if _, err := s.SetThreadRequestNotify("tok-back", true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}
	if err := s.QueueThreadWake("tok-back", false, FlushQueueItem{
		ID: "queue:back", ThreadID: "t-caller", SendID: "thread-wake:tok-back", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if err := s.WithdrawThreadWake("tok-back", false, "queue:back", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake: %v", err)
	}
	row := mustGetThreadRequest(t, s, "tok-back")
	if row.DeliveredAt != 0 || row.DeliveredHow != "" || row.Notify {
		t.Fatalf("withdrawn: delivered %q at %d, notify %v; want undelivered and disarmed", row.DeliveredHow, row.DeliveredAt, row.Notify)
	}
	if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 0 {
		t.Fatalf("the withdrawn wake left %+v (err %v)", rows, err)
	}

	// The late reply's wake: the first answer was delivered inline.
	if _, err := s.MarkThreadRequestDelivered("tok-back", ThreadWakeInline, 920, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-back", []byte("on reflection"), 940, nil); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	if _, err := s.SetThreadRequestNotify("tok-back", true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}
	if err := s.QueueThreadWake("tok-back", true, FlushQueueItem{
		ID: "queue:back-late", ThreadID: "t-caller", SendID: "thread-wake-late:tok-back", Message: "on reflection", EnqueuedAt: 950,
	}); err != nil {
		t.Fatalf("QueueThreadWake(late): %v", err)
	}
	if err := s.WithdrawThreadWake("tok-back", true, "queue:back-late", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake(late): %v", err)
	}
	row = mustGetThreadRequest(t, s, "tok-back")
	if row.LateDeliveredAt != 0 || row.LateDeliveredHow != "" || row.Notify || row.DeliveredAt != 920 || row.DeliveredHow != ThreadWakeInline {
		t.Fatalf("late withdrawn: late delivery %q at %d, notify %v, first delivery %q at %d; want the late wake owed and disarmed, the first delivery kept",
			row.LateDeliveredHow, row.LateDeliveredAt, row.Notify, row.DeliveredHow, row.DeliveredAt)
	}

	// Gone already (dispatched or withdrawn): nothing changes.
	if _, err := s.SetThreadRequestNotify("tok-back", true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}
	if err := s.WithdrawThreadWake("tok-back", false, "queue:absent", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake(absent): %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-back"); !row.Notify || row.DeliveredAt != 920 {
		t.Fatalf("an absent row changed the request: notify %v, delivered at %d", row.Notify, row.DeliveredAt)
	}

	// The first answer's wake, withdrawn after the late reply was read
	// inline: the late reply's delivery stands.
	seedSettledThreadRequest(t, s, "tok-first", "t-caller", ThreadRequestSend, 100, "the answer")
	if _, err := s.SetThreadRequestNotify("tok-first", true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}
	if err := s.QueueThreadWake("tok-first", false, FlushQueueItem{
		ID: "queue:first", ThreadID: "t-caller", SendID: "thread-wake:tok-first", Message: "the answer", EnqueuedAt: 960,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-first", []byte("on reflection"), 970, nil); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	if _, err := s.MarkThreadRequestDelivered("tok-first", ThreadWakeInline, 980, true); err != nil {
		t.Fatalf("MarkThreadRequestDelivered(late): %v", err)
	}
	if err := s.WithdrawThreadWake("tok-first", false, "queue:first", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake(first): %v", err)
	}
	row = mustGetThreadRequest(t, s, "tok-first")
	if row.DeliveredAt != 0 || row.DeliveredHow != "" || row.LateDeliveredAt != 980 || row.LateDeliveredHow != ThreadWakeInline {
		t.Fatalf("first withdrawn: first delivery %q at %d, late delivery %q at %d; want the first owed, the late delivery kept",
			row.DeliveredHow, row.DeliveredAt, row.LateDeliveredHow, row.LateDeliveredAt)
	}
}

// A wake the provider was handed and never confirmed has no queue row left,
// and goes back all the same while its request still reads queued: owed and
// disarmed. A request delivered some other way is left alone.
func TestWithdrawThreadWakeReturnsAnUnconfirmedWakeWithNoRow(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-sent", "t-caller", ThreadRequestSend, 100, "the answer")
	if err := s.QueueThreadWake("tok-sent", false, FlushQueueItem{
		ID: "queue:sent", ThreadID: "t-caller", SendID: "thread-wake:tok-sent", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if _, err := s.SetThreadRequestNotify("tok-sent", true); err != nil {
		t.Fatalf("SetThreadRequestNotify: %v", err)
	}
	// The dispatch retired the row at the provider write.
	if err := s.DeleteDispatchedFlushQueueItem("queue:sent"); err != nil {
		t.Fatalf("DeleteDispatchedFlushQueueItem: %v", err)
	}
	if err := s.WithdrawThreadWake("tok-sent", false, "queue:sent", WithdrawUnconfirmed); err != nil {
		t.Fatalf("WithdrawThreadWake: %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-sent"); row.DeliveredAt != 0 || row.DeliveredHow != "" || row.Notify {
		t.Fatalf("unconfirmed: delivered %q at %d, notify %v; want undelivered and disarmed", row.DeliveredHow, row.DeliveredAt, row.Notify)
	}

	seedSettledThreadRequest(t, s, "tok-inline", "t-caller", ThreadRequestSend, 100, "the answer")
	if _, err := s.MarkThreadRequestDelivered("tok-inline", ThreadWakeInline, 920, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}
	if err := s.WithdrawThreadWake("tok-inline", false, "", WithdrawUnconfirmed); err != nil {
		t.Fatalf("WithdrawThreadWake(inline): %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-inline"); row.DeliveredAt != 920 || row.DeliveredHow != ThreadWakeInline {
		t.Fatalf("an inline delivery was taken back: delivered %q at %d", row.DeliveredHow, row.DeliveredAt)
	}
}

func TestNextThreadRequestWorkReportsEachPassApart(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")

	schedule, err := s.NextThreadRequestWork()
	if err != nil {
		t.Fatalf("NextThreadRequestWork: %v", err)
	}
	if schedule.Reminder.Set || schedule.Poll.Set || schedule.Wake.Set {
		t.Fatalf("empty ledger schedule = %+v, want nothing set", schedule)
	}

	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-remind", CallerThreadID: "t-caller", Kind: ThreadRequestRemind,
		State: ThreadRequestAccepted, DueAt: 5000,
	}); err != nil {
		t.Fatalf("insert reminder: %v", err)
	}
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-poll", CallerThreadID: "t-caller", Kind: ThreadRequestSend,
		State: ThreadRequestUnconfirmed, TargetComputerID: "backend-2", TargetThreadID: "t-far",
	}); err != nil {
		t.Fatalf("insert remote request: %v", err)
	}
	if err := s.RescheduleThreadRequest("tok-poll", 7000, 0, ""); err != nil {
		t.Fatalf("reschedule: %v", err)
	}
	seedThreadRequest(t, s, "tok-wake", "t-caller", ThreadRequestSend, 100)
	if _, err := s.SetThreadRequestNotify("tok-wake", true); err != nil {
		t.Fatalf("arm notify: %v", err)
	}
	if _, err := s.SettleThreadRequest("tok-wake", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
	}); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := s.ScheduleThreadRequestWake("tok-wake", 9000, 1); err != nil {
		t.Fatalf("schedule wake: %v", err)
	}

	schedule, err = s.NextThreadRequestWork()
	if err != nil {
		t.Fatalf("NextThreadRequestWork: %v", err)
	}
	want := ThreadRequestSchedule{
		Reminder: ScheduledMoment{At: 5000, Set: true},
		Poll:     ScheduledMoment{At: 7000, Set: true},
		Wake:     ScheduledMoment{At: 9000, Set: true},
	}
	if schedule != want {
		t.Fatalf("schedule = %+v, want %+v", schedule, want)
	}
	if _, err := s.RetireThreadRequestPoll("tok-poll"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if schedule, err = s.NextThreadRequestWork(); err != nil || schedule.Poll.Set {
		t.Fatalf("after retirement poll = %+v err=%v, want nothing to poll", schedule.Poll, err)
	}
}

// A queued answer is delivered in two phases: the queue write marks it, and
// only the provider's echo confirms it. Until then it is listed for the
// boot sweep and an unconfirmed give-back takes it back; once confirmed,
// neither does. An inline delivery is confirmed by its mark, so a late
// reply queued after one lists only the late slot, and a give-back clears
// the confirmation with the mark, so a requeue starts unconfirmed.
func TestThreadWakeDeliveryIsConfirmedByItsEcho(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	unconfirmed := func() []string {
		t.Helper()
		rows, err := s.UnconfirmedThreadWakes()
		if err != nil {
			t.Fatalf("UnconfirmedThreadWakes: %v", err)
		}
		var got []string
		for _, row := range rows {
			if row.DeliveredAt != 0 && row.ConfirmedAt == 0 {
				got = append(got, row.Token)
			}
			if row.LateDeliveredAt != 0 && row.LateConfirmedAt == 0 {
				got = append(got, row.Token+":late")
			}
		}
		return got
	}
	queue := func(token string, late bool, id string) {
		t.Helper()
		if err := s.QueueThreadWake(token, late, FlushQueueItem{
			ID: id, ThreadID: "t-caller", SendID: id, Message: "the answer", EnqueuedAt: 900,
		}); err != nil {
			t.Fatalf("QueueThreadWake %s: %v", id, err)
		}
		if err := s.DeleteDispatchedFlushQueueItem(id); err != nil {
			t.Fatalf("DeleteDispatchedFlushQueueItem %s: %v", id, err)
		}
	}

	seedSettledThreadRequest(t, s, "tok-queued", "t-caller", ThreadRequestSend, 100, "the answer")
	seedSettledThreadRequest(t, s, "tok-inline", "t-caller", ThreadRequestSend, 100, "the answer")
	queue("tok-queued", false, "queue:queued")
	if _, err := s.MarkThreadRequestDelivered("tok-inline", ThreadWakeInline, 920, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}
	if got := unconfirmed(); !slices.Equal(got, []string{"tok-queued"}) {
		t.Fatalf("unconfirmed after the queue write = %q, want [tok-queued]", got)
	}
	if row := mustGetThreadRequest(t, s, "tok-inline"); row.ConfirmedAt != 920 {
		t.Fatalf("inline delivery confirmed at %d, want 920", row.ConfirmedAt)
	}

	if confirmed, err := s.ConfirmThreadWake("tok-queued", false, "", 950); err != nil || !confirmed {
		t.Fatalf("ConfirmThreadWake = %v, %v; want true", confirmed, err)
	}
	if confirmed, err := s.ConfirmThreadWake("tok-queued", false, "", 960); err != nil || confirmed {
		t.Fatalf("second ConfirmThreadWake = %v, %v; want false", confirmed, err)
	}
	if got := unconfirmed(); len(got) != 0 {
		t.Fatalf("unconfirmed after the echo = %q, want none", got)
	}
	if err := s.WithdrawThreadWake("tok-queued", false, "queue:queued", WithdrawUnconfirmed); err != nil {
		t.Fatalf("WithdrawThreadWake: %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-queued"); row.DeliveredAt == 0 || row.ConfirmedAt != 950 {
		t.Fatalf("a confirmed answer was given back: delivered at %d, confirmed at %d", row.DeliveredAt, row.ConfirmedAt)
	}

	// A late reply after an inline answer: only the late slot is owed a
	// confirmation.
	if _, err := s.StoreThreadRequestLateReply("tok-inline", []byte("more"), 1000, nil); err != nil {
		t.Fatalf("StoreThreadRequestLateReply: %v", err)
	}
	queue("tok-inline", true, "queue:late")
	if got := unconfirmed(); !slices.Equal(got, []string{"tok-inline:late"}) {
		t.Fatalf("unconfirmed after the late queue write = %q, want [tok-inline:late]", got)
	}
	if err := s.WithdrawThreadWake("tok-inline", true, "queue:late", WithdrawUnconfirmed); err != nil {
		t.Fatalf("WithdrawThreadWake(late): %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-inline"); row.LateDeliveredAt != 0 || row.DeliveredAt != 920 || row.ConfirmedAt != 920 {
		t.Fatalf("late give-back: late delivered %d, delivered %d confirmed %d", row.LateDeliveredAt, row.DeliveredAt, row.ConfirmedAt)
	}
	if got := unconfirmed(); len(got) != 0 {
		t.Fatalf("unconfirmed after the late give-back = %q, want none", got)
	}

	// Given back and queued again, an answer is unconfirmed again.
	seedSettledThreadRequest(t, s, "tok-again", "t-caller", ThreadRequestSend, 100, "the answer")
	if err := s.QueueThreadWake("tok-again", false, FlushQueueItem{
		ID: "queue:again", ThreadID: "t-caller", SendID: "queue:again", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if err := s.WithdrawThreadWake("tok-again", false, "queue:again", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake: %v", err)
	}
	queue("tok-again", false, "queue:again2")
	if got := unconfirmed(); !slices.Equal(got, []string{"tok-again"}) {
		t.Fatalf("unconfirmed after a requeue = %q, want [tok-again]", got)
	}
}

// A lineage-only row keeps the delivery marks the retention sweep left
// (clearLineagePayloadSQL). One queued and never confirmed, answer and
// late reply both, is not listed at boot, not given back with or without
// its queue row, and not confirmed: a cleared delivered_at would read as a
// wake still owed.
func TestALineageOnlyRowIsNeverGivenBackOrConfirmed(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok", "t-caller", ThreadRequestSpawn, 100, "the answer")
	if stored, err := s.StoreThreadRequestLateReply("tok", []byte("one more thing"), 300, nil); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	for _, late := range []bool{false, true} {
		if marked, err := s.MarkThreadRequestDelivered("tok", ThreadWakeQueued, 900, late); err != nil || !marked {
			t.Fatalf("MarkThreadRequestDelivered(late=%v): marked=%v err=%v", late, marked, err)
		}
	}
	unconfirmed := func() []string {
		t.Helper()
		rows, err := s.UnconfirmedThreadWakes()
		if err != nil {
			t.Fatalf("UnconfirmedThreadWakes: %v", err)
		}
		var tokens []string
		for _, row := range rows {
			tokens = append(tokens, row.Token)
		}
		return tokens
	}
	if got := unconfirmed(); !slices.Equal(got, []string{"tok"}) {
		t.Fatalf("unconfirmed before the sweep = %q, want [tok]", got)
	}
	if _, err := s.db.Exec(clearLineagePayloadSQL+` AND token = ?`, "tok"); err != nil {
		t.Fatalf("mark the row lineage-only: %v", err)
	}
	if err := insertFlushQueueItem(s.db, FlushQueueItem{
		ID: "queue:tok", ThreadID: "t-caller", SendID: "thread-wake:tok", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("insert queue row: %v", err)
	}
	before := rawThreadRequest(t, s, "tok")

	if got := unconfirmed(); len(got) != 0 {
		t.Errorf("unconfirmed at boot = %q, want the lineage-only row left out", got)
	}
	for _, late := range []bool{false, true} {
		if err := s.WithdrawThreadWake("tok", late, "", WithdrawUnconfirmed); err != nil {
			t.Fatalf("WithdrawThreadWake(late=%v, unconfirmed): %v", late, err)
		}
		if confirmed, err := s.ConfirmThreadWake("tok", late, "", 1000); err != nil || confirmed {
			t.Errorf("ConfirmThreadWake(late=%v) on the lineage-only row: confirmed=%v err=%v", late, confirmed, err)
		}
	}
	if err := s.WithdrawThreadWake("tok", false, "queue:tok", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake with its row: %v", err)
	}
	after := rawThreadRequest(t, s, "tok")
	if after.DeliveredAt != before.DeliveredAt || after.DeliveredHow != ThreadWakeQueued || after.LateDeliveredAt != before.LateDeliveredAt ||
		after.ConfirmedAt != 0 || after.LateConfirmedAt != 0 || after.Notify || !after.LineageOnly {
		t.Errorf("the lineage-only row changed:\n got %+v\nwant %+v", after, before)
	}
	if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 0 {
		t.Errorf("queue rows after the withdraw = %+v, %v; want the row taken out", rows, err)
	}
}

// A wake whose queue row is still there never reached the provider, so it
// goes back whatever its confirmation reads: a row queued before v149 was
// recorded as confirmed by the migration's backfill.
func TestWithdrawThreadWakeWithItsRowIgnoresTheConfirmation(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok", "t-caller", ThreadRequestSend, 100, "the answer")
	if err := s.QueueThreadWake("tok", false, FlushQueueItem{
		ID: "queue:tok", ThreadID: "t-caller", SendID: "thread-wake:tok", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if _, err := s.ConfirmThreadWake("tok", false, "", 950); err != nil {
		t.Fatalf("ConfirmThreadWake: %v", err)
	}
	if err := s.WithdrawThreadWake("tok", false, "queue:tok", WithdrawQueued); err != nil {
		t.Fatalf("WithdrawThreadWake: %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok"); row.DeliveredAt != 0 || row.ConfirmedAt != 0 {
		t.Fatalf("a queued wake was kept: delivered at %d, confirmed at %d", row.DeliveredAt, row.ConfirmedAt)
	}
}
