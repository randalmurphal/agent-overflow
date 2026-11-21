package trackerapp

import (
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
)

// A card changed by one board's read reaches the subscribers of every
// other board showing the issue, and their patch holds the new card.
func TestAChangedCardReachesEveryBoardShowingIt(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	held := card("10001", 1_799_000_000_000)
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	m.subscribe(t, testBoard)
	m.subscribe(t, "2")
	m.events.boardEvents(t, 2)

	held.Updated, held.Title = held.Updated.Add(time.Minute), "Renamed"
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	if _, err := m.svc.syncTick(t.Context(), m.scope(), BoardDue{Delta: true}, Background); err != nil {
		t.Fatal(err)
	}
	other := Scope{Connection: m.conn.ID, Board: "2"}
	m.events.waitFor(t, "board 2's event", func(b []BoardEvent, _ []IssueEvent, _ []error) bool {
		return slices.ContainsFunc(b[2:], func(e BoardEvent) bool {
			return e.Scope == other && slices.Equal(e.Changed, ids("10001"))
		})
	})
	patch := must[BoardPatch](t)(m.svc.BoardPatch(other, []Section{{Kind: SectionBacklog}}, ids("10001"), TotalsAt{}))
	if rows := patch.Sections[0].Rows; len(rows) != 1 || rows[0].Card.Title != "Renamed" {
		t.Fatalf("board 2's patch: %+v", patch.Sections[0])
	}
}

// A parent whose row only moves in its board's order (a rank) tells the
// boards showing its children nothing of them, which a row shows only the
// parent's key and title of; a parent that gains a row, and may be kept
// again, tells them.
func TestAParentsRankTellsNoBoardOfItsChildren(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour})
	rows := []tracker.Row{row("10001", 1_799_000_000_000, "a")}
	for i := range 3 {
		child := row(fmt.Sprint(10002+i), 1_799_000_000_000, fmt.Sprint("b", i))
		child.Card.Parent = "10001"
		rows = append(rows, child)
	}
	m.tracks(rows)
	m.subscribe(t, testBoard)
	m.subscribe(t, "2")
	idleBoards(t, m.svc, m.p)
	other := Scope{Connection: m.conn.ID, Board: "2"}
	told := func(what string, scope Scope, rows []tracker.Row, want map[tracker.BoardID][]tracker.IssueID) {
		t.Helper()
		before, _, _ := m.events.snapshot()
		m.tracks(rows)
		if _, err := m.svc.syncTick(t.Context(), scope, BoardDue{Delta: true}, Background); err != nil {
			t.Fatal(err)
		}
		after, _, _ := m.events.snapshot()
		got := map[tracker.BoardID][]tracker.IssueID{}
		for _, e := range after[len(before):] {
			got[e.Scope.Board] = unite(got[e.Scope.Board], e.Changed)
		}
		if !maps.EqualFunc(got, want, slices.Equal) {
			t.Errorf("%s told %v; want %v", what, got, want)
		}
	}
	ranked := slices.Clone(rows)
	ranked[0].Order = "z"
	told("a rank of the parent", m.scope(), ranked, map[tracker.BoardID][]tracker.IssueID{testBoard: ids("10001")})
	// The parent leaves board 2 and comes back, its card unchanged. Its row
	// was read under a minute ago, so the reconcile reads it by ID too.
	m.p.set(func(p *scripted) {
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{IDs: ids("10002", "10003", "10004"), Complete: true}, nil
		}
		held := p.boardRows
		p.boardRows = func(b tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
			out, err := held(b, asked)
			if i := slices.IndexFunc(out.Rows, func(r tracker.Row) bool { return r.Card.ID == "10001" }); b == other.Board && i >= 0 {
				out.Rows = slices.Delete(out.Rows, i, i+1)
				out.Missing = append(out.Missing, "10001")
			}
			return out, err
		}
	})
	if out := must[Outcome](t)(m.svc.syncTick(t.Context(), other, BoardDue{Reconcile: true}, Background)); !slices.Equal(out.Changes().Removed, ids("10001")) {
		t.Fatalf("the reconcile removed %v", out.Changes().Removed)
	}
	told("the parent's row added again", other, ranked, map[tracker.BoardID][]tracker.IssueID{
		testBoard: ids("10002", "10003", "10004"), other.Board: ids("10001", "10002", "10003", "10004")})
}

