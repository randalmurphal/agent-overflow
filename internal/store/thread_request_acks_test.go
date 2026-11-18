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
	if err := s.BindRecordPairing("pc", "session"); err != nil {
		t.Fatal(err)
	}
	if owed := requestRow(t, s, "r").AckOwed; owed != 1 {
		t.Fatalf("owed after the collection = %d, want 1", owed)
	}
	acks, err := s.OwedThreadRequestAcks("pc", "session", 10)
	if err != nil || len(acks) != 1 || acks[0] != (ThreadRequestAck{Token: "r", ComputerID: "pc", Revision: 1, Pairing: "session"}) {
		t.Fatalf("owed to pc = %+v, %v", acks, err)
	}
	if other, err := s.OwedThreadRequestAcks("elsewhere", "session", 10); err != nil || len(other) != 0 {
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

	open, err := s.OpenThreadRequestTokensOn(pc, "")
	if err != nil || len(open) != 1 || open[0] != "open" {
		t.Fatalf("open on pc = %v, %v", open, err)
	}
	ended, err := s.EndPairing(pc, "", open, PairingEnd{RequestAnswer: []byte("gone"), CommandError: "unknown now", Disarm: true})
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
	if ended, err := s.EndPairing(pc, "", []string{"armed"}, PairingEnd{RequestAnswer: []byte("gone")}); err != nil ||
		len(ended.Settled) != 1 || !ended.Settled[0].Notify || len(ended.CommandThreads) != 0 {
		t.Fatalf("an armed end = %+v, %v", ended, err)
	}
}

// The end of a pairing reaches only what the pairings other than the live
// one admitted: a request or command the live pairing admitted keeps its
// poll, its acknowledgement and its owed cancel. A row that names no
// pairing belongs to none, so it is ended with the rest.
func TestEndPairingEndsOnlyWhatTheEndedPairingAdmitted(t *testing.T) {
	s := newTestStore(t)
	pc, lead := uuid.NewString(), uuid.NewString()
	mustCreateThread(t, s, lead)
	seed := func(token, pairing string, finished bool) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{Token: token, CallerThreadID: lead, Kind: ThreadRequestSpawn,
			TargetThreadID: "t-" + token, TargetComputerID: pc, State: ThreadRequestRunning, Pairing: pairing,
			CreatedAt: 100, UpdatedAt: 100}); err != nil {
			t.Fatal(err)
		}
		if finished {
			if _, err := s.CollectThreadRequest(token, ThreadReceiptAnswer{
				Revision: 1, State: ThreadReceiptFinished, Answer: []byte("first"), AnswerKind: ThreadAnswerFinal, SettledAt: 200,
			}, ThreadRequestWake{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("old", "s1", false)
	seed("old-done", "s1", true)
	seed("new", "s2", false)
	seed("new-done", "s2", true)
	seed("legacy", "", false)
	seed("legacy-done", "", true)
	command := func(pairing string) RemoteWatch {
		t.Helper()
		w := RemoteWatch{ComputerID: pc, RequestID: uuid.NewString(), ThreadID: lead, Fingerprint: strings.Repeat("a", 64),
			Label: "build", Pairing: pairing, ComputerName: "GPU computer"}
		if _, err := s.RegisterRemoteWatch(w); err != nil {
			t.Fatal(err)
		}
		if err := s.ObserveRemoteWatch(pc, w.RequestID, RemoteJob{ID: w.RequestID, SourceThreadID: lead, State: "running", ExitCode: -1}, "", time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := s.OweRemoteCancel(pc, w.RequestID, 100, 200); err != nil {
			t.Fatal(err)
		}
		return w
	}
	oldCommand, newCommand := command("s1"), command("s2")

	open, err := s.OpenThreadRequestTokensOn(pc, "s2")
	if err != nil || !slices.Equal(open, []string{"legacy", "old"}) {
		t.Fatalf("open under an ended pairing = %v, %v; want the old pairing's and the one naming none", open, err)
	}
	acks, err := s.OwedThreadRequestAcks(pc, "s2", 10)
	if err != nil || len(acks) != 1 || acks[0].Token != "new-done" {
		t.Fatalf("owed under the live pairing = %+v, %v; want new-done alone", acks, err)
	}
	if _, err := s.EndPairing(pc, "s2", open, PairingEnd{RequestAnswer: []byte("gone"), CommandError: "unknown now"}); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"old", "legacy"} {
		if row := requestRow(t, s, token); row.State != ThreadRequestErrored {
			t.Fatalf("the ended open request %s = %+v", token, row)
		}
	}
	for _, token := range []string{"old-done", "legacy-done"} {
		if row := requestRow(t, s, token); row.Polling || row.AckOwed != 0 {
			t.Fatalf("the ended settled request %s = %+v", token, row)
		}
	}
	if row := requestRow(t, s, "new"); row.State != ThreadRequestRunning || !row.Polling {
		t.Fatalf("the live pairing's open request = %+v", row)
	}
	if row := requestRow(t, s, "new-done"); !row.Polling || row.AckOwed != 1 {
		t.Fatalf("the live pairing's settled request = %+v", row)
	}
	if got, err := s.GetRemoteWatch(pc, oldCommand.RequestID); err != nil || got.Receipt.State != RemoteOutcomeUnknown || got.CancelOwedAt != 0 {
		t.Fatalf("the old pairing's command = %+v, %v", got, err)
	}
	if got, err := s.GetRemoteWatch(pc, newCommand.RequestID); err != nil || got.Receipt.State != "running" || got.CancelOwedAt == 0 ||
		got.Pairing != "s2" || got.ComputerName != "GPU computer" {
		t.Fatalf("the live pairing's command = %+v, %v", got, err)
	}

	// No pairing held: everything ends, and a later pairing owes nothing.
	open, err = s.OpenThreadRequestTokensOn(pc, "")
	if err != nil || !slices.Equal(open, []string{"new"}) {
		t.Fatalf("open with no pairing held = %v, %v", open, err)
	}
	if _, err := s.EndPairing(pc, "", open, PairingEnd{RequestAnswer: []byte("gone"), CommandError: "unknown now"}); err != nil {
		t.Fatal(err)
	}
	if row := requestRow(t, s, "new-done"); row.Polling || row.AckOwed != 0 {
		t.Fatalf("the settled request once no pairing is held = %+v", row)
	}
	if acks, err := s.OwedThreadRequestAcks(pc, "s3", 10); err != nil || len(acks) != 0 {
		t.Fatalf("owed under a later pairing = %+v, %v", acks, err)
	}
	if got, err := s.GetRemoteWatch(pc, newCommand.RequestID); err != nil || got.Receipt.State != RemoteOutcomeUnknown || got.CancelOwedAt != 0 {
		t.Fatalf("the command once no pairing is held = %+v, %v", got, err)
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
