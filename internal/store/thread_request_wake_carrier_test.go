package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// undeliveredTokens is the retry pass's due set, as tokens.
func undeliveredTokens(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.UndeliveredThreadRequestWakes(1<<62, 32)
	if err != nil {
		t.Fatalf("UndeliveredThreadRequestWakes: %v", err)
	}
	tokens := make([]string, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, row.Token)
	}
	return tokens
}

// A settlement that arms the wake puts a request nobody asked to be told
// about into the retry pass's set. One that does not leaves `notify` as the
// caller set it.
func TestSettlementArmsTheWakeOnlyWhenAsked(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-reply", "t-caller", ThreadRequestSend, 100)
	seedThreadRequest(t, s, "tok-final", "t-caller", ThreadRequestSend, 100)

	if settled, err := s.SettleThreadRequest("tok-reply", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestReplied, Answer: []byte("done"), AnswerKind: ThreadAnswerReply, ArmWake: true,
	}); err != nil || !settled {
		t.Fatalf("settle the reply: settled=%v err=%v", settled, err)
	}
	if settled, err := s.SettleThreadRequest("tok-final", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestFinished, Answer: []byte("started"), AnswerKind: ThreadAnswerFinal,
	}); err != nil || !settled {
		t.Fatalf("settle the final message: settled=%v err=%v", settled, err)
	}

	if row := mustGetThreadRequest(t, s, "tok-reply"); !row.Notify {
		t.Error("a settlement that arms the wake left notify off")
	}
	if row := mustGetThreadRequest(t, s, "tok-final"); row.Notify {
		t.Error("a settlement that does not arm the wake turned notify on")
	}
	if got := undeliveredTokens(t, s); len(got) != 1 || got[0] != "tok-reply" {
		t.Fatalf("owed wakes = %v, want the armed reply alone", got)
	}
}

// A late reply that arms the wake on a request whose first answer was never
// delivered owes one message, the reply: it supersedes the first answer, as
// it does in thread_status. Once the reply is delivered nothing is owed.
func TestLateReplySupersedesAnUndeliveredFirstAnswer(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-late", "t-caller", ThreadRequestSend, 100, "started the tests")

	if stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("all green"), 300, true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	row := mustGetThreadRequest(t, s, "tok-late")
	if !row.Notify {
		t.Fatal("the late reply did not arm the wake")
	}
	if row.WakeOwed(false) || !row.WakeOwed(true) {
		t.Fatalf("owed = first %v, late %v; want the late reply alone", row.WakeOwed(false), row.WakeOwed(true))
	}
	if got := undeliveredTokens(t, s); len(got) != 1 || got[0] != "tok-late" {
		t.Fatalf("owed wakes = %v, want the row once for its late reply", got)
	}

	item := FlushQueueItem{ID: "queue:late", ThreadID: "t-caller", SendID: "thread-wake-late:tok-late", Message: "all green", EnqueuedAt: 400}
	if err := s.QueueThreadWake("tok-late", nil, true, item); err != nil {
		t.Fatalf("QueueThreadWake(late): %v", err)
	}
	if got := undeliveredTokens(t, s); len(got) != 0 {
		t.Fatalf("owed wakes after the reply was delivered = %v, want none: the first answer is superseded", got)
	}
	schedule, err := s.NextThreadRequestWork()
	if err != nil {
		t.Fatalf("NextThreadRequestWork: %v", err)
	}
	if schedule.Wake.Set {
		t.Fatalf("the sweep still has a wake scheduled: %+v", schedule.Wake)
	}
}

