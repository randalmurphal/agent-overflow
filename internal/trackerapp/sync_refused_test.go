package trackerapp

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
)

// removalBeforeWrite is a store that, before the first rows write it
// passes on (each one with every), stores a read by ID that found issue
// gone from the page's board, begun after the page's read: another read
// losing a row of the board while the page was in flight.
type removalBeforeWrite struct {
	Store
	m     *mirrorFixture
	issue string
	every bool
	once  sync.Once
}

func (s *removalBeforeWrite) remove(page store.MirrorRowPage) {
	remove := func() {
		gone := store.MirrorRowPage{BoardID: page.BoardID, Lifetime: page.Lifetime, Missing: []string{s.issue}, ReadAt: s.m.now().UnixMilli()}
		if _, err := s.Store.WriteMirrorRows(s.m.root(), gone); err != nil {
			s.m.t.Error(err)
		}
	}
	if s.every {
		remove()
		return
	}
	s.once.Do(remove)
}

func (s *removalBeforeWrite) WriteMirrorRows(root int64, page store.MirrorRowPage) (store.MirrorChanges, error) {
	s.remove(page)
	return s.Store.WriteMirrorRows(root, page)
}

func (s *removalBeforeWrite) WriteMirrorRowPage(root int64, page store.MirrorRowPage, cursor []byte) (store.MirrorChanges, error) {
	s.remove(page)
	return s.Store.WriteMirrorRowPage(root, page, cursor)
}

// A reconcile's read of a listed issue's row that another read's removal
// of an unrelated row overtook is refused by the store, and read again:
// the issue is on the board when the tick ends.
func TestAReconcileReadsARefusedRowAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")}
	m.p.changedRows = pagedDelta(t, board[:1])
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001", "10002")
		p.boardRows = boardOf(board...)
	})
	m.svc.store = &removalBeforeWrite{Store: m.svc.store, m: m, issue: "10009"}
	reads := m.p.count("BoardRows")
	out := must[Outcome](t)(m.svc.tickReconcile(t.Context(), m.scope(), Background))
	if got := m.rowIDs(testBoard); !slices.Equal(got, []string{"10001", "10002"}) || !slices.Equal(out.Changes().Added, ids("10002")) {
		t.Fatalf("rows %v, added %v; want 10002 read again and stored", got, out.Changes().Added)
	}
	if n := m.p.count("BoardRows") - reads; n != 2 {
		t.Fatalf("%d row reads; want the refused one again", n)
	}
}

// A row refused again on its second read fails the read, which the tick's
// retry owes, rather than read it again at once.
func TestARowRefusedTwiceFailsTheRead(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	board := []tracker.Row{row("10001", 1_799_000_000_000, "a"), row("10002", 1_799_000_000_000, "b")}
	m.p.changedRows = pagedDelta(t, board[:1])
	must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	m.p.set(func(p *scripted) {
		p.boardIssues = listing(true, "10001", "10002")
		p.boardRows = boardOf(board...)
	})
	m.svc.store = &removalBeforeWrite{Store: m.svc.store, m: m, issue: "10009", every: true}
	reads := m.p.count("BoardRows")
	if _, err := m.svc.tickReconcile(t.Context(), m.scope(), Background); err == nil || !strings.Contains(err.Error(), "rows moved during both reads") {
		t.Fatalf("a row refused twice: %v; want the read failed", err)
	}
	if n := m.p.count("BoardRows") - reads; n != 2 || slices.Contains(m.rowIDs(testBoard), "10002") {
		t.Fatalf("%d row reads, rows %v; want two reads and 10002 not stored", n, m.rowIDs(testBoard))
	}
}

// A delta page whose new row another read's removal overtook stores no
// cursor and stops the walk with More; the next tick reads the page again
// and stores the row.
func TestADeltaReadsARefusedPageAgain(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	must[Outcome](t)(m.svc.tickStructure(t.Context(), m.scope(), Background))
	m.p.changedRows = pagedDelta(t, []tracker.Row{row("10001", 1_799_000_000_000, "a")})
	m.svc.store = &removalBeforeWrite{Store: m.svc.store, m: m, issue: "10009"}
	out := must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if !out.More() || len(m.rowIDs(testBoard)) != 0 || m.sync(testBoard).Cursor != nil {
		t.Fatalf("the refused page: more %v, rows %v, cursor %q; want more, no row and no cursor", out.More(), m.rowIDs(testBoard), m.sync(testBoard).Cursor)
	}
	out = must[Outcome](t)(m.svc.tickDelta(t.Context(), m.scope(), Background))
	if out.More() || !slices.Equal(m.rowIDs(testBoard), []string{"10001"}) || string(m.sync(testBoard).Cursor) != string(cursorAfter("10001")) {
		t.Fatalf("the page read again: more %v, rows %v, cursor %q", out.More(), m.rowIDs(testBoard), m.sync(testBoard).Cursor)
	}
}

// structureFailsAfterCommit is a store whose structure write fails after
// the structure committed, as a failed settlement of its marks does.
type structureFailsAfterCommit struct{ Store }

func (s *structureFailsAfterCommit) ReplaceMirrorStructure(root int64, st store.MirrorStructure) (store.MirrorStructureWrite, error) {
	w, err := s.Store.ReplaceMirrorStructure(root, st)
	return w, errors.Join(err, errors.New("injected: settle the board's marks"))
}

// A structure write that failed after its structure committed tells what
// the structure changed with the failure.
func TestAStructureThatFailedAfterItsCommitTellsTheChange(t *testing.T) {
	t.Parallel()
	m := newMirrorFixture(t)
	m.tickingClock()
	m.seedBoard(t)
	m.p.set(func(p *scripted) {
		p.structure = func(b tracker.BoardID, _ time.Time) (tracker.Structure, error) { return timeStructure(b), nil }
	})
	m.svc.store = &structureFailsAfterCommit{Store: m.svc.store}
	out, err := m.svc.tickStructure(t.Context(), m.scope(), Background)
	if err == nil || !out.Changes().Structure {
		t.Fatalf("the tick = %+v, %v; want the failure with the structure's change", out, err)
	}
}
