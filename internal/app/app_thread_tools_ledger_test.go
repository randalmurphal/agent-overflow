package app

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
)

// What each computer owes the other for one request, over the real wire:
// the acknowledgement the source owes once it holds an answer, and the
// record of a newer reply the destination dropped before the source could
// collect it.

// faultyReachPair is a reachPair whose destination is served through the
// fault wrapper.
func faultyReachPair(t *testing.T) (*reachPair, *peerFaults) {
	t.Helper()
	var faults *peerFaults
	pair := newReachPairServedBy(t, func(t *testing.T, app *App) *pairedBackend {
		wire, served := serveFaultyApp(t, app)
		faults = served
		return wire
	})
	return pair, faults
}

// settledThere starts a request on the destination whose turn stays open,
// and settles its receipt there with answer, the way a turn end does.
func (p *reachPair) settledThere(t *testing.T, kind, answer string) string {
	t.Helper()
	installMockClaudeTurns(t, p.dest, [][]string{{mockClaudeInitLine}})
	// The answer can wake the caller here, whose session then starts.
	installMockClaudeReplies(t, p.source, "noted")
	spawn := p.spawnThere(t, "work over there", nil)
	waitUntilE2E(t, 30*time.Second, "the destination starts "+spawn.Token, func() bool {
		receipt, found, err := p.dest.store.GetThreadRequestReceipt(spawn.Token)
		return err == nil && found && receipt.State == store.ThreadReceiptRunning
	})
	answerKind := store.ThreadAnswerFinal
	if kind == store.ThreadReceiptReplied {
		answerKind = store.ThreadAnswerReply
	}
	result, err := p.dest.store.SettleLocalThreadRequest(spawn.Token, store.ThreadReceiptOpenStates(),
		store.ThreadRequestSettlement{State: kind, Answer: []byte(answer), AnswerKind: answerKind},
		store.ThreadRequestStateForReceipt(kind))
	if err != nil || !result.Settled {
		t.Fatalf("settle the receipt there: settled=%v err=%v", result.Settled, err)
	}
	return spawn.Token
}

