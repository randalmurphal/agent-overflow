package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadmode"
	"agent-overflow/internal/triage"
)

// queuedRequest is a request whose message was queued while turn 0 of the
// target runs: the row sits in turn 0, and the message opens turn 1 if the
// provider picks it up after turn 0 instead of draining it.
type queuedRequest struct {
	f      *requestFixture
	target store.Thread
	token  string
	row    store.Item
}

func newQueuedRequest(t *testing.T) *queuedRequest {
	t.Helper()
	f := newRequestFixture(t)
	target, err := createTestThread(t, f.app, string(provider.Claude), t.TempDir(), "claude-opus-4-7", threadmode.ModeChat)
	if err != nil {
		t.Fatalf("create target thread: %v", err)
	}
	q := &queuedRequest{f: f, target: target, token: newThreadRequestToken()}
	q.handle(t, "turn 0 start", provider.ProviderEvent{Kind: provider.EventTurnStart, TurnIndex: 0})
	q.say(t, 0, "turn zero's own last words")

	now := time.Now().UnixMilli()
	q.row = store.Item{
		ID: "user:0:flush:1", ThreadID: target.ID, TurnIndex: 0, Kind: "user_text", Role: "user",
		Status: "completed", Summary: "what did the backfill touch?", CreatedAt: now, UpdatedAt: now,
	}
	if err := f.app.triage.PersistAndRegisterPendingQuietFlushSendWithExpectation(
		target.ID, "queue-1", q.row, 1, now, triage.PendingSendExpectation{ProviderItemID: "echo-1"},
	); err != nil {
		t.Fatalf("register queued send: %v", err)
	}
	if err := f.app.store.InsertThreadRequest(store.ThreadRequest{
		Token: q.token, CallerThreadID: f.caller.ID, Kind: store.ThreadRequestSend,
		TargetThreadID: target.ID, State: store.ThreadRequestAccepted,
	}); err != nil {
		t.Fatalf("InsertThreadRequest: %v", err)
	}
	if _, _, err := f.app.store.AcceptThreadRequestReceipt(store.ThreadRequestReceipt{
		Token: q.token, OwnerDeviceID: threadReceiptLocalOwner, SourceThreadID: f.caller.ID,
		Kind: store.ThreadRequestSend, TargetThreadID: target.ID,
	}); err != nil {
		t.Fatalf("AcceptThreadRequestReceipt: %v", err)
	}
	return q
}

