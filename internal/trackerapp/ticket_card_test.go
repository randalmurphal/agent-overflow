package trackerapp

import (
	"context"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// A project key rename moves no update time: a board read stores the
// renamed card at the update the stored body shows, and the issue record
// owns the key (store StoreMirrorTickets). A detail read in flight while the
// board read lands, its answer stored after, shows the board's new key;
// so do a reopen, which checks the stored document with a cards read, and
// an open ticket, with the one full read of the first opening.
func TestASameUpdateKeyRenameShowsOnTheBoardAndTheTicket(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	current := seededCard()
	m.p.updatedMovesWithDetail(cardsAnswering(&current))
	// The tracker's detail is of the version the read found: the key it
	// had when the read began.
	read := gatedDetailRead(t, m, detailOfCard(seededCard()))
	current.Key = "APP-1"
	renamed := tracker.Row{Card: current, Order: "a"}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{renamed}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	got := read.finish(t)
	if got.err != nil || got.v.Card.Key != "APP-1" {
		t.Fatalf("the first read after the rename shows %q, %v; want APP-1", got.v.Card.Key, got.err)
	}
	if v := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); v.Card.Key != "APP-1" {
		t.Fatalf("the board's card is %s", v.Card.Key)
	}
	if v, _ := m.openDetail(t, "10001"); v.Card.Key != "APP-1" {
		t.Fatalf("the reopened ticket shows %s", v.Card.Key)
	}
	// The mirror holds the document, so the ticket opens loaded.
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied))
	if !sub.Loaded || sub.Detail.Card.Key != "APP-1" {
		t.Fatalf("the ticket opens %+v", sub.TicketView)
	}
	quiet(t, m.svc)
	if v := m.ticketView(t, sub.ID); !v.Loaded || v.Detail.Card.Key != "APP-1" {
		t.Fatalf("the open ticket shows %+v", v.Detail.Card)
	}
	// A second rename the board reads while the ticket is open: the
	// ticket's check finds the update unmoved and shows the board's card,
	// with no full read.
	current.Key = "APP-2"
	renamed = tracker.Row{Card: current, Order: "a"}
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	quiet(t, m.svc)
	if v := m.ticketView(t, sub.ID); !v.Loaded || v.Detail.Card.Key != "APP-2" {
		t.Fatalf("the open ticket after a second rename shows %+v", v.Detail.Card)
	}
	if n := m.p.count("Details"); n != 1 {
		t.Fatalf("%d full reads; want the first opening's alone", n)
	}
}

// A detail read that overlaps a board read of the version before it: the
// board read began before the edit and lands while the detail read is at
// the tracker, so the document lands newer than the mirror's card. The
// document's card is read by ID and stored with it, and a reopen finds the
// mirror's copy current with a cards read, no second full read.
func TestADetailReadOverlappingAnOlderBoardReadStoresItsCard(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	before, edited := seededCard(), seededCard()
	edited.Title, edited.Updated = "Renamed", edited.Updated.Add(time.Minute)
	m.p.updatedMovesWithDetail(cardsAnswering(&edited))
	read := gatedDetailRead(t, m, detailOfCard(edited))
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{Rows: []tracker.Row{{Card: before, Order: "a"}}, Next: cursorAfter("10001")}, nil
		}
	})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if got := read.finish(t); got.err != nil || got.v.Card.Title != "Renamed" {
		t.Fatalf("the read shows %q, %v", got.v.Card.Title, got.err)
	}
	if b := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); b.Card.Title != "Renamed" {
		t.Fatalf("the board's card is %q", b.Card.Title)
	}
	if v, _ := m.openDetail(t, "10001"); v.Card.Title != "Renamed" {
		t.Fatalf("the reopened ticket shows %q", v.Card.Title)
	}
	if n := m.p.count("Details"); n != 1 {
		t.Fatalf("%d full reads; want the first opening's alone", n)
	}
}

// gatedReadAnswer is what a gated detail read answered.
type gatedReadAnswer struct {
	v   DetailView
	err error
}

// gatedRead is a person's detail read of issue 10001 held at the tracker.
type gatedRead struct {
	done    chan gatedReadAnswer
	release func()
}

// gatedDetailRead starts a person's detail read of issue 10001 that the
// tracker answers with d, and returns once the read is at the tracker,
// held there until finish. The hold ends with the read's context and with
// the test, so a failure before the release leaves no read waiting.
func gatedDetailRead(t *testing.T, m *mirrorFixture, d tracker.Detail) gatedRead {
	t.Helper()
	arrived, released := make(chan struct{}), make(chan struct{})
	var arrive, end sync.Once
	release := func() { end.Do(func() { close(released) }) }
	t.Cleanup(release)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return d, nil }
		p.block = func(ctx context.Context, name string) error {
			if name != "Details" {
				return nil
			}
			arrive.Do(func() { close(arrived) })
			select {
			case <-released:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	r := gatedRead{done: make(chan gatedReadAnswer, 1), release: release}
	go func() {
		v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "10001", Interactive)
		r.done <- gatedReadAnswer{v, err}
	}()
	waitClosed(t, "the detail read at the tracker", arrived)
	return r
}

