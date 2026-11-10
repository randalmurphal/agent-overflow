package app

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// queuedRequests is a target thread whose turn 0 runs while requests are
// queued behind it. Each request's row sits in turn 0, and its message
// reserved a turn of its own for a pickup after turn 0.
type queuedRequests struct {
	f      *requestFixture
	target store.Thread
}

// queuedRequest is one of them.
type queuedRequest struct {
	q     *queuedRequests
	token string
	row   store.Item
	echo  string
}

func newQueuedRequests(t *testing.T) *queuedRequests {
	t.Helper()
	f := newRequestFixture(t)
	// The consumed hook is what tells the receipts which turn answers.
	f.app.configureTriageQueueCallbacks()
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	q := &queuedRequests{f: f, target: target}
	q.handle(t, "turn 0 start", provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0})
	q.say(t, 0, "turn zero's own last words")
	return q
}

// queue adds the n-th request queued during turn 0, reserving responseTurn.
func (q *queuedRequests) queue(t *testing.T, n, responseTurn int) *queuedRequest {
	t.Helper()
	return q.queueDuring(t, 0, n, responseTurn)
}

// queueDuring adds the n-th request queued while rowTurn runs.
func (q *queuedRequests) queueDuring(t *testing.T, rowTurn, n, responseTurn int) *queuedRequest {
	t.Helper()
	r := &queuedRequest{q: q, token: newThreadRequestToken(), echo: "echo-" + strconv.Itoa(n)}
	meta, err := json.Marshal(map[string]string{"sendId": threadRequestSendID(r.token)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	r.row = store.Item{
		ID: "user:" + strconv.Itoa(rowTurn) + ":flush:" + strconv.Itoa(n), ThreadID: q.target.ID, TurnIndex: rowTurn, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "question " + strconv.Itoa(n), Meta: string(meta), CreatedAt: now, UpdatedAt: now,
	}
	if err := q.f.app.triage.PersistAndRegisterPendingQuietFlushSendWithExpectation(
		q.target.ID, "queue-"+strconv.Itoa(n), r.row, responseTurn, now, triage.PendingSendExpectation{ProviderItemID: r.echo},
	); err != nil {
		t.Fatalf("register queued send: %v", err)
	}
	if err := q.f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: r.token, CallerThreadID: q.f.caller.ID, Kind: store.ThreadRequestSend,
		TargetThreadID: q.target.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if _, _, err := q.f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: r.token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: q.f.caller.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: q.target.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	return r
}

func (q *queuedRequests) handle(t *testing.T, what string, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = q.target.ID
	evt.Timestamp = time.Now()
	if err := q.f.app.triage.Handle(evt); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func (q *queuedRequests) say(t *testing.T, turnIndex int, text string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := q.f.app.store.UpsertItem(store.Item{
		ID: q.target.ID + ":a" + strconv.Itoa(turnIndex), ThreadID: q.target.ID, TurnIndex: turnIndex, ItemIndex: 5,
		Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: text, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
}

func (q *queuedRequests) endTurn(t *testing.T) {
	t.Helper()
	q.handle(t, "turn end", provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
}

// startPickupTurn is the provider starting a turn for a queued message.
func (q *queuedRequests) startPickupTurn(t *testing.T) {
	t.Helper()
	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: provider.TurnOpenerCommand})
	if err != nil {
		t.Fatal(err)
	}
	q.handle(t, "pickup init", provider.ProviderEvent{Kind: provider.EventInit, Meta: meta})
}

// consumed is the provider's echo of the request's message.
func (r *queuedRequest) consumed(t *testing.T) {
	t.Helper()
	r.q.handle(t, "echo", provider.ProviderEvent{
		Kind: provider.EventUserText, Content: r.row.Summary,
		Meta: json.RawMessage(`{"provider_item_id":"` + r.echo + `"}`),
	})
}

// bind is the dispatch marking the request running once the provider has
// its message.
func (r *queuedRequest) bind(t *testing.T) {
	t.Helper()
	r.q.f.adapter().markRequestRunning(r.token, r.q.target.ID, r.row)
}

func (r *queuedRequest) observeTurnEnd(t *testing.T) {
	t.Helper()
	if err := r.q.f.app.settleReceiptOnTurnEnd(r.q.target.ID, r.token); err != nil {
		t.Fatalf("settleReceiptOnTurnEnd: %v", err)
	}
}

func (r *queuedRequest) wantRunning(t *testing.T, when string) store.ThreadRequestReceipt {
	t.Helper()
	receipt := r.q.f.receipt(t, r.token)
	if receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("%s: receipt = %q answering %q, want it still running", when, receipt.State, receipt.Answer)
	}
	return receipt
}

func (r *queuedRequest) wantRunningIn(t *testing.T, when string, turnIndex int) {
	t.Helper()
	receipt := r.wantRunning(t, when)
	if want := threadRequestTurnKey(r.q.target.ID, turnIndex); receipt.TurnID != want {
		t.Fatalf("%s: receipt turn = %q, want %q", when, receipt.TurnID, want)
	}
}

func (r *queuedRequest) wantFinishedWith(t *testing.T, text string) {
	t.Helper()
	receipt := r.q.f.receipt(t, r.token)
	if receipt.State != store.ThreadReceiptFinished {
		t.Fatalf("receipt = %q, want finished", receipt.State)
	}
	if !strings.Contains(string(receipt.Answer), text) {
		t.Fatalf("answer = %q, want %q", receipt.Answer, text)
	}
}

// A request queued behind a running turn is answered by the turn its message
// opens. The running turn ending first did not read the message, so its last
// words are not the answer and its end settles nothing.
func TestAQueuedRequestWaitsForTheTurnItsMessageOpens(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	r := q.queue(t, 1, 1)
	r.bind(t)

	q.endTurn(t)
	r.observeTurnEnd(t)
	r.wantRunning(t, "after turn 0 ended without the message")

	q.startPickupTurn(t)
	r.consumed(t)
	r.observeTurnEnd(t)
	r.wantRunningIn(t, "while turn 1 runs", 1)

	q.say(t, 1, "the backfill touched 41,220 rows")
	q.endTurn(t)
	r.observeTurnEnd(t)
	r.wantFinishedWith(t, "41,220 rows")
}

// A provider that starts the message's turn and never echoes the message
// still answers in that turn.
func TestAQueuedRequestIsAnsweredByATurnThatNeverEchoedIt(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	q.endTurn(t)
	q.startPickupTurn(t)

	r.observeTurnEnd(t)
	r.wantRunningIn(t, "turn 0's end observed during turn 1", 1)

	q.say(t, 1, "the backfill touched 41,220 rows")
	q.endTurn(t)
	r.observeTurnEnd(t)
	r.wantFinishedWith(t, "41,220 rows")
}

// A message the provider drains into the running turn is answered there.
func TestAQueuedRequestDrainedIntoTheRunningTurnSettlesThere(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	r.consumed(t)
	q.endTurn(t)
	r.observeTurnEnd(t)
	r.wantFinishedWith(t, "turn zero's own last words")
}

// The observers can run after the session that answered was replaced, which
// drops everything triage held about the send. The receipt already names the
// turn that answered.
func TestAQueuedRequestSettlesOnItsAnswerAfterTheSessionIsReplaced(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	q.endTurn(t)
	q.startPickupTurn(t)
	r.consumed(t)
	q.say(t, 1, "the backfill touched 41,220 rows")
	q.endTurn(t)

	q.f.app.triage.CleanupThread(q.target.ID)
	q.f.app.triage.MarkThreadActive(q.target.ID)

	r.observeTurnEnd(t)
	r.wantFinishedWith(t, "41,220 rows")
}

// Two requests queued in separate flushes reserve two turns. The provider
// takes the first at the boundary and answers both in the second's turn, so
// the first's reserved turn never opens. Both settle on the turn that ran.
func TestRequestsBatchedAtTheBoundarySettleOnTheTurnThatRan(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	first, second := q.queue(t, 1, 1), q.queue(t, 2, 2)
	first.bind(t)
	second.bind(t)

	q.endTurn(t)
	first.observeTurnEnd(t)
	second.observeTurnEnd(t)
	first.wantRunning(t, "first, after turn 0 ended")
	second.wantRunning(t, "second, after turn 0 ended")

	first.consumed(t)
	first.observeTurnEnd(t)
	first.wantRunning(t, "first, taken at the boundary")

	q.startPickupTurn(t)
	first.wantRunningIn(t, "first, once the turn that answers opened", 2)
	second.consumed(t)
	q.say(t, 2, "both answers, in one turn")
	q.endTurn(t)
	first.observeTurnEnd(t)
	second.observeTurnEnd(t)
	first.wantFinishedWith(t, "both answers")
	second.wantFinishedWith(t, "both answers")
}

// A confirmation can arrive after its turn closed, with another request
// waiting behind it. The request settles on the turn that took its message,
// not on the turn the waiting request reserved.
func TestALateConfirmationSettlesTheRequestOnTheTurnThatTookIt(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	first := q.queue(t, 1, 1)
	first.bind(t)
	q.endTurn(t)
	q.startPickupTurn(t)
	second := q.queueDuring(t, 1, 1, 2)
	second.bind(t)
	q.say(t, 1, "the answer to the first question")
	q.endTurn(t)

	first.consumed(t)
	first.observeTurnEnd(t)
	second.observeTurnEnd(t)
	first.wantFinishedWith(t, "the answer to the first question")
	second.wantRunning(t, "second, still waiting for its own turn")
}

// The binding can land after turn 0 ended, and after the provider consumed
// the message. It never settles on the turn that did not read the message.
func TestALateBindingOfAQueuedRequestSkipsTheTurnThatDidNotReadIt(t *testing.T) {
	t.Parallel()
	t.Run("still waiting", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequests(t)
		r := q.queue(t, 1, 1)
		q.endTurn(t)
		r.bind(t)
		r.wantRunning(t, "bound after turn 0 ended")
	})
	t.Run("consumed", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequests(t)
		r := q.queue(t, 1, 1)
		q.endTurn(t)
		q.startPickupTurn(t)
		r.consumed(t)
		r.bind(t)
		r.wantRunningIn(t, "bound during turn 1", 1)
	})
	t.Run("consumed and answered", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequests(t)
		r := q.queue(t, 1, 1)
		q.endTurn(t)
		q.startPickupTurn(t)
		r.consumed(t)
		q.say(t, 1, "the backfill touched 41,220 rows")
		q.endTurn(t)
		r.bind(t)
		r.wantFinishedWith(t, "41,220 rows")
	})
}
