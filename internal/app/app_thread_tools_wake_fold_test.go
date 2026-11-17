package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/agentmeta"
	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"

	"github.com/google/uuid"
)

// How an answer reaches the thread that asked: a reply always does, a
// refusal the call itself returned never does again, and answers that say
// the same thing arrive as one message.

// unattendedRunningRequest spawns a thread whose turn stays open, with no
// wait and no notify: the sender asked to be told nothing.
func (f *requestFixture) unattendedRunningRequest(t *testing.T, prompt string) threadtools.RequestAck {
	t.Helper()
	ack, err := f.adapter().Spawn(t.Context(), f.callerIdentity(), threadtools.SpawnCall{Prompt: prompt})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	waitUntil(t, 10*time.Second, func() bool {
		row, found, err := f.app.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && row.State == store.ThreadReceiptRunning
	})
	if row := f.request(t, ack.Token); row.Notify {
		t.Fatalf("an unattended request was made with notify armed: %+v", row)
	}
	return ack
}

// wakesQueuedFor returns the caller's queued wake messages once every named
// request has been delivered.
func (f *requestFixture) wakesQueuedFor(t *testing.T, tokens ...string) []store.FlushQueueItem {
	t.Helper()
	waitUntil(t, 10*time.Second, func() bool {
		for _, token := range tokens {
			if f.request(t, token).DeliveredAt == 0 {
				return false
			}
		}
		return true
	})
	var wakes []store.FlushQueueItem
	for _, row := range durableQueueRows(t, f.app, f.caller.ID) {
		if strings.HasPrefix(row.SendID, threadWakeSendID("")) {
			wakes = append(wakes, row)
		}
	}
	return wakes
}

// nothingOwed asserts no wake is left for the retry pass to deliver.
func (f *requestFixture) nothingOwed(t *testing.T) {
	t.Helper()
	owed, err := f.app.store.UndeliveredThreadRequestWakes(time.Now().Add(time.Hour).UnixMilli(), 32)
	if err != nil {
		t.Fatalf("UndeliveredThreadRequestWakes: %v", err)
	}
	if len(owed) != 0 {
		t.Fatalf("wakes still owed: %+v", owed)
	}
}

// A thread_reply is the answer, so it reaches the sender as a message even
// when the sender asked for no notification. Before this the reply sat on
// the request record until the sender happened to call thread_status.
func TestAReplyReachesASenderThatAskedForNoNotification(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "starting on it")
	f.holdCallerTurn(t)
	ack := f.unattendedRunningRequest(t, "which items block the merge?")

	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "three items block it: P-031, P-032 and P-040",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	wake := awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(ack.Token))
	if !strings.HasPrefix(wake.Message, "Reply from ") || !strings.Contains(wake.Message, "P-031, P-032 and P-040") {
		t.Fatalf("wake = %q, want the reply", wake.Message)
	}
	row := f.request(t, ack.Token)
	if !row.Notify || row.DeliveredHow != store.ThreadWakeQueued {
		t.Fatalf("request = notify %v, delivered %q; want the reply's wake armed and queued", row.Notify, row.DeliveredHow)
	}
}

// The turn ending without a reply is still not delivered to a sender that
// asked for nothing: only the explicit reply is owed.
func TestAFinalMessageIsNotDeliveredToASenderThatAskedForNoNotification(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "I started the tests")
	f.holdCallerTurn(t)
	ctx, pending := withThreadToolsPending(t.Context())
	ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "run the tests"})
	pending.run(false)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	f.app.threadRequestSettleLock(ack.Token)()

	row := f.request(t, ack.Token)
	if row.Notify || row.DeliveredAt != 0 {
		t.Fatalf("request = notify %v, delivered at %d; want nothing armed or delivered", row.Notify, row.DeliveredAt)
	}
	f.nothingOwed(t)
}

