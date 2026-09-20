package store

import (
	"bytes"
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

	stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("second thoughts"), 300)
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

	stored, err = s.StoreThreadRequestLateReply("tok-late", []byte("third"), 400)
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
	stored, err = s.StoreThreadRequestLateReply("tok-open", []byte("early"), 500)
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
	if _, err := s.SettleThreadRequestReceipt("tok-answered", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("the answer"), AnswerKind: ThreadAnswerFinal,
	}); err != nil {
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

	running, err := s.MarkThreadReceiptRunning("tok-r", "item-1", "turn-1")
	if err != nil || !running {
		t.Fatalf("mark running: running=%v err=%v", running, err)
	}
	running, err = s.MarkThreadReceiptRunning("tok-r", "item-2", "turn-2")
	if err != nil {
		t.Fatalf("repeat mark running: %v", err)
	}
	if running {
		t.Error("a running receipt must not be re-bound to another turn")
	}
	if _, err := s.MarkThreadReceiptRunning("tok-r", "item-3", ""); err == nil {
		t.Error("a receipt cannot run without a turn")
	}

	open, err := s.ListOpenThreadRequestReceipts()
	if err != nil {
		t.Fatalf("list open receipts: %v", err)
	}
	if len(open) != 1 || open[0].Token != "tok-r" {
		t.Fatalf("open receipts = %+v", open)
	}

	settled, err := s.SettleThreadRequestReceipt("tok-r", []string{ThreadReceiptRunning}, ThreadRequestSettlement{
		State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: 500,
	})
	if err != nil || !settled {
		t.Fatalf("settle receipt: settled=%v err=%v", settled, err)
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
		if _, err := s.SettleThreadRequestReceipt(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("answer"), AnswerKind: ThreadAnswerFinal,
			SettledAt: 1000, ExpiresAt: 2000,
		}); err != nil {
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

	// A late reply is a new uncollected revision with its own hold.
	if _, err := s.StoreThreadReceiptLateReply("tok-collected", []byte("more"), 3000); err != nil {
		t.Fatalf("store receipt late reply: %v", err)
	}
	dropped, err = s.ExpireThreadRequestAnswers(3000 + threadAnswerHoldMillis)
	if err != nil {
		t.Fatalf("late expiry sweep: %v", err)
	}
	if dropped != 1 {
		t.Errorf("late reply sweep dropped %d, want 1", dropped)
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
		applied, err := s.SettleThreadRequestReceipt(token, ThreadReceiptOpenStates(), ThreadRequestSettlement{
			State: ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal, SettledAt: at,
		})
		if err != nil || !applied {
			t.Fatalf("settle receipt %s: applied=%v err=%v", token, applied, err)
		}
	}

	swept, err := s.DeleteThreadRequestsBefore(1000)
	if err != nil {
		t.Fatalf("retention sweep: %v", err)
	}
	if swept != 2 {
		t.Fatalf("swept = %d, want one settled row from each table", swept)
	}
	for _, token := range []string{"tok-settled-recent", "tok-open-old", "tok-remind-later"} {
		if _, found, err := s.GetThreadRequest(token); err != nil || !found {
			t.Errorf("%s was swept: found=%v err=%v", token, found, err)
		}
	}
	if _, found, err := s.GetThreadRequest("tok-settled-old"); err != nil || found {
		t.Errorf("a request settled before the floor survived: found=%v err=%v", found, err)
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
	settled, err := s.SettleThreadRequestReceipt("tok-ask", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptInterrupted, AnswerKind: ThreadAnswerNote, SettledAt: 200,
	})
	if err != nil || !settled {
		t.Fatalf("settle untargeted receipt: settled=%v err=%v", settled, err)
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

	set, err := s.SetThreadRequestTarget("tok-target", "", "t-scratch-fork")
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
	set, err = s.SetThreadRequestTarget("tok-target", "backend-2", "t-moved")
	if err != nil || !set {
		t.Fatalf("repoint target: set=%v err=%v", set, err)
	}
	row = mustGetThreadRequest(t, s, "tok-target")
	if row.TargetComputerID != "backend-2" || row.TargetThreadID != "t-moved" || row.State != ThreadRequestRunning {
		t.Fatalf("repointed row = %+v", row)
	}
	// It is now a remote request, so the poller must see it.
	polls, err := s.DueThreadRequestPolls(nowMillis(), 10)
	if err != nil {
		t.Fatalf("due polls: %v", err)
	}
	if len(polls) != 1 || polls[0].Token != "tok-target" {
		t.Fatalf("due polls = %+v", polls)
	}

	set, err = s.SetThreadRequestTarget("tok-unknown", "backend-2", "t-moved")
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
	if stored, err := s.StoreThreadRequestLateReply("tok-wake", []byte("on reflection"), 940); err != nil || !stored {
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

// A queued wake the previous process never delivered is restored into the
// composer at boot. The row is already marked delivered by then, so the
// correction applies to a delivered row rather than an undelivered one.
func TestMarkThreadRequestDeliveredAsDraftCorrectsAQueuedWake(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-draft", "t-caller", ThreadRequestSend, 100, "the answer")

	// Nothing to correct before a wake was queued.
	if corrected, err := s.MarkThreadRequestDeliveredAsDraft("tok-draft", false); err != nil || corrected {
		t.Fatalf("correction applied to an undelivered request: corrected=%v err=%v", corrected, err)
	}
	if err := s.QueueThreadWake("tok-draft", false, FlushQueueItem{
		ID: "queue:draft", ThreadID: "t-caller", SendID: "thread-wake:tok-draft", Message: "the answer", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	corrected, err := s.MarkThreadRequestDeliveredAsDraft("tok-draft", false)
	if err != nil || !corrected {
		t.Fatalf("correction did not apply: corrected=%v err=%v", corrected, err)
	}
	row := mustGetThreadRequest(t, s, "tok-draft")
	if row.DeliveredHow != ThreadWakeDraft {
		t.Fatalf("delivery = %q, want the draft correction", row.DeliveredHow)
	}
	if row.DeliveredAt != 900 {
		t.Errorf("the correction moved the delivery time to %d", row.DeliveredAt)
	}
	// An inline delivery is not a queued wake and is never corrected.
	seedSettledThreadRequest(t, s, "tok-inline", "t-caller", ThreadRequestSend, 100, "the answer")
	if _, err := s.MarkThreadRequestDelivered("tok-inline", ThreadWakeInline, 800, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}
	if corrected, err := s.MarkThreadRequestDeliveredAsDraft("tok-inline", false); err != nil || corrected {
		t.Fatalf("an inline delivery was rewritten as a draft: corrected=%v err=%v", corrected, err)
	}
}
