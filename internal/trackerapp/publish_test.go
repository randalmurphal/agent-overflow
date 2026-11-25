package trackerapp

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// Every write of a run is told as it returns (run.told), before the run's
// next tracker call: a later call that waits or fails holds back no
// committed change. Each test holds that next call at the tracker and
// waits for the open ticket of the changed issue to be told its card
// changed (readNowLocked moves its nudge) before releasing it.

// toldWhileHeld fails unless tp's nudge moves past before within a bound
// no healthy run reaches.
func toldWhileHeld(t *testing.T, s *Service, tp *ticketPump, before uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.subMu.Lock()
		nudged := tp.nudged
		s.subMu.Unlock()
		if nudged > before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the ticket was not told of the stored card while the run's next call was held")
		}
		time.Sleep(time.Millisecond)
	}
}

// nudgeOf is tp's nudge stamp now.
func nudgeOf(s *Service, tp *ticketPump) uint64 {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return tp.nudged
}

// A delta walk's first page, which changes an open ticket's card, is told
// while the request for its second page is held.
func TestATicksStoredPageIsToldBeforeItsNextCall(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	m.seedBoard(t)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(seededCard()), nil }
	})
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", untied))
	m.settledTickets(t)
	quiet(t, m.svc)
	tp := m.svc.ticketPumpOf(m.conn.ID, "10001")
	before := nudgeOf(m.svc, tp)
	changed := tracker.Row{Card: seededCard(), Order: "a"}
	changed.Card.Title, changed.Card.Updated = "Renamed", changed.Card.Updated.Add(time.Minute)
	m.p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
		if bytes.Equal(q.From, cursorAfter("page1")) {
			return tracker.RowPage{Rows: []tracker.Row{row("10002", 1_799_000_100_000, "b")}, Next: cursorAfter("page2")}, nil
		}
		return tracker.RowPage{Rows: []tracker.Row{changed}, Next: cursorAfter("page1"), More: true}, nil
	}
	h := holdNth(m.p, "ChangedRows", 2)
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := m.svc.tickDelta(context.Background(), m.scope(), Background)
		done <- err
	}()
	waitClosed(t, "the second page's request", h.arrived)
	toldWhileHeld(t, m.svc, tp, before)
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A write's refresh that reads an edited card's rows on its two boards
// tells the subscribed board after the first board's write, while the
// read through the second board is held.
func TestARefreshsStoredRowsAreToldBeforeItsNextCall(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, testBoard)
	f.events.boardEvents(t, 1)
	told := len(f.events.boardEvents(t, 1))
	edited := writeRow("10001", 1, "a")
	edited.Card.Title = "Renamed"
	f.state.set(testBoard, edited)
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"))}})
	h := holdNth(f.p, "BoardRows", 2)
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Edit(context.Background(), f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"Renamed"}}}})
		done <- err
	}()
	waitClosed(t, "the read through the second board", h.arrived)
	f.events.waitFor(t, "the board told of the stored card while the second read is held", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(b[told:], func(e BoardEvent) bool { return slices.Contains(e.Changed, "10001") })
	})
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A write's refresh cancelled during its read through the second board
// tells the edited issue's open ticket what the first board's write
// stored: the refresh reaches no read of the ticket, which would have
// settled it.
func TestACancelledRefreshTellsTheOpenTicketWhatItStored(t *testing.T) {
	t.Parallel()
	f := newWriteFixture(t)
	f.subscribe(t, testBoard)
	f.events.boardEvents(t, 1)
	sub := must[TicketSubscription](t)(f.svc.SubscribeTicket(f.conn.ID, "10001", untied))
	f.events.issueEvents(t, 1)
	f.settledTickets(t)
	_, issues, _ := f.events.snapshot()
	told := len(issues)
	edited := writeRow("10001", 1, "a")
	edited.Card.Title = "Renamed"
	f.state.set(testBoard, edited)
	f.answer(tracker.Result{Parts: []tracker.Part{appliedPart("10001", tracker.StepEdit, tracker.CardOf("10001"), tracker.DetailOf("10001"))}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	rows := 0
	f.p.set(func(p *scripted) {
		p.block = func(callCtx context.Context, name string) error {
			if name != "BoardRows" {
				return callCtx.Err()
			}
			mu.Lock()
			rows++
			second := rows == 2
			mu.Unlock()
			if second {
				cancel()
				return context.Canceled
			}
			return nil
		}
	})
	if _, err := f.svc.Edit(ctx, f.conn.ID, EditRequest{Issue: "10001", Fields: []FieldEditInput{{Field: "title", Typed: []string{"Renamed"}}}}); err != nil {
		t.Fatal(err)
	}
	f.p.set(func(p *scripted) { p.block = nil })
	f.events.waitFor(t, "the open ticket told of the card the first board's read stored", func(_ []BoardEvent, is []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(is[told:], func(e IssueEvent) bool { return e.Issue == "10001" && e.Changed })
	})
	if v := must[TicketView](t)(f.svc.TicketDetail(sub.ID)); v.Detail == nil || v.Detail.Card.Title != "Renamed" {
		t.Fatalf("after the cancelled refresh the ticket shows %+v", v.Detail)
	}
}

// A ticket round that stored an open ticket's changed card tells the
// ticket that links it (its view shows that card) while the round's
// history read, which follows, is held: on Jira the check's cards read
// stores the card, on Linear the full read does.
func TestARoundsStoredTicketIsToldBeforeItsHistoryRead(t *testing.T) {
	t.Parallel()
	for name, jira := range map[string]bool{"jira": true, "linear": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, st, issues, _ := newRoundFixture(t, 2, jira)
			st.mu.Lock()
			st.links[issues[0]] = issues[1]
			st.mu.Unlock()
			linking := m.svc.ticketPumpOf(m.conn.ID, issues[0])
			m.svc.ticketTick(linking, Interactive, true)
			m.settledTickets(t)
			m.svc.subMu.Lock()
			dirty := linking.dirty
			m.svc.subMu.Unlock()
			// The edit is inside the tracker's settle margin, so the read
			// cannot settle its status time and the round reads its history
			// after it.
			m.p.set(func(p *scripted) { p.caps.HistorySettle = 1000 * time.Hour })
			st.edit(issues[1], func(c *tracker.Card) { c.Title = "Edited" })
			h := holdNext(m.p, "History")
			var once sync.Once
			release := func() { once.Do(func() { close(h.release) }) }
			t.Cleanup(release)
			m.readSoon(issues[1])
			waitClosed(t, "the round's history read", h.arrived)
			deadline := time.Now().Add(10 * time.Second)
			for {
				m.svc.subMu.Lock()
				moved := linking.dirty != dirty
				m.svc.subMu.Unlock()
				if moved {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the linking ticket was not told of the stored card while the history read was held")
				}
				time.Sleep(time.Millisecond)
			}
			release()
			quiet(t, m.svc)
		})
	}
}

// The linked cards tick tells an open ticket of a linked issue that no
// board holds that its card changed, as the write returns: no other
// write of the tick tells it.
func TestALinkedCardsWriteIsTold(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.linkThread("thread-1", "10005")
	first := card("10005", 1_799_000_000_000)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(first), nil }
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{Cards: []tracker.Card{first}}, nil
		}
	})
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10005", untied))
	m.settledTickets(t)
	must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	quiet(t, m.svc)
	tp := m.svc.ticketPumpOf(m.conn.ID, "10005")
	before := nudgeOf(m.svc, tp)
	changed := first
	changed.Title, changed.Updated = "Renamed", changed.Updated.Add(time.Minute)
	m.p.set(func(p *scripted) {
		p.cards = func([]tracker.IssueID) (tracker.Cards, error) {
			return tracker.Cards{Cards: []tracker.Card{changed}}, nil
		}
	})
	out := must[Outcome](t)(m.svc.tickCards(t.Context(), m.conn.ID, Background))
	if !slices.Equal(out.Changes().Cards, ids("10005")) {
		t.Fatalf("the cards tick %+v; want 10005's card changed", out)
	}
	toldWhileHeld(t, m.svc, tp, before)
}
