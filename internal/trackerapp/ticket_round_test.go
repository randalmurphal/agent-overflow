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

// A round that finds nothing changed costs one provider call for every
// shown ticket of the connection, and store calls that do not grow with
// them: on Jira one cards read checked against the details' states (no
// document decoded, nothing written); on Linear one many-issue detail read
// stored in one transaction. It emits nothing.
func TestIdleRoundCosts(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		for _, n := range []int{1, 3, 10} {
			name := fmt.Sprintf("%s, %d tickets", map[bool]string{true: "jira", false: "linear"}[jira], n)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m, _, issues, _ := newRoundFixture(t, n, jira)
				m.round(t) // settles each ticket's check
				cs := newCountingStore(m.st)
				m.svc.store = cs
				calls, events := len(m.p.callsInOrder()), m.issueEventCount()
				if res := m.round(t); res.failed {
					t.Fatalf("round %+v", res)
				}
				provider, stored := countsOf(m.p.callsInOrder()[calls:]), cs.take()
				wantProvider, wantStore := "Details:1", "GetMirrorSync:1 StoreMirrorDetails:1"
				if jira {
					wantProvider, wantStore = "Cards:1", "GetMirrorSync:1 ListMirrorDetailStates:1 UpsertMirrorCards:1"
				}
				t.Logf("idle round: provider %s, store %s", describeCounts(provider), describeCounts(stored))
				if describeCounts(provider) != wantProvider || describeCounts(stored) != wantStore {
					t.Errorf("an idle round cost provider %s, store %s; pinned %s and %s", describeCounts(provider), describeCounts(stored), wantProvider, wantStore)
				}
				if !jira && !slices.Equal(sortedIDs(m.lastDetailsRead()), issues) {
					t.Errorf("the round read %v", m.lastDetailsRead())
				}
				if got := m.issueEventsSince(events); len(got) != 0 {
					t.Errorf("an idle round told %+v", got)
				}
			})
		}
	}
}

// One ticket of three changes: only it is read in full and only it emits.
func TestARoundReadsOnlyTheChangedTicketInFull(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 3, jira)
			m.round(t)
			calls, events := len(m.p.callsInOrder()), m.issueEventCount()
			if jira {
				st.edit(issues[1], func(c *tracker.Card) { c.Title = "Renamed" })
			} else {
				st.comment(issues[1], "Second")
			}
			m.round(t)
			provider := countsOf(m.p.callsInOrder()[calls:])
			want := map[string]int{"Details": 1}
			if jira {
				want["Cards"] = 1
			}
			if describeCounts(provider) != describeCounts(want) {
				t.Fatalf("the round called %s", describeCounts(provider))
			}
			if jira && !slices.Equal(m.lastDetailsRead(), issues[1:2]) {
				t.Fatalf("the round read %v in full", m.lastDetailsRead())
			}
			got := m.issueEventsSince(events)
			if len(got) != 1 || got[0].Issue != issues[1] || !got[0].Changed || got[0].Failure != nil {
				t.Fatalf("events %+v", got)
			}
			v := m.ticketView(t, subs[1].ID)
			if jira && v.Detail.Card.Title != "Renamed" || !jira && v.Detail.Comments[0].Body != "Second" {
				t.Fatalf("the changed ticket's view %+v", v.Detail)
			}
		})
	}
}

