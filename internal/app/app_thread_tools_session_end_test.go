package app

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
	"agent-overflow/internal/threadtools"
	"agent-overflow/internal/triage"
)

// A session's end settles only the requests its own session was handed.
// Its pass runs off the event worker and can run after a replacement
// session has taken a request in: that request belongs to the replacement,
// whether it is a new one or one the ended session never took in, given
// back and dispatched again.

// replaceTargetSession makes the queued requests' target live in another
// session, as a replacement start does once the first one ended: triage
// admits only its frames, and the frames handle passes are its.
func (q *queuedRequests) replaceTargetSession(t *testing.T, token string) {
	t.Helper()
	q.f.app.triage.MarkThreadActive(q.target.ID, token)
	q.session = token
	q.putLiveSession(t, token)
}

// putLiveSession makes token the target's live session and nothing else:
// frames of the session before it are still handled, as one already past
// triage's gate is when the replacement starts.
func (q *queuedRequests) putLiveSession(t *testing.T, token string) {
	t.Helper()
	sess, ok := q.f.app.sessionManager().get(q.target.ID)
	if !ok {
		t.Fatal("fixture: the target has no session to replace")
	}
	sess.Token = token
	q.f.app.sessionManager().put(q.target.ID, sess)
}

// endSession is the observer handling the disconnect of session token.
func (q *queuedRequests) endSession(t *testing.T, token string) {
	t.Helper()
	q.f.app.dispatchTurnObservers(q.target.ID, provider.ProviderEvent{
		Kind: provider.EventSessionStatus, Content: "disconnected", ThreadID: q.target.ID, SessionToken: token,
	})
	q.f.app.threadRequestsWG.Wait()
}

func TestALateSessionEndLeavesTheReplacementsRequestsRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// take hands the replacement its request: r, queued while the
		// first session ran.
		take func(t *testing.T, q *queuedRequests, r *queuedRequest)
	}{
		{"a request the replacement took in", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			q.replaceTargetSession(t, "replacement")
			r.bind(t)
		}},
		{"a request given back and dispatched again", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			r.bind(t)
			if !q.f.adapter().returnRequestNotTakenIn(q.target.ID, r.token) {
				t.Fatal("fixture: the request was not given back")
			}
			q.replaceTargetSession(t, "replacement")
			r.bind(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newQueuedRequests(t)
			f := q.f
			f.mockClaudeHoldingTheTurn(t, "mid-thought")
			f.holdCallerTurn(t)
			q.notify = true
			q.endTurn(t)
			q.liveTarget(t)
			r := q.queue(t, 1, 1)
			tc.take(t, q, r)
			q.startPickupTurn(t)
			r.consumed(t)
			q.say(t, 1, "halfway there")
			r.wantRunningIn(t, "before the first session's end is settled", 1)

			// The first session's end is settled only now.
			q.endSession(t, queuedTargetSession)
			r.wantRunningIn(t, "after the first session's end", 1)

			// The replacement's own end settles it.
			q.endSession(t, "replacement")
			if row := f.request(t, r.token); row.State != store.ThreadRequestInterrupted {
				t.Fatalf("request = %s, want the replacement's end to settle it", row.State)
			}
		})
	}
}

// started is the provider's `started` for the request's message: it took
// the message in, before any turn reading it opened.
func (r *queuedRequest) started(t *testing.T) {
	t.Helper()
	meta, err := json.Marshal(provider.CommandLifecycleMeta{CommandUUID: r.echo, State: provider.CommandStarted})
	if err != nil {
		t.Fatal(err)
	}
	r.q.handle(t, "started", provider.ProviderEvent{Kind: provider.EventCommandLifecycle, ItemID: r.echo, Meta: meta})
}

