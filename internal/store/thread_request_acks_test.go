package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// remoteFinished seeds a request on computer that collected a finished
// answer at revision 1 and still listens for a late reply.
func remoteFinished(t *testing.T, s *Store, token, caller, computer string) {
	t.Helper()
	seedRemoteSpawn(t, s, token, caller, computer, "t-far")
	if _, err := s.CollectThreadRequest(token, ThreadReceiptAnswer{
		Revision: 1, State: ThreadReceiptFinished, Answer: []byte("first"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
	}, ThreadRequestWake{}); err != nil {
		t.Fatal(err)
	}
}

func requestRow(t *testing.T, s *Store, token string) ThreadRequest {
	t.Helper()
	row, found, err := s.GetThreadRequest(token)
	if err != nil || !found {
		t.Fatalf("read %s: found=%v err=%v", token, found, err)
	}
	return row
}

// A collection owes the acknowledgement of the revision it took in its own
// transaction, and only the destination taking that same revision clears
// it: one collected while the acknowledgement was on its way stays owed.
func TestACollectedRevisionIsOwedUntilThatRevisionIsTaken(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	remoteFinished(t, s, "r", "lead", "pc")
	if owed := requestRow(t, s, "r").AckOwed; owed != 1 {
		t.Fatalf("owed after the collection = %d, want 1", owed)
	}
	acks, err := s.OwedThreadRequestAcks("pc", 10)
	if err != nil || len(acks) != 1 || acks[0] != (ThreadRequestAck{Token: "r", ComputerID: "pc", Revision: 1}) {
		t.Fatalf("owed to pc = %+v, %v", acks, err)
	}
	if other, err := s.OwedThreadRequestAcks("elsewhere", 10); err != nil || len(other) != 0 {
		t.Fatalf("owed to another computer = %+v, %v", other, err)
	}
	if _, err := s.CollectThreadRequest("r", ThreadReceiptAnswer{
		Revision: 2, State: ThreadReceiptFinished, Answer: []byte("first"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
		LateReply: []byte("later"), LateReplyAt: 300,
	}, ThreadRequestWake{}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearThreadRequestAcks(acks); err != nil {
		t.Fatal(err)
	}
	if owed := requestRow(t, s, "r").AckOwed; owed != 2 {
		t.Fatalf("owed after revision 1 was taken = %d, want revision 2 still owed", owed)
	}
	if err := s.ClearThreadRequestAcks([]ThreadRequestAck{{Token: "r", Revision: 2}}); err != nil {
		t.Fatal(err)
	}
	if owed := requestRow(t, s, "r").AckOwed; owed != 0 {
		t.Fatalf("owed after revision 2 was taken = %d", owed)
	}
	// Owed again for a revision the row holds, never for one past it.
	if changed, err := s.OweThreadRequestAck("r", 3); err != nil || changed {
		t.Fatalf("owe a revision the row does not hold = %v, %v", changed, err)
	}
	if changed, err := s.OweThreadRequestAck("r", 2); err != nil || !changed {
		t.Fatalf("owe a revision the row holds = %v, %v", changed, err)
	}
}

// An acknowledgement owed is due for its retry by the row's clock, whether
// or not the row is still polled, and a failed send puts it off.
func TestAnOwedAcknowledgementIsScheduledWhenItsRowIsNoLongerPolled(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	remoteFinished(t, s, "r", "lead", "pc")
	if _, err := s.RetireThreadRequestPoll("r"); err != nil {
		t.Fatal(err)
	}
	if err := s.RescheduleThreadRequest("r", 5000, 0, ""); err != nil {
		t.Fatal(err)
	}
	schedule, err := s.NextThreadRequestWork()
	if err != nil || schedule.Poll != (ScheduledMoment{At: 5000, Set: true}) {
		t.Fatalf("schedule = %+v, %v; want the owed acknowledgement at 5000", schedule.Poll, err)
	}
	if due, err := s.DueThreadRequestAcks(4999, 10); err != nil || len(due) != 0 {
		t.Fatalf("due before its moment = %+v, %v", due, err)
	}
	due, err := s.DueThreadRequestAcks(5000, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due at its moment = %+v, %v", due, err)
	}
	if err := s.DeferThreadRequestAcks(due, 9000); err != nil {
		t.Fatal(err)
	}
	if again, err := s.DueThreadRequestAcks(8999, 10); err != nil || len(again) != 0 {
		t.Fatalf("due after it was put off = %+v, %v", again, err)
	}
	if err := s.ClearThreadRequestAcks(due); err != nil {
		t.Fatal(err)
	}
	if schedule, err := s.NextThreadRequestWork(); err != nil || schedule.Poll.Set {
		t.Fatalf("schedule with nothing owed = %+v, %v", schedule.Poll, err)
	}
}

// A late reply its destination dropped before this computer collected it
// is recorded with when it arrived, the first answer kept, the revision
// held, and no later late reply taken.
func TestALateReplyThatExpiredThereIsRecordedWithTheFirstAnswerKept(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	remoteFinished(t, s, "r", "lead", "pc")
	collected, err := s.CollectThreadRequest("r", ThreadReceiptAnswer{
		Revision: 2, State: ThreadReceiptExpired, SettledAt: 200, LateReplyAt: 300,
	}, ThreadRequestWake{})
	if err != nil || collected != (ThreadRequestCollected{LateReplyExpired: true}) {
		t.Fatalf("collect the expiry = %+v, %v", collected, err)
	}
	row := requestRow(t, s, "r")
	if !row.LateReplyExpired || row.LateReplyAt != 300 || row.Revision != 2 || row.AckOwed != 2 ||
		row.State != ThreadRequestFinished || string(row.Answer) != "first" {
		t.Fatalf("after the expiry = %+v", row)
	}
	if again, err := s.CollectThreadRequest("r", ThreadReceiptAnswer{
		Revision: 2, State: ThreadReceiptExpired, SettledAt: 200, LateReplyAt: 300,
	}, ThreadRequestWake{}); err != nil || again.Any() {
		t.Fatalf("collect the expiry again = %+v, %v", again, err)
	}
	if stored, err := storeThreadRequestLateReply(s.db, "r", []byte("too late"), 400, nil, false, ""); err != nil || stored {
		t.Fatalf("a late reply after the expiry was stored = %v, %v", stored, err)
	}
	// An expiry with no late reply is the answer's own and records nothing.
	remoteFinished(t, s, "q", "lead", "pc")
	if none, err := s.CollectThreadRequest("q", ThreadReceiptAnswer{
		Revision: 1, State: ThreadReceiptExpired, SettledAt: 200,
	}, ThreadRequestWake{}); err != nil || none.Any() {
		t.Fatalf("collect an expiry with no late reply = %+v, %v", none, err)
	}
}

// The end of a pairing, in one transaction: the open requests settle, the
// settled ones leave the poll with their answers and owe nothing, a command
// still running is recorded with its outcome unknown and no cancel owed,
// and nothing on another computer changes.
func TestEndPairingEndsBothLedgersForThatComputerAlone(t *testing.T) {
	s := newTestStore(t)
	pc, lead := uuid.NewString(), uuid.NewString()
	mustCreateThread(t, s, lead)
	seedRemoteSpawn(t, s, "open", lead, pc, "t-open")
	if _, err := s.SetThreadRequestNotify("open", true); err != nil {
		t.Fatal(err)
	}
	remoteFinished(t, s, "done", lead, pc)
	seedRemoteSpawn(t, s, "elsewhere", lead, "other", "t-other")
	running := RemoteWatch{ComputerID: pc, RequestID: uuid.NewString(), ThreadID: lead, Fingerprint: strings.Repeat("a", 64), Label: "build"}
	finished := RemoteWatch{ComputerID: pc, RequestID: uuid.NewString(), ThreadID: lead, Fingerprint: strings.Repeat("b", 64), Label: "test"}
	for _, w := range []RemoteWatch{running, finished} {
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
	}
	state := map[string]string{running.RequestID: "running", finished.RequestID: "succeeded"}
	for id, st := range state {
		if err := s.ObserveRemoteWatch(pc, id, RemoteJob{ID: id, SourceThreadID: lead, State: st, ExitCode: -1}, "", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.OweRemoteCancel(pc, running.RequestID, 100, 200); err != nil {
		t.Fatal(err)
	}

	open, err := s.OpenThreadRequestTokensOn(pc)
	if err != nil || len(open) != 1 || open[0] != "open" {
		t.Fatalf("open on pc = %v, %v", open, err)
	}
	ended, err := s.EndPairing(pc, open, PairingEnd{RequestAnswer: []byte("gone"), CommandError: "unknown now", Disarm: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(ended.Settled) != 1 || ended.Settled[0] != (EndedRequest{Token: "open"}) || len(ended.CommandThreads) != 1 {
		t.Fatalf("ended = %+v", ended)
	}
	if row := requestRow(t, s, "open"); row.State != ThreadRequestErrored || string(row.Answer) != "gone" || row.Notify || row.Polling {
		t.Fatalf("the open request = %+v", row)
	}
	if row := requestRow(t, s, "done"); row.State != ThreadRequestFinished || string(row.Answer) != "first" || row.Polling || row.AckOwed != 0 {
		t.Fatalf("the finished request = %+v", row)
	}
	if row := requestRow(t, s, "elsewhere"); row.State != ThreadRequestRunning || !row.Polling {
		t.Fatalf("the request on another computer = %+v", row)
	}
	got, err := s.GetRemoteWatch(pc, running.RequestID)
	if err != nil || got.Receipt.State != RemoteOutcomeUnknown || got.Receipt.Error != "unknown now" || got.Receipt.FinishedAt == 0 ||
		got.CancelOwedAt != 0 || got.Notification != "pending" {
		t.Fatalf("the running command = %+v, %v", got, err)
	}
	if kept, err := s.GetRemoteWatch(pc, finished.RequestID); err != nil || kept.Receipt.State != "succeeded" {
		t.Fatalf("the finished command = %+v, %v", kept, err)
	}
	// Armed: the person did not choose silence.
	seedRemoteSpawn(t, s, "armed", lead, pc, "t-armed")
	if _, err := s.SetThreadRequestNotify("armed", true); err != nil {
		t.Fatal(err)
	}
	if ended, err := s.EndPairing(pc, []string{"armed"}, PairingEnd{RequestAnswer: []byte("gone")}); err != nil ||
		len(ended.Settled) != 1 || !ended.Settled[0].Notify || len(ended.CommandThreads) != 0 {
		t.Fatalf("an armed end = %+v, %v", ended, err)
	}
}

// The end of a pairing writes the name the computer has then onto every
// record there that kept none, as a row from v0.0.19 did not, in the same
// write; a record that kept its admission name keeps it, and nothing on
// another computer is named.
func TestEndPairingNamesTheComputerOnTheRecordsThatKeptNone(t *testing.T) {
	s := newTestStore(t)
	pc, other, lead := uuid.NewString(), uuid.NewString(), uuid.NewString()
	mustCreateThread(t, s, lead)
	request := func(token, computer, name string) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: lead, Kind: ThreadRequestSend, TargetThreadID: "t-" + token,
			TargetComputerID: computer, State: ThreadRequestRunning, TargetComputerName: name}); err != nil {
			t.Fatal(err)
		}
	}
	request("legacy", pc, "")
	request("named", pc, "GPU computer")
	request("elsewhere", other, "")
	command := func(computer, name string) RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: computer, RequestID: uuid.NewString(), ThreadID: lead, Fingerprint: strings.Repeat("a", 64), Label: "build", ComputerName: name}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		return w
	}
	legacyCommand, namedCommand, otherCommand := command(pc, ""), command(pc, "GPU computer"), command(other, "")

	open, err := s.OpenThreadRequestTokensOn(pc)
	if err != nil || !slices.Equal(open, []string{"legacy", "named"}) {
		t.Fatalf("open on pc = %v, %v", open, err)
	}
	if _, err := s.EndPairing(pc, open, PairingEnd{RequestAnswer: []byte("gone"), CommandError: "unknown now", ComputerName: "Render box"}); err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]string{"legacy": "Render box", "named": "GPU computer", "elsewhere": ""} {
		if row := requestRow(t, s, token); row.TargetComputerName != want {
			t.Errorf("request %s names its computer %q, want %q", token, row.TargetComputerName, want)
		}
	}
	for w, want := range map[RemoteWatch]string{legacyCommand: "Render box", namedCommand: "GPU computer", otherCommand: ""} {
		if got, err := s.GetRemoteWatch(w.ComputerID, w.RequestID); err != nil || got.ComputerName != want {
			t.Errorf("command %s names its computer %q (%v), want %q", w.RequestID, got.ComputerName, err, want)
		}
	}
}

// The forget warning's read: a bounded list, oldest first, with the total.
func TestOpenRequestsForAComputerListABoundedPageAndTheTotal(t *testing.T) {
	s := newTestStore(t)
	mustCreateThread(t, s, "lead")
	for i, token := range []string{"a", "b", "c"} {
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: "lead", Kind: ThreadRequestSend,
			State: ThreadRequestRunning, TargetComputerID: "pc", TargetThreadID: "t", CreatedAt: int64(100 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.ListOpenThreadRequestsForComputer("pc", 2)
	if err != nil || total != 3 || len(rows) != 2 || rows[0].Token != "a" || rows[1].Token != "b" {
		t.Fatalf("listed %d of %d: %+v, %v", len(rows), total, rows, err)
	}
}