// One ticket of three is gone: only it fails, told once with its own
// issue, while the others settle; it recovers when the tracker returns it.
func TestARoundToldOnlyTheGoneTicket(t *testing.T) {
	t.Parallel()
	for _, jira := range []bool{true, false} {
		t.Run(map[bool]string{true: "jira", false: "linear"}[jira], func(t *testing.T) {
			t.Parallel()
			m, st, issues, subs := newRoundFixture(t, 3, jira)
			m.round(t)
			events := m.issueEventCount()
			st.setGone(issues[2], true)
			st.comment(issues[0], "Changed meanwhile")
			for range 2 {
				if res := m.round(t); res.failed {
					t.Fatalf("a gone ticket failed the round: %+v", res)
				}
			}
			got := m.issueEventsSince(events)
			failures := slices.DeleteFunc(slices.Clone(got), func(e IssueEvent) bool { return e.Failure == nil })
			if len(failures) != 1 || failures[0].Issue != issues[2] || failures[0].Failure.Kind != string(FailureNotFound) {
				t.Fatalf("events %+v", got)
			}
			if v := m.ticketView(t, subs[2].ID); v.Failure == nil || !v.Loaded {
				t.Fatalf("the gone ticket's view %+v", v)
			}
			if v := m.ticketView(t, subs[1].ID); v.Failure != nil {
				t.Fatalf("a ticket still returned shows %+v", v.Failure)
			}
			if !jira && m.ticketView(t, subs[0].ID).Detail.Comments[0].Body != "Changed meanwhile" {
				t.Fatal("the round did not settle the other tickets")
			}
			events = m.issueEventCount()
			st.setGone(issues[2], false)
			m.round(t)
			if got := m.issueEventsSince(events); len(got) != 1 || got[0].Issue != issues[2] || got[0].Failure != nil {
				t.Fatalf("the recovery told %+v", got)
			}
		})
	}
}

// A round's failure is told once to each shown ticket, with its issue,
// and cleared once by the next success; the round arms its retry.
func TestARoundFailureIsToldOncePerTicket(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 2, false)
	events := m.issueEventCount()
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{}, fmt.Errorf("refused") }
	})
	for range 2 {
		if res := m.round(t); !res.failed {
			t.Fatalf("a failed round %+v", res)
		}
	}
	got := m.issueEventsSince(events)
	if len(got) != 2 || got[0].Issue == got[1].Issue || got[0].Failure == nil || got[1].Failure == nil {
		t.Fatalf("the failure told %+v", got)
	}
	if v := m.ticketView(t, subs[0].ID); v.Failure == nil || !v.Loaded {
		t.Fatalf("the view during the failure %+v", v)
	}
	st := &trackerState{cards: map[tracker.IssueID]tracker.Card{}, comments: map[tracker.IssueID]string{}, gone: map[tracker.IssueID]bool{}}
	for _, id := range issues {
		st.cards[id], st.comments[id] = card(string(id), 1_799_000_000_000), "First"
	}
	m.p.set(func(p *scripted) { p.detail = st.detail })
	events = m.issueEventCount()
	m.round(t)
	if got := m.issueEventsSince(events); len(got) != 2 || got[0].Failure != nil || got[1].Failure != nil {
		t.Fatalf("the recovery told %+v", got)
	}
}

// A hidden ticket is left out of the round and reads when shown again
// after a round passed without it; a ticket opened between rounds reads at
// once and joins the next round.
func TestARoundReadsTheShownTickets(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 2, false)
	if err := m.svc.SetSubscriptionVisible(subs[0].ID, false); err != nil {
		t.Fatal(err)
	}
	m.round(t)
	if got := m.lastDetailsRead(); !slices.Equal(got, issues[1:]) {
		t.Fatalf("the round with %s hidden read %v", issues[0], got)
	}
	reads := m.p.count("Details")
	if err := m.svc.SetSubscriptionVisible(subs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, m.p, "Details", reads+1)
	m.settledTickets(t)
	if got := m.p.detailsRead()[reads]; !slices.Equal(got, issues[:1]) {
		t.Fatalf("shown again, it read %v", got)
	}

	// A third ticket, opened between rounds.
	third := tracker.IssueID("10009")
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a000"), row("10002", 1_799_000_000_000, "a001"), row(string(third), 1_799_000_000_000, "a002")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		h := p.detail
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			if id == third {
				return detailOfCard(card(string(third), 1_799_000_000_000)), nil
			}
			return h(id)
		}
	})
	reads = m.p.count("Details")
	if _, err := m.svc.SubscribeTicket(m.conn.ID, third); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, m.p, "Details", reads+1)
	m.settledTickets(t)
	if got := m.p.detailsRead()[reads]; !slices.Equal(got, []tracker.IssueID{third}) {
		t.Fatalf("the new ticket's first read read %v", got)
	}
	m.round(t)
	if got := m.lastDetailsRead(); !slices.Equal(got, []tracker.IssueID{"10001", "10002", third}) {
		t.Fatalf("the next round read %v", got)
	}
}