// A reply to an archived caller is stored and arms nothing, in the statement
// that stores it: an archive that lands after the app read the thread cannot
// be followed by a wake that brings the thread back.
func TestAReplyNeverArmsAnArchivedCaller(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedThreadRequest(t, s, "tok-reply", "t-caller", ThreadRequestSend, 100)
	seedSettledThreadRequest(t, s, "tok-late", "t-caller", ThreadRequestSend, 100, "started")
	if _, _, err := s.ArchiveThread("t-caller"); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	if settled, err := s.SettleThreadRequest("tok-reply", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestReplied, Answer: []byte("done"), AnswerKind: ThreadAnswerReply, ArmWake: true,
	}); err != nil || !settled {
		t.Fatalf("settle the reply: settled=%v err=%v", settled, err)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("all green"), 300, true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}

	if row := mustGetThreadRequest(t, s, "tok-reply"); row.Notify || string(row.Answer) != "done" {
		t.Errorf("reply = notify %v, answer %q; want it stored and unarmed", row.Notify, row.Answer)
	}
	if row := mustGetThreadRequest(t, s, "tok-late"); row.Notify || string(row.LateReply) != "all green" {
		t.Errorf("late reply = notify %v, reply %q; want it stored and unarmed", row.Notify, row.LateReply)
	}
	if got := undeliveredTokens(t, s); len(got) != 0 {
		t.Fatalf("owed wakes = %v, want none for an archived caller", got)
	}
}

// A late reply that does not arm the wake changes nothing about who is told.
func TestLateReplyWithoutArmingLeavesNotifyAlone(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedSettledThreadRequest(t, s, "tok-late", "t-caller", ThreadRequestSend, 100, "started")
	if stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("all green"), 300, false); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-late"); row.Notify {
		t.Error("an unarmed late reply turned notify on")
	}
}

// A refusal the caller's own call returns is settled and delivered in one
// transaction, so a request made with notify never appears settled and
// undelivered to the retry pass.
func TestSettleThreadRequestDeliveredOwesNoWake(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	if err := s.InsertThreadRequest(ThreadRequest{
		Token: "tok-refused", CallerThreadID: "t-caller", Kind: ThreadRequestSpawn,
		State: ThreadRequestUnconfirmed, Notify: true, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}

	settled, err := s.SettleThreadRequestDelivered("tok-refused", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestRefused, Answer: []byte("git refused the branch"), AnswerKind: ThreadAnswerError,
	})
	if err != nil || !settled {
		t.Fatalf("SettleThreadRequestDelivered: settled=%v err=%v", settled, err)
	}
	row := mustGetThreadRequest(t, s, "tok-refused")
	if row.State != ThreadRequestRefused || row.DeliveredAt == 0 || row.DeliveredHow != ThreadWakeInline {
		t.Fatalf("request = %q delivered %q at %d, want refused and delivered inline", row.State, row.DeliveredHow, row.DeliveredAt)
	}
	if got := undeliveredTokens(t, s); len(got) != 0 {
		t.Fatalf("owed wakes = %v, want none", got)
	}

	// A state that leaves the request open is not a delivery, and nothing
	// is written for it.
	seedThreadRequest(t, s, "tok-open", "t-caller", ThreadRequestSend, 100)
	if settled, err := s.SettleThreadRequestDelivered("tok-open", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestAccepted, Answer: []byte("not an end"), AnswerKind: ThreadAnswerError,
	}); err == nil || settled {
		t.Fatalf("an open state was settled as delivered: settled=%v err=%v", settled, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-open"); row.SettledAt != 0 || row.DeliveredAt != 0 || len(row.Answer) != 0 {
		t.Fatalf("the refused settlement wrote the row: %+v", row)
	}

	// A row another path settled first is left exactly as that path left it.
	seedSettledThreadRequest(t, s, "tok-settled", "t-caller", ThreadRequestSend, 100, "the answer")
	settled, err = s.SettleThreadRequestDelivered("tok-settled", ThreadRequestOpenStates(), ThreadRequestSettlement{
		State: ThreadRequestRefused, Answer: []byte("too late"), AnswerKind: ThreadAnswerError,
	})
	if err != nil || settled {
		t.Fatalf("a settled row was settled again: settled=%v err=%v", settled, err)
	}
	if row := mustGetThreadRequest(t, s, "tok-settled"); row.DeliveredAt != 0 || string(row.Answer) != "the answer" {
		t.Fatalf("the lost race changed the row: %+v", row)
	}
}

