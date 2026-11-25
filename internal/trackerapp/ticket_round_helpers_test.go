package trackerapp

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// roundOf is connection id's ticket round; nil for none.
func (s *Service) roundOf(id string) *ticketRound {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subs.rounds[id]
}

// ticketTick is a read of one ticket, as its pump makes one: in full when
// full is set, which records an opening as a person's refresh does.
func (s *Service) ticketTick(tp *ticketPump, u Urgency, full bool) ticketsResult {
	return s.readTickets(tp.core.Context(), tp.key.connection, []*ticketPump{tp}, u, full, full)
}

// roundTick is a round at which every shown ticket of rd's connection is
// due, as one is once a poll interval passed since the last.
func (s *Service) roundTick(rd *ticketRound) ticketsResult {
	return s.roundRead(rd, time.Time{}, true)
}

// readSoon makes the tickets of issues on m's connection due a read at
// once, as a stored change of their cards does.
func (m *mirrorFixture) readSoon(issues ...tracker.IssueID) {
	m.svc.subMu.Lock()
	defer m.svc.subMu.Unlock()
	for _, issue := range issues {
		if tp := m.svc.subs.tickets[ticketKey{connection: m.conn.ID, issue: issue}]; tp != nil {
			m.svc.readNowLocked(tp)
		}
	}
}

// quiet waits until no ticket pump of s owes work: each first read has
// settled, no shown ticket is due a read now or owes a view, no build is in
// flight and no round is reading (its history read after
// its tickets settled included). A test calls it before comparing what the
// subscribers were told with what the service holds.
func quiet(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.subMu.Lock()
		now, busy := s.now(), ""
		for k, tp := range s.subs.tickets {
			switch {
			case tp.over || tp.core.DeadLocked():
			case tp.settled == 0:
				busy = fmt.Sprint(k.issue, " has its first read in flight")
			case tp.shownLocked() && !tp.due.After(now):
				busy = fmt.Sprint(k.issue, " is due a read")
			case tp.building > 0, tp.dirty != tp.built && tp.shownLocked():
				busy = fmt.Sprint(k.issue, " owes a view")
			}
		}
		for conn, rd := range s.subs.rounds {
			if rd.reading {
				busy = fmt.Sprint("the round of ", conn, " is reading")
			}
		}
		s.subMu.Unlock()
		if busy == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ticket pumps did not settle: %s", busy)
		}
		time.Sleep(time.Millisecond)
	}
}

// trackerState is the scripted tracker's issues for ticket tests: each
// issue's card, which its detail shows with a comment of its own, the
// issue it links when it links one, and the issues it no longer returns.
// When answered is set, each detail answers a minute after the one before
// and is final through its answer less two minutes (HistoryTo), as a
// provider derives it from the tracker's response time.
type trackerState struct {
	mu       sync.Mutex
	cards    map[tracker.IssueID]tracker.Card
	comments map[tracker.IssueID]string
	links    map[tracker.IssueID]tracker.IssueID
	gone     map[tracker.IssueID]bool
	answered time.Time
}

func (s *trackerState) detail(id tracker.IssueID) (tracker.Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	if !ok || s.gone[id] {
		return tracker.Detail{}, &trackerapi.StatusError{Tracker: "jira", Status: 404}
	}
	d := enteredAtStatusBegan(detailOfCard(c))
	if !s.answered.IsZero() {
		s.answered = s.answered.Add(time.Minute)
		d.HistoryTo = s.answered.Add(-2 * time.Minute)
	}
	d.Comments.Comments[0].Body = s.comments[id]
	// A ticket's view depends on its own issue alone unless it links one.
	d.Links = nil
	if to, ok := s.links[id]; ok {
		o := s.cards[to]
		d.Links = []tracker.Link{{ID: "l1", Type: tracker.LinkType{ID: "10000", Name: "Blocks", Outward: "blocks", Inward: "is blocked by"}, Outward: true,
			Issue: tracker.IssueRef{ID: to, Key: o.Key, Title: o.Title, Status: o.Status}}}
	}
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
// cards read) or as Linear (read in full). Its rounds read when a test
// runs them (roundTick) or a ticket is due a read at once; a failed read
// retries an hour later.
func newRoundFixture(t *testing.T, n int, jira bool) (*mirrorFixture, *trackerState, []tracker.IssueID, []TicketSubscription) {
	t.Helper()
	return newRoundFixtureThen(t, n, jira, func(*mirrorFixture) {})
}

// newRoundFixtureThen is newRoundFixture with beforeOpen run once the
// board is read, before the tickets open.
func newRoundFixtureThen(t *testing.T, n int, jira bool, beforeOpen func(*mirrorFixture)) (*mirrorFixture, *trackerState, []tracker.IssueID, []TicketSubscription) {
	t.Helper()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	st := &trackerState{cards: map[tracker.IssueID]tracker.Card{}, comments: map[tracker.IssueID]string{},
		links: map[tracker.IssueID]tracker.IssueID{}, gone: map[tracker.IssueID]bool{}}
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
	// Either tracker answers a cards read, as a Jira round's check reads
	// one.
	m.p.set(func(p *scripted) { p.detail, p.cards = st.detail, st.cardsRead })
	if jira {
		m.p.updatedMovesWithDetail(st.cardsRead)
	}
	beforeOpen(m)
	var subs []TicketSubscription
	for i, id := range issues {
		sub, err := m.svc.SubscribeTicket(m.conn.ID, id, true, untied)
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

// historyGate holds the first history write, once armed, until release:
// the round's history read, stored after its read of the tickets settled.
type historyGate struct {
	*store.Store
	arrived, release chan struct{}
	once             sync.Once
}

func (g *historyGate) ReplaceMirrorHistories(root, sweep int64, hs []store.MirrorHistory, catalog store.MirrorCatalog, read store.MirrorRead) ([]store.MirrorHistoryResult, store.MirrorCatalogWrite, error) {
	g.once.Do(func() {
		close(g.arrived)
		<-g.release
	})
	return g.Store.ReplaceMirrorHistories(root, sweep, hs, catalog, read)
}

// quiet waits for a round's history read, which the round makes after its
// read of the tickets settled them: what that read stores changes the
// views it is told to, so a test that compares them after quiet would race
// it.
func TestQuietWaitsForTheRoundsHistoryRead(t *testing.T) {
	t.Parallel()
	m, _, issues, _ := newRoundFixture(t, 1, false)
	quiet(t, m.svc)
	if r := m.recordOf(t, string(issues[0])); r.StatusFinal {
		t.Fatal("the ticket's status time is final; the round would read no history")
	}
	gate := &historyGate{Store: m.st, arrived: make(chan struct{}), release: make(chan struct{})}
	m.svc.store = gate
	release := sync.OnceFunc(func() { close(gate.release) })
	t.Cleanup(release)
	m.readSoon(issues[0])
	waitClosed(t, "the round's history write", gate.arrived)
	done := make(chan struct{})
	go func() {
		defer close(done)
		quiet(t, m.svc)
	}()
	select {
	case <-done:
		t.Fatal("quiet returned while the round's history read was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	<-done
}
