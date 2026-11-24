package trackerapp

import (
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A returned detail holds a body of the rendered card's version or is
// marked behind (DetailView.BodyBehind), whichever card the view renders.

// unkeptAfterConfirm is a store that stops keeping every issue of its
// connection (no board listed) once a body's opening is confirmed: the
// keep lost between MirroredDetail's confirmation and its card lookup.
type unkeptAfterConfirm struct {
	*store.Store
	t    *testing.T
	conn string
}

func (s *unkeptAfterConfirm) ConfirmMirrorBody(root int64, b store.MirrorBody, opened store.MirrorRead) (bool, error) {
	ok, err := s.Store.ConfirmMirrorBody(root, b, opened)
	if err == nil && ok {
		listBoards(s.t, s.Store, s.conn)
	}
	return ok, err
}

// The mirror holds a body of version 1 under a record whose card a board
// read moved to version 2, and the board stops being listed after the
// first paint confirmed the body: the lookup finds no record card, so the
// view renders the card the body was read with, version 2, over the
// version 1 body, and marks it behind.
func TestAFirstPaintWhoseKeepGoesIsMarkedBehindItsCard(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	const v1, v2 = 1_799_000_000_000, 1_799_000_060_000
	m.tracks([]tracker.Row{row("10001", v1, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) {
		return enteredAtStatusBegan(detailOfCard(card("10001", v1))), nil
	}
	if v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive); err != nil || v.BodyBehind || v.Card.Updated != v1 {
		t.Fatalf("the read of version 1 shows %d, behind %v, %v", v.Card.Updated, v.BodyBehind, err)
	}
	moved := row("10001", v2, "a")
	moved.Card.Title = "Renamed"
	m.tracks([]tracker.Row{moved})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.svc.store = &unkeptAfterConfirm{Store: m.st, t: t, conn: m.conn.ID}
	v, _, err := m.svc.MirroredDetail(m.conn.ID, "10001")
	if err != nil {
		t.Fatal(err)
	}
	if v.Card.Updated != v2 || v.Card.Title != "Renamed" || !v.BodyBehind {
		t.Fatalf("the first paint shows the card of %d %q, body behind %v; want version 2's card marked behind its body", v.Card.Updated, v.Card.Title, v.BodyBehind)
	}
}

// beforeCards is a store that runs hook once, before the first card
// lookup after it is armed: what lands between a read's write and its
// view's lookup.
type beforeCards struct {
	*store.Store
	mu   sync.Mutex
	hook func()
}

func (s *beforeCards) ListMirrorCards(root int64, issues []string) ([]store.MirrorCard, error) {
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return s.Store.ListMirrorCards(root, issues)
}

// A direct read of an issue the mirror does not keep holds the version 2
// document; before its view's card lookup a board read stores the issue's
// first keep from an earlier answer, version 1. The view renders the
// document's version 2 card, not the older record card, and is not
// behind; the record keeps its version 1 card until a read moves it.
func TestADirectReadNeverRendersACardOlderThanItsDocument(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	const v1, v2 = 1_799_000_000_000, 1_799_000_060_000
	m.tracks([]tracker.Row{row("10001", v1, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	read := card("10050", v2)
	read.Title = "Second"
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return enteredAtStatusBegan(detailOfCard(read)), nil }
	old := row("10050", v1, "b")
	old.Card.Title = "First"
	gate := &beforeCards{Store: m.st}
	gate.hook = func() {
		m.tracks([]tracker.Row{row("10001", v1, "a"), old})
		must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	}
	m.svc.store = gate
	v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10050", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	if got := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10050")); got.Card.Updated != v1 {
		t.Fatalf("the board read stored the card of %d; want the earlier answer's", got.Card.Updated)
	}
	if v.Card.Updated != v2 || v.Card.Title != "Second" || v.BodyBehind {
		t.Fatalf("the direct read shows the card of %d %q, behind %v; want its own version 2 card", v.Card.Updated, v.Card.Title, v.BodyBehind)
	}
}

// detailGate holds the scripted provider's detail reads: the first, of
// the tracker's state when it arrives, until release; every later one at
// arrival until next.
type detailGate struct {
	arrived, release, later, next chan struct{}
	once, laterOnce               sync.Once
	mu                            sync.Mutex
	calls                         int
}

func newDetailGate(t *testing.T) *detailGate {
	g := &detailGate{arrived: make(chan struct{}), release: make(chan struct{}), later: make(chan struct{}), next: make(chan struct{})}
	t.Cleanup(func() {
		g.once.Do(func() { close(g.release) })
		close(g.next)
	})
	return g
}

// of wraps detail.
func (g *detailGate) of(detail func(tracker.IssueID) (tracker.Detail, error)) func(tracker.IssueID) (tracker.Detail, error) {
	return func(id tracker.IssueID) (tracker.Detail, error) {
		g.mu.Lock()
		g.calls++
		first := g.calls == 1
		g.mu.Unlock()
		if first {
			d, err := detail(id)
			close(g.arrived)
			<-g.release
			return d, err
		}
		g.laterOnce.Do(func() { close(g.later) })
		<-g.next
		return detail(id)
	}
}

// eventuallyView waits for sub's view to satisfy ok.
func eventuallyView(t *testing.T, m *mirrorFixture, sub string, ok func(TicketView) bool) TicketView {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		v := m.ticketView(t, sub)
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ticket's view %+v", v.Detail)
		}
		time.Sleep(time.Millisecond)
	}
}

// An open ticket's full read of version 2 meets a board read of version 3,
// stored while the tracker answered (during) or after the read stored its
// body (after): either way the view renders the record's version 3 card
// marked behind the version 2 body, and the pump is due a read, which it
// makes (held here, so the view stays behind until it lands).
func TestAFullReadOvertakenByABoardReadIsMarkedBehindAndDue(t *testing.T) {
	t.Parallel()
	for _, during := range []bool{true, false} {
		name := "after"
		if during {
			name = "during"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 1, true)
			g := newDetailGate(t)
			m.p.set(func(p *scripted) { p.detail = g.of(st.detail) })
			st.edit(issues[0], func(c *tracker.Card) { c.Title = "Second" })
			done := make(chan ticketsResult, 1)
			go func() { done <- m.round(t) }()
			waitClosed(t, "the ticket's full read", g.arrived)
			board := func() {
				st.edit(issues[0], func(c *tracker.Card) { c.Title = "Third" })
				st.mu.Lock()
				third := st.cards[issues[0]]
				st.mu.Unlock()
				m.tracks([]tracker.Row{{Card: third, Order: "a000"}})
				must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
			}
			if during {
				board()
			}
			g.once.Do(func() { close(g.release) })
			if res := <-done; res.failed {
				t.Fatalf("the round %+v", res)
			}
			if !during {
				board()
			}
			v := eventuallyView(t, m, subs[0].ID, func(v TicketView) bool { return v.Detail != nil && v.Detail.Card.Title == "Third" })
			if !v.Detail.BodyBehind {
				t.Fatalf("the ticket shows the record's card over the version 2 body, not marked behind: %+v", v.Detail)
			}
			waitClosed(t, "the pump's read of version 3", g.later)
			if v := m.ticketView(t, subs[0].ID); !v.Detail.BodyBehind {
				t.Fatalf("while the read of version 3 is held the ticket shows %+v", v.Detail)
			}
		})
	}
}
