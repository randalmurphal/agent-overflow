package trackerapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// heldCall holds the next provider call of name until released: arrived
// closes when it is made.
type heldCall struct {
	arrived, release chan struct{}
}

// holdNext holds p's next call of name.
func holdNext(p *scripted, name string) *heldCall { return holdNth(p, name, 1) }

// holdNth holds p's n-th call of name from now.
func holdNth(p *scripted, name string, n int) *heldCall {
	h := &heldCall{arrived: make(chan struct{}), release: make(chan struct{})}
	var mu sync.Mutex
	calls := 0
	p.set(func(p *scripted) {
		p.block = func(ctx context.Context, called string) error {
			held := false
			if called == name {
				mu.Lock()
				calls++
				held = calls == n
				mu.Unlock()
			}
			if !held {
				return nil
			}
			close(h.arrived)
			select {
			case <-h.release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})
	return h
}

// A read that began before the one last settled into a ticket's pump
// changes nothing and emits nothing, whichever of a pump's own read and
// the round's began first; the stored document is the later read's.
func TestCrossingTicketReadsKeepTheLater(t *testing.T) {
	t.Parallel()
	for _, kept := range []bool{true, false} {
		for _, pumpFirst := range []bool{true, false} {
			name := fmt.Sprintf("kept %v, the pump's read first %v", kept, pumpFirst)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
				issue := tracker.IssueID("20001")
				if kept {
					issue = "10001"
					m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
					must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
				}
				var version atomic.Value
				version.Store("first")
				m.p.set(func(p *scripted) {
					p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
						d := detailOfCard(card(string(id), 1_799_000_000_000))
						d.Description = version.Load().(string)
						return d, nil
					}
				})
				answerCardsFromDetails(m.p)
				sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, issue, untied))
				m.events.issueEvents(t, 1)
				m.settledTickets(t)
				tp := m.svc.ticketPumpOf(m.conn.ID, issue)

				version.Store("older")
				held := holdNext(m.p, "Details")
				done := make(chan struct{})
				go func() {
					defer close(done)
					if pumpFirst {
						m.svc.ticketTick(tp, Background, false)
					} else {
						m.round(t)
					}
				}()
				<-held.arrived
				version.Store("newer")
				if pumpFirst {
					m.round(t)
				} else {
					m.svc.ticketTick(tp, Background, false)
				}
				events := m.issueEventCount()
				close(held.release)
				<-done
				if got := m.issueEventsSince(events); len(got) != 0 {
					t.Fatalf("the earlier read told %+v", got)
				}
				if v := m.ticketView(t, sub.ID); v.Detail == nil || v.Detail.Description != "newer" {
					t.Fatalf("the view %+v", v.Detail)
				}
				if _, d, ok := m.storedDetail(t, string(issue)); kept && (!ok || d.Description != "newer") {
					t.Fatalf("the stored detail %q, %v", d.Description, ok)
				}
			})
		}
	}
}

// Overlapping reads of a stored detail leave the document of the full read
// that began last: two checks that each read in full, and a person's
// refresh overlapping a check. A check itself writes no document.
func TestOverlappingDetailReadsStoreTheLatestFullRead(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"check", "refresh"} {
		t.Run(first, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			st := &trackerState{cards: map[tracker.IssueID]tracker.Card{"10001": card("10001", 1_799_000_000_000)},
				comments: map[tracker.IssueID]string{"10001": "v1"}, gone: map[tracker.IssueID]bool{}}
			m.p.changedRows = pagedDelta(t, []tracker.Row{{Card: st.cards["10001"], Order: "a"}})
			must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
			m.p.set(func(p *scripted) { p.detail = st.detail })
			m.p.updatedMovesWithDetail(st.cardsRead)
			m.openDetail(t, "10001")
			// The issue changes: the next check reads it in full.
			st.edit("10001", func(c *tracker.Card) {})
			st.comment("10001", "v2")
			held := holdNext(m.p, "Details")
			done := make(chan error, 1)
			go func() {
				var err error
				if first == "check" {
					_, _, err = m.svc.IssueDetail(context.Background(), m.conn.ID, "10001", Interactive)
				} else {
					_, _, err = m.svc.issueDetail(context.Background(), m.conn.ID, "10001", Interactive, true)
				}
				done <- err
			}()
			<-held.arrived
			st.edit("10001", func(c *tracker.Card) {})
			st.comment("10001", "v3")
			m.openDetail(t, "10001")
			close(held.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, d, ok := m.storedDetail(t, "10001"); !ok || d.Comments.Comments[0].Body != "v3" {
				t.Fatalf("the stored detail holds %q", d.Comments.Comments[0].Body)
			}
			// Checks that find the card unchanged write no document.
			before, _, _ := m.storedDetail(t, "10001")
			m.openDetail(t, "10001")
			after, _, _ := m.storedDetail(t, "10001")
			if after.ReadAt != before.ReadAt || string(after.Data) != string(before.Data) {
				t.Fatal("a check rewrote the stored document")
			}
		})
	}
}

