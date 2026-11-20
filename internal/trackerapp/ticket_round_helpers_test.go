package trackerapp

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// roundOf is connection id's ticket round; nil for none.
func (s *Service) roundOf(id string) *ticketRound {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subs.rounds[id]
}

// ticketTick is a pump's own read of its ticket, as its goroutine makes
// it: in full when full is set, which records an opening as a person's
// refresh does.
func (s *Service) ticketTick(tp *ticketPump, u Urgency, full bool) ticketsResult {
	return s.readOwnTicket(tp, u, full, full)
}

// trackerState is the scripted tracker's issues for ticket tests: each
// issue's card, which its detail shows with a comment of its own, and the
// issues it no longer returns.
type trackerState struct {
	mu       sync.Mutex
	cards    map[tracker.IssueID]tracker.Card
	comments map[tracker.IssueID]string
	gone     map[tracker.IssueID]bool
}

func (s *trackerState) detail(id tracker.IssueID) (tracker.Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	if !ok || s.gone[id] {
		return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 404}
	}
	d := detailOfCard(c)
	d.Comments.Comments[0].Body = s.comments[id]
	// No links: a ticket's view here depends on its own issue alone.
	d.Links = nil
	return d, nil
}

func (s *trackerState) cardsRead(asked []tracker.IssueID) (tracker.Cards, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out tracker.Cards
	for _, id := range asked {
		if c, ok := s.cards[id]; ok && !s.gone[id] {
			out.Cards = append(out.Cards, c)
		}
	}
	return out, nil
}

// edit changes issue's card, moving its update time a minute.
func (s *trackerState) edit(id tracker.IssueID, f func(*tracker.Card)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cards[id]
	f(&c)
	c.Updated = c.Updated.Add(time.Minute)
	s.cards[id] = c
}

// comment changes issue's comment, which moves no update time.
func (s *trackerState) comment(id tracker.IssueID, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.comments[id] = body
}

func (s *trackerState) setGone(id tracker.IssueID, gone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone[id] = gone
}

// newRoundFixture is a mirror fixture whose board holds n issues, 10001
// on, each open as a shown ticket whose first read settled, on a tracker
// shaped as Jira (its update time moves with its detail, checked by a
// cards read) or as Linear (read in full). Its rounds run only when a test
// runs them (roundTick).
func newRoundFixture(t *testing.T, n int, jira bool) (*mirrorFixture, *trackerState, []tracker.IssueID, []TicketSubscription) {
	t.Helper()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	st := &trackerState{cards: map[tracker.IssueID]tracker.Card{}, comments: map[tracker.IssueID]string{}, gone: map[tracker.IssueID]bool{}}
	var rows []tracker.Row
	var issues []tracker.IssueID
	for i := range n {
		r := row(fmt.Sprint(10001+i), 1_799_000_000_000, fmt.Sprintf("a%03d", i))
		rows = append(rows, r)
		issues = append(issues, r.Card.ID)
		st.cards[r.Card.ID], st.comments[r.Card.ID] = r.Card, "First"
	}
	m.tracks(rows)
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) { p.detail = st.detail })
	if jira {
		m.p.updatedMovesWithDetail(st.cardsRead)
	}
	var subs []TicketSubscription
	for i, id := range issues {
		sub, err := m.svc.SubscribeTicket(m.conn.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
		m.events.issueEvents(t, i+1)
	}
	return m, st, issues, subs
}

// settledTickets waits until every ticket pump of m's connection has
// settled its first read (settled).
func (m *mirrorFixture) settledTickets(t *testing.T) {
	t.Helper()
	settled(t, m.svc)
}

// round runs one round of m's connection, failing when it has none.
func (m *mirrorFixture) round(t *testing.T) ticketsResult {
	t.Helper()
	rd := m.svc.roundOf(m.conn.ID)
	if rd == nil {
		t.Fatal("no ticket round runs")
	}
	return m.svc.roundTick(rd)
}

// issueEventsSince are the issue events after the first n.
func (m *mirrorFixture) issueEventsSince(n int) []IssueEvent {
	_, issues, _ := m.events.snapshot()
	return issues[n:]
}

func (m *mirrorFixture) issueEventCount() int {
	_, issues, _ := m.events.snapshot()
	return len(issues)
}

// lastDetailsRead is the issues the last Details call read.
func (m *mirrorFixture) lastDetailsRead() []tracker.IssueID {
	reads := m.p.detailsRead()
	if len(reads) == 0 {
		return nil
	}
	return reads[len(reads)-1]
}

// ticketView is handle id's ticket as its subscribers were last told.
func (m *mirrorFixture) ticketView(t *testing.T, id string) TicketView {
	t.Helper()
	return must[TicketView](t)(m.svc.TicketDetail(id))
}

// waitClosed waits for c to close, failing after a bound no healthy run
// reaches.
func waitClosed(t *testing.T, what string, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not exit", what)
	}
}

func sortedIDs(ids []tracker.IssueID) []tracker.IssueID {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}