// An archived caller is not brought back by a reply to a request nobody was
// waiting on: archiving is how a person says they are done with the thread.
// The reply stays on the request.
func TestAReplyDoesNotWakeAnArchivedSender(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "starting on it")
	ack := f.unattendedRunningRequest(t, "how big is the index?")
	// An owned worker is archived with its owner, which interrupts the
	// request; a standalone one keeps working and can still reply.
	if _, err := f.app.store.SetThreadPlacement([]string{ack.ThreadID}, store.PlacementStandalone); err != nil {
		t.Fatalf("SetThreadPlacement: %v", err)
	}
	if err := archiveThread(f.app, f.caller.ID); err != nil {
		t.Fatalf("ArchiveThread: %v", err)
	}

	if _, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "4.2 GB",
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	row := f.request(t, ack.Token)
	if row.State != store.ThreadRequestReplied || string(row.Answer) != "4.2 GB" {
		t.Fatalf("request = %+v, want the reply stored", row)
	}
	if row.Notify || row.DeliveredAt != 0 {
		t.Fatalf("request = notify %v, delivered at %d; want no wake for an archived sender", row.Notify, row.DeliveredAt)
	}
	if thread, err := f.app.store.GetThread(f.caller.ID); err != nil || !thread.Archived {
		t.Fatalf("caller = archived %v err %v, want it left archived", thread.Archived, err)
	}
	f.nothingOwed(t)
}

// A reply that arrives after the turn already ended is delivered too, and it
// is the only message: the final message nobody asked to be told about is
// superseded, as it is in thread_status.
func TestALateReplyToAnUnattendedRequestIsTheOnlyMessage(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "I'll post the full report once they finish")
	f.holdCallerTurn(t)
	ctx, pending := withThreadToolsPending(t.Context())
	ack, err := f.adapter().Spawn(ctx, f.callerIdentity(), threadtools.SpawnCall{Prompt: "report when the gates pass"})
	pending.run(false)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	f.awaitRequestState(t, ack.Token, store.ThreadRequestFinished)
	f.app.threadRequestSettleLock(ack.Token)()

	late, err := f.adapter().Reply(t.Context(), f.targetIdentity(t, ack.ThreadID), threadtools.ReplyCall{
		Token: ack.Token, Text: "all gates pass at 0d8718b427",
	})
	if err != nil || !late.Late {
		t.Fatalf("late Reply = %+v err %v", late, err)
	}

	waitUntil(t, 10*time.Second, func() bool { return f.request(t, ack.Token).LateDeliveredAt != 0 })
	rows := durableQueueRows(t, f.app, f.caller.ID)
	if len(rows) != 1 || rows[0].SendID != threadWakeLateSendID(ack.Token) || !strings.Contains(rows[0].Message, "0d8718b427") {
		t.Fatalf("queued = %+v, want the late reply alone", rows)
	}
	// The retry pass finds nothing left: the first answer is not owed.
	f.nothingOwed(t)
}

// A reply collected from a paired computer is owed the same way as one
// written on this computer.
func TestAReplyFromAnotherComputerReachesASenderThatAskedForNoNotification(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaude(t, "noted")
	f.holdCallerTurn(t)
	computer := uuid.NewString()
	token := f.seedRemoteRequest(t, computer)

	revision, wakes, err := f.app.applyThreadPeerRequest(token, computer, ThreadPeerRequest{
		Token: token, Known: true, State: store.ThreadReceiptReplied, Title: "Release checklist",
		Answer: "the tag is pushed", AnswerKind: store.ThreadAnswerReply,
		Revision: 1, SettledAt: time.Now().UnixMilli(),
	})
	if err != nil || revision != 1 {
		t.Fatalf("applyThreadPeerRequest = revision %d err %v", revision, err)
	}
	if len(wakes) != 1 || wakes[0].token != token || wakes[0].late {
		t.Fatalf("wakes owed = %+v, want the reply's", wakes)
	}
	f.app.deliverThreadWakes(wakes)
	wake := awaitQueuedWake(t, f.app, f.caller.ID, threadWakeSendID(token))
	if !strings.Contains(wake.Message, "the tag is pushed") {
		t.Fatalf("wake = %q, want the reply", wake.Message)
	}
}

