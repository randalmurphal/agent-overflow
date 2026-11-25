package trackerapp

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// ticketStore fails the details' write or the cards a view reads while
// the test sets it.
type ticketStore struct {
	*store.Store
	failDetails, failViews atomic.Bool
}

func (s *ticketStore) StoreMirrorTickets(root, sweep int64, tickets []store.MirrorTicket, catalog store.MirrorCatalog, opened, read store.MirrorRead) (store.MirrorTicketsWrite, error) {
	if s.failDetails.Load() && slices.ContainsFunc(tickets, func(t store.MirrorTicket) bool { return t.Body != nil }) {
		return store.MirrorTicketsWrite{}, errStoreDown
	}
	return s.Store.StoreMirrorTickets(root, sweep, tickets, catalog, opened, read)
}

// ListMirrorBodies is the first read of a view's build.
func (s *ticketStore) ListMirrorBodies(root int64, issues []string) ([]store.MirrorBody, error) {
	if s.failViews.Load() {
		return nil, errStoreDown
	}
	return s.Store.ListMirrorBodies(root, issues)
}

// pumpOf is the pump of issue's open ticket on m's connection.
func (m *mirrorFixture) pumpOf(issue tracker.IssueID) *ticketPump {
	m.svc.subMu.Lock()
	defer m.svc.subMu.Unlock()
	return m.svc.subs.tickets[ticketKey{connection: m.conn.ID, issue: issue}]
}

// A Jira round checks two tickets: one is current, the other changed and
// its full read fails, at the tracker, by a rate limit or in the store.
// Only the ticket read in full is told the failure and goes on the retry
// schedule, which the other's success leaves running; the current one
// settles as read, due with the round's next regular read. The check
// stored the changed card in the issue record, so the failed ticket is
// told the failure with its view with the record's card over the body it
// held, in one event, and is not read again before its retry.
func TestARoundTellsItsFailureOnlyToTheTicketsItDidNotRead(t *testing.T) {
	t.Parallel()
	until := time.UnixMilli(1_900_000_000_000).UTC()
	for _, tc := range []struct {
		name    string
		details error
		store   bool
		kind    FailureKind
	}{
		{"tracker", &trackerapi.TransientError{Err: errors.New("reset")}, false, FailureTransient},
		{"rate limit", &trackerapi.RateLimitedError{Tracker: "jira", Until: until}, false, FailureRateLimited},
		{"store", nil, true, FailureInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 2, true)
			m.round(t)
			ts := &ticketStore{Store: m.st}
			m.svc.store = ts
			st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
			if tc.details != nil {
				m.p.set(func(p *scripted) {
					p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{}, tc.details }
				})
			}
			ts.failDetails.Store(tc.store)
			events := m.issueEventCount()
			if res := m.round(t); !res.failed {
				t.Fatalf("round %+v", res)
			}
			if got := m.lastDetailsRead(); !slices.Equal(got, issues[:1]) {
				t.Fatalf("the round read %v in full", got)
			}
			got := m.issueEventsSince(events)
			if len(got) != 1 || !got[0].Changed {
				t.Fatalf("events %+v", got)
			}
			for _, e := range got {
				if e.Issue != issues[0] || e.Failure == nil || e.Failure.Kind != string(tc.kind) {
					t.Fatalf("events %+v", got)
				}
			}
			if v := m.ticketView(t, subs[0].ID); v.Detail.Card.Title != "Renamed" || !v.Detail.BodyBehind {
				t.Fatalf("the ticket not read shows %q, body behind %v; want the record's card over the held body", v.Detail.Card.Title, v.Detail.BodyBehind)
			}
			if v := m.ticketView(t, subs[1].ID); v.Failure != nil {
				t.Fatalf("the ticket the round read shows %+v", v.Failure)
			}
			failed, read := m.pumpOf(issues[0]), m.pumpOf(issues[1])
			m.svc.subMu.Lock()
			defer m.svc.subMu.Unlock()
			rd := m.svc.subs.rounds[m.conn.ID]
			if !read.due.Equal(rd.next) {
				t.Errorf("the ticket read is due %v; the round's next read is %v", read.due, rd.next)
			}
			switch {
			case tc.details != nil && tc.kind == FailureRateLimited:
				if !failed.due.Equal(until) || !rd.hold.Equal(until) {
					t.Errorf("the ticket not read is due %v, the round held to %v; the limit lifts %v", failed.due, rd.hold, until)
				}
			case rd.delay == 0 || !failed.due.Equal(rd.retryAt):
				t.Errorf("the ticket not read is due %v; the retry schedule is at %v, step %v", failed.due, rd.retryAt, rd.delay)
			}
		})
	}
}

// A Linear round reads two tickets in full: the tracker leaves one out and
// the store refuses the other's document. Each is told its own outcome.
func TestARoundTellsEachTicketItsOwnOutcome(t *testing.T) {
	t.Parallel()
	m, st, issues, _ := newRoundFixture(t, 2, false)
	ts := &ticketStore{Store: m.st}
	m.svc.store = ts
	st.setGone(issues[1], true)
	ts.failDetails.Store(true)
	events := m.issueEventCount()
	m.round(t)
	got := m.issueEventsSince(events)
	kinds := map[tracker.IssueID]string{}
	for _, e := range got {
		if e.Failure != nil {
			kinds[e.Issue] = e.Failure.Kind
		}
	}
	if len(got) != 2 || kinds[issues[0]] != string(FailureInternal) || kinds[issues[1]] != string(FailureNotFound) {
		t.Fatalf("events %+v", got)
	}
}

