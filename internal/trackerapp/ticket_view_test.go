package trackerapp

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A subscriber that joins while nothing holds the ticket's content, after
// a drop no event told, is answered not loaded and is told when the
// content is held again, though that content is what the first subscriber
// was told before.
func TestAJoinerAnsweredNotLoadedIsToldTheLoad(t *testing.T) {
	t.Parallel()
	m, _ := capFixture(t)
	m.p.updatedMovesWithDetail(cardsOf)
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000"))
	m.settledTickets(t)
	m.round(t)
	m.evictUntold(t)
	joined := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000"))
	if joined.Loaded {
		t.Fatal("the joiner was answered with a dropped detail")
	}
	for range 3 {
		m.round(t)
	}
	quiet(t, m.svc)
	if v := m.ticketView(t, joined.ID); !v.Loaded {
		t.Fatal("the content is not held again")
	}
	_, issues, _ := m.events.snapshot()
	if !slices.ContainsFunc(issues, func(e IssueEvent) bool { return e.Seq > joined.Seq && e.Changed }) {
		t.Fatalf("the joiner was never told its ticket loaded: %+v", issues)
	}
}

// Finding a ticket not loaded again, while the read that loads it is in
// flight, leaves that read standing: it settles, and the ticket is read
// once.
func TestFindingATicketNotLoadedAgainKeepsTheReadInFlight(t *testing.T) {
	t.Parallel()
	m, _ := capFixture(t)
	m.p.updatedMovesWithDetail(cardsOf)
	must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000"))
	quiet(t, m.svc)
	m.evictUntold(t)
	reads := m.p.count("Details")
	call := holdNext(m.p, "Details")
	joined := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000"))
	if joined.Loaded {
		t.Fatal("the joiner was answered with a dropped detail")
	}
	<-call.arrived
	for range 2 {
		if v := m.ticketView(t, joined.ID); v.Loaded {
			t.Fatal("the ticket is loaded before its read answered")
		}
	}
	close(call.release)
	quiet(t, m.svc)
	if v := m.ticketView(t, joined.ID); !v.Loaded {
		t.Fatal("the ticket did not load")
	}
	if got := m.p.count("Details") - reads; got != 1 {
		t.Fatalf("%d detail reads; want 1", got)
	}
}

// A history read that moves an open ticket's status time, with its
// document and its card unchanged, rebuilds its view with no tracker call
// and tells it once; the rounds after it tell nothing.
func TestAHistoryChangeReachesTheOpenTicket(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, _, issues, subs := newRoundFixture(t, 1, jira)
			m.round(t)
			if since := m.ticketView(t, subs[0].ID).Detail.Card.StatusSince; since != 0 {
				t.Fatalf("the status time before any history %d", since)
			}
			began := time.UnixMilli(1_798_500_000_000).UTC()
			m.p.set(func(p *scripted) {
				p.history = func(q tracker.HistoryQuery) (tracker.History, error) {
					var h tracker.History
					for _, id := range q.Issues {
						h.Issues = append(h.Issues, tracker.IssueHistory{Issue: id, CompleteTo: m.now(),
							Status: []tracker.StatusChange{{Change: tracker.Change{At: began}, From: "s2", To: "s1"}}})
					}
					return h, nil
				}
			})
			calls, events := len(m.p.callsInOrder()), m.issueEventCount()
			out := must[Outcome](t)(m.svc.tickHistory(t.Context(), m.scope(), Background))
			if !slices.Equal(out.Changes().History, issues) || len(out.Changes().Cards) != 0 {
				t.Fatalf("the history read changed %+v", out.Changes())
			}
			quiet(t, m.svc)
			if got := countsOf(m.p.callsInOrder()[calls:]); describeCounts(got) != "History:1" {
				t.Fatalf("the rebuild called %s", describeCounts(got))
			}
			if got := m.issueEventsSince(events); len(got) != 1 || !got[0].Changed {
				t.Fatalf("the history told %+v", got)
			}
			if since := m.ticketView(t, subs[0].ID).Detail.Card.StatusSince; since != began.UnixMilli() {
				t.Fatalf("the view's status time %d; the history says %d", since, began.UnixMilli())
			}
			events = m.issueEventCount()
			m.round(t)
			if got := m.issueEventsSince(events); len(got) != 0 {
				t.Fatalf("a round after it told %+v", got)
			}
		})
	}
}

