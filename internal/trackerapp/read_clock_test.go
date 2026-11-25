package trackerapp

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// titleOf is the title of issue's card as the mirror serves it.
func (m *mirrorFixture) titleOf(issue tracker.IssueID) string {
	m.t.Helper()
	return must[IssueView](m.t)(m.svc.Issue(m.conn.ID, issue)).Card.Title
}

// After the wall clock steps back and the service starts again, a read the
// new service begins is ordered after every read the root stored: a card
// it reads with the same update time and another title replaces the
// stored one, where a read stamped by the stepped-back clock would be
// refused as the older.
func TestAReadAfterTheClockStepsBackAndARestartIsStored(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	const updated = 1_799_000_000_000
	m.tracks([]tracker.Row{row("10001", updated, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	before := m.sync(testBoard).ReadAt
	m.pass(-time.Hour)
	m.restart(t)
	renamed := row("10001", updated, "a")
	renamed.Card.Title = "Renamed"
	m.tracks([]tracker.Row{renamed})
	if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); out.Failure != nil {
		t.Fatalf("the delta after the step failed: %+v", out.Failure)
	}
	if title := m.titleOf("10001"); title != "Renamed" {
		t.Fatalf("after the step the mirror serves %q; want the card read after it", title)
	}
	if after := m.sync(testBoard).ReadAt; after <= before {
		t.Fatalf("the board's read after the step began at %d; want after the stored %d", after, before)
	}
}

// After the wall clock steps back past a board's stored reads, the board's
// structure and listing are read again once, at the next tick, and the
// intervals are measured from those reads: the tick after reads neither.
// The listing, which does not show yet an issue a delta read just before
// the step, removes no row: the issue was read less than the listing's lag
// ago by the wall clock, so it is left to a read by ID and stays.
func TestAClockStepBackRefreshesOnceAndRemovesNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Reconcile: time.Hour, Structure: time.Hour})
	const updated = 1_799_000_000_000
	m.tracks([]tracker.Row{row("10001", updated, "a")})
	p := idlePump(m)
	tick := func(what string) {
		t.Helper()
		if res := m.svc.boardTick(p, false, Background); res.failed {
			t.Fatalf("%s: the tick failed: %+v", what, res)
		}
	}
	reads := func() string {
		return fmt.Sprintf("%d structure reads, %d listings", m.p.count("Structure"), m.p.count("BoardIssues"))
	}
	tick("the first tick")
	m.tracks([]tracker.Row{row("10001", updated, "a"), row("10002", updated+1, "b")})
	m.p.set(func(p *scripted) {
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: ids("10001"), Complete: true}, nil
		}
	})
	tick("the delta of a new issue")
	both := []string{"10001", "10002"}
	if got := m.rowIDs(testBoard); !slices.Equal(got, both) || reads() != "1 structure reads, 1 listings" {
		t.Fatalf("before the step: rows %v, %s", got, reads())
	}
	m.pass(-2 * time.Hour)
	for _, what := range []string{"right after the step back", "the tick after"} {
		tick(what)
		if got := m.rowIDs(testBoard); reads() != "2 structure reads, 2 listings" || !slices.Equal(got, both) {
			t.Fatalf("%s: %s, rows %v; want each read again once and rows %v", what, reads(), got, both)
		}
	}
}

// After the wall clock steps forward an hour, the interval of a board's
// structure and listing, the board's structure and listing are read again
// once, and the intervals are measured from those reads.
func TestAForwardClockStepOfAnHourRefreshesOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Reconcile: time.Hour, Structure: time.Hour})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	p := idlePump(m)
	reads := func() string {
		return fmt.Sprintf("%d structure reads, %d listings", m.p.count("Structure"), m.p.count("BoardIssues"))
	}
	if res := m.svc.boardTick(p, false, Background); res.failed {
		t.Fatalf("the first tick failed: %+v", res)
	}
	m.pass(time.Hour)
	for _, what := range []string{"right after the step", "the tick after"} {
		if res := m.svc.boardTick(p, false, Background); res.failed {
			t.Fatalf("%s: the tick failed: %+v", what, res)
		}
		if got := reads(); got != "2 structure reads, 2 listings" {
			t.Fatalf("%s: %s; want each read again once", what, got)
		}
	}
}