// A Linear round of two tickets, one of which changed, whose view cannot
// be built: only that ticket is told the failure.
func TestAViewThatCannotBeBuiltFailsOnlyItsTicket(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 2, false)
	m.round(t)
	ts := &ticketStore{Store: m.st}
	m.svc.store = ts
	st.comment(issues[0], "Second")
	ts.failViews.Store(true)
	events := m.issueEventCount()
	m.round(t)
	got := m.issueEventsSince(events)
	if len(got) != 1 || got[0].Issue != issues[0] || got[0].Failure == nil || got[0].Failure.Kind != string(FailureInternal) {
		t.Fatalf("events %+v", got)
	}
	ts.failViews.Store(false)
	if v := m.ticketView(t, subs[1].ID); v.Failure != nil {
		t.Fatalf("the unchanged ticket shows %+v", v.Failure)
	}
}

// A Jira ticket whose view could not be built is checked by the next
// round against the mirror's copy, which that round confirms while its
// other ticket's full read fails: the confirmed ticket shows the copy.
func TestAnAnsweredTicketShowsItsDocumentWhenTheReadFails(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 2, true)
	m.round(t)
	ts := &ticketStore{Store: m.st}
	m.svc.store = ts
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	ts.failViews.Store(true)
	m.round(t)
	ts.failViews.Store(false)
	st.edit(issues[1], func(c *tracker.Card) { c.Title = "Renamed too" })
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{}, errors.New("test: refused") }
	})
	events := m.issueEventCount()
	m.round(t)
	var own []IssueEvent
	for _, e := range m.issueEventsSince(events) {
		if e.Issue == issues[0] {
			own = append(own, e)
		}
	}
	if len(own) != 1 || !own[0].Changed || own[0].Failure != nil {
		t.Fatalf("the confirmed ticket was told %+v", own)
	}
	if v := m.ticketView(t, subs[0].ID); v.Detail.Card.Title != "Renamed" {
		t.Fatalf("the confirmed ticket shows %q", v.Detail.Card.Title)
	}
}

// A Jira read of two tickets stopped while the changed one is read in
// full: the current one, which the check answered, settles; the other is
// not settled, and shows the card the check stored over the body it held,
// due a read at once for it.
func TestAStoppedReadSettlesTheTicketsItAnswered(t *testing.T) {
	t.Parallel()
	m, st, issues, _ := newRoundFixture(t, 2, true)
	m.round(t)
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	ctx, cancel := context.WithCancel(t.Context())
	m.p.set(func(p *scripted) {
		p.block = func(_ context.Context, name string) error {
			if name == "Details" {
				cancel()
				return context.Canceled
			}
			return nil
		}
	})
	unread, read := m.pumpOf(issues[0]), m.pumpOf(issues[1])
	m.svc.subMu.Lock()
	unreadSettled, readSettled := unread.settled, read.settled
	m.svc.subMu.Unlock()
	events := m.issueEventCount()
	m.svc.readTickets(ctx, m.conn.ID, []*ticketPump{unread, read}, Background, false, false)
	if got := m.issueEventsSince(events); len(got) != 1 || got[0].Issue != issues[0] || !got[0].Changed || got[0].Failure != nil {
		t.Fatalf("events %+v; want the unread ticket's new card", got)
	}
	m.svc.subMu.Lock()
	defer m.svc.subMu.Unlock()
	if read.settled == readSettled || unread.settled != unreadSettled {
		t.Fatalf("the answered ticket settled %d (was %d); the other settled %d (was %d)",
			read.settled, readSettled, unread.settled, unreadSettled)
	}
	if v := unread.view.detail; v.Card.Title != "Renamed" || !v.BodyBehind || unread.due.After(m.now()) {
		t.Fatalf("the unread ticket shows %q, behind %v, due %v", v.Card.Title, v.BodyBehind, unread.due)
	}
}

// A write's refresh reads two open tickets in full: the tracker leaves one
// out and the store refuses the other's document. Each ticket is told its
// own outcome, and only the detail not stored is stale.
func TestARefreshSettlesEachOpenTicketWithItsOwnRead(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	ts := &ticketStore{Store: f.st}
	f.svc.store = ts
	for i, id := range []tracker.IssueID{"10001", "10002"} {
		must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, id, untied))
		f.events.issueEvents(t, i+1)
	}
	f.settledTickets(t)
	var gone atomic.Bool
	f.p.set(func(p *scripted) {
		detail := p.detail
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			if id == "10002" && gone.Load() {
				return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 404}
			}
			return detail(id)
		}
	})
	gone.Store(true)
	ts.failDetails.Store(true)
	f.answer(tracker.Result{Parts: []tracker.Part{
		appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001")),
		appliedPart("10002", tracker.StepEdit, tracker.CardOf("10002"), tracker.DetailOf("10002")),
	}})
	_, before, _ := f.events.snapshot()
	view, err := f.svc.Edit(t.Context(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"New"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Stale) != 1 || len(view.Stale[0].Resources) != 1 || view.Stale[0].Resources[0].Issue != "10001" {
		t.Fatalf("stale %+v", view.Stale)
	}
	_, after, _ := f.events.snapshot()
	kinds := map[tracker.IssueID]string{}
	for _, e := range after[len(before):] {
		if e.Failure != nil {
			kinds[e.Issue] = e.Failure.Kind
		}
	}
	if len(after) != len(before)+2 || kinds["10001"] != string(FailureInternal) || kinds["10002"] != string(FailureNotFound) {
		t.Fatalf("events %+v", after[len(before):])
	}
}