func (q *queuedRequest) handle(t *testing.T, what string, evt provider.ProviderEvent) {
	t.Helper()
	evt.ThreadID = q.target.ID
	evt.Timestamp = time.Now()
	if err := q.f.app.triage.Handle(evt); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func (q *queuedRequest) say(t *testing.T, turnIndex int, text string) {
	t.Helper()
	now := time.Now().UnixMilli()
	if _, err := q.f.app.store.UpsertItem(store.Item{
		ID: q.target.ID + ":a" + string(rune('0'+turnIndex)), ThreadID: q.target.ID, TurnIndex: turnIndex, ItemIndex: 5,
		Kind: "assistant_text", Role: "assistant", Status: "completed", Summary: text, CreatedAt: now, UpdatedAt: now,
	}, nil); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
}

func (q *queuedRequest) endTurn(t *testing.T) {
	t.Helper()
	q.handle(t, "turn end", provider.ProviderEvent{Kind: provider.EventTurnComplete, TurnComplete: &provider.WireTurnCompleteMeta{StopReason: "end_turn"}})
}

// pickUp is the provider taking the message after the running turn ended:
// an init that opens turn 1, then the message's echo.
func (q *queuedRequest) pickUp(t *testing.T) {
	t.Helper()
	meta, err := json.Marshal(provider.SessionInfo{TurnOpener: provider.TurnOpenerCommand})
	if err != nil {
		t.Fatal(err)
	}
	q.handle(t, "pickup init", provider.ProviderEvent{Kind: provider.EventInit, Meta: meta})
	q.echo(t)
}

func (q *queuedRequest) echo(t *testing.T) {
	t.Helper()
	q.handle(t, "echo", provider.ProviderEvent{Kind: provider.EventUserText, Content: q.row.Summary, Meta: json.RawMessage(`{"provider_item_id":"echo-1"}`)})
}

func (q *queuedRequest) bind(t *testing.T) {
	t.Helper()
	q.f.adapter().markRequestRunning(q.token, q.target.ID, q.row)
}

func (q *queuedRequest) observeTurnEnd(t *testing.T) {
	t.Helper()
	if err := q.f.app.settleReceiptOnTurnEnd(q.target.ID, q.token); err != nil {
		t.Fatalf("settleReceiptOnTurnEnd: %v", err)
	}
}

func (q *queuedRequest) wantRunningIn(t *testing.T, when string, turnIndex int) {
	t.Helper()
	receipt := q.f.receipt(t, q.token)
	if receipt.State != store.ThreadReceiptRunning {
		t.Fatalf("%s: receipt = %q answering %q, want it still running", when, receipt.State, receipt.Answer)
	}
	if want := threadRequestTurnKey(q.target.ID, turnIndex); receipt.TurnID != want {
		t.Fatalf("%s: receipt turn = %q, want %q", when, receipt.TurnID, want)
	}
}

func (q *queuedRequest) wantFinishedWith(t *testing.T, text string) {
	t.Helper()
	receipt := q.f.receipt(t, q.token)
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
	q := newQueuedRequest(t)
	q.bind(t)
	q.wantRunningIn(t, "bound while turn 0 runs", 0)

	q.endTurn(t)
	q.observeTurnEnd(t)
	q.wantRunningIn(t, "after turn 0 ended without the message", 1)

	q.pickUp(t)
	q.observeTurnEnd(t)
	q.wantRunningIn(t, "while turn 1 runs", 1)

	q.say(t, 1, "the backfill touched 41,220 rows")
	q.endTurn(t)
	q.observeTurnEnd(t)
	q.wantFinishedWith(t, "41,220 rows")
}

// The observer for turn 0 can run after the provider already picked the
// message up, and after the pending entry is gone: the receipt still moves.
func TestAQueuedRequestMovesWhenTheObserverRunsAfterThePickup(t *testing.T) {
	t.Parallel()
	q := newQueuedRequest(t)
	q.bind(t)
	q.endTurn(t)
	q.pickUp(t)

	q.observeTurnEnd(t)
	q.wantRunningIn(t, "turn 0's end observed during turn 1", 1)

	q.say(t, 1, "the backfill touched 41,220 rows")
	q.endTurn(t)
	q.observeTurnEnd(t)
	q.wantFinishedWith(t, "41,220 rows")
}

// A message the provider drains into the running turn is answered there.
func TestAQueuedRequestDrainedIntoTheRunningTurnSettlesThere(t *testing.T) {
	t.Parallel()
	q := newQueuedRequest(t)
	q.bind(t)
	q.echo(t)
	q.endTurn(t)
	q.observeTurnEnd(t)
	q.wantFinishedWith(t, "turn zero's own last words")
}

// The binding can land after turn 0 ended. A message still waiting, or one
// already picked up, binds to turn 1 and is not settled on turn 0.
func TestALateBindingOfAQueuedRequestSkipsTheTurnThatDidNotReadIt(t *testing.T) {
	t.Parallel()
	t.Run("still waiting", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequest(t)
		q.endTurn(t)
		q.bind(t)
		q.wantRunningIn(t, "bound after turn 0 ended", 1)
	})
	t.Run("picked up", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequest(t)
		q.endTurn(t)
		q.pickUp(t)
		q.bind(t)
		q.wantRunningIn(t, "bound during turn 1", 1)
	})
	t.Run("picked up and answered", func(t *testing.T) {
		t.Parallel()
		q := newQueuedRequest(t)
		q.endTurn(t)
		q.pickUp(t)
		q.say(t, 1, "the backfill touched 41,220 rows")
		q.endTurn(t)
		q.bind(t)
		q.wantFinishedWith(t, "41,220 rows")
	})
}
