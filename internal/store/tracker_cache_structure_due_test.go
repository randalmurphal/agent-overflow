package store

import (
	"fmt"
	"testing"
)

// structureDue is board 10's MirrorSync.StructureDueAt.
func structureDue(t *testing.T, s *Store, root int64) int64 {
	t.Helper()
	st, _, err := s.GetMirrorSync(root, "10")
	if err != nil {
		t.Fatal(err)
	}
	return st.StructureDueAt
}

// writeDuePage writes a page of board 10's rows read at readAt and returns
// its changes and the statements it ran; its callers run alone, since the
// count is the package's.
func writeDuePage(t *testing.T, s *Store, root, readAt int64, rows ...MirrorRow) (MirrorChanges, int) {
	t.Helper()
	page := currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: rows, ReadAt: readAt})
	var changes MirrorChanges
	var err error
	statements := countMirrorStatements(t, func() { changes, err = s.WriteMirrorRowPage(root, page, []byte(fmt.Sprint("c", readAt))) })
	if err != nil {
		t.Fatal(err)
	}
	return changes, statements
}

func replaceDueStructure(t *testing.T, s *Store, root, readAt int64, complete bool, sprints ...MirrorSprint) {
	t.Helper()
	st := webStructure()
	st.ReadAt, st.Complete, st.Sprints = readAt, complete, append(st.Sprints, sprints...)
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
		t.Fatal(err)
	}
}

// A row page that names a sprint the mirror lacks makes the board due a
// structure read, durably and in the page's own transaction: one more
// statement than a page naming only known references. A later delta, a
// failed structure read and a structure read that began before the page
// leave it due; one that began after it clears it.
func TestMirrorRowNamingAnUnknownSprintMakesTheStructureDue(t *testing.T) {
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	if changes, statements := writeDuePage(t, s, root, 2, webRow("10001", 5, "a", "21")); changes.StructureDue || statements != 7 {
		t.Fatalf("a page naming known references = %+v in %d statements; want not due in 7", changes, statements)
	}
	if changes, statements := writeDuePage(t, s, root, 5, webRow("10002", 5, "b", "23")); !changes.StructureDue || statements != 8 {
		t.Fatalf("a page naming a new sprint = %+v in %d statements; want due in 8", changes, statements)
	}
	if changes, statements := writeDuePage(t, s, root, 6, webRow("10003", 5, "c", "21")); !changes.StructureDue || statements != 7 {
		t.Fatalf("a later page naming known references = %+v in %d statements; want still due in 7", changes, statements)
	}
	for name, attempt := range map[string]MirrorSyncAttempt{
		"a delta that succeeded":   {ReadAt: 7},
		"a structure read failure": {Failure: MirrorSyncFailure{At: 8, Kind: MirrorFailureTransient, Message: "timed out"}},
	} {
		if _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", attempt)); err != nil {
			t.Fatal(err)
		}
		if due := structureDue(t, s, root); due != 5 {
			t.Fatalf("after %s: structure due %d; want 5", name, due)
		}
	}
	replaceDueStructure(t, s, root, 4, true)
	if due := structureDue(t, s, root); due != 5 {
		t.Fatalf("after a structure read from before the page: due %d; want 5", due)
	}
	replaceDueStructure(t, s, root, 9, true, MirrorSprint{ID: "23", OriginBoardID: "10", Name: "WEB Sprint 4", State: MirrorSprintActive})
	if due := structureDue(t, s, root); due != 0 {
		t.Fatalf("after a structure read that supplies the sprint: due %d; want none", due)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained`, "0")
}

// A complete structure read marks, durably, the references the board's
// rows name that it did not supply; a marked reference no longer makes the
// board due, however many there are, and an unmarked one still does. A
// read that is not complete marks nothing. A structure read that supplies
// a marked reference removes its mark, as one does for a reference no row
// names any longer, and the board's drain removes them all.
func TestMirrorUnexplainedReferencesStopMakingTheStructureDue(t *testing.T) {
	s, jira, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	odd := webRow("10001", 5, "a", "99")
	odd.Card.AssigneeID, odd.Card.StatusID = "acc-gone", "77"
	writeDuePage(t, s, root, 2, odd)
	replaceDueStructure(t, s, root, 3, false)
	if due := structureDue(t, s, root); due != 0 {
		t.Fatalf("after a structure read: due %d", due)
	}
	if changes, _ := writeDuePage(t, s, root, 4, odd); !changes.StructureDue {
		t.Fatalf("after an incomplete structure read the page = %+v; want due again", changes)
	}
	replaceDueStructure(t, s, root, 5, true)
	marks := `SELECT group_concat(kind || ':' || ref_id, ',') FROM (SELECT kind, ref_id FROM tracker_cache_board_unexplained ORDER BY kind, ref_id)`
	assertRows(t, s.db, marks, "1:77,2:acc-gone,3:99")
	if changes, statements := writeDuePage(t, s, root, 6, odd); changes.StructureDue || statements != 7 {
		t.Fatalf("a page naming marked references = %+v in %d statements; want not due in 7", changes, statements)
	}
	var many []MirrorRow
	for r := range 6 {
		var sprints []string
		for i := range mirrorMaxCardSprints {
			sprints = append(sprints, fmt.Sprint(1000+r*mirrorMaxCardSprints+i))
		}
		many = append(many, webRow(fmt.Sprint(10100+r), 5, "m", sprints...))
	}
	writeDuePage(t, s, root, 7, many...)
	replaceDueStructure(t, s, root, 8, true)
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained WHERE kind = 3`, "301")
	if changes, _ := writeDuePage(t, s, root, 9, webRow("10002", 5, "b", "24")); !changes.StructureDue {
		t.Fatalf("a new sprint after 301 marked ones = %+v; want due", changes)
	}
	var missing []string
	for _, row := range many {
		missing = append(missing, row.Card.IssueID)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: missing, ReadAt: 10})); err != nil {
		t.Fatal(err)
	}
	kept := []MirrorSprint{{ID: "99", OriginBoardID: "10", Name: "WEB Sprint 0", State: MirrorSprintClosed},
		{ID: "24", OriginBoardID: "10", Name: "WEB Sprint 5", State: MirrorSprintFuture}}
	replaceDueStructure(t, s, root, 11, true, kept...)
	assertRows(t, s.db, marks, "1:77,2:acc-gone")
	// A structure read that supplies a marked sprint, here one that clears
	// no due state, removes its mark: once a later read drops the sprint
	// again, a row naming it makes the board due.
	writeDuePage(t, s, root, 12, webRow("10003", 5, "c", "98"))
	replaceDueStructure(t, s, root, 13, true, kept...)
	assertRows(t, s.db, marks, "1:77,2:acc-gone,3:98")
	replaceDueStructure(t, s, root, 14, true, append(kept, MirrorSprint{ID: "98", OriginBoardID: "10", Name: "WEB Sprint 6", State: MirrorSprintFuture})...)
	assertRows(t, s.db, marks, "1:77,2:acc-gone")
	replaceDueStructure(t, s, root, 15, true, kept...)
	if changes, _ := writeDuePage(t, s, root, 16, webRow("10003", 5, "c", "98")); !changes.StructureDue {
		t.Fatalf("a sprint supplied and dropped again = %+v; want due", changes)
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "11", "12")
	if err := s.FinishMirrorCleanup(nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained`, "0")
}