// A late reply the destination wrote and then dropped, its hold run out
// while this computer was away, is a revision the source never read. The
// source keeps the first answer, records that a later reply arrived and
// when, and only then tells the destination the revision is held; the
// calling agent reads both on thread_status.
func TestALateReplyThatExpiredUncollectedIsRecordedBeforeItIsAcknowledged(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestFinished || string(row.Answer) != "the first answer" {
		t.Fatalf("collected %q %q, want the first answer", row.State, row.Answer)
	}

	// The reply arrives there while this computer is away, and its hold
	// runs out before this computer is back.
	lateAt := time.Now().UnixMilli()
	if stored, err := pair.dest.store.StoreThreadReceiptLateReply(token, []byte("a correction"), lateAt, nil); err != nil || !stored {
		t.Fatalf("store the late reply there: stored=%v err=%v", stored, err)
	}
	held := pair.receipt(t, token)
	pair.dest.expireThreadRequests(time.UnixMilli(held.ExpiresAt))
	if expired := pair.receipt(t, token); expired.State != store.ThreadReceiptExpired || expired.Revision != 2 {
		t.Fatalf("the hold ran out and the receipt is %q at revision %d", expired.State, expired.Revision)
	}

	pair.poll(t)
	row := pair.request(t, token)
	if row.State != store.ThreadRequestFinished || string(row.Answer) != "the first answer" {
		t.Fatalf("the first answer was not kept: %q %q", row.State, row.Answer)
	}
	if row.Polling {
		t.Fatal("still polled for a late reply after the one it had expired")
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != receipt.Revision {
		t.Fatalf("the destination still waits for revision %d, holds %d acknowledged", receipt.Revision, receipt.CollectedRevision)
	}
	status := pair.call(t, "thread_status", `{"tokens":["`+token+`"]}`)
	requests, _ := status["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("thread_status listed %d requests: %+v", len(requests), status)
	}
	state, _ := requests[0].(map[string]any)
	if state["answer"] != "the first answer" {
		t.Fatalf("thread_status answers %v, want the first answer", state["answer"])
	}
	if at, _ := state["late_reply_expired_at"].(float64); int64(at) != lateAt {
		t.Fatalf("thread_status does not say a later reply arrived at %d and expired: %+v", lateAt, state)
	}
}

// An acknowledgement that does not arrive is owed, not dropped: a replied
// request leaves the poll the moment it is collected, so nothing else would
// ever tell the destination it may let the answer go. It is sent again,
// across a restart of the source, until the destination takes it.
func TestAnAcknowledgementThatFailsIsOwedUntilTheDestinationTakesIt(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptReplied, "the answer")

	faults.refuseAcks.Store(true)
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestReplied || string(row.Answer) != "the answer" {
		t.Fatalf("collected %q %q, want the reply", row.State, row.Answer)
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != 0 {
		t.Fatalf("the destination recorded an acknowledgement it refused: %+v", receipt)
	}
	faults.refuseAcks.Store(false)

	// A second App over the same database: nothing of the first one's
	// memory is left but what it wrote.
	restarted, _ := setupE2EAppOn(t, pair.dbPath, pair.settingsDir)
	restarted.configDir = pair.source.configDir
	restarted.backends = pair.manager
	publishReachIdentity(t, restarted, pair.sourceID)
	pair.pollOn(t, restarted)

	if receipt := pair.receipt(t, token); receipt.CollectedRevision != receipt.Revision {
		t.Fatalf("the owed acknowledgement never reached the destination: collected %d of %d", receipt.CollectedRevision, receipt.Revision)
	}
}

// A status answer about a revision the asker already holds names the
// revision and its state, not the answer again: a settled request is asked
// about for a late reply for a day, and its answer can be large.
func TestAStatusAnswerOmitsTheTextOfARevisionTheAskerHolds(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "a long final answer")
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestFinished {
		t.Fatalf("collected %q, want finished", row.State)
	}

	var mu sync.Mutex
	var answered []ThreadPeerRequest
	record := func(answer *ThreadPeerRequest) {
		mu.Lock()
		defer mu.Unlock()
		answered = append(answered, *answer)
	}
	faults.reword.Store(&record)
	pair.poll(t)
	mu.Lock()
	defer mu.Unlock()
	if len(answered) != 1 {
		t.Fatalf("the poll asked about %d requests, want the one waiting for a late reply", len(answered))
	}
	if got := answered[0]; got.Answer != "" || got.Revision != 1 || got.State != store.ThreadReceiptFinished {
		t.Fatalf("the answer about a held revision is %q at %d, %q: want the revision and state alone", got.Answer, got.Revision, got.State)
	}
	if row := pair.request(t, token); string(row.Answer) != "a long final answer" {
		t.Fatalf("the compact answer changed the row: %q", row.Answer)
	}
	answered = nil
	mu.Unlock()

	// An asker that names no held revision (v0.0.19) is answered in full.
	full, err := pair.source.callThreadRequestStatus(pair.computer, ThreadPeerPoll{Tokens: []string{token}})
	if err != nil || len(full.Requests) != 1 || full.Requests[0].Answer != "a long final answer" {
		t.Fatalf("an asker that holds nothing named was answered %+v, %v", full.Requests, err)
	}
	// An answerer that ignores it (v0.0.19) answers in full, which is read
	// as already collected.
	ignore := func(poll *ThreadPeerPoll) { poll.Held = nil }
	faults.asked.Store(&ignore)
	mu.Lock()
	answered = nil
	mu.Unlock()
	before := pair.request(t, token)
	pair.poll(t)
	mu.Lock()
	if len(answered) != 1 || answered[0].Answer != "a long final answer" {
		t.Fatalf("an answerer that ignores the held revision answered %+v", answered)
	}
	if after := pair.request(t, token); string(after.Answer) != "a long final answer" || after.Revision != before.Revision || after.LateReplyAt != 0 {
		t.Fatalf("a full answer about a held revision changed the row: %+v", after)
	}
}