// Every time a person is shown is a wall time. After the wall clock steps
// back past the stored reads, the board tick and the ticket read the step
// makes due show the times they began by the clock, none ahead of it: the
// board's read and reconcile in the pump's sync view and the board view, a
// failure after them, and the open ticket's read; the order times those
// reads stored run ahead of the clock.
func TestNoShownTimeIsAheadOfTheClockAfterAStepBack(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Reconcile: time.Hour, Structure: time.Hour, Ticket: time.Hour, RetryBase: time.Hour})
	r := row("10001", 1_799_000_000_000, "a")
	m.tracks([]tracker.Row{r})
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return detailOfCard(r.Card), nil }
	})
	p := idlePump(m)
	tick := func(what string, refresh bool) tickResult {
		t.Helper()
		res := m.svc.boardTick(p, refresh, Interactive)
		t.Logf("%s: %+v", what, res)
		return res
	}
	if res := tick("the first tick", false); res.failed {
		t.Fatal("the first tick failed")
	}
	sub := must[TicketSubscription](t)(m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied))
	quiet(t, m.svc)
	m.pass(-2 * time.Hour)
	if res := tick("the tick after the step", false); res.failed {
		t.Fatal("the tick after the step failed")
	}
	m.round(t)
	now := millis(m.now())
	ahead := func(what string, at int64) {
		t.Helper()
		if at == 0 || at > now {
			t.Errorf("%s shows %d; want a time no later than the clock's %d", what, at, now)
		}
	}
	m.svc.subMu.Lock()
	told := p.toldLocked()
	m.svc.subMu.Unlock()
	ahead("the pump's read", told.ReadAt)
	ahead("the pump's reconcile", told.ReconciledAt)
	board := must[BoardView](t)(m.svc.Board(m.scope(), nil))
	ahead("the board view's read", board.Sync.ReadAt)
	ahead("the board view's reconcile", board.Sync.ReconciledAt)
	ahead("the open ticket's read", m.ticketView(t, sub.ID).ReadAt)
	if st := m.sync(testBoard); st.ReadAt <= now {
		t.Fatalf("the board's stored read is at order time %d, not ahead of the clock's %d: the step back was not past it", st.ReadAt, now)
	}
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			return tracker.RowPage{}, &trackerapi.StatusError{Tracker: "jira", Status: 503}
		}
	})
	if res := tick("a refresh that fails", true); !res.failed {
		t.Fatal("the failing refresh did not fail")
	}
	m.svc.subMu.Lock()
	told = p.toldLocked()
	m.svc.subMu.Unlock()
	if told.Failure == nil {
		t.Fatal("the pump shows no failure")
	}
	ahead("the pump's failure", told.Failure.At)
	board = must[BoardView](t)(m.svc.Board(m.scope(), nil))
	if board.Sync.Failure == nil {
		t.Fatal("the board view shows no failure")
	}
	ahead("the board view's failure", board.Sync.Failure.At)
}

// beginRefused is the store with every read's begin failing with err.
type beginRefused struct {
	*store.Store
	err error
}

func (s *beginRefused) BeginMirrorRead(int64, int64) (store.MirrorRead, error) {
	return store.MirrorRead{}, s.err
}

// A tick whose read cannot begin makes no tracker call: a root gone ends
// the tick as a failed write does, and any other failure is the tick's
// error.
func TestATickWhoseReadCannotBeginCallsNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	calls := len(m.p.callsInOrder())
	m.svc.store = &beginRefused{Store: m.st, err: fmt.Errorf("test: %w", store.ErrTrackerCacheGone)}
	if out, err := m.svc.tickDelta(t.Context(), m.scope(), Background); err != nil || !out.Ended || out.Failure != nil {
		t.Fatalf("a gone root's tick = %+v, %v; want it ended with no failure", out, err)
	}
	m.svc.store = &beginRefused{Store: m.st, err: errStoreDown}
	if _, err := m.svc.tickDelta(t.Context(), m.scope(), Background); !errors.Is(err, errStoreDown) {
		t.Fatalf("a failed begin's tick: %v; want the store's failure", err)
	}
	if got := m.p.callsInOrder()[calls:]; len(got) != 0 {
		t.Fatalf("the ticks called the tracker: %v", got)
	}
}

// Reads begun in one millisecond of a frozen clock run ahead of it, a
// millisecond a read, each after the last: three delta ticks in that
// millisecond each store their card's new title, and the board's read
// began three or more milliseconds after a read begun before them.
func TestReadsInOneMillisecondRunAheadOfTheClock(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	start := m.read()
	for i := range 3 {
		r := row("10001", 1_799_000_000_000, "a")
		r.Card.Title = fmt.Sprint("Title ", i)
		m.tracks([]tracker.Row{r})
		if out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background)); out.Failure != nil {
			t.Fatalf("delta %d failed: %+v", i, out.Failure)
		}
		if title := m.titleOf("10001"); title != r.Card.Title {
			t.Fatalf("after delta %d the mirror serves %q; want %q", i, title, r.Card.Title)
		}
	}
	if now, got := millis(m.now()), m.sync(testBoard).ReadAt; start.At() <= now || got < start.At()+3 {
		t.Fatalf("the clock reads %d, the board's read began at %d; want the read begun before the ticks, %d, ahead of the clock and the board's at least 3 ms after it", now, got, start.At())
	}
}
