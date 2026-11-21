package store

import (
	"fmt"
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
	results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, hs))
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

// A reconcile that fails after a transaction of it committed returns the
// rows those transactions removed with the error, and none of the failed
// one.
func TestReconcileMirrorBoardReportsWhatCommittedBeforeAFailure(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", mirrorRows(10001, clearMirrorChunk+1, "a"))
	// The removal walks the board's order: the last row is the second
	// transaction's.
	last := fmt.Sprint(10001 + clearMirrorChunk)
	failMirrorWrite(t, s, "DELETE", "tracker_cache_board_issues", "OLD.issue_id = '"+last+"'")
	got, err := s.ReconcileMirrorBoard(root, "10", currentLifetime(s, root, "10"), nil, true, 5, 5)
	if err == nil || len(got.Removed) != clearMirrorChunk || len(got.Missing) != 0 {
		t.Fatalf("removed %d, missing %d, %v; want the failure with the first transaction's %d removals", len(got.Removed), len(got.Missing), err, clearMirrorChunk)
	}
	var left []string
	if left, err = queryIDs(s.db, `SELECT issue_id FROM tracker_cache_board_issues WHERE root_id = ? AND board_id = '10'`, root); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != last {
		t.Fatalf("rows left %v, want only %s", left, last)
	}
	for _, id := range got.Removed {
		if id == last {
			t.Fatalf("the failed transaction's row %s is reported removed", last)
		}
	}
}
