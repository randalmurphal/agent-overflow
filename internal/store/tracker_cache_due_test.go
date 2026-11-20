package store

import (
	"fmt"
	"testing"
)

// mirrorDue is what ListMirrorHistoryDue returns, as issue:boards.
func mirrorDue(t *testing.T, s *Store, root int64) string {
	t.Helper()
	due, _, err := s.ListMirrorHistoryDue(root, "")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprint(due)
}

// An issue whose history is final through its update time is due again
// when it gains a row on another shown board: estimate changes are per
// board, and the stored history was read for the boards it had.
func TestMirrorIssueGainingABoardIsHistoryDue(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the history = %s; want none", due)
	}
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after the issue gained board 11 = %s", due)
	}
	// A history read for board 10 alone, begun before the issue gained
	// board 11, lands after: superseded if the stored one covers more,
	// and the issue stays due either way.
	early := webHistory("10001")
	early.BoardsRev = 1
	if results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}); err != nil || results[0].Changed {
		t.Fatalf("a history read for the earlier boards = %+v, %v", results, err)
	}
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after a history read for the earlier boards = %s", due)
	}
	full := webHistory("10001")
	full.BoardsRev = 2
	mustReplaceHistory(t, s, root, full, MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the history for both boards = %s", due)
	}
	if results, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), []MirrorHistory{early}); err != nil || !results[0].Superseded {
		t.Fatalf("a history read for fewer boards than the stored one = %+v, %v; want superseded", results, err)
	}
	// Losing a board does not make it due.
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "11", Missing: []string{"10001"}, ReadAt: 5})); err != nil {
		t.Fatal(err)
	}
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the issue lost board 11 = %s", due)
	}
}

// A staged (long) history records the boards revision it was read for as
// the batch does, and an identical one read for a later revision records
// it with no change written.
func TestMirrorLongHistoryRecordsItsBoards(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	long := statusHistory("10001", clearMirrorChunk+100)
	mustReplaceHistory(t, s, root, long, MirrorWrite{Changed: true})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	if due := mirrorDue(t, s, root); due != "[{10001 [10 11] 2}]" {
		t.Fatalf("due after the issue gained board 11 = %s", due)
	}
	long.BoardsRev = 2
	mustReplaceHistory(t, s, root, long, MirrorWrite{Changed: true})
	if due := mirrorDue(t, s, root); due != "[]" {
		t.Fatalf("due after the same long history read for both boards = %s", due)
	}
}
