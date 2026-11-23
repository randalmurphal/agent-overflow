package store

import (
	"errors"
	"testing"
)

// A board's lifetime outlives its cached rows: a board unlisted, drained
// and listed again starts a lifetime no earlier one had, and every write
// and section continuation captured in the earlier lifetime is refused in
// the later one, before its drain, after it and once the board is synced
// afresh.
func TestMirrorBoardLifetimeFencesItsReads(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	// Board 12 holds the newest lifetime, the one a reused ID would give again.
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	old := currentLifetime(s, root, "12")
	if st, ok, err := s.GetMirrorSync(root, "12", testNow); err != nil || st.Lifetime != old {
		t.Fatalf("captured lifetime = %+v, %v, %v; want %d", st, ok, err, old)
	}
	column := MirrorSection{Kind: MirrorSectionColumn}
	first, err := s.ListMirrorSection(root, "12", column, MirrorRowKey{})
	if err != nil || first.Next.Lifetime != old {
		t.Fatalf("first page = %+v, %v", first.Next, err)
	}
	after := MirrorRowKey{OrderKey: "a", IssueID: "10001", Lifetime: old, Revision: first.Revision}

	late := webStructure()
	late.ReadAt, late.Lifetime, late.Board.Name = mirrorReadAt(50), old, "Late name"
	page := MirrorRowPage{BoardID: "12", Lifetime: old, Rows: []MirrorRow{webRow("10003", 9, "c")}, ReadAt: mirrorReadAt(50)}
	writes := map[string]func() error{
		"structure": func() error { _, err := s.ReplaceMirrorStructure(root, late); return err },
		"row page":  func() error { _, err := s.WriteMirrorRowPage(root, page, []byte("late")); return err },
		"rows":      func() error { _, err := s.WriteMirrorRows(root, page); return err },
		"reconcile": func() error {
			_, err := s.ReconcileMirrorBoard(root, "12", old, []string{"10003"}, true, mirrorReadAt(50), 0)
			return err
		},
		"sync": func() error {
			_, err := s.RecordMirrorSyncAttempt(root, "12", MirrorSyncAttempt{Lifetime: old, ReadAt: mirrorReadAt(50)})
			return err
		},
	}
	refused := func(stage string) {
		t.Helper()
		for name, write := range writes {
			if err := write(); !errors.Is(err, ErrTrackerCacheUnlisted) {
				t.Errorf("%s %s: %v", stage, name, err)
			}
		}
		if _, err := s.ListMirrorSection(root, "12", column, after); !errors.Is(err, ErrTrackerCacheStale) {
			t.Errorf("%s continuation: %v", stage, err)
		}
	}

	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	refused("retired")
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11", "12")
	refused("listed again before its drain")
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	next := currentLifetime(s, root, "12")
	if next <= old {
		t.Fatalf("lifetime after the drain = %d; want one after %d", next, old)
	}
	refused("drained and listed again")
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	refused("synced afresh")
	assertRows(t, s.db, `SELECT name FROM tracker_cache_boards WHERE board_id = '12'`, "WEB board")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_issues WHERE issue_id = '10003'`, "0")
	if st, _, err := s.GetMirrorSync(root, "12", testNow); err != nil || st.Lifetime != next || st.Cursor != nil {
		t.Fatalf("sync of the new lifetime = %+v, %v", st, err)
	}

	// Unlisted and drained with no listing back, the board has no lifetime,
	// and a lifetime of zero is refused as invalid.
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_boards WHERE board_id = '12'`, "0")
	if st, _, err := s.GetMirrorSync(root, "12", testNow); err != nil || st.Lifetime != 0 {
		t.Fatalf("sync of an unlisted board = %+v, %v", st, err)
	}
	if _, err := s.ReconcileMirrorBoard(root, "12", 0, nil, true, mirrorReadAt(60), 0); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a write naming no lifetime: %v", err)
	}
}