// An answer from another computer that finished and then took a late reply
// is collected whole, with the reply as its one wake, and the revision it
// acknowledges is the one it holds. An acknowledgement lost after the
// collection (a crash, a failed call) is sent again by the next poll that
// finds the destination still behind, and by no other.
func TestARemoteAnswerIsCollectedWholeAndItsLostAcknowledgementResent(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	computer := uuid.NewString()
	token := f.seedRemoteRequest(t, computer)
	now := time.Now().UnixMilli()
	answer := ThreadPeerRequest{
		Token: token, Known: true, State: store.ThreadReceiptFinished,
		Answer: "original final", AnswerKind: store.ThreadAnswerFinal,
		LateReply: "actual requested reply", LateReplyAt: now,
		Revision: 2, SettledAt: now - 1000, ExpiresAt: now + time.Hour.Milliseconds(),
	}

	revision, wakes, err := f.app.applyThreadPeerRequest(token, computer, answer)
	if err != nil || revision != 2 {
		t.Fatalf("applyThreadPeerRequest = revision %d err %v; want 2", revision, err)
	}
	if len(wakes) != 1 || wakes[0] != (threadWakeRef{token: token, late: true}) {
		t.Fatalf("wakes owed = %+v, want the late reply's alone", wakes)
	}
	row := f.request(t, token)
	if row.State != store.ThreadRequestFinished || string(row.Answer) != "original final" ||
		string(row.LateReply) != "actual requested reply" || row.LateReplyAt != now || row.Revision != 2 {
		t.Fatalf("request = %+v, want the final and the late reply at revision 2", row)
	}

	// The acknowledgement never arrived: the destination still reports
	// nothing collected, and the poll acknowledges again, writing nothing.
	revision, wakes, err = f.app.applyThreadPeerRequest(token, computer, answer)
	if err != nil || revision != 2 || len(wakes) != 0 {
		t.Fatalf("poll after a lost ack = revision %d wakes %+v err %v; want revision 2 and no wake", revision, wakes, err)
	}
	if again := f.request(t, token); again.Revision != row.Revision || again.UpdatedAt != row.UpdatedAt {
		t.Fatalf("poll after a lost ack rewrote the request: %+v", again)
	}
	answer.CollectedRevision = 2
	if revision, _, err = f.app.applyThreadPeerRequest(token, computer, answer); err != nil || revision != 0 {
		t.Fatalf("poll after the ack = revision %d err %v; want nothing to acknowledge", revision, err)
	}
}

// The destination reports the revision its source acknowledged, which is
// what lets the source tell a lost acknowledgement from one that landed.
func TestADestinationReportsTheRevisionItsSourceCollected(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	target := f.busyThread(t, "")
	token := newThreadRequestToken()
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: token, OwnerDeviceID: "device-source", SourceThreadID: uuid.NewString(),
		Kind: store.ThreadRequestSend, TargetThreadID: target.ID, State: store.ThreadReceiptRunning,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	// The request is another computer's, so only the receipt is written.
	if _, err := f.app.store.SettleLocalThreadRequest(token, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptFinished, Answer: []byte("done"), AnswerKind: store.ThreadAnswerFinal,
	}, store.ThreadRequestFinished); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.store.AckThreadRequestReceipt(token, 1, 0); err != nil {
		t.Fatal(err)
	}
	state, err := f.app.threadPeerRequestState(t.Context(), "device-source", token)
	if err != nil || state.Revision != 1 || state.CollectedRevision != 1 {
		t.Fatalf("state = %+v, %v; want revision 1 collected", state, err)
	}
}