// The round runs while a ticket of its connection is shown: hiding the
// last, releasing the last handle, turning the connection off and removing
// it each stop its goroutine, the last two before they return. Two
// connections of different kinds run their rounds at their own intervals.
func TestTheRoundFollowsTheShownTickets(t *testing.T) {
	t.Parallel()
	m, _, issues, subs := newRoundFixture(t, 1, false)
	rd := m.svc.roundOf(m.conn.ID)
	if rd == nil || rd.every != time.Hour {
		t.Fatalf("round %+v", rd)
	}
	if err := m.svc.SetSubscriptionVisible(subs[0].ID, false); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, "the round of a hidden ticket", rd.done)
	if m.svc.roundOf(m.conn.ID) != nil {
		t.Fatal("a round with no shown ticket")
	}
	if err := m.svc.SetSubscriptionVisible(subs[0].ID, true); err != nil {
		t.Fatal(err)
	}
	rd = m.svc.roundOf(m.conn.ID)
	if rd == nil {
		t.Fatal("no round once shown again")
	}
	m.svc.ReleaseSubscription(subs[0].ID)
	waitClosed(t, "the round after the last release", rd.done)
	if m.svc.roundOf(m.conn.ID) != nil {
		t.Fatal("a round after the last release")
	}

	// The change comes while the round is inside a read that finishes only
	// after its context ends: the change returns once the round exited.
	m.svc.cadence.RetryBase = time.Millisecond
	for _, end := range []string{"off", "removed"} {
		m.p.set(func(p *scripted) { p.block = nil })
		if _, err := m.svc.SubscribeTicket(m.conn.ID, issues[0]); err != nil {
			t.Fatal(err)
		}
		m.settledTickets(t)
		rd = m.svc.roundOf(m.conn.ID)
		inRead := make(chan struct{})
		var once sync.Once
		m.p.set(func(p *scripted) {
			p.block = func(ctx context.Context, name string) error {
				if name != "Details" {
					return nil
				}
				once.Do(func() { close(inRead) })
				<-ctx.Done()
				time.Sleep(50 * time.Millisecond)
				return ctx.Err()
			}
		})
		m.svc.subMu.Lock()
		m.svc.oweRetryLocked(m.conn.ID, time.Time{})
		m.svc.subMu.Unlock()
		select {
		case <-inRead:
		case <-time.After(20 * time.Second):
			t.Fatal("the round's retry made no read")
		}
		if end == "off" {
			if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
				t.Fatal(err)
			}
		} else if err := m.svc.Remove(m.conn.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case <-rd.done:
		default:
			t.Fatalf("the round runs after the connection was turned %s", end)
		}
		if end == "off" {
			if _, err := m.svc.SetEnabled(m.conn.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Two kinds at their own intervals.
	f := newFixtureWith(t, Cadence{Delta: time.Hour})
	p := &scripted{t: t}
	p.detail = func(id tracker.IssueID) (tracker.Detail, error) { return detailOfCard(card(string(id), 1)), nil }
	f.svc.makeProvider = func(store.TrackerConnection, *clients, *clients) (tracker.Provider, error) { return p, nil }
	jira, linear := f.addJira(), f.addLinear()
	for _, c := range []struct {
		conn  Connection
		every time.Duration
	}{{jira, JiraPollEvery}, {linear, LinearPollEvery}} {
		sub, err := f.svc.SubscribeTicket(c.conn.ID, "10001")
		if err != nil {
			t.Fatal(err)
		}
		if rd := f.svc.roundOf(c.conn.ID); rd == nil || rd.every != c.every || sub.PollEvery != c.every.Milliseconds() {
			t.Fatalf("%s: round %+v, answer every %d", c.conn.Kind, rd, sub.PollEvery)
		}
	}
}
