package app

import (
	"testing"

	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// A session's end settles only the requests its own session was handed.
// Its pass runs off the event worker and can run after a replacement
// session has taken a request in: that request belongs to the replacement,
// whether it is a new one or one the ended session never took in, given
// back and dispatched again.

// replaceTargetSession makes the queued requests' target live in another
// session, as a replacement start does once the first one ended.
func (q *queuedRequests) replaceTargetSession(t *testing.T, token string) {
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
			q.endSession(t, "stop-target")
			r.wantRunningIn(t, "after the first session's end", 1)

			// The replacement's own end settles it.
			q.endSession(t, "replacement")
			if row := f.request(t, r.token); row.State != store.ThreadRequestInterrupted {
				t.Fatalf("request = %s, want the replacement's end to settle it", row.State)
			}
		})
	}
}