// A delta read that redelivers rows identical to the stored ones, as the
// delta's overlap does on every tick for a minute after a change, tells
// no board and no open ticket anything, so no client reads a patch; it
// costs the store calls pinned here (statements in the store's
// TestIdleTickStatements).
func TestRedeliveredRowsTellNothing(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: time.Hour, Ticket: time.Hour})
	rows := realisticRows(0, 3, time.UnixMilli(1_799_000_000_000).UTC())
	m.tracks(rows)
	m.p.set(func(p *scripted) {
		p.detail = func(tracker.IssueID) (tracker.Detail, error) { return tracker.Detail{Card: rows[0].Card}, nil }
	})
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	if _, err := m.svc.SubscribeTicket(m.conn.ID, rows[0].Card.ID); err != nil {
		t.Fatal(err)
	}
	m.events.issueEvents(t, 1)
	reads := 0
	m.p.set(func(p *scripted) {
		p.changedRows = func(tracker.DeltaQuery) (tracker.RowPage, error) {
			reads++
			return tracker.RowPage{Rows: rows, Next: cursorAfter(fmt.Sprint("r", reads)), Catalog: tracker.Catalog{Users: []tracker.User{user1}}}, nil
		}
	})
	cs := newCountingStore(m.st)
	m.svc.store = cs
	boards, issues, _ := m.events.snapshot()
	p := m.svc.boardPumpOf(m.scope())
	for range 2 {
		before := len(m.p.callsInOrder())
		cs.take()
		m.svc.boardTick(p, false, Background)
		provider, stored := describeCounts(countsOf(m.p.callsInOrder()[before:])), describeCounts(cs.take())
		if provider != "ChangedRows:1" || stored != "GetMirrorSync:1 RecordMirrorSyncAttempt:1 WriteMirrorRowPage:1" {
			t.Errorf("a redelivering tick cost provider %s, store %s", provider, stored)
		}
	}
	if b, i, _ := m.events.snapshot(); len(b) != len(boards) || len(i) != len(issues) {
		t.Fatalf("redelivered rows told boards %+v, issues %+v", b[len(boards):], i[len(issues):])
	}
}

// A board's pending event unites what it is told until it is sent, and
// past maxEventIssues IDs says only to read the board again.
func TestPendingBoardEventIsBounded(t *testing.T) {
	t.Parallel()
	var b boardPending
	b.add(nil, nil, false)
	if b.due {
		t.Fatal("nothing made an event due")
	}
	b.add(ids("1"), nil, false)
	b.add(ids("1", "2"), ids("3"), false)
	if !b.due || !slices.Equal(b.changed, ids("1", "2")) || !slices.Equal(b.removed, ids("3")) || b.readAgain || b.truncated {
		t.Fatalf("pending %+v", b)
	}
	var many []tracker.IssueID
	for i := range maxEventIssues {
		many = append(many, tracker.IssueID(fmt.Sprint(100+i)))
	}
	b.add(many, nil, false)
	if !b.truncated || !b.readAgain || b.changed != nil || b.removed != nil {
		t.Fatalf("pending past the bound %+v", b)
	}
	b.add(ids("4"), nil, false)
	if b.changed != nil {
		t.Fatalf("a truncated event gathered %v", b.changed)
	}
}

// A shown board's tick reads the history when an issue's history is
// behind its card, and not again once it caught up.
func TestABoardReadsTheHistoryDue(t *testing.T) {
	t.Parallel()
	m := newMirrorFixtureWith(t, Cadence{Delta: 10 * time.Millisecond})
	var clock atomic.Int64
	clock.Store(m.now().UnixMilli())
	m.svc.now = func() time.Time { return time.UnixMilli(clock.Load()) }
	held := card("10001", 1_799_000_000_000)
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	m.subscribe(t, testBoard)
	m.events.boardEvents(t, 1)
	read := m.p.count("History")

	// Changed after the history read covered, which a read now covers.
	held.Updated = m.now().Add(time.Minute)
	clock.Add(2 * time.Minute.Milliseconds())
	m.tracks([]tracker.Row{{Card: held, Order: "a"}})
	waitCalls(t, m.p, "History", read+1)
	deltas := m.p.count("ChangedRows")
	waitCalls(t, m.p, "ChangedRows", deltas+3)
	if n := m.p.count("History"); n != read+1 {
		t.Fatalf("the history was read %d times after it caught up", n-read-1)
	}
}