// sweepUntold removes issue's row from the fixture's board and runs the
// cleanup, which deletes the issue and its detail, telling no one.
func (m *mirrorFixture) sweepUntold(t *testing.T, issue string) {
	t.Helper()
	page := store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime, Missing: []string{issue}, ReadAt: m.now().UnixMilli()}
	if _, err := m.st.WriteMirrorRows(m.root(), page); err != nil {
		t.Error(err)
	}
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Error(err)
	}
}

// A read succeeds only with the ticket's content held by the mirror or by
// the pump, whatever the cap dropped or the cleanup swept untold while the
// tracker was asked or before the read: before it, during the cards read
// that checks the document, and during the detail read; for a round's
// background read and for a person's opening.
func TestAReadLeavesTheContentHeld(t *testing.T) {
	t.Parallel()
	for _, when := range []string{"before", "Cards", "Details"} {
		for _, how := range []string{"evicted", "swept"} {
			for _, opening := range []bool{false, true} {
				name := fmt.Sprintf("%s %s, opening %v", how, when, opening)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					m, _ := capFixture(t)
					// The cards read checks a document on Jira; Linear reads in
					// full.
					if when != "Details" {
						m.p.updatedMovesWithDetail(cardsOf)
					}
					remove := func() {
						if how == "evicted" {
							m.evictUntold(t)
						} else {
							m.sweepUntold(t, "40000")
						}
					}
					var sub TicketSubscription
					if !opening {
						sub = must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000"))
						m.settledTickets(t)
						m.round(t)
					}
					var once sync.Once
					if when == "before" {
						remove()
					} else {
						m.p.set(func(p *scripted) {
							p.block = func(_ context.Context, name string) error {
								if name == when {
									once.Do(remove)
								}
								return nil
							}
						})
					}
					if opening {
						v, _, err := m.svc.IssueDetail(t.Context(), m.conn.ID, "40000", Interactive)
						if err != nil || v.Card.ID != "40000" {
							t.Fatalf("the opening %+v, %v", v.Card, err)
						}
						// An opening whose read began before the openings that
						// evicted it is the least recent: the read returns what
						// it found and the mirror keeps the later ones.
						if _, _, ok := m.storedDetail(t, "40000"); how == "evicted" && when != "Details" && !ok {
							t.Fatal("the opening of a kept issue did not store its detail")
						}
						return
					}
					if res := m.round(t); res.failed {
						t.Fatalf("the round %+v", res)
					}
					v := m.ticketView(t, sub.ID)
					if !v.Loaded || v.Detail.Card.ID != "40000" {
						t.Fatalf("the ticket after a successful read %+v", v)
					}
					tp := m.svc.ticketPumpOf(m.conn.ID, "40000")
					m.svc.subMu.Lock()
					held := tp.held != nil
					m.svc.subMu.Unlock()
					if _, _, stored := m.storedDetail(t, "40000"); held == stored {
						t.Fatalf("the pump holds a view %v, the mirror holds the detail %v", held, stored)
					}
				})
			}
		}
	}
}

// One board read that changes the cards of ten shown tickets makes one
// read of them, together, in full; changes stored while that read runs
// make one more read of the tickets they changed.
func TestChangedTicketsShareOneRead(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, _ := newRoundFixture(t, 10, jira)
			m.round(t)
			rename := func(changed []tracker.IssueID, title string) {
				t.Helper()
				var rows []tracker.Row
				for i, issue := range issues {
					if slices.Contains(changed, issue) {
						st.edit(issue, func(c *tracker.Card) { c.Title = title })
					}
					st.mu.Lock()
					rows = append(rows, tracker.Row{Card: st.cards[issue], Order: tracker.OrderKey(fmt.Sprintf("a%03d", i))})
					st.mu.Unlock()
				}
				m.tracks(rows)
				must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
			}
			reads := len(m.p.detailsRead())
			held := holdNext(m.p, "Details")
			rename(issues, "Renamed")
			<-held.arrived
			rename(issues[2:5], "Renamed again")
			close(held.release)
			quiet(t, m.svc)
			got := m.p.detailsRead()[reads:]
			if len(got) != 2 || !slices.Equal(got[0], issues) || !slices.Equal(got[1], issues[2:5]) {
				t.Fatalf("the reads %v", got)
			}
			if n := m.p.count("Cards"); jira && n != 1 {
				t.Fatalf("%d cards reads; the round before the change made the one", n)
			}
		})
	}
}