// Both of a request's replies can expire there before this computer asks at
// all: the first answer, and the late reply that extended its hold. The
// first collection settles the request `expired` and records, in the same
// write, that a later reply arrived and when, at the revision it then
// acknowledges; thread_status and the Agents row both say so.
func TestALateReplyThatExpiredWithTheFirstAnswerIsRecordedOnTheFirstCollection(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	lateAt := time.Now().UnixMilli()
	if stored, err := pair.dest.store.StoreThreadReceiptLateReply(token, []byte("a correction"), lateAt, nil); err != nil || !stored {
		t.Fatalf("store the late reply there: stored=%v err=%v", stored, err)
	}
	held := pair.receipt(t, token)
	pair.dest.expireThreadRequests(time.UnixMilli(held.ExpiresAt))
	if expired := pair.receipt(t, token); expired.State != store.ThreadReceiptExpired || expired.Revision != 2 || expired.CollectedRevision != 0 {
		t.Fatalf("the receipt there is %q at revision %d, %d collected", expired.State, expired.Revision, expired.CollectedRevision)
	}

	pair.poll(t)
	row := pair.request(t, token)
	if row.State != store.ThreadRequestExpired || row.Revision != 2 || !row.LateReplyExpired || row.LateReplyAt != lateAt {
		t.Fatalf("collected %q at revision %d, late reply expired=%v at %d; want expired at 2 with the late reply at %d",
			row.State, row.Revision, row.LateReplyExpired, row.LateReplyAt, lateAt)
	}
	if receipt := pair.receipt(t, token); receipt.CollectedRevision != 2 {
		t.Fatalf("the destination holds revision %d acknowledged, want 2", receipt.CollectedRevision)
	}
	status := pair.call(t, "thread_status", `{"tokens":["`+token+`"]}`)
	requests, _ := status["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("thread_status listed %d requests: %+v", len(requests), status)
	}
	state, _ := requests[0].(map[string]any)
	if at, _ := state["late_reply_expired_at"].(float64); state["state"] != "expired" || int64(at) != lateAt {
		t.Fatalf("thread_status does not say the request expired with a later reply at %d: %+v", lateAt, state)
	}
	if agent := agentRowOf(t, pair.source, pair.caller.ID, token); agent.State != "expired" || agent.LateReplyExpiredAt != lateAt {
		t.Fatalf("the Agents row is %q and says a later reply arrived at %d, want expired with it at %d", agent.State, agent.LateReplyExpiredAt, lateAt)
	}
}

// heldCancel parks the destination's reply to a thread_cancel once it has
// settled the request there, until release is closed.
func heldCancel(faults *peerFaults) (settled <-chan struct{}, release chan struct{}) {
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	hold := func(call ThreadPeerCall) {
		if call.Tool != "thread_cancel" {
			return
		}
		once.Do(func() { close(parked) })
		<-release
	}
	faults.answered.Store(&hold)
	return parked, release
}

// runningThere starts a request on the destination whose turn stays open,
// with its wake armed here.
func (p *reachPair) runningThere(t *testing.T) string {
	t.Helper()
	installMockClaudeTurns(t, p.dest, [][]string{{
		mockClaudeInitLine,
		`{"type":"assistant","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"working"}]}}`,
	}})
	installMockClaudeReplies(t, p.source, "noted")
	ack := p.spawnThere(t, "keep working", map[string]any{"notify": true})
	waitUntilE2E(t, 30*time.Second, "the destination runs "+ack.Token, func() bool {
		receipt, found, err := p.dest.store.GetThreadRequestReceipt(ack.Token)
		return err == nil && found && receipt.State == store.ThreadReceiptRunning
	})
	if row := p.request(t, ack.Token); !row.Notify {
		t.Fatalf("the spawn did not arm a wake: %+v", row)
	}
	return ack.Token
}

// An agent's own cancel is told to it once, by the call's own reply: a poll
// that reads the cancelled receipt while that reply is on its way collects
// the settlement and queues no message about it.
func TestAnAgentsOwnCancelIsToldByItsReplyAlone(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair, faults := faultyReachPair(t)
	token := pair.runningThere(t)
	settled, release := heldCancel(faults)

	cancelled := make(chan threadtools.CancelReport, 1)
	go func() {
		report, err := pair.adapter().Cancel(context.Background(), pair.callerIdentity(), threadtools.CancelCall{Token: token})
		if err != nil {
			t.Errorf("cancel: %v", err)
		}
		cancelled <- report
	}()
	<-settled
	pair.poll(t)
	if row := pair.request(t, token); row.State != store.ThreadRequestCancelled || row.DeliveredHow != "" {
		close(release)
		<-cancelled
		t.Fatalf("while the cancel's reply was on its way the poll left the request %q delivered %q, want cancelled and its delivery the reply's",
			row.State, row.DeliveredHow)
	}
	close(release)
	if report := <-cancelled; report.State != threadtools.RequestCancelled {
		t.Fatalf("the cancel reported %+v", report)
	}
	pair.source.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
	pair.source.threadRequestsWG.Wait()
	if row := pair.request(t, token); row.DeliveredHow != store.ThreadWakeInline {
		t.Fatalf("the cancellation reached the agent by %q, want its own call's reply alone", row.DeliveredHow)
	}
}

