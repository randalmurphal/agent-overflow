package app

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/buildvariant/remotetest"
	"agent-overflow/internal/store"
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