// A round reads in full a ticket whose stored detail's read began more
// than detailMaxAge ago, and checks it again after.
func TestARoundReadsADetailPastTheAgeBound(t *testing.T) {
	t.Parallel()
	m, _, issues, _ := newRoundFixture(t, 1, true)
	m.round(t)
	m.pass(detailMaxAge + time.Minute)
	calls := len(m.p.callsInOrder())
	m.round(t)
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Details:1" || !slices.Equal(m.lastDetailsRead(), issues) {
		t.Fatalf("past the bound the round called %s", describeCounts(got))
	}
	calls = len(m.p.callsInOrder())
	m.round(t)
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Cards:1" {
		t.Fatalf("after the read the round called %s", describeCounts(got))
	}
}

// After the wall clock steps back past a stored detail's read, a round
// reads the ticket in full once, and checks it again after.
func TestARoundReadsADetailOnceAfterTheClockStepsBack(t *testing.T) {
	t.Parallel()
	m, _, issues, _ := newRoundFixture(t, 1, true)
	m.round(t)
	m.pass(-2 * time.Hour)
	calls := len(m.p.callsInOrder())
	m.round(t)
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Details:1" || !slices.Equal(m.lastDetailsRead(), issues) {
		t.Fatalf("after the step back the round called %s", describeCounts(got))
	}
	calls = len(m.p.callsInOrder())
	m.round(t)
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Cards:1" {
		t.Fatalf("after the read the round called %s", describeCounts(got))
	}
}

// A person's refresh of an open ticket reads it in full, with no check,
// and records the opening.
func TestATicketRefreshReadsInFull(t *testing.T) {
	t.Parallel()
	m, _, _, subs := newRoundFixture(t, 1, true)
	m.tickingClock()
	before, _, _ := m.storedDetail(t, "10001")
	calls := len(m.p.callsInOrder())
	if err := m.svc.RefreshSubscription(t.Context(), subs[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Details:1" {
		t.Fatalf("a refresh called %s", describeCounts(got))
	}
	if after, _, _ := m.storedDetail(t, "10001"); after.OpenedAt <= before.OpenedAt {
		t.Fatalf("a refresh recorded no opening: %d after %d", after.OpenedAt, before.OpenedAt)
	}
}

// A read that stores a changed card of an issue an open ticket links
// rebuilds the ticket's view from the store with no tracker call, told
// once.
func TestALinkedCardChangeRebuildsTheTicketView(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	ticket, linked := card("10001", 1_799_000_000_000), card("10002", 1_799_000_000_000)
	m.tracks([]tracker.Row{{Card: ticket, Order: "a"}, {Card: linked, Order: "b"}})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(ticket), nil }
	})
	m.p.updatedMovesWithDetail(cardsAnswering(&ticket))
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", untied))
	m.events.issueEvents(t, 1)
	m.settledTickets(t)
	calls, events := len(m.p.callsInOrder()), m.issueEventCount()
	linked.Title, linked.Updated = "Issue 10002, renamed", linked.Updated.Add(time.Minute)
	m.tracks([]tracker.Row{{Card: ticket, Order: "a"}, {Card: linked, Order: "b"}})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	got := m.events.issueEvents(t, events+1)[events:]
	if len(got) != 1 || got[0].Issue != "10001" || !got[0].Changed {
		t.Fatalf("events %+v", got)
	}
	if v := m.ticketView(t, sub.ID); v.Detail.Links[0].Issue.Title != linked.Title {
		t.Fatalf("the link shows %+v", v.Detail.Links[0].Issue)
	}
	m.round(t) // a sync point: the round finds the ticket unchanged
	if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "Cards:1 ChangedRows:1" {
		t.Fatalf("the rebuild called %s", describeCounts(got))
	}
	if got := m.issueEventsSince(events); len(got) != 1 {
		t.Fatalf("told %d times: %+v", len(got), got)
	}
}