// One message can answer several requests. The carrier and every rider are
// marked delivered with the one queue row, and each rider records the
// carrier.
func TestQueueThreadWakeDeliversItsRidersWithTheCarrier(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	for _, token := range []string{"tok-carrier", "tok-rider", "tok-read"} {
		seedOwedThreadWake(t, s, token, "t-caller", ThreadRequestSend, 100, "the turn ended")
	}
	if _, err := s.MarkThreadRequestDelivered("tok-read", ThreadWakeInline, 800, false); err != nil {
		t.Fatalf("MarkThreadRequestDelivered: %v", err)
	}

	item := FlushQueueItem{ID: "queue:carrier", ThreadID: "t-caller", SendID: "thread-wake:tok-carrier", Message: "the turn ended", EnqueuedAt: 900}
	if err := s.QueueThreadWake("tok-carrier", []string{"tok-rider"}, false, item); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}

	carrier := mustGetThreadRequest(t, s, "tok-carrier")
	if carrier.DeliveredAt != 900 || carrier.DeliveredHow != ThreadWakeQueued || carrier.DeliveredWith != "" {
		t.Errorf("carrier = %q at %d with %q", carrier.DeliveredHow, carrier.DeliveredAt, carrier.DeliveredWith)
	}
	rider := mustGetThreadRequest(t, s, "tok-rider")
	if rider.DeliveredAt != 900 || rider.DeliveredHow != ThreadWakeQueued || rider.DeliveredWith != "tok-carrier" {
		t.Errorf("rider = %q at %d with %q, want it delivered with the carrier", rider.DeliveredHow, rider.DeliveredAt, rider.DeliveredWith)
	}
	if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 1 {
		t.Fatalf("queued rows = %+v err=%v, want the one message", rows, err)
	}

	// A restore to the composer follows the message: the carrier and the
	// rider it delivered are in the composer now, and the inline delivery is
	// not.
	if err := restoreQueueToDraft(t, s, "t-caller", []string{"queue:carrier"}, []string{item.SendID}); err != nil {
		t.Fatalf("RestoreFlushQueueToDraft: %v", err)
	}
	for token, want := range map[string]string{
		"tok-carrier": ThreadWakeDraft, "tok-rider": ThreadWakeDraft, "tok-read": ThreadWakeInline,
	} {
		if got := mustGetThreadRequest(t, s, token).DeliveredHow; got != want {
			t.Errorf("%s delivered %q after the restore, want %q", token, got, want)
		}
	}
}

