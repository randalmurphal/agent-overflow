package trackerapp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// project is the app project the mirror fixture's boards are listed in.
func (m *mirrorFixture) project() string { return "boards-" + m.conn.ID }

// tracks makes the scripted tracker hold rows on every board: a delta
// that returns them all in one page once after each call of tracks and
// nothing after, a complete listing, their rows by ID and no history.
func (m *mirrorFixture) tracks(rows []tracker.Row) {
	m.p.set(func(p *scripted) {
		m.version++
		now := cursorAfter(fmt.Sprint("v", m.version))
		p.changedRows = func(q tracker.DeltaQuery) (tracker.RowPage, error) {
			if bytes.Equal(q.From, now) {
				return tracker.RowPage{Next: now}, nil
			}
			return tracker.RowPage{Rows: rows, Next: now, Catalog: tracker.Catalog{Users: []tracker.User{user1}}}, nil
		}
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: idsOfRows(rows), Complete: true}, nil
		}
		p.boardRows = func(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			var out tracker.Rows
			for _, r := range rows {
				if slices.Contains(asked, r.Card.ID) {
					out.Rows = append(out.Rows, r)
				}
			}
			for _, id := range asked {
				if !slices.ContainsFunc(rows, func(r tracker.Row) bool { return r.Card.ID == id }) {
					out.Missing = append(out.Missing, id)
				}
			}
			return out, nil
		}
		p.history = func(tracker.HistoryQuery) (tracker.History, error) { return tracker.History{}, nil }
	})
}

// subscribe subscribes to board b of the fixture's connection, showing
// the backlog.
func (m *mirrorFixture) subscribe(t *testing.T, b tracker.BoardID) BoardSubscription {
	t.Helper()
	sub, err := m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: b}, []Section{{Kind: SectionBacklog}}, true, untied)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// boardPumpOf is scope's pump; nil for none.
func (s *Service) boardPumpOf(scope Scope) *boardPump {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subs.boards[scope]
}

// A subscribe answers from the mirror alone and never waits for the
// tracker: "not loaded" before the board was ever read, the view after.
// The pump's first tick follows the answer, interactive, and runs every
// unit for a board never read.
func TestBoardSubscribeAnswersFromTheMirror(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	release := make(chan struct{})
	m.p.block = func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	sub := m.subscribe(t, testBoard)
	if sub.Loaded || sub.View != nil || sub.Seq == 0 || sub.PollEvery != JiraPollEvery.Milliseconds() {
		t.Fatalf("answer before any read: %+v", sub)
	}
	close(release)
	events := m.events.boardEvents(t, 1)
	if e := events[0]; e.Seq <= sub.Seq || !e.ReadAgain || !slices.Equal(e.Changed, ids("10001")) || e.Sync.ReadAt == 0 {
		t.Fatalf("first tick's event %+v after answer %d", e, sub.Seq)
	}
	calls, urgent := m.p.callsInOrder(), m.p.interactiveCalls()
	if !slices.Equal(calls, []string{"Structure", "ChangedRows", "BoardIssues", "History"}) || slices.Contains(urgent, false) {
		t.Fatalf("first tick's calls %v, interactive %v", calls, urgent)
	}
	again := m.subscribe(t, testBoard)
	if !again.Loaded || len(again.View.Sections) != 1 || len(again.View.Sections[0].Rows) != 1 || again.Seq <= events[0].Seq {
		t.Fatalf("answer after the first tick: %+v", again)
	}
	if got := len(m.p.callsInOrder()); got != len(calls) {
		t.Fatalf("a second handle on the board made %d more calls", got-len(calls))
	}
	if _, err := m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: "99"}, nil, true, untied); !errors.Is(err, ErrNotListed) {
		t.Fatalf("a board the project does not list: %v", err)
	}
	if _, err := m.svc.SubscribeBoard("other", m.scope(), nil, true, untied); !errors.Is(err, ErrNotListed) {
		t.Fatalf("a project that lists nothing: %v", err)
	}
}

