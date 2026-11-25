package store

import (
	"fmt"
	"reflect"
	"testing"
)

// failMirrorWrite makes every write the trigger body names fail, as a
// store failure between the transactions of one call does.
func failMirrorWrite(t *testing.T, s *Store, event, table, when string) {
	t.Helper()
	mustExec(t, s.db, fmt.Sprintf(`CREATE TRIGGER test_fail_write BEFORE %s ON %s WHEN %s
		BEGIN SELECT RAISE(ABORT, 'injected'); END`, event, table, when))
}

// A history write that fails after a transaction of it committed returns
// the committed outcomes with the error: the histories it changed are
// stored, and a caller that drops them never tells their change.
func TestReplaceMirrorHistoriesReportsWhatCommittedBeforeAFailure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, 5, "a"))
	// Four histories fill one transaction's change budget; the fifth is
	// written by the next, which fails.
	per := mirrorMaxBatchChanges/4 - 50
	var hs []MirrorHistory
	for i := range 5 {
		hs = append(hs, longHistory(fmt.Sprint(10001+i), per, 0))
	}
	failMirrorWrite(t, s, "INSERT", "tracker_cache_changes", "NEW.issue_id = '10005'")
	results, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, hs), MirrorCatalog{}, nextMirrorRead(s, root))
	if err == nil || len(results) != 5 {
		t.Fatalf("results %+v, %v; want the failure with the five outcomes", results, err)
	}
	for i, res := range results {
		if want := i < 4; res.Changed != want {
			t.Errorf("issue %s: changed %v, want %v", res.IssueID, res.Changed, want)
		}
		var stored int
		if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_changes WHERE root_id = ? AND issue_id = ?`, root, res.IssueID).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: per, false: 0}[i < 4]; stored != want {
			t.Errorf("issue %s: %d changes stored, want %d", res.IssueID, stored, want)
		}
	}
}

// A structure write whose mark settlement fails after the structure
// committed returns what the structure changed with the error, and the
// next structure read settles the marks, identical or not.
func TestReplaceMirrorStructureReportsWhatCommittedBeforeAFailure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	odd := webRow("10001", 5, "a")
	odd.Card.StatusID = "77"
	page := webPage("10", odd)
	page.ReadAt = mirrorReadAt(2)
	mustWriteRowPage(t, s, root, page, nil)
	failMirrorWrite(t, s, "UPDATE", "tracker_cache_board_unexplained", "true")
	st := webStructure()
	st.Board.EstimateUnit, st.ReadAt, st.Complete = MirrorEstimateTime, mirrorReadAt(30), true
	got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st))
	if err == nil || !reflect.DeepEqual(got, MirrorStructureWrite{MirrorWrite: MirrorWrite{Changed: true}, EstimateResetAt: 30}) {
		t.Fatalf("a structure whose marks failed = %+v, %v; want the failure with the change and the reset", got, err)
	}
	assertRows(t, s.db, `SELECT estimate_unit || ':' || estimate_reset_at FROM tracker_cache_boards WHERE board_id = '10'`, "2:30")
	assertRows(t, s.db, `SELECT named_at FROM tracker_cache_board_unexplained`, "2")
	mustExec(t, s.db, `DROP TRIGGER test_fail_write`)
	st.ReadAt = mirrorReadAt(40)
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil || !reflect.DeepEqual(got, MirrorStructureWrite{EstimateResetAt: 30}) {
		t.Fatalf("the same structure again = %+v, %v", got, err)
	}
	assertRows(t, s.db, `SELECT named_at FROM tracker_cache_board_unexplained`, "0")
}

// A mark a failed settlement left pending was named before the stored
// structure's read, which looked for it, so it is owed no structure read:
// the board is not due. A later page that names it again re-times it and
// makes the board due once; the complete structure read that follows
// explains it, and a page naming it after that does not.
func TestAMarkAFailedSettlementLeftIsOwedNoRead(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	odd := webRow("10001", 5, "a")
	odd.Card.StatusID = "77"
	page := func(readAt int64) MirrorChanges {
		t.Helper()
		got, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{odd}, ReadAt: mirrorReadAt(readAt)}), nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	complete := func(readAt int64) error {
		st := webStructure()
		st.ReadAt, st.Complete = mirrorReadAt(readAt), true
		_, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st))
		return err
	}
	if !page(2).StructureDue || structureDue(t, s, root) != 2 {
		t.Fatal("a page naming an unknown status leaves the board not due")
	}
	failMirrorWrite(t, s, "UPDATE", "tracker_cache_board_unexplained", "true")
	if err := complete(30); err == nil {
		t.Fatal("the settlement's failure is not returned")
	}
	mustExec(t, s.db, `DROP TRIGGER test_fail_write`)
	assertRows(t, s.db, `SELECT named_at FROM tracker_cache_board_unexplained`, "2")
	if due := structureDue(t, s, root); due != 0 {
		t.Fatalf("after the failed settlement: due %d; want none, the structure read at 30 having looked", due)
	}
	if !page(40).StructureDue || structureDue(t, s, root) != 40 {
		t.Fatalf("a page naming the reference again: due %d; want 40", structureDue(t, s, root))
	}
	if err := complete(50); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT named_at FROM tracker_cache_board_unexplained`, "0")
	if page(60).StructureDue || structureDue(t, s, root) != 0 {
		t.Fatalf("a page naming the explained reference: due %d; want none", structureDue(t, s, root))
	}
}
