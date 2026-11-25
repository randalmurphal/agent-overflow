package trackerapp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"agent-overflow/internal/store"
)

// failingBodies fails, while fail is set, the mirror's bodies read a
// ticket view's build makes.
type failingBodies struct {
	Store
	fail atomic.Bool
}

func (s *failingBodies) ListMirrorBodies(root int64, ids []string) ([]store.MirrorBody, error) {
	if s.fail.Load() {
		return nil, errStoreDown
	}
	return s.Store.ListMirrorBodies(root, ids)
}

// A person's refresh of a ticket whose changed document's view cannot be
// built answers that failure, as well as telling the subscribers; on
// both tracker shapes.
func TestATicketRefreshWhoseViewFailsAnswersTheFailure(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 1, jira)
			quiet(t, m.svc)
			fs := &failingBodies{Store: m.svc.store}
			m.svc.store = fs
			st.comment(issues[0], "Changed")
			fs.fail.Store(true)
			err := m.svc.RefreshSubscription(t.Context(), subs[0].ID)
			fs.fail.Store(false)
			if !errors.Is(err, errStoreDown) {
				t.Fatalf("the refresh answered %v; want the view's build failure", err)
			}
			if v := m.ticketView(t, subs[0].ID); v.Failure == nil || v.Failure.Kind != string(FailureInternal) {
				t.Fatalf("the subscribers were told %+v; want a failure outside the tracker", v.Failure)
			}
		})
	}
}

// A person's refresh of a ticket the tracker answers gone succeeds: the
// answer reaches the subscribers as the ticket's failure, as a tracker's
// failure of a board refresh does.
func TestATicketRefreshAnsweredGoneSucceeds(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 1, false)
	quiet(t, m.svc)
	st.setGone(issues[0], true)
	if err := m.svc.RefreshSubscription(t.Context(), subs[0].ID); err != nil {
		t.Fatalf("the refresh answered %v; want nil", err)
	}
	if v := m.ticketView(t, subs[0].ID); v.Failure == nil || v.Failure.Kind != string(FailureNotFound) {
		t.Fatalf("the subscribers were told %+v; want not found", v.Failure)
	}
}

// A read of a ticket stopped by its context before the tracker answered
// answers the stop for that ticket, not a success.
func TestATicketReadStoppedAnswersTheStop(t *testing.T) {
	t.Parallel()
	m, _, issues, _ := newRoundFixture(t, 1, false)
	quiet(t, m.svc)
	tp := m.svc.ticketPumpOf(m.conn.ID, issues[0])
	call := holdNext(m.p, "Details")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan ticketsResult, 1)
	go func() { done <- m.svc.readTickets(ctx, m.conn.ID, []*ticketPump{tp}, Interactive, true, true) }()
	<-call.arrived
	cancel()
	if res := <-done; res.over || !errors.Is(res.errOf(0), context.Canceled) {
		t.Fatalf("the stopped read %+v; want its ticket answered the stop", res)
	}
}
