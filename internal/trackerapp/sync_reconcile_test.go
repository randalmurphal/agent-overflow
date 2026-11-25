package trackerapp

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/tracker"
	"agent-overflow/internal/tracker/trackerapi"
)

// boardOf scripts BoardRows as a board holding rows by ID; an issue not
// in it is Missing.
func boardOf(rows ...tracker.Row) func(tracker.BoardID, []tracker.IssueID) (tracker.Rows, error) {
	return func(_ tracker.BoardID, asked []tracker.IssueID) (tracker.Rows, error) {
		var out tracker.Rows
		for _, id := range asked {
			i := slices.IndexFunc(rows, func(r tracker.Row) bool { return r.Card.ID == id })
			if i < 0 {
				out.Missing = append(out.Missing, id)
				continue
			}
			out.Rows = append(out.Rows, rows[i])
		}
		return out, nil
	}
}

func listing(complete bool, s ...string) func(tracker.BoardID, time.Time) (tracker.Listing, error) {
	return func(tracker.BoardID, time.Time) (tracker.Listing, error) {
		return tracker.Listing{IDs: ids(s...), Complete: complete}, nil
	}
}

func TestReconcileAddsAndRemoves(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b"), row("10003", 1_799_000_000_000, "c")}
	m.p.boardIssues = listing(true, "10001", "10002")
	m.p.boardRows = boardOf(board...)
	var windows []time.Time
	m.p.set(func(p *scripted) {
		list := p.boardIssues
		p.boardIssues = func(b tracker.BoardID, w time.Time) (tracker.Listing, error) {
			windows = append(windows, w)
			return list(b, w)
		}
	})
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if !slices.Equal(out.Changes().Rows, ids("10001", "10002")) || out.More() || len(out.Changes().Removed) != 0 {
		t.Fatalf("first reconcile %+v", out)
	}
	if !windows[0].Equal(m.now().Add(-HistoryWindow)) {
		t.Fatalf("listed from %v", windows[0])
	}
	if st := m.sync(testBoard); st.ReconciledAt != m.now().UnixMilli() {
		t.Fatalf("sync state %+v", st)
	}
	// 10001 leaves the board, 10003 joins it. The listing does not judge
	// 10001, read under a minute ago; the read by ID finds it gone.
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10002", "10003")
		p.boardRows = boardOf(board[1:]...)
	})
	out = must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if !slices.Equal(out.Changes().Rows, ids("10003")) || !slices.Equal(out.Changes().Removed, ids("10001")) {
		t.Fatalf("second reconcile %+v", out)
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10002", "10003"}) {
		t.Fatalf("rows %v", got)
	}
	// Called again it reads nothing more.
	reads := m.p.count("BoardRows")
	if out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background)); !out.Changes().Empty() || m.p.count("BoardRows") != reads {
		t.Fatalf("third reconcile %+v", out)
	}
}

// A listing that may show an issue late removes no row on its own word,
// however long ago the row was read: an unlisted row is read by ID in the
// same tick, kept while the board holds it (the listing's index has not
// caught up) and removed once it does not. The rows the first reconcile
// reads by ID are named by the second's listing.
func TestReconcileReadsUnlistedRowsByID(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")}
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001", "10002")
		p.boardRows = boardOf(board...)
	})
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	// 10002 is on the board and its list misses it: read by ID, kept, told
	// nothing.
	m.pass(tracker.DeltaOverlap - time.Millisecond)
	m.p.set(func(p *scripted) { p.boardIssues = listing(true, "10001") })
	reads := m.p.count("BoardRows")
	if out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background)); !out.Changes().Empty() || m.p.count("BoardRows") != reads+1 {
		t.Fatalf("a listing that missed a row read %v ago: %+v, %d rows reads", tracker.DeltaOverlap, out, m.p.count("BoardRows")-reads)
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002"}) {
		t.Fatalf("rows %v", got)
	}
	// Long past the overlap, the listing still leaves the row to the read
	// by ID, which keeps it while the board holds it and removes it once
	// the board does not.
	m.pass(time.Hour)
	reads = m.p.count("BoardRows")
	if out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background)); !out.Changes().Empty() || m.p.count("BoardRows") != reads+1 {
		t.Fatalf("a listing that missed a row read an hour ago: %+v, %d rows reads", out, m.p.count("BoardRows")-reads)
	}
	m.p.set(func(p *scripted) { p.boardRows = boardOf(board[0]) })
	reads = m.p.count("BoardRows")
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if !slices.Equal(out.Changes().Removed, ids("10002")) || m.p.count("BoardRows") != reads+1 {
		t.Fatalf("a listing that missed a row the board no longer holds: %+v, %d rows reads", out, m.p.count("BoardRows")-reads)
	}
}