// A board subscribed hidden reads nothing from the tracker until a handle
// shows it or a person asks: a refresh while hidden is its first tick, and
// the first visible vote runs the first tick of a board never refreshed.
func TestABoardSubscribedHiddenReadsWhenShown(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	hidden := func(b tracker.BoardID) BoardSubscription {
		return must[BoardSubscription](t)(m.svc.SubscribeBoard(m.project(), Scope{Connection: m.conn.ID, Board: b}, nil, false, untied))
	}
	sub := hidden(testBoard)
	if err := m.svc.RefreshSubscription(t.Context(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if got := m.p.count("Structure"); got != 1 {
		t.Fatalf("a refresh of a board subscribed hidden ran %d structure reads; want its own alone", got)
	}

	other := hidden("2")
	if err := m.svc.SetSubscriptionVisible(other.ID, true); err != nil {
		t.Fatal(err)
	}
	if e := m.events.boardEvents(t, 2)[1]; e.Scope.Board != "2" || !e.ReadAgain {
		t.Fatalf("the first tick after the vote told %+v", e)
	}
}

// An idle shown board's tick costs one request and two store calls (its
// sync state and the read it records) and emits nothing. An idle round of
// open tickets is pinned by TestIdleRoundCosts.
func TestIdlePumpTickCosts(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	at := time.UnixMilli(1_799_000_000_000).UTC()
	rows := realisticRows(0, 3, at)
	m.tracks(rows)
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	p := m.svc.boardPumpOf(m.scope())
	cs := newCountingStore(m.st)
	m.svc.store = cs
	before := len(m.p.callsInOrder())
	m.svc.boardTick(p, false, Background)
	provider, stored := countsOf(m.p.callsInOrder()[before:]), cs.take()
	t.Logf("idle board tick: provider %s store %s", describeCounts(provider), describeCounts(stored))
	if describeCounts(provider) != "ChangedRows:1" || describeCounts(stored) != "GetMirrorSync:1 RecordMirrorSyncAttempt:1" {
		t.Errorf("an idle board tick cost provider %s, store %s", describeCounts(provider), describeCounts(stored))
	}
	if b, _, _ := m.events.snapshot(); len(b) != 1 {
		t.Errorf("an idle tick emitted %+v", b[1:])
	}

}

// ticketPumpOf is the pump of issue on connection id; nil for none.
func (s *Service) ticketPumpOf(id string, issue tracker.IssueID) *ticketPump {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subs.tickets[ticketKey{connection: id, issue: issue}]
}

// Two handles share one pump; it stops with the last release in either
// order; a release twice does nothing; a subscribe after the last release
// makes a new pump; the cap refuses one handle too many.
func TestBoardHandlesShareOnePump(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"first released first", "second released first"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
			a := m.subscribe(t, testBoard)
			b := m.subscribe(t, testBoard)
			p := m.svc.boardPumpOf(m.scope())
			if p == nil || len(m.svc.subs.handles) != 2 {
				t.Fatalf("pump %v, %d handles", p, len(m.svc.subs.handles))
			}
			first, second := a, b
			if order != "first released first" {
				first, second = b, a
			}
			m.svc.ReleaseSubscription(first.ID)
			m.svc.ReleaseSubscription(first.ID)
			if m.svc.boardPumpOf(m.scope()) != p {
				t.Fatal("one release of two stopped the pump")
			}
			m.svc.ReleaseSubscription(second.ID)
			<-p.exited
			if m.svc.boardPumpOf(m.scope()) != nil {
				t.Fatal("the last release left the pump")
			}
			if err := m.svc.SetSubscriptionVisible(second.ID, false); !errors.Is(err, ErrUnknownSubscription) {
				t.Fatalf("a vote on a released handle: %v", err)
			}
			if err := m.svc.RefreshSubscription(t.Context(), second.ID); !errors.Is(err, ErrUnknownSubscription) {
				t.Fatalf("a refresh on a released handle: %v", err)
			}
			m.subscribe(t, testBoard)
			if q := m.svc.boardPumpOf(m.scope()); q == nil || q == p {
				t.Fatal("a subscribe after the last release did not make a new pump")
			}
		})
	}
	t.Run("cap", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		m.tracks(nil)
		for range maxBoardSubscriptions {
			m.subscribe(t, testBoard)
		}
		if _, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, true, untied); !errors.Is(err, ErrTooManySubscriptions) {
			t.Fatalf("one handle over the cap: %v", err)
		}
	})
}

// A tick whose outcome says More runs again at once, not after the
// interval.
func TestMoreRunsAgainAtOnce(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	var pages [][]tracker.Row
	for p := range deltaPages + 2 {
		pages = append(pages, []tracker.Row{row(string(rune('a'+p))+"0001", 1_799_000_000_000, string(rune('a'+p)))})
	}
	m.tracks(nil)
	m.p.set(func(p *scripted) { p.changedRows = pagedDelta(t, pages...) })
	m.subscribe(t, testBoard)
	// The first tick reads deltaPages pages and stops with More; the walk's
	// last two pages come from the tick that follows at once.
	waitCalls(t, m.p, "ChangedRows", deltaPages+2)
}

