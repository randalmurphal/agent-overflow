package store

import (
	"database/sql/driver"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// A board view is one snapshot: a sync that commits between its reads
// changes none of it, so its totals count the rows its pages hold, its
// names are those the rows had, and a page continued from it at the
// revision it reported is stale once the board changed. It runs its reads
// in one transaction, one statement per part (pinned for three sections
// whose cards name a user, statuses and a parent). It runs alone: it
// hooks the package's statement runs.
func TestMirrorBoardViewIsOneSnapshot(t *testing.T) {
	var armed atomic.Bool
	var s *Store
	var root int64
	var lifetime int64
	checked := cachedStmtRan
	cachedStmtRan = func(stmt driver.Stmt, query string) {
		checked(stmt, query)
		if strings.Contains(query, "FROM tracker_cache_board_column_statuses") && armed.CompareAndSwap(true, false) {
			// A sync commits between the view's reads.
			_, err := s.WriteMirrorRows(root, MirrorRowPage{BoardID: "10", Lifetime: lifetime, Rows: []MirrorRow{webRow("10003", 5, "c")}, ReadAt: mirrorReadAt(9)})
			if err != nil {
				t.Error(err)
			}
			if _, err := s.db.Exec(`UPDATE tracker_cache_statuses SET name = 'Renamed'`); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { cachedStmtRan = checked })
	s, _, root = newMirror(t)
	child := webRow("10001", 5, "a", "21")
	child.Card.ParentID = "10002"
	writeMirrorBoard(t, s, root, "10", []MirrorRow{child, webRow("10002", 5, "b")})
	lifetime = currentLifetime(s, root, "10")
	sections := []MirrorSection{{Kind: MirrorSectionBacklog}, {Kind: MirrorSectionSprint, SprintID: "21"}, {Kind: MirrorSectionColumn}}
	var view MirrorBoardView
	var ok bool
	var err error
	statements := countMirrorStatements(t, func() { view, ok, err = s.ReadMirrorBoardView(root, "10", sections) })
	if err != nil || !ok || statements != 11 {
		t.Fatalf("view = %v, %v in %d statements; want 11", ok, err, statements)
	}
	armed.Store(true)
	view, ok, err = s.ReadMirrorBoardView(root, "10", sections)
	if err != nil || !ok || armed.Load() {
		t.Fatalf("view = %v, %v; the sync ran %v", ok, err, !armed.Load())
	}
	if backlog := view.Pages[0]; view.Counts.Backlog.Rows != len(backlog.Rows) || view.Counts.All.Rows != 2 ||
		backlog.Revision != view.Counts.Revision || view.Pages[2].Revision != view.Counts.Revision {
		t.Fatalf("backlog total %d with %d rows, all %d, revisions %d %d %d; want one snapshot",
			view.Counts.Backlog.Rows, len(backlog.Rows), view.Counts.All.Rows, view.Counts.Revision, backlog.Revision, view.Pages[2].Revision)
	}
	if view.Structure.Board.ID != "10" || len(view.Structure.Sprints) != 3 || view.Structure.Lifetime != lifetime {
		t.Fatalf("structure = %+v", view.Structure)
	}
	names := view.Names.Catalog
	if len(names.Users) != 1 || len(names.Statuses) == 0 || len(view.Names.Parents) != 1 || view.Names.Parents[0].Key != "WEB-10002" || slices.ContainsFunc(names.Statuses, func(st MirrorStatus) bool { return st.Name == "Renamed" }) {
		t.Fatalf("names = %+v; want those of the snapshot", names)
	}
	page, err := s.ListMirrorSection(root, "10", MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
	if err != nil || len(page.Names.Catalog.Statuses) == 0 || page.Names.Catalog.Statuses[0].Name != "Renamed" {
		t.Fatalf("a section page's names = %+v, %v; want them read with its rows", page.Names, err)
	}
	if _, err := s.ListMirrorSection(root, "10", MirrorSection{Kind: MirrorSectionBacklog}, view.Pages[0].Next); !errors.Is(err, ErrTrackerCacheStale) {
		t.Fatalf("a page continued from the view after the board changed: %v; want stale", err)
	}
}