// One turn that consumed two of a thread's messages ends once, and the
// thread is told once: one message naming both requests. Each request still
// records its own delivery, and the one that rode along knows which message
// carried it, through a queued wake and through a restart that puts that
// message back in the composer.
func TestOneTurnAnsweringTwoRequestsWakesTheirSenderOnce(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	q.notify = true
	first, second := q.queue(t, 1, 1), q.queue(t, 2, 1)
	first.bind(t)
	second.bind(t)
	q.endTurn(t)
	q.startPickupTurn(t)
	first.consumed(t)
	second.consumed(t)
	q.say(t, 1, "both checked")
	q.endTurn(t)
	f.app.settleReceiptsForEndedTurn(q.target.ID)
	tokens := []string{first.token, second.token}

	wakes := f.wakesQueuedFor(t, tokens...)
	if len(wakes) != 1 {
		t.Fatalf("queued wakes = %+v, want one message for the one turn", wakes)
	}
	// The message names its requests oldest first, whatever order the turn
	// end observed them in; two made in the same millisecond order by token.
	carrier, rider := f.request(t, tokens[0]), f.request(t, tokens[1])
	if rider.CreatedAt < carrier.CreatedAt || (rider.CreatedAt == carrier.CreatedAt && rider.Token < carrier.Token) {
		carrier, rider = rider, carrier
	}
	if wakes[0].SendID != threadWakeSendID(carrier.Token) {
		t.Fatalf("wake send id = %q, want the older request's", wakes[0].SendID)
	}
	if want := "[tokens " + carrier.Token + ", " + rider.Token + "] finished its turn without calling thread_reply"; !strings.Contains(wakes[0].Message, want) {
		t.Fatalf("wake = %q, want %q", wakes[0].Message, want)
	}
	if carrier.DeliveredHow != store.ThreadWakeQueued || carrier.DeliveredWith != "" {
		t.Errorf("carrier = delivered %q with %q", carrier.DeliveredHow, carrier.DeliveredWith)
	}
	if rider.DeliveredHow != store.ThreadWakeQueued || rider.DeliveredWith != carrier.Token {
		t.Errorf("rider = delivered %q with %q, want the carrier's message", rider.DeliveredHow, rider.DeliveredWith)
	}
	// thread_status on the request that rode along still says its message is
	// on the way: it reads the carrier's.
	if !f.adapter().wakeQueued(rider, false) {
		t.Error("the request delivered by another request's message does not report that message as queued")
	}
	f.nothingOwed(t)

	// A restart finds the message still queued: it never reached the agent,
	// and it goes back to every request it answered, owed again and
	// disarmed, rather than to the composer.
	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()
	for _, token := range tokens {
		row := after.request(t, token)
		if row.DeliveredAt != 0 || row.DeliveredWith != "" || row.Notify || !row.WakeOwed(false) {
			t.Errorf("request %s after the restart = delivered %q at %d with %q, notify %v; want owed again and disarmed",
				token, row.DeliveredHow, row.DeliveredAt, row.DeliveredWith, row.Notify)
		}
	}
	if rows := durableQueueRows(t, after.app, f.caller.ID); len(rows) != 0 {
		t.Errorf("queue after the restart = %+v, want the message given back", rows)
	}
}

// A session that ends during the turn that read two of one sender's
// requests tells the sender once, as the turn's own end would have.
func TestASessionEndDuringOneTurnAnsweringTwoRequestsWakesTheirSenderOnce(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	q.notify = true
	first, second := q.queue(t, 1, 1), q.queue(t, 2, 1)
	first.bind(t)
	second.bind(t)
	q.endTurn(t)
	q.startPickupTurn(t)
	first.consumed(t)
	second.consumed(t)
	q.say(t, 1, "halfway through both")
	f.app.settleReceiptsForEndedSession(q.target.ID, q.session)

	wakes := f.wakesQueuedFor(t, first.token, second.token)
	if len(wakes) != 1 {
		t.Fatalf("queued wakes = %+v, want one message for the one session end", wakes)
	}
	if !strings.Contains(wakes[0].Message, "halfway through both") {
		t.Fatalf("wake = %q, want what the turn wrote before the session ended", wakes[0].Message)
	}
	f.nothingOwed(t)
}