// A wake handed to a session that is stopped before confirming it is taken
// back with its row, and every request the message carried is retracted in
// the same transaction; a failed retire retracts nothing.
func TestRetireDroppedUserRowRetractsTheWakeItCarried(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	for _, token := range []string{"tok-carrier", "tok-rider", "tok-other"} {
		seedOwedThreadWake(t, s, token, "t-caller", ThreadRequestSend, 100, "the turn ended")
	}
	if err := s.QueueThreadWake("tok-carrier", []string{"tok-rider"}, false, FlushQueueItem{
		ID: "queue:carrier", ThreadID: "t-caller", SendID: "thread-wake:tok-carrier", Message: "the turn ended", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if err := s.QueueThreadWake("tok-other", nil, false, FlushQueueItem{
		ID: "queue:other", ThreadID: "t-caller", SendID: "thread-wake:tok-other", Message: "the turn ended", EnqueuedAt: 900,
	}); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	// The dispatch wrote the carrier's message and dropped its queue row.
	if err := s.DeleteFlushQueueItem("queue:carrier"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendItem(Item{
		ID: "user:1:flush:1", ThreadID: "t-caller", TurnIndex: 1, Kind: "user_text", Role: "user", Status: "completed",
		Summary: "the turn ended", Meta: `{"sendId":"thread-wake:tok-carrier"}`, CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec(`CREATE TRIGGER refuse_row_delete BEFORE DELETE ON items WHEN OLD.id = 'user:1:flush:1'
		BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireDroppedUserRow("t-caller", "user:1:flush:1", []string{"thread-wake:tok-carrier"}); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatal("the retire succeeded past the injected failure")
	}
	if got := mustGetThreadRequest(t, s, "tok-carrier").DeliveredHow; got != ThreadWakeQueued {
		t.Fatalf("carrier delivered %q after a failed retire, want queued", got)
	}
	if _, err := s.db.Exec(`DROP TRIGGER refuse_row_delete`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RetireDroppedUserRow("t-caller", "user:1:flush:1", []string{"thread-wake:tok-carrier"}); err != nil {
		t.Fatalf("RetireDroppedUserRow: %v", err)
	}
	for _, token := range []string{"tok-carrier", "tok-rider"} {
		row := mustGetThreadRequest(t, s, token)
		if row.DeliveredAt != 0 || row.DeliveredHow != "" || row.DeliveredWith != "" || row.Notify || row.WakeIssue == "" {
			t.Errorf("%s after the drop = delivered %q at %d with %q, notify %v, issue %q; want undelivered, disarmed and explained",
				token, row.DeliveredHow, row.DeliveredAt, row.DeliveredWith, row.Notify, row.WakeIssue)
		}
	}
	if got := mustGetThreadRequest(t, s, "tok-other").DeliveredHow; got != ThreadWakeQueued {
		t.Errorf("a wake the row did not carry reads %q, want it untouched", got)
	}
}

// A message is written only if every request it names still owes the wake.
// One that another route delivered, as carrier or as rider, refuses the whole
// message and writes nothing, so no answer is named in two messages.
func TestQueueThreadWakeRefusesAMessageNamingADeliveredRequest(t *testing.T) {
	for _, delivered := range []string{"tok-carrier", "tok-rider"} {
		t.Run(delivered, func(t *testing.T) {
			s := newTestStore(t)
			mustCreateThread(t, s, "t-caller")
			for _, token := range []string{"tok-carrier", "tok-rider", "tok-other"} {
				seedOwedThreadWake(t, s, token, "t-caller", ThreadRequestSend, 100, "same")
			}
			if _, err := s.MarkThreadRequestDelivered(delivered, ThreadWakeInline, 800, false); err != nil {
				t.Fatalf("MarkThreadRequestDelivered: %v", err)
			}

			item := FlushQueueItem{ID: "queue:carrier", ThreadID: "t-caller", SendID: "thread-wake:tok-carrier", Message: "same", EnqueuedAt: 900}
			err := s.QueueThreadWake("tok-carrier", []string{"tok-other", "tok-rider"}, false, item)
			if !errors.Is(err, ErrThreadWakeNotOwed) {
				t.Fatalf("QueueThreadWake = %v, want ErrThreadWakeNotOwed", err)
			}
			for _, token := range []string{"tok-carrier", "tok-rider", "tok-other"} {
				row := mustGetThreadRequest(t, s, token)
				if token == delivered {
					if row.DeliveredAt != 800 || row.DeliveredHow != ThreadWakeInline {
						t.Errorf("%s = %q at %d, want the inline delivery untouched", token, row.DeliveredHow, row.DeliveredAt)
					}
					continue
				}
				if row.DeliveredAt != 0 || row.DeliveredWith != "" {
					t.Errorf("the refused message still marked %s: at %d with %q", token, row.DeliveredAt, row.DeliveredWith)
				}
			}
			if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 0 {
				t.Fatalf("the refused message left a row: %+v err=%v", rows, err)
			}
		})
	}
}

// A late reply that lands while the first answer's message is still being
// written supersedes it: the first message is refused, whether the request
// is its carrier or rides in it, and the reply's own message is the one sent.
func TestQueueThreadWakeRefusesAFirstAnswerALateReplySuperseded(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	seedOwedThreadWake(t, s, "tok-late", "t-caller", ThreadRequestSend, 100, "started the tests")
	seedOwedThreadWake(t, s, "tok-other", "t-caller", ThreadRequestSend, 100, "started the tests")
	if stored, err := s.StoreThreadRequestLateReply("tok-late", []byte("all green"), 300, true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}

	first := FlushQueueItem{ID: "queue:first", ThreadID: "t-caller", SendID: "thread-wake:tok-late", Message: "started the tests", EnqueuedAt: 400}
	if err := s.QueueThreadWake("tok-late", nil, false, first); !errors.Is(err, ErrThreadWakeNotOwed) {
		t.Fatalf("QueueThreadWake(first answer) = %v, want ErrThreadWakeNotOwed", err)
	}
	first.SendID = "thread-wake:tok-other"
	if err := s.QueueThreadWake("tok-other", []string{"tok-late"}, false, first); !errors.Is(err, ErrThreadWakeNotOwed) {
		t.Fatalf("QueueThreadWake(first answer as a rider) = %v, want ErrThreadWakeNotOwed", err)
	}
	late := FlushQueueItem{ID: "queue:late", ThreadID: "t-caller", SendID: "thread-wake-late:tok-late", Message: "all green", EnqueuedAt: 500}
	if err := s.QueueThreadWake("tok-late", nil, true, late); err != nil {
		t.Fatalf("QueueThreadWake(late): %v", err)
	}
	rows, err := s.ListFlushQueueItems("t-caller")
	if err != nil || len(rows) != 1 || rows[0].Message != "all green" {
		t.Fatalf("queued rows = %+v err=%v, want the reply alone", rows, err)
	}
	if err := s.QueueThreadWake("tok-other", []string{"tok-late"}, true, late); err == nil || errors.Is(err, ErrThreadWakeNotOwed) {
		t.Fatalf("a late reply carrying another request = %v, want it refused as invalid", err)
	}
}

// The owed rule is written once in SQL, for the queries that find work, and
// once in Go, for a row already read. This holds the two to one answer for
// every combination of the four columns the rule reads.
func TestWakeOwedAgreesWithItsSQL(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	for bits := range 16 {
		settled, delivered, late, lateDelivered := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		token := fmt.Sprintf("tok-%02d", bits)
		seedThreadRequest(t, s, token, "t-caller", ThreadRequestSend, 100)
		set := func(column string, on bool, value any) {
			if !on {
				value = nil
			}
			if _, err := s.db.Exec(`UPDATE thread_requests SET `+column+` = ? WHERE token = ?`, value, token); err != nil {
				t.Fatalf("set %s on %s: %v", column, token, err)
			}
		}
		set("settled_at", settled, 200)
		set("delivered_at", delivered, 300)
		set("late_reply", late, []byte("late"))
		set("late_delivered_at", lateDelivered, 400)

		row := mustGetThreadRequest(t, s, token)
		var first, second, either bool
		if err := s.db.QueryRow(`SELECT `+threadFirstWakeOwedSQL+`, `+threadLateWakeOwedSQL+`, `+threadWakeOwedSQL+
			` FROM thread_requests WHERE token = ?`, token).Scan(&first, &second, &either); err != nil {
			t.Fatalf("read the SQL rule for %s: %v", token, err)
		}
		if first != row.WakeOwed(false) || second != row.WakeOwed(true) || either != (first || second) {
			t.Errorf("settled=%v delivered=%v late=%v lateDelivered=%v: SQL first=%v late=%v either=%v, Go first=%v late=%v",
				settled, delivered, late, lateDelivered, first, second, either, row.WakeOwed(false), row.WakeOwed(true))
		}
	}
}

// A poll of one computer asks about every request the due threads still have
// open there, due or not, so answers one event wrote are collected together.
// A request whose call has not returned is left out, as are settled ones,
// other computers' and other threads', and the read stays on the
// outstanding-rows index.
func TestOpenThreadRequestPollsOnReturnsTheCallersOpenRequestsOnOneComputer(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-second")
	mustCreateThread(t, s, "t-other")
	caller := "t-caller"
	seed := func(token, computer, state string, nextCheck int64) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: ThreadRequestSend, TargetComputerID: computer,
			State: ThreadRequestUnconfirmed, NextCheck: nextCheck, CreatedAt: 100, UpdatedAt: 100,
		}); err != nil {
			t.Fatalf("insert %s: %v", token, err)
		}
		if state == ThreadRequestUnconfirmed {
			return
		}
		if applied, err := s.AdvanceThreadRequestState(token, ThreadRequestUnconfirmed, ThreadRequestAccepted); err != nil || !applied {
			t.Fatalf("accept %s: applied=%v err=%v", token, applied, err)
		}
		switch state {
		case ThreadRequestRunning:
			if applied, err := s.AdvanceThreadRequestState(token, ThreadRequestAccepted, ThreadRequestRunning); err != nil || !applied {
				t.Fatalf("run %s: applied=%v err=%v", token, applied, err)
			}
		case ThreadRequestFinished:
			if settled, err := s.SettleThreadRequest(token, ThreadRequestOpenStates(), ThreadRequestSettlement{
				State: ThreadRequestFinished, Answer: []byte("done"), AnswerKind: ThreadAnswerFinal,
			}); err != nil || !settled {
				t.Fatalf("settle %s: settled=%v err=%v", token, settled, err)
			}
		}
	}
	far := int64(1) << 60
	seed("tok-due", "c-one", ThreadRequestRunning, 10)
	seed("tok-later", "c-one", ThreadRequestAccepted, far)
	seed("tok-unconfirmed", "c-one", ThreadRequestUnconfirmed, 5)
	seed("tok-finished", "c-one", ThreadRequestFinished, 5)
	seed("tok-elsewhere", "c-two", ThreadRequestRunning, 5)
	caller = "t-second"
	seed("tok-second", "c-one", ThreadRequestRunning, 20)
	caller = "t-other"
	seed("tok-other", "c-one", ThreadRequestRunning, 1)

	rows, err := s.OpenThreadRequestPollsOn("c-one", []string{"t-caller", "t-second"}, 10)
	if err != nil {
		t.Fatalf("OpenThreadRequestPollsOn: %v", err)
	}
	var tokens []string
	for _, row := range rows {
		tokens = append(tokens, row.Token)
	}
	if strings.Join(tokens, ",") != "tok-due,tok-second,tok-later" {
		t.Fatalf("open polls on c-one = %v, want the two callers' open requests, soonest first", tokens)
	}
	if rows, err := s.OpenThreadRequestPollsOn("c-one", nil, 10); err != nil || len(rows) != 0 {
		t.Fatalf("open polls for no caller = %+v err %v, want none", rows, err)
	}

	plan := explainPlan(t, s, openThreadRequestPollsOnSQL+"10", "c-one", `["t-caller","t-second"]`)
	var details []string
	for _, row := range plan {
		details = append(details, row.detail)
	}
	if joined := strings.Join(details, "\n"); !strings.Contains(joined, "idx_thread_requests_poll") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("open polls plan = %q, want idx_thread_requests_poll and no sort", joined)
	}
}

// A message is not written for a request whose wake was disarmed after the
// message was built, as its carrier or as a rider: the caller was archived,
// deleted or moved in between, and the answer stays on the request.
func TestQueueThreadWakeRefusesADisarmedRequest(t *testing.T) {
	for _, disarmed := range []string{"tok-carrier", "tok-rider", "every request of the caller"} {
		t.Run(disarmed, func(t *testing.T) {
			s := newTestStore(t)
			mustCreateThread(t, s, "t-caller")
			for _, token := range []string{"tok-carrier", "tok-rider"} {
				seedOwedThreadWake(t, s, token, "t-caller", ThreadRequestSend, 100, "same")
			}
			if strings.HasPrefix(disarmed, "tok-") {
				if _, err := s.SetThreadRequestNotify(disarmed, false); err != nil {
					t.Fatalf("SetThreadRequestNotify: %v", err)
				}
			} else if _, err := s.DisarmThreadRequestWakesForCaller("t-caller"); err != nil {
				t.Fatalf("DisarmThreadRequestWakesForCaller: %v", err)
			}

			item := FlushQueueItem{ID: "queue:carrier", ThreadID: "t-caller", SendID: "thread-wake:tok-carrier", Message: "same", EnqueuedAt: 900}
			if err := s.QueueThreadWake("tok-carrier", []string{"tok-rider"}, false, item); !errors.Is(err, ErrThreadWakeNotOwed) {
				t.Fatalf("QueueThreadWake = %v, want ErrThreadWakeNotOwed", err)
			}
			for _, token := range []string{"tok-carrier", "tok-rider"} {
				if row := mustGetThreadRequest(t, s, token); row.DeliveredAt != 0 || row.DeliveredWith != "" {
					t.Errorf("%s = delivered at %d with %q, want nothing written", token, row.DeliveredAt, row.DeliveredWith)
				}
			}
			queued, err := s.ListFlushQueueItems("t-caller")
			if err != nil || len(queued) != 0 {
				t.Fatalf("queued = %+v err %v, want no message", queued, err)
			}
		})
	}
}

// A message answers requests of the thread it is queued on and no other,
// names each once, and a late reply's message leaves the record of which
// message carried the first answer.
func TestQueueThreadWakeAnswersOnlyItsOwnThreadsRequests(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-caller")
	mustCreateThread(t, s, "t-other")
	seedOwedThreadWake(t, s, "tok-carrier", "t-caller", ThreadRequestSend, 100, "same")
	seedOwedThreadWake(t, s, "tok-rider", "t-caller", ThreadRequestSend, 100, "same")
	seedOwedThreadWake(t, s, "tok-theirs", "t-other", ThreadRequestSend, 100, "same")
	item := FlushQueueItem{ID: "queue:carrier", ThreadID: "t-caller", SendID: "thread-wake:tok-carrier", Message: "same", EnqueuedAt: 900}

	if err := s.QueueThreadWake("tok-carrier", []string{"tok-theirs"}, false, item); !errors.Is(err, ErrThreadWakeNotOwed) {
		t.Fatalf("a message naming another thread's request = %v, want ErrThreadWakeNotOwed", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-theirs"); row.DeliveredAt != 0 || row.DeliveredWith != "" {
		t.Fatalf("another thread's request was marked: %+v", row)
	}
	for _, riders := range [][]string{{"tok-rider", "tok-rider"}, {"tok-carrier"}, {""}} {
		if err := s.QueueThreadWake("tok-carrier", riders, false, item); err == nil || errors.Is(err, ErrThreadWakeNotOwed) {
			t.Fatalf("riders %q = %v, want them refused as invalid, not as a lost race", riders, err)
		}
	}
	if rows, err := s.ListFlushQueueItems("t-caller"); err != nil || len(rows) != 0 {
		t.Fatalf("a refused message left a row: %+v err=%v", rows, err)
	}

	if err := s.QueueThreadWake("tok-carrier", []string{"tok-rider"}, false, item); err != nil {
		t.Fatalf("QueueThreadWake: %v", err)
	}
	if stored, err := s.StoreThreadRequestLateReply("tok-rider", []byte("all green"), 950, true); err != nil || !stored {
		t.Fatalf("StoreThreadRequestLateReply: stored=%v err=%v", stored, err)
	}
	late := FlushQueueItem{ID: "queue:late", ThreadID: "t-caller", SendID: "thread-wake-late:tok-rider", Message: "all green", EnqueuedAt: 960}
	if err := s.QueueThreadWake("tok-rider", nil, true, late); err != nil {
		t.Fatalf("QueueThreadWake(late): %v", err)
	}
	if row := mustGetThreadRequest(t, s, "tok-rider"); row.DeliveredWith != "tok-carrier" || row.LateDeliveredAt != 960 {
		t.Fatalf("after its late reply the rider = with %q late at %d", row.DeliveredWith, row.LateDeliveredAt)
	}
}

// A receipt has no caller to wake, so a settlement that asks to arm one is a
// caller's mistake and is refused.
func TestAReceiptSettlementRefusesToArmAWake(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "t-target")
	if _, _, err := s.AcceptThreadRequestReceipt(ThreadRequestReceipt{
		Token: "tok-receipt", OwnerDeviceID: "local", SourceThreadID: "t-source",
		Kind: ThreadRequestSend, TargetThreadID: "t-target",
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	if settled, err := s.SettleThreadRequestReceipt("tok-receipt", ThreadReceiptOpenStates(), ThreadRequestSettlement{
		State: ThreadReceiptReplied, Answer: []byte("done"), AnswerKind: ThreadAnswerReply, ArmWake: true,
	}); err == nil || settled {
		t.Fatalf("a receipt settlement armed a wake: settled=%v err=%v", settled, err)
	}
}