// A failed tick retries on the doubling schedule, its failure told once
// however often it repeats and once when it clears; a rate limit waits
// for its release.
func TestFailureScheduleAndEvents(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, RetryBase: 5 * time.Millisecond})
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	var mu sync.Mutex
	var failures int
	var times []time.Time
	m.p.set(func(p *scripted) {
		p.block = func(_ context.Context, name string) error {
			if name != "ChangedRows" {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			times = append(times, time.Now())
			if failures > 0 {
				failures--
				return &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return nil
		}
	})
	mu.Lock()
	failures = 4
	mu.Unlock()
	base := m.p.count("ChangedRows")
	if err := m.svc.RefreshSubscription(t.Context(), m.events.firstHandle(t, m)); err != nil {
		t.Fatal(err)
	}
	// The refresh failed; four retries on the doubling schedule, the last
	// succeeding, with the delta an hour away.
	waitCalls(t, m.p, "ChangedRows", base+5)
	m.events.waitFor(t, "the failure and its clear", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		return len(b) >= 3 && b[len(b)-1].Sync.Failure == nil
	})
	b, _, _ := m.events.snapshot()
	var told []string
	for _, e := range b[1:] {
		if e.Sync.Failure != nil {
			told = append(told, e.Sync.Failure.Kind)
		} else {
			told = append(told, "clear")
		}
	}
	if !slices.Equal(told, []string{string(FailureTransient), "clear"}) {
		t.Fatalf("five reads, four failing, told %v", told)
	}
	mu.Lock()
	gaps := make([]time.Duration, 0, 4)
	for i := len(times) - 4; i < len(times); i++ {
		gaps = append(gaps, times[i].Sub(times[i-1]))
	}
	mu.Unlock()
	for i := 1; i < len(gaps); i++ {
		if gaps[i] < 5*time.Millisecond<<i {
			t.Errorf("retry %d came after %v; the schedule doubles from 5ms: %v", i+1, gaps[i], gaps)
		}
	}

	// A rate limit holds the next read until its release, well past the
	// doubling curve's first step. The release is set when the limit is
	// answered, so a slow run cannot pass it before the read is made.
	var until, next time.Time
	m.p.set(func(p *scripted) {
		p.block = func(_ context.Context, name string) error {
			if name != "ChangedRows" {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case until.IsZero():
				until = time.Now().Add(300 * time.Millisecond)
				return &trackerapi.RateLimitedError{Until: until}
			case next.IsZero():
				next = time.Now()
			}
			return nil
		}
	})
	base = m.p.count("ChangedRows")
	if err := m.svc.RefreshSubscription(t.Context(), m.events.firstHandle(t, m)); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, m.p, "ChangedRows", base+2)
	mu.Lock()
	early := until.Sub(next)
	mu.Unlock()
	if early > 0 {
		t.Fatalf("the read after a rate limit came %v before its release", early)
	}
	m.events.waitFor(t, "the rate limit and its clear", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		return b[len(b)-1].Sync.Failure == nil && slices.ContainsFunc(b, func(e BoardEvent) bool {
			return e.Sync.Failure != nil && e.Sync.Failure.Kind == string(FailureRateLimited)
		})
	})
}

// firstHandle is the first handle the service holds.
func (l *eventLog) firstHandle(t *testing.T, m *mirrorFixture) string {
	t.Helper()
	m.svc.subMu.Lock()
	defer m.svc.subMu.Unlock()
	for id := range m.svc.subs.handles {
		return id
	}
	t.Fatal("no handle")
	return ""
}

func newMirrorFixtureWith(t *testing.T, cad Cadence) *mirrorFixture {
	t.Helper()
	return newMirrorFixtureOn(t, newFixtureWith(t, cad))
}

// The connection list is told only when what it shows moves: a shown
// board's ticks that keep succeeding tell nothing, a failure is told when
// it starts and when it clears, and the same failure again is not.
func TestConnectionsToldOnlyWhenTheProblemMoves(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	p := m.svc.boardPumpOf(m.scope())
	var failing atomic.Bool
	m.p.set(func(s *scripted) {
		s.block = func(_ context.Context, name string) error {
			if name == "ChangedRows" && failing.Load() {
				return &trackerapi.TransientError{Err: errors.New("reset")}
			}
			return nil
		}
	})
	step := func(what string, fail bool, want int64) {
		t.Helper()
		failing.Store(fail)
		before := m.changes.Load()
		m.svc.boardTick(p, false, Background)
		if got := m.changes.Load() - before; got != want {
			t.Fatalf("%s told the connection list %d times, want %d", what, got, want)
		}
	}
	for range 3 {
		step("a tick that succeeded again", false, 0)
	}
	step("the first failed tick", true, 1)
	step("the same failure again", true, 0)
	step("the tick that recovered", false, 1)
	step("a success after the recovery", false, 0)
}