// A restore that settles two of one sender's requests as never read, because
// the provider did not take them in or their queue refused them again,
// tells the sender once.
func TestARestoreSettlingTwoRequestsUnreadWakesTheirSenderOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		settle func(t *testing.T, q *queuedRequests, requests []*queuedRequest)
	}{
		{"not taken in", func(t *testing.T, q *queuedRequests, requests []*queuedRequest) {
			var unread []unreadRequest
			for _, r := range requests {
				r.bind(t)
				unread = append(unread, unreadRequest{token: r.token, why: threadtools.CauseSessionEndedUnread})
			}
			q.f.app.settleNotTakenIn(q.target.ID, []notTakenIn{{unread: unread}})
		}},
		{"queue refused at teardown", func(t *testing.T, q *queuedRequests, requests []*queuedRequest) {
			if _, _, err := q.f.app.store.ArchiveThread(q.target.ID); err != nil {
				t.Fatalf("ArchiveThread: %v", err)
			}
			var owed teardownRequests
			for _, r := range requests {
				owed.queued = append(owed.queued, returnedRequest{token: r.token, message: storedAgentMessage(r.token, r.row.Summary, agentmeta.OriginAgentThread, nil, nil)})
			}
			q.f.app.giveBackTeardownRequests(q.target.ID, owed)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newQueuedRequests(t)
			f := q.f
			f.mockClaudeHoldingTheTurn(t, "mid-thought")
			f.holdCallerTurn(t)
			q.notify = true
			requests := []*queuedRequest{q.queue(t, 1, 1), q.queue(t, 2, 1)}
			tc.settle(t, q, requests)

			wakes := f.wakesQueuedFor(t, requests[0].token, requests[1].token)
			if len(wakes) != 1 {
				t.Fatalf("queued wakes = %+v, want one message for the one restore", wakes)
			}
			f.nothingOwed(t)
		})
	}
}

// A restart interrupts every open request. A thread with requests open on
// several threads is told in one message which of them it interrupted,
// attributed to the first of them as an agent's message, never the
// person's.
func TestARestartTellsASenderOnceAboutEveryInterruptedRequest(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	first := f.runningRequest(t, "keep going")
	second := f.runningRequest(t, "you too")

	after := f.restart(t)
	after.app.sweepThreadRequestsAtBoot()
	after.app.finishThreadRequestSweepAtBoot()

	wakes := after.wakesQueuedFor(t, first.Token, second.Token)
	if len(wakes) != 1 {
		t.Fatalf("queued wakes = %+v, want one message for the restart", wakes)
	}
	message := wakes[0].Message
	if !strings.HasPrefix(message, "2 threads were interrupted by a restart of this computer.\n") {
		t.Fatalf("wake = %q, want the restart said once for both threads", message)
	}
	for _, ack := range []threadtools.RequestAck{first, second} {
		if !strings.Contains(message, "("+ack.ThreadID+") [token "+ack.Token+"]") {
			t.Errorf("wake = %q, want thread %s named with token %s", message, ack.ThreadID, ack.Token)
		}
	}
	var payload flushqueue.Payload
	if err := json.Unmarshal(wakes[0].Payload, &payload); err != nil {
		t.Fatalf("decode wake payload: %v", err)
	}
	if payload.Origin != agentmeta.OriginAgentThread || payload.OriginThread == nil ||
		payload.OriginThread.Kind != agentmeta.OriginKindWake || payload.OriginThread.Token != first.Token {
		t.Errorf("wake payload = %+v, want an agent's wake attributed to its carrier %s", payload, first.Token)
	} else if facts := payload.OriginThread.Facts; facts == nil || facts.Outcome != threadtools.WakeInterrupted ||
		facts.StoppedBy != "a restart of this computer" || len(facts.Threads) != 2 ||
		facts.Threads[0].ThreadID != first.ThreadID || !slices.Equal(facts.Threads[0].Tokens, []string{first.Token}) ||
		facts.Threads[1].ThreadID != second.ThreadID || !slices.Equal(facts.Threads[1].Tokens, []string{second.Token}) {
		t.Errorf("wake facts = %+v, want both threads with their tokens, stopped by the restart", payload.OriginThread.Facts)
	}
	after.nothingOwed(t)
}

