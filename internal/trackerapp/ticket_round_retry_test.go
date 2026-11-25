package trackerapp

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// A failed read, a pump's own or a round's, is retried by the round on its
// schedule with no round due: the ticket recovers on its own, told once
// that it failed and once that it recovered.
func TestTheRoundRetriesFailedTicketReads(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour, RetryBase: time.Millisecond})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	var reads, failing atomic.Int32
	m.p.set(func(p *scripted) {
		p.detail = func(id tracker.IssueID) (tracker.Detail, error) {
			reads.Add(1)
			if failing.Add(-1) >= 0 {
				return tracker.Detail{}, errors.New("test: refused")
			}
			return detailOfCard(card(string(id), 1_799_000_000_000)), nil
		}
	})
	// The pump's first read and the round's first retry fail.
	failing.Store(2)
	if _, err := m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied); err != nil {
		t.Fatal(err)
	}
	got := m.events.issueEvents(t, 2)
	if len(got) != 2 || got[0].Failure == nil || got[1].Failure != nil || !got[1].Changed {
		t.Fatalf("events %+v", got)
	}
	if n := reads.Load(); n != 3 {
		t.Fatalf("%d reads; the pump's and two by the round's retries", n)
	}

	// A round of two tickets that fails retries itself.
	failing.Store(0)
	if _, err := m.svc.SubscribeTicket(m.conn.ID, "10002", true, untied); err != nil {
		t.Fatal(err)
	}
	m.events.issueEvents(t, 3)
	m.settledTickets(t)
	failing.Store(1)
	m.readSoon("10001", "10002")
	got = m.events.issueEvents(t, 7)[3:]
	if len(got) != 4 || got[0].Failure == nil || got[1].Failure == nil || got[2].Failure != nil || got[3].Failure != nil {
		t.Fatalf("events %+v", got)
	}
}

// goneStore reports the connection's mirror gone while gone is set, as a
// detach does to a run that held its root.
type goneStore struct {
	*store.Store
	gone atomic.Bool
}

func (s *goneStore) GetMirrorSync(root int64, board string, now int64) (store.MirrorSync, bool, error) {
	if s.gone.Load() {
		return store.MirrorSync{}, false, store.ErrTrackerCacheGone
	}
	return s.Store.GetMirrorSync(root, board, now)
}

func (s *goneStore) GetOrCreateMirrorRoot(id string) (int64, bool, error) {
	if s.gone.Load() {
		return 0, false, store.ErrTrackerCacheGone
	}
	return s.Store.GetOrCreateMirrorRoot(id)
}

// A read that finds the connection's mirror gone ends its tickets, each
// told once, and the round, left with no shown ticket, stops.
func TestTheRoundStopsWhenAReadEndsItsTickets(t *testing.T) {
	t.Parallel()
	m, _, issues, _ := newRoundFixture(t, 1, false)
	rd := m.svc.roundOf(m.conn.ID)
	gs := &goneStore{Store: m.st}
	m.svc.store = gs
	gs.gone.Store(true)
	events := m.issueEventCount()
	m.round(t)
	if got := m.issueEventsSince(events); len(got) != 1 || got[0].Issue != issues[0] || !got[0].Ended {
		t.Fatalf("events %+v", got)
	}
	waitClosed(t, "the round with every ticket ended", rd.done)
	if m.svc.roundOf(m.conn.ID) != nil {
		t.Fatal("a round with no ticket")
	}
}
