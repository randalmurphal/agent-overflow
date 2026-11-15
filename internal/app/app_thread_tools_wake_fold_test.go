package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/flushqueue"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/usermessage"

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
	if err := f.app.ArchiveThread(f.caller.ID); err != nil {
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

	after := f.restart(t)
	after.app.restoreDurableFlushQueueAtBoot()
	for _, token := range tokens {
		if row := after.request(t, token); row.DeliveredHow != store.ThreadWakeDraft {
			t.Errorf("request %s = delivered %q after its message was restored to the composer, want draft", token, row.DeliveredHow)
		}
	}
}

// A restart interrupts every open request. A thread with requests open on
// several threads is told in one message, in the app's own voice, which of
// them it interrupted.
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
	if !strings.HasPrefix(message, "2 threads were interrupted, each for the same reason:\n") || !strings.HasSuffix(message, "\n"+threadReceiptRestarted) {
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
	if payload.Origin != usermessage.OriginApp || payload.OriginThread != nil {
		t.Errorf("wake payload = %+v, want the app's own notice", payload)
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

	if err := f.app.DeleteThread(running.ThreadID); err != nil {
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