// A thread that goes away with two of one sender's requests open tells the
// sender once.
func TestDeletingATargetTellsASenderOnceAboutBothRequests(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	f.mockClaudeHoldingTheTurn(t, "starting")
	f.holdCallerTurn(t)
	running := f.runningRequest(t, "what does the log say?")
	queued, err := f.adapter().Send(t.Context(), f.callerIdentity(), threadtools.SendCall{
		ThreadID: running.ThreadID, Message: "and the trace?", Notify: true,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := f.app.deleteThread(running.ThreadID); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	wakes := f.wakesQueuedFor(t, running.Token, queued.Token)
	if len(wakes) != 1 {
		t.Fatalf("queued wakes = %+v, want one message for the deletion", wakes)
	}
	message := wakes[0].Message
	if !strings.Contains(message, "errored: The thread was deleted before it answered.") ||
		!strings.Contains(message, running.Token) || !strings.Contains(message, queued.Token) {
		t.Fatalf("wake = %q, want the deletion said once with both tokens", message)
	}
}

// Answers that differ are separate messages, however they are delivered
// together, and one message answers a bounded number of requests.
func TestWakesFoldOnlyWhenTheySayTheSameThing(t *testing.T) {
	t.Parallel()
	f := newRequestFixture(t)
	// Each request is made a millisecond after the last, so "oldest first"
	// is the order they are seeded in and not the order of their tokens.
	made := time.Now().UnixMilli()
	settled := func(answer string) threadWakeRef {
		token := uuid.NewString()
		made++
		if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
			Token: token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
			State: store.ThreadRequestRunning, TargetThreadID: f.caller.ID, Notify: true,
			CreatedAt: made, UpdatedAt: made,
		}); err != nil {
			t.Fatalf("InsertThreadRequest: %v", err)
		}
		if _, err := f.app.store.SettleThreadRequest(token, store.ThreadRequestOpenStates(), store.ThreadRequestSettlement{
			State: store.ThreadRequestFinished, Answer: []byte(answer), AnswerKind: store.ThreadAnswerFinal,
		}); err != nil {
			t.Fatalf("SettleThreadRequest: %v", err)
		}
		return threadWakeRef{token: token}
	}

	messages := f.app.threadWakeMessages([]threadWakeRef{settled("tests pass"), settled("lint fails"), settled("tests pass")})
	if len(messages) != 2 || len(messages[0].rows) != 2 || len(messages[1].rows) != 1 {
		t.Fatalf("messages = %d with %v rows, want the two that agree folded and the other alone", len(messages), messageSizes(messages))
	}

	var many []threadWakeRef
	for range threadWakeMessageRequests + 1 {
		many = append(many, settled("same"))
	}
	messages = f.app.threadWakeMessages(many)
	if len(messages) != 2 || len(messages[0].rows) != threadWakeMessageRequests || len(messages[1].rows) != 1 {
		t.Fatalf("messages = %v, want one full message and the overflow in a second", messageSizes(messages))
	}
}

func messageSizes(messages []threadWakeMessage) []int {
	sizes := make([]int, 0, len(messages))
	for _, message := range messages {
		sizes = append(sizes, len(message.rows))
	}
	return sizes
}