// A cancel whose reply never reached the agent leaves the cancellation owed:
// the wake tells it instead.
func TestAnAgentsCancelWhoseReplyWasLostIsToldByAWake(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	token := pair.runningThere(t)

	ctx, pending := withThreadToolsPending(context.Background())
	if _, err := pair.adapter().Cancel(ctx, pair.callerIdentity(), threadtools.CancelCall{Token: token}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	pending.run(false)
	pair.source.retryUndeliveredThreadWakes(time.Now().Add(time.Hour))
	waitUntilE2E(t, 30*time.Second, "the cancellation is told by a wake", func() bool {
		return pair.request(t, token).DeliveredHow == store.ThreadWakeQueued
	})
}

// A status answer leaves out each text the asker holds at the revision it
// names (the answer, an interruption's cause, the late reply), and sends
// what it does not hold: a holder of the first answer asking about a late
// reply gets the reply alone. An asker that names nothing gets everything.
func TestAStatusAnswerSendsOnlyTheTextsTheAskerLacks(t *testing.T) {
	t.Parallel()
	remotetest.Require(t)
	pair := newReachPair(t)
	answered := pair.settledThere(t, store.ThreadReceiptFinished, "a long final answer")
	late := pair.settledThere(t, store.ThreadReceiptFinished, "the first answer")
	if stored, err := pair.dest.store.StoreThreadReceiptLateReply(late, []byte("a correction"), 0, nil); err != nil || !stored {
		t.Fatalf("store the late reply there: stored=%v err=%v", stored, err)
	}
	installMockClaudeTurns(t, pair.dest, [][]string{{mockClaudeInitLine}})
	stopped := pair.spawnThere(t, "stopped there", nil).Token
	waitUntilE2E(t, 30*time.Second, "the destination starts "+stopped, func() bool {
		return pair.receipt(t, stopped).State == store.ThreadReceiptRunning
	})
	if result, err := pair.dest.store.SettleLocalThreadRequest(stopped, store.ThreadReceiptOpenStates(), store.ThreadRequestSettlement{
		State: store.ThreadReceiptInterrupted, Answer: []byte("half the work"), AnswerKind: store.ThreadAnswerFinal,
		Cause: threadtools.CauseTrayStop,
	}, store.ThreadRequestInterrupted); err != nil || !result.Settled {
		t.Fatalf("interrupt the receipt there: %+v %v", result, err)
	}
	tokens := []string{answered, late, stopped}
	ask := func(held map[string]int64) map[string]ThreadPeerRequest {
		t.Helper()
		poll := ThreadPeerPoll{Tokens: tokens}
		for token, revision := range held {
			poll.Held = append(poll.Held, ThreadPeerAck{Token: token, Revision: revision})
		}
		reply, err := pair.source.callThreadRequestStatus(pair.computer, poll)
		if err != nil {
			t.Fatalf("ask about %v holding %v: %v", tokens, held, err)
		}
		out := make(map[string]ThreadPeerRequest, len(reply.Requests))
		for _, request := range reply.Requests {
			out[request.Token] = request
		}
		return out
	}

	full := ask(nil)
	if full[answered].Answer != "a long final answer" || full[late].Answer != "the first answer" || full[late].LateReply != "a correction" ||
		full[stopped].Cause == nil || full[stopped].Cause.Written != "half the work" {
		t.Fatalf("an asker that holds nothing was not answered in full: %+v", full)
	}
	current := ask(map[string]int64{answered: 1, late: 2, stopped: 1})
	if got := current[answered]; got.Answer != "" || got.Revision != 1 {
		t.Errorf("the answer the asker holds was sent again: %+v", got)
	}
	if got := current[late]; got.LateReply != "" || got.Answer != "" || got.Revision != 2 {
		t.Errorf("the late reply the asker holds was sent again: %+v", got)
	}
	if got := current[stopped]; got.Cause != nil || got.Answer != "" || got.Revision != 1 {
		t.Errorf("the interruption the asker holds was sent again: %+v", got)
	}
	behind := ask(map[string]int64{late: 1})
	if got := behind[late]; got.LateReply != "a correction" || got.Answer != "" || got.Revision != 2 {
		t.Errorf("a holder of the first answer asking about the late reply got %+v, want the reply alone", got)
	}
}

// callThreadRequestStatus asks one computer about requests this one made
// there.
func (a *App) callThreadRequestStatus(computerID string, poll ThreadPeerPoll) (ThreadPeerPollReply, error) {
	reply, _, err := a.readThreadRequestStatus(computerID, poll)
	return reply, err
}
