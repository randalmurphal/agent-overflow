package trackerapp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
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

// After the wall clock steps back, a board's structure and listing are
// read again only once their interval has passed since their stored reads
// by the clock: the step stretches the interval, and nothing is read
// sooner.
func TestAClockStepBackReadsNothingSooner(t *testing.T) {
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
	first := reads()
	for _, step := range []struct {
		pass time.Duration
		what string
	}{{-2 * time.Hour, "right after the step back"}, {2*time.Hour + 59*time.Minute, "59 minutes after the stored reads"}} {
		m.pass(step.pass)
		if res := m.svc.boardTick(p, false, Background); res.failed {
			t.Fatalf("%s: the tick failed: %+v", step.what, res)
		}
		if got := reads(); got != first {
			t.Fatalf("%s: %s; want the first tick's %s", step.what, got, first)
		}
	}
	// The first tick's reads began a few milliseconds apart on the frozen
	// clock: a second covers them.
	m.pass(time.Minute + time.Second)
	if res := m.svc.boardTick(p, false, Background); res.failed {
		t.Fatalf("an hour after the stored reads: the tick failed: %+v", res)
	}
	if m.p.count("Structure") != 2 || m.p.count("BoardIssues") != 2 {
		t.Fatalf("an hour after the stored reads: %s; want each read again once", reads())
	}
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
