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

// An open ticket whose body the cap drops keeps the view it showed, which
// a subscriber who joins is answered, and is read again at once. The cap
// drops the body that read brings too (200 tickets opened later are open),
// so the ticket is told that failure, still showing its view.
func TestAnEvictedTicketKeepsItsViewAndIsReadAgain(t *testing.T) {
	t.Parallel()
	m, evict := capFixture(t)
	m.p.updatedMovesWithDetail(cardsOf)
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000", true, untied))
	quiet(t, m.svc)
	calls, events := len(m.p.callsInOrder()), m.issueEventCount()
	call := holdNext(m.p, "Details")
	evict()
	waitClosed(t, "the read the eviction makes due", call.arrived)
	if v := m.ticketView(t, sub.ID); !v.Loaded {
		t.Fatal("the eviction unloaded the ticket")
	}
	if joined := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000", true, untied)); !joined.Loaded {
		t.Fatal("a joiner was answered not loaded")
	}
	close(call.release)
	quiet(t, m.svc)
	if got := describeCounts(countsOf(m.p.callsInOrder()[calls:])); got != "Cards:1 Details:1" {
		t.Fatalf("after the eviction the ticket's reads called %s", got)
	}
	got := m.issueEventsSince(events)
	if len(got) != 1 || got[0].Changed || got[0].Failure == nil || got[0].Failure.Kind != string(FailureInternal) {
		t.Fatalf("the eviction and the read told %+v; want the read's failure alone", got)
	}
	if v := m.ticketView(t, sub.ID); !v.Loaded || v.Detail.Card.ID != "40000" {
		t.Fatalf("after the read the ticket shows %+v", v)
	}
}

// An eviction told while the ticket's read is in flight is left to that
// read: a build made meanwhile asks for no read, the read stores the body
// again and settles, and the ticket is read once.
func TestAnEvictionDuringAReadIsLeftToTheRead(t *testing.T) {
	t.Parallel()
	m, evict := capFixture(t)
	m.p.updatedMovesWithDetail(cardsOf)
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000", true, untied))
	quiet(t, m.svc)
	reads := m.p.count("Details")
	call := holdNext(m.p, "Details")
	tp := m.svc.ticketPumpOf(m.conn.ID, "40000")
	done := make(chan ticketsResult, 1)
	go func() {
		done <- m.svc.readTickets(context.Background(), m.conn.ID, []*ticketPump{tp}, Background, true, false)
	}()
	waitClosed(t, "the held call", call.arrived)
	evict()
	// A build of the connection's views made meanwhile leaves the ticket to
	// its read: it asks for no read of its own.
	m.svc.buildShown(m.conn.ID)
	close(call.release)
	if res := <-done; res.failed {
		t.Fatalf("the read %+v", res)
	}
	quiet(t, m.svc)
	if v := m.ticketView(t, sub.ID); !v.Loaded {
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
	page := store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime, Missing: []string{issue}, ReadAt: m.read()}
	if _, err := m.st.WriteMirrorRows(m.root(), page); err != nil {
		t.Error(err)
	}
	if err := m.svc.FinishMirrorCleanup(); err != nil {
		t.Error(err)
	}
}

// A read succeeds only with the ticket's content held by the mirror,
// whatever the cap dropped or the cleanup swept untold while the tracker
// was asked or before the read: before it, during the cards read that
// checks the document, and during the detail read; for a round's
// background read and for a person's opening. A ticket whose body the cap
// drops again as the read stores it (200 tickets opened later are open)
// keeps the view it showed and is told the failure.
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
						sub = must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "40000", true, untied))
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
						t.Fatalf("the ticket after the read %+v", v)
					}
					_, _, stored := m.storedDetail(t, "40000")
					switch {
					case how == "evicted" && (stored || v.Failure == nil || v.Failure.Kind != string(FailureInternal)):
						t.Fatalf("a read whose body the cap dropped: stored %v, failure %+v", stored, v.Failure)
					case how == "swept" && (!stored || v.Failure != nil):
						t.Fatalf("a read after a sweep: stored %v, failure %+v", stored, v.Failure)
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
			waitClosed(t, "the held build", held.arrived)
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

// A tracker whose documents lag its cards (the record's card is newer than
// any body a read brings) costs one read at once per card version, not a
// loop: the view built after that read is still behind the same card, and
// a later build of it asks for no read before the round's regular one.
func TestABodyBehindItsCardIsReadOncePerCardVersion(t *testing.T) {
	t.Parallel()
	m, st, issues, subs := newRoundFixture(t, 1, true)
	m.settledTickets(t)
	quiet(t, m.svc)
	stale := must[tracker.Detail](t)(st.detail(issues[0]))
	m.p.set(func(p *scripted) { p.detail = func(tracker.IssueID) (tracker.Detail, error) { return stale, nil } })
	st.edit(issues[0], func(c *tracker.Card) { c.Title = "Renamed" })
	reads := m.p.count("Details")
	m.tracks([]tracker.Row{{Card: st.cards[issues[0]], Order: "a000"}})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	quiet(t, m.svc)
	if v := m.ticketView(t, subs[0].ID); v.Detail.Card.Title != "Renamed" || !v.Detail.BodyBehind {
		t.Fatalf("the ticket shows %q, behind %v; want the record's card over the lagging body", v.Detail.Card.Title, v.Detail.BodyBehind)
	}
	if got := m.p.count("Details") - reads; got != 1 {
		t.Fatalf("%d full reads for the new card; want 1", got)
	}
	m.publishRaw(Changes{Catalog: true})
	quiet(t, m.svc)
	if got := m.p.count("Details") - reads; got != 1 {
		t.Fatalf("%d full reads after a later build of the same card; want 1", got)
	}
}

// A body whose issue the mirror no longer keeps, left for the cleanup to
// sweep, is not shown: a ticket opened on it is answered not loaded, and
// its first read brings the body again.
func TestABodyLeftToTheSweepIsNotShown(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
		return tracker.Detail{Card: card(string(id), 1_799_000_000_000)}, nil
	}
	m.openDetail(t, "10001")
	page := store.MirrorRowPage{BoardID: string(testBoard), Lifetime: m.sync(testBoard).Lifetime, Missing: []string{"10001"}, ReadAt: m.read()}
	// No cleanup runs: the body stays stored, its issue kept by nothing.
	must[store.MirrorChanges](t)(m.st.WriteMirrorRows(m.root(), page))
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied))
	if sub.Loaded || sub.Detail != nil {
		t.Fatalf("a ticket opened on a body left to the sweep was answered %+v", sub.TicketView)
	}
	quiet(t, m.svc)
	if v := m.ticketView(t, sub.ID); !v.Loaded || v.Failure != nil {
		t.Fatalf("after its first read the ticket shows %+v", v)
	}
}

// A change told to a ticket whose read is in flight is counted and wakes
// no build: the read's settlement claims the ticket's build
// (settleTickets). One told to a ticket with no read in flight wakes its
// pump.
func TestATellDuringAReadWakesNoBuild(t *testing.T) {
	t.Parallel()
	s := &Service{}
	for _, reading := range []int{1, 0} {
		tp := &ticketPump{key: ticketKey{connection: "c", issue: "10001"}, viewChanged: make(chan struct{}, 1), reading: reading}
		s.tellTicketLocked(tp, Changes{Cards: []tracker.IssueID{"10001"}})
		if tp.dirty != 1 || (len(tp.viewChanged) == 1) != (reading == 0) {
			t.Fatalf("a tell with %d reads in flight: dirty %d, %d nudges", reading, tp.dirty, len(tp.viewChanged))
		}
	}
}