// A row only a read by ID placed, which no listing has named since, is
// never judged by a listing that leaves it out, however long after: each
// reconcile reads it by ID, which keeps it while the board holds it and
// removes it once the board does not.
func TestARowNoListingNamedIsReadByID(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001", "10002")
		p.boardRows = boardOf(row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b"))
	})
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) { p.boardIssues = listing(true, "10001") })
	for range 2 {
		m.pass(10 * tracker.DeltaOverlap)
		reads := m.p.count("BoardRows")
		if out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background)); !out.Changes().Empty() || m.p.count("BoardRows") != reads+1 {
			t.Fatalf("a listing leaving out a row only read by ID: %+v, %d rows reads", out, m.p.count("BoardRows")-reads)
		}
		if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002"}) {
			t.Fatalf("rows %v", got)
		}
	}
	m.p.set(func(p *scripted) { p.boardRows = boardOf(row("10001", 1_799_000_000_000, "a")) })
	m.pass(10 * tracker.DeltaOverlap)
	if out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background)); !slices.Equal(out.Changes().Removed, ids("10002")) {
		t.Fatalf("once the board does not hold it: %+v", out)
	}
}

func TestAnIncompleteListingAddsAndNeverRemoves(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.boardIssues = listing(true, "10001", "10002")
	m.p.boardRows = boardOf(row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b"), row("10003", 1_799_000_000_000, "c"))
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) { p.boardIssues = listing(false, "10003") })
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if !slices.Equal(out.Changes().Rows, ids("10003")) || len(out.Changes().Removed) != 0 {
		t.Fatalf("incomplete reconcile %+v", out)
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002", "10003"}) {
		t.Fatalf("rows %v", got)
	}
}

// A board newly listed over issues another board already mirrored gets
// their rows: the reconcile reads them, though their cards are stored.
func TestReconcileOfABoardNewlyListedOverMirroredIssues(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	shared := row("10001", 1_799_000_000_000, "a")
	m.p.changedRows = pagedDelta(t, []tracker.Row{shared})
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	other := Scope{Connection: m.conn.ID, Board: "2"}
	onOther := shared
	onOther.Order = "z"
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001")
		p.boardRows = boardOf(onOther)
	})
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), other, Background))
	if !slices.Equal(out.Changes().Rows, ids("10001")) || len(out.Changes().Cards) != 0 {
		t.Fatalf("reconcile of the new board %+v", out)
	}
	rows, _, err := m.st.ListMirrorIssueRows(m.root(), "10001", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].OrderKey != "a" || rows[1].OrderKey != "z" {
		t.Fatalf("the issue's rows %+v", rows)
	}
}