// A session's report of a request's delivery can be handled after its
// replacement is live. The report names the session whose frame made it, so
// that session's end settles the request, as read and not answered, and the
// replacement's end leaves it alone.
func TestADeliveryHandledOnceTheReplacementIsLiveSettlesAtItsOwnSessionsEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		deliver func(t *testing.T, q *queuedRequests, r *queuedRequest)
		want    string
	}{
		{"taken in before its turn opened", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			r.started(t)
		}, threadtools.InterruptSessionEndedUnanswered},
		{"delivered into its turn", func(t *testing.T, q *queuedRequests, r *queuedRequest) {
			q.startPickupTurn(t)
			r.consumed(t)
			q.say(t, 1, "halfway there")
		}, threadtools.InterruptSessionEndedMidTurn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := newQueuedRequests(t)
			f := q.f
			f.mockClaudeHoldingTheTurn(t, "mid-thought")
			f.holdCallerTurn(t)
			q.notify = true
			q.endTurn(t)
			q.liveTarget(t)
			r := q.queue(t, 1, 1)
			r.bind(t)

			// The first session's frames are handled only once the
			// replacement is the thread's live session.
			q.putLiveSession(t, "replacement")
			tc.deliver(t, q, r)
			if got := f.app.receiptSession(q.target.ID, r.token); got != queuedTargetSession {
				t.Fatalf("gate names session %q, want %q, the one that reported the delivery", got, queuedTargetSession)
			}

			q.endSession(t, "replacement")
			r.wantRunning(t, "after the replacement's end")
			q.endSession(t, queuedTargetSession)
			receipt := f.receipt(t, r.token)
			if receipt.State != store.ThreadReceiptInterrupted || !strings.HasPrefix(string(receipt.Answer), tc.want) {
				t.Fatalf("receipt = %s %q, want interrupted with %q", receipt.State, receipt.Answer, tc.want)
			}
		})
	}
}

// A request the first session gave back is written again to the
// replacement. A report from the first session handled after that is about
// the earlier write: it binds nothing, the request stays the replacement's,
// and only the replacement's delivery and end settle it.
func TestAStaleReportDoesNotTakeARequestBackFromTheSessionItWasWrittenTo(t *testing.T) {
	t.Parallel()
	q := newQueuedRequests(t)
	f := q.f
	f.mockClaudeHoldingTheTurn(t, "mid-thought")
	f.holdCallerTurn(t)
	q.notify = true
	q.endTurn(t)
	q.liveTarget(t)
	r := q.queue(t, 1, 1)
	r.bind(t)
	if !f.adapter().returnRequestNotTakenIn(q.target.ID, r.token) {
		t.Fatal("fixture: the request was not given back")
	}

	q.putLiveSession(t, "replacement")
	f.app.noteReceiptWriting(q.target.ID, r.token, "replacement")
	f.adapter().markRequestDispatched(r.token, q.target.ID, "replacement", r.row.ID)

	// The first session's delivery of its own earlier write.
	q.startPickupTurn(t)
	r.started(t)
	r.consumed(t)
	if receipt := r.wantRunning(t, "after the first session's late report"); receipt.TurnID != "" {
		t.Fatalf("receipt turn = %q, want it unbound: the report was about the earlier write", receipt.TurnID)
	}
	if got := f.app.receiptSession(q.target.ID, r.token); got != "replacement" {
		t.Fatalf("gate names session %q, want the replacement's", got)
	}
	if f.app.receiptTakenIn(q.target.ID, r.token) {
		t.Fatal("the first session's late `started` marked the replacement's request taken in")
	}
	q.endSession(t, queuedTargetSession)
	r.wantRunning(t, "after the first session's end")

	f.app.bindDeliveredRequests(q.target.ID, triage.SendDelivery{
		UserItemID: r.row.ID, SendIDs: []string{threadRequestSendID(r.token)}, Members: []string{threadRequestSendID(r.token)},
		TurnIndex: 2, OwnTurn: true, Session: "replacement",
	})
	r.wantRunningIn(t, "after the replacement's delivery", 2)
	q.endSession(t, "replacement")
	if receipt := f.receipt(t, r.token); receipt.State != store.ThreadReceiptInterrupted {
		t.Fatalf("receipt = %s, want the replacement's end to settle it", receipt.State)
	}
}