// tieLog records the ties a board subscribe made and the unties run.
type tieLog struct {
	mu      sync.Mutex
	tied    []string
	untied  map[string]int
	refused error
}

func (l *tieLog) tie(id string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refused != nil {
		return nil, l.refused
	}
	l.tied = append(l.tied, id)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.untied == nil {
			l.untied = map[string]int{}
		}
		l.untied[id]++
	}, nil
}

func (l *tieLog) untiedOf(id string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.untied[id]
}

// A board or ticket handle is tied to its client as it is published and
// untied by every removal: a release, its pump's end (the connection
// turned off), and Stop. A refused tie publishes no handle and makes no
// pump.
func TestBoardHandlesAreUntiedByEveryRemoval(t *testing.T) {
	t.Parallel()
	type kind struct {
		subscribe func(m *mirrorFixture, tie Tie) (string, error)
		// held reads the handle's subject, failing once it is gone.
		held func(m *mirrorFixture, id string) error
	}
	kinds := map[string]kind{
		"board": {
			subscribe: func(m *mirrorFixture, tie Tie) (string, error) {
				sub, err := m.svc.SubscribeBoard(m.project(), m.scope(), nil, true, tie)
				return sub.ID, err
			},
			held: func(m *mirrorFixture, id string) error {
				scope, _, err := m.svc.SubscriptionBoard(id)
				if err == nil && scope != m.scope() {
					return fmt.Errorf("the handle's board is %+v", scope)
				}
				return err
			},
		},
		"ticket": {
			subscribe: func(m *mirrorFixture, tie Tie) (string, error) {
				sub, err := m.svc.SubscribeTicket(m.conn.ID, "10001", true, tie)
				return sub.ID, err
			},
			held: func(m *mirrorFixture, id string) error {
				_, err := m.svc.TicketDetail(id)
				return err
			},
		},
	}
	removals := map[string]func(*testing.T, *mirrorFixture, string){
		"released": func(_ *testing.T, m *mirrorFixture, id string) { m.svc.ReleaseSubscription(id) },
		"its connection turned off": func(t *testing.T, m *mirrorFixture, _ string) {
			if _, err := m.svc.SetEnabled(m.conn.ID, false); err != nil {
				t.Fatal(err)
			}
		},
		"stopped": func(_ *testing.T, m *mirrorFixture, _ string) { m.svc.Stop() },
	}
	for kindName, k := range kinds {
		for name, remove := range removals {
			t.Run(kindName+" "+name, func(t *testing.T) {
				t.Parallel()
				m := newMirrorFixture(t)
				m.tracks([]tracker.Row{row("10001", 1_799_000_000_000, "a")})
				ties := &tieLog{}
				id, err := k.subscribe(m, ties.tie)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(ties.tied, id) {
					t.Fatalf("handle %s was published untied (tied %v)", id, ties.tied)
				}
				if err := k.held(m, id); err != nil {
					t.Fatal(err)
				}
				remove(t, m, id)
				if n := ties.untiedOf(id); n != 1 {
					t.Fatalf("the handle was untied %d times, want 1", n)
				}
				if err := k.held(m, id); !errors.Is(err, ErrUnknownSubscription) {
					t.Fatalf("the removed handle: %v", err)
				}
				m.svc.ReleaseSubscription(id)
				if n := ties.untiedOf(id); n != 1 {
					t.Fatalf("a release after the removal untied it again: %d", n)
				}
			})
		}
		t.Run(kindName+" refused", func(t *testing.T) {
			t.Parallel()
			m := newMirrorFixture(t)
			refusal := errors.New("the client's connection is closing")
			ties := &tieLog{refused: refusal}
			if _, err := k.subscribe(m, ties.tie); !errors.Is(err, refusal) {
				t.Fatalf("a refused tie's subscribe = %v", err)
			}
			if b, tk, pumps, tickets := m.svc.SubscriptionCountsForTesting(); b+tk+pumps+tickets != 0 {
				t.Fatalf("a refused tie left %d board handles, %d ticket handles, %d board pumps and %d ticket pumps", b, tk, pumps, tickets)
			}
			if _, err := k.subscribe(m, nil); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("a subscribe with no tie = %v", err)
			}
		})
	}
	t.Run("a ticket's handle", func(t *testing.T) {
		t.Parallel()
		m := newMirrorFixture(t)
		sub, err := m.svc.SubscribeTicket(m.conn.ID, "10001", true, untied)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.svc.SubscriptionBoard(sub.ID); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("a ticket handle's board: %v", err)
		}
	})
}