// finish lets the read go and returns its answer.
func (r gatedRead) finish(t *testing.T) gatedReadAnswer {
	t.Helper()
	r.release()
	select {
	case got := <-r.done:
		return got
	case <-time.After(20 * time.Second):
		t.Fatal("the detail read never ended")
		return gatedReadAnswer{}
	}
}

// A ticket read of a changed issue stores its card with its body in the
// issue record, in one write with no cards read: the mirror holds both, of
// one version, and the board shows the new card.
func TestAChangedTicketReadStoresItsCardWithItsBody(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.seedBoard(t)
	c := seededCard()
	c.Title, c.Updated = "Renamed", c.Updated.Add(time.Minute)
	m.p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(c), nil }
	m.p.cards = cardsAnswering(&c)
	v, ch := m.openDetail(t, "10001")
	if v.Card.Title != "Renamed" || !containsIssue(ch.Cards, "10001") || !containsIssue(ch.Details, "10001") || m.p.count("Cards") != 0 {
		t.Fatalf("the read shows %q, changed %+v, %d card reads", v.Card.Title, ch, m.p.count("Cards"))
	}
	if b := must[IssueView](t)(m.svc.Issue(m.conn.ID, "10001")); b.Card.Title != "Renamed" || b.Card.Updated != millis(c.Updated) {
		t.Fatalf("the board's card %+v", b.Card)
	}
	stored, _, ok := m.storedDetail(t, "10001")
	if record, _ := cardOf(stored.Card); !ok || stored.UpdatedAt != millis(c.Updated) || millis(record.Updated) != millis(c.Updated) || record.Title != "Renamed" {
		t.Fatalf("the mirror's body %+v held %v", stored, ok)
	}
}

// A trashed Linear issue is one the tracker no longer shows: the open
// ticket's next read finds it missing and tells the person so, a failure
// naming why, not a ticket left loading.
func TestATrashedLinearTicketSaysItIsGone(t *testing.T) {
	t.Parallel()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Linear: []trackerfake.LinearWorkspace{linearSeed(at)}}
	})
	conn := s.addLinear()
	s.writeToken(conn, "lin_api_fake00001")
	scope := Scope{Connection: conn.ID, Board: tracker.BoardID(lid(10))}
	s.listBoards(conn.ID, scope.Board)
	s.syncBoard(t, scope)
	issue := tracker.IssueID(lid(60))
	sub := must[TicketSubscription](t)(s.svc.SubscribeTicket(conn.ID, issue, true, untied))
	settled(t, s.svc)
	if v := must[TicketView](t)(s.svc.TicketDetail(sub.ID)); !v.Loaded || v.Failure != nil {
		t.Fatalf("the ticket before the trash %+v", v)
	}
	world := s.e.Snapshot()
	for i := range world.Linear[0].Issues {
		if world.Linear[0].Issues[i].ID == string(issue) {
			world.Linear[0].Issues[i].Trashed = true
		}
	}
	// A snapshot leaves the key out.
	world.Linear[0].APIKey = "lin_api_fake00001"
	s.e.Reset()
	if err := s.e.Seed(world); err != nil {
		t.Fatal(err)
	}
	s.svc.roundTick(s.svc.roundOf(conn.ID))
	v := must[TicketView](t)(s.svc.TicketDetail(sub.ID))
	if v.Failure == nil || v.Failure.Kind != string(FailureNotFound) {
		t.Fatalf("the trashed ticket shows %+v", v)
	}
	t.Logf("the trashed ticket tells: %s", v.Failure.Message)
}

// A person's refresh of a changed ticket that is open makes one full
// read on either tracker and no cards read: what the refresh stored is
// told to the open ticket as its body stored of the new version
// (ticketInputs), which the pump builds from the mirror's body, not as a
// card change owing a read.
func TestARefreshOfAChangedOpenTicketReadsItOnce(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 1, jira)
			quiet(t, m.svc)
			details, cards := m.p.count("Details"), m.p.count("Cards")
			st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
			if _, _, err := m.svc.issueDetail(t.Context(), m.conn.ID, issues[0], Interactive, true); err != nil {
				t.Fatal(err)
			}
			quiet(t, m.svc)
			if v := m.ticketView(t, subs[0].ID); v.Detail == nil || v.Detail.Card.Title != "Renamed" {
				t.Fatalf("the open ticket after the refresh %+v", v)
			}
			if d, c := m.p.count("Details")-details, m.p.count("Cards")-cards; d != 1 || c != 0 {
				t.Fatalf("the refresh made %d full reads and %d card reads; want 1 and 0", d, c)
			}
		})
	}
}

func containsIssue(ids []tracker.IssueID, id tracker.IssueID) bool {
	for _, i := range ids {
		if i == id {
			return true
		}
	}
	return false
}