// A reconcile past its bound stops with More and records no reconcile
// time until a call reads every missing row.
func TestReconcileStopsAtItsBound(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	n := reconcileBatches*readBatch + 1
	listed := make([]tracker.IssueID, n)
	var all []tracker.Row
	for i := range listed {
		id := fmt.Sprintf("%05d", 20000+i)
		listed[i] = tracker.IssueID(id)
		all = append(all, row(id, 1_799_000_000_000, id))
	}
	m.p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
		return tracker.Listing{IDs: listed, Complete: true}, nil
	}
	m.p.boardRows = boardOf(all...)
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if !out.More() || len(out.Changes().Rows) != n-1 || m.sync(testBoard).ReconciledAt != 0 {
		t.Fatalf("bounded reconcile: more %v, %d rows, reconciled at %d", out.More(), len(out.Changes().Rows), m.sync(testBoard).ReconciledAt)
	}
	out = must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if out.More() || len(out.Changes().Rows) != 1 || m.sync(testBoard).ReconciledAt == 0 {
		t.Fatalf("finishing reconcile %+v", out)
	}
}

// A first sync whose delta walk is longer than one tick's pages, run as
// the pump runs it, reads each row once through the walk and lists the
// board once, after the walk: the reconcile of a tick whose delta has
// more to read waits for it.
func TestAFirstSyncLongerThanATickListsTheBoardOnce(t *testing.T) {
	t.Parallel()
	svc, p, scope := benchService(t)
	n := deltaPages*readBatch + readBatch/2
	newScaleBoard(n).serve(p, false)
	ticks := pumpTicks(t, svc, scope)
	calls := countsOf(p.callsInOrder())
	// The walk's pages, then one idle page a tick; the history of every
	// issue, historyBatches reads a tick.
	pages := (n+readBatch-1)/readBatch + ticks - 2
	want := map[string]int{"Structure": 1, "ChangedRows": pages, "BoardIssues": 1, "History": (n + historyBatch - 1) / historyBatch}
	if !maps.Equal(calls, want) {
		t.Errorf("a first sync of %d rows in %d ticks called %s; want %s", n, ticks, describeCounts(calls), describeCounts(want))
	}
	if v := must[BoardView](t)(svc.Board(scope, StatusGroups{}, nil)); v.All.Rows != n || v.Sync.ReconciledAt == 0 {
		t.Errorf("the board holds %d rows of %d, reconciled at %d", v.All.Rows, n, v.Sync.ReconciledAt)
	}
}

func TestReconcileFailureKeepsTheRows(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.p.boardIssues = listing(true, "10001")
	m.p.boardRows = boardOf(row("10001", 1_799_000_000_000, "a"))
	must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.boardIssues = func(tracker.BoardID, time.Time) (tracker.Listing, error) {
			return tracker.Listing{}, &trackerapi.StatusError{Tracker: "jira", Status: 403}
		}
	})
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if out.Failure == nil || out.Failure.Kind != FailureForbidden || m.problem() != FailureForbidden {
		t.Fatalf("failed reconcile %+v", out)
	}
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001"}) {
		t.Fatalf("a failure removed rows: %v", got)
	}
}

func TestStructureUnit(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	out := must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	if !out.Changes().Structure || out.Failure != nil {
		t.Fatalf("first structure %+v", out)
	}
	if out := must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background)); out.Changes().Structure {
		t.Fatalf("an unchanged structure %+v", out)
	}
	m.p.set(func(p *scripted) {
		p.structure = func(tracker.BoardID, time.Time) (tracker.Structure, error) {
			return tracker.Structure{}, &trackerapi.TransientError{Err: errors.New("timeout")}
		}
	})
	if out := must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background)); out.Failure == nil || out.Failure.Kind != FailureTransient {
		t.Fatalf("failed structure %+v", out)
	}
	// The tracker answering with another board is a failure, not stored.
	m.p.set(func(p *scripted) {
		p.structure = func(tracker.BoardID, time.Time) (tracker.Structure, error) { return webStructure("7"), nil }
	})
	if out := must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background)); out.Failure == nil || out.Failure.Kind != FailureTracker {
		t.Fatalf("structure of another board %+v", out)
	}
	if _, ok, _ := m.st.GetMirrorBoard(m.root(), "7"); ok {
		t.Fatal("the other board was stored")
	}
}