// A pump's end is told before its core stops: a request waiting on the
// pump while another tick ends it finds the end told when it is refused.
// At the end's emission the core has not stopped yet.
func TestAnEndIsToldBeforeTheCoreStops(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"board", "ticket"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			m.p.set(func(p *scripted) {
				p.detail = func(id tracker.IssueID) (tracker.Detail, error) { return detailOfCard(card(string(id), 1)), nil }
			})
			answerCardsFromDetails(m.p)
			var done <-chan struct{}
			var handle string
			ended := make(chan struct{})
			var stoppedFirst atomic.Bool
			hook := func(e any) {
				switch e := e.(type) {
				case BoardEvent:
					if !e.Ended {
						return
					}
				case IssueEvent:
					if !e.Ended {
						return
					}
				}
				select {
				case <-done:
					stoppedFirst.Store(true)
				default:
				}
				close(ended)
			}
			m.events.onEmit.Store(&hook)
			var held *heldCall
			if kind == "board" {
				sub := m.subscribe(t, "2")
				m.events.boardEvents(t, 1)
				done, handle = m.svc.boardPumpOf(Scope{Connection: m.conn.ID, Board: "2"}).core.Done(), sub.ID
				held = holdNext(m.p, "ChangedRows")
				refreshed := make(chan error, 1)
				go func() { refreshed <- m.svc.RefreshSubscription(context.Background(), handle) }()
				<-held.arrived
				// The board stops being listed while its tick runs: the tick ends
				// the pump, refusing the waiting request.
				waiter := make(chan error, 1)
				go func() { waiter <- m.svc.RefreshSubscription(context.Background(), handle) }()
				m.listBoards(m.conn.ID, testBoard)
				close(held.release)
				<-refreshed
				err := <-waiter
				select {
				case <-ended:
				default:
					t.Fatalf("a refused request (%v) returned before the end was told", err)
				}
			} else {
				sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", untied))
				m.events.issueEvents(t, 1)
				m.settledTickets(t)
				tp := m.svc.ticketPumpOf(m.conn.ID, "10001")
				done, handle = tp.core.Done(), sub.ID
				held = holdNext(m.p, "Details")
				refreshed := make(chan error, 1)
				go func() { refreshed <- m.svc.RefreshSubscription(context.Background(), handle) }()
				<-held.arrived
				waiter := make(chan error, 1)
				go func() { waiter <- m.svc.RefreshSubscription(context.Background(), handle) }()
				// The connection turns off behind the service's back: another read
				// finds it ended and ends the pump.
				if _, _, err := m.st.SetTrackerConnectionEnabled(m.conn.ID, false); err != nil {
					t.Fatal(err)
				}
				m.svc.opMu.Lock()
				m.svc.forgetHolds()
				m.svc.opMu.Unlock()
				m.svc.ticketTick(tp, Background, false)
				err := <-waiter
				select {
				case <-ended:
				default:
					t.Fatalf("a refused request (%v) returned before the end was told", err)
				}
				close(held.release)
				<-refreshed
			}
			if stoppedFirst.Load() {
				t.Fatal("the core stopped before the end was told")
			}
			if !errors.Is(<-func() chan error {
				c := make(chan error, 1)
				c <- m.svc.RefreshSubscription(t.Context(), handle)
				return c
			}(), ErrUnknownSubscription) {
				t.Fatal("the ended handle is still held")
			}
		})
	}
}
