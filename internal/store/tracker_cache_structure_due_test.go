package store

import (
	"context"
	"fmt"
	"testing"
)

// structureDue is board 10's MirrorSync.StructureDueAt.
func structureDue(t *testing.T, s *Store, root int64) int64 {
	t.Helper()
	st, _, err := s.GetMirrorSync(root, "10", testNow)
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
	page := currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: rows, ReadAt: mirrorReadAt(readAt)})
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
	st.ReadAt, st.Complete, st.Sprints = mirrorReadAt(readAt), complete, append(st.Sprints, sprints...)
	if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
		t.Fatal(err)
	}
}

// A row page that names a sprint the mirror lacks makes the board due a
// structure read, durably and in the page's own transaction, by the
// statement that checks every card write's references (mirrorCardMarkSQL),
// so it costs what a page naming only known references does. A later delta, a
// failed structure read, a structure read that began before the page and
// one that is not complete and began in its millisecond (not known to
// follow it) leave it due; one that began after it clears it.
func TestMirrorRowNamingAnUnknownSprintMakesTheStructureDue(t *testing.T) {
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	if changes, statements := writeDuePage(t, s, root, 2, webRow("10001", 5, "a", "21")); changes.StructureDue || statements != 10 {
		t.Fatalf("a page naming known references = %+v in %d statements; want not due in 10", changes, statements)
	}
	if changes, statements := writeDuePage(t, s, root, 5, webRow("10002", 5, "b", "23")); !changes.StructureDue || statements != 10 {
		t.Fatalf("a page naming a new sprint = %+v in %d statements; want due in 10", changes, statements)
	}
	if changes, statements := writeDuePage(t, s, root, 6, webRow("10003", 5, "c", "21")); !changes.StructureDue || statements != 10 {
		t.Fatalf("a later page naming known references = %+v in %d statements; want still due in 10", changes, statements)
	}
	for name, attempt := range map[string]MirrorSyncAttempt{
		"a delta that succeeded":   {ReadAt: mirrorReadAt(7)},
		"a structure read failure": {Failure: MirrorAttemptFailure{At: mirrorReadAt(8), Kind: MirrorFailureTransient, Message: "timed out"}},
	} {
		if _, _, err := s.RecordMirrorSyncAttempt(root, "10", currentAttempt(s, root, "10", attempt)); err != nil {
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
	replaceDueStructure(t, s, root, 5, false)
	if due := structureDue(t, s, root); due != 5 {
		t.Fatalf("after an incomplete structure read in the page's millisecond: due %d; want 5", due)
	}
	if changes, _ := writeDuePage(t, s, root, 8, webRow("10003", 5, "c", "21")); !changes.StructureDue {
		t.Fatalf("a page after an incomplete structure read in the due page's millisecond = %+v; want still due", changes)
	}
	replaceDueStructure(t, s, root, 9, true, MirrorSprint{ID: "23", OriginBoardID: "10", Name: "WEB Sprint 4", State: MirrorSprintActive})
	if due := structureDue(t, s, root); due != 0 {
		t.Fatalf("after a structure read that supplies the sprint: due %d; want none", due)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained`, "0")
}

// A pending reference a structure read covered without explaining it (the
// read was not complete) makes the board due again, durably, when a later
// page names it: a status, a user and a sprint alike.
func TestMirrorCoveredPendingReferencesMakeTheBoardDueAgain(t *testing.T) {
	status, user, sprint := webRow("10001", 5, "a"), webRow("10001", 5, "a"), webRow("10001", 5, "a", "99")
	status.Card.StatusID, user.Card.AssigneeID = "77", "acc-gone"
	for name, row := range map[string]MirrorRow{"status": status, "user": user, "sprint": sprint} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		writeDuePage(t, s, root, 2, row)
		replaceDueStructure(t, s, root, 3, false)
		if due := structureDue(t, s, root); due != 0 {
			t.Fatalf("%s: after an incomplete structure read: due %d", name, due)
		}
		if changes, _ := writeDuePage(t, s, root, 4, row); !changes.StructureDue || structureDue(t, s, root) != 4 {
			t.Fatalf("%s: a page naming it again = %+v, due %d; want due at 4", name, changes, structureDue(t, s, root))
		}
	}
}

// mirrorMarksSQL lists board marks as kind:ref, with @named_at for a
// pending one, leaving out the sprints numbered 1000 to 1999.
const mirrorMarksSQL = `SELECT coalesce(group_concat(kind || ':' || ref_id || iif(named_at > 0, '@' || named_at, ''), ','), '')
	FROM (SELECT kind, ref_id, named_at FROM tracker_cache_board_unexplained
	       WHERE NOT (kind = 3 AND length(ref_id) = 4 AND ref_id BETWEEN '1000' AND '1999') ORDER BY kind, ref_id)`

// A row page marks pending each reference it names that the mirror lacks,
// and a complete structure read makes the pending marks no later than it
// unexplained; an unexplained reference no longer makes the board due,
// however many there are, and an unmarked one still does. A read that is
// not complete covers the pending marks without explaining them, so a
// later row naming one makes the board due again. A structure read that
// supplies a marked sprint removes its mark, so once a later read drops
// the sprint again a row naming it makes the board due; a mark no row
// names any more stays until the board's drain removes it.
func TestMirrorUnexplainedReferencesStopMakingTheStructureDue(t *testing.T) {
	const cardSprints = 50
	s, jira, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	odd := webRow("10001", 5, "a", "99")
	odd.Card.AssigneeID, odd.Card.StatusID = "acc-gone", "77"
	writeDuePage(t, s, root, 2, odd)
	assertRows(t, s.db, mirrorMarksSQL, "1:77@2,2:acc-gone@2,3:99@2")
	replaceDueStructure(t, s, root, 3, false)
	if due := structureDue(t, s, root); due != 0 {
		t.Fatalf("after a structure read: due %d", due)
	}
	assertRows(t, s.db, mirrorMarksSQL, "1:77@2,2:acc-gone@2,3:99@2")
	if changes, _ := writeDuePage(t, s, root, 4, odd); !changes.StructureDue {
		t.Fatalf("after an incomplete structure read the page = %+v; want due again", changes)
	}
	replaceDueStructure(t, s, root, 5, true)
	assertRows(t, s.db, mirrorMarksSQL, "1:77,2:acc-gone,3:99")
	// The page is the one read at 4 again: it moves its row's and its
	// card's read time, one statement each, and runs the copy to the links
	// of its cards, which no link stops at its guard.
	if changes, statements := writeDuePage(t, s, root, 6, odd); changes.StructureDue || statements != 8 {
		t.Fatalf("a page naming marked references = %+v in %d statements; want not due in 8", changes, statements)
	}
	var many []MirrorRow
	for r := range 6 {
		var sprints []string
		for i := range cardSprints {
			sprints = append(sprints, fmt.Sprint(1000+r*cardSprints+i))
		}
		many = append(many, webRow(fmt.Sprint(10100+r), 5, "m", sprints...))
	}
	writeDuePage(t, s, root, 7, many...)
	replaceDueStructure(t, s, root, 8, true)
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained WHERE kind = 3 AND named_at = 0`, "301")
	if changes, _ := writeDuePage(t, s, root, 9, webRow("10002", 5, "b", "24")); !changes.StructureDue {
		t.Fatalf("a new sprint after 301 marked ones = %+v; want due", changes)
	}
	var missing []string
	for _, row := range many {
		missing = append(missing, row.Card.IssueID)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: missing, ReadAt: mirrorReadAt(10)})); err != nil {
		t.Fatal(err)
	}
	kept := []MirrorSprint{{ID: "99", OriginBoardID: "10", Name: "WEB Sprint 0", State: MirrorSprintClosed},
		{ID: "24", OriginBoardID: "10", Name: "WEB Sprint 5", State: MirrorSprintFuture}}
	replaceDueStructure(t, s, root, 11, true, kept...)
	assertRows(t, s.db, mirrorMarksSQL, "1:77,2:acc-gone")
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained WHERE kind = 3`, "300")
	// A structure read that supplies a marked sprint removes its mark: once
	// a later read drops the sprint again, a row naming it makes the board
	// due. The read at 14 clears no due state.
	writeDuePage(t, s, root, 12, webRow("10003", 5, "c", "98"))
	replaceDueStructure(t, s, root, 13, true, kept...)
	assertRows(t, s.db, mirrorMarksSQL, "1:77,2:acc-gone,3:98")
	replaceDueStructure(t, s, root, 14, true, append(kept, MirrorSprint{ID: "98", OriginBoardID: "10", Name: "WEB Sprint 6", State: MirrorSprintFuture})...)
	assertRows(t, s.db, mirrorMarksSQL, "1:77,2:acc-gone")
	replaceDueStructure(t, s, root, 15, true, kept...)
	if changes, _ := writeDuePage(t, s, root, 16, webRow("10003", 5, "c", "98")); !changes.StructureDue {
		t.Fatalf("a sprint supplied and dropped again = %+v; want due", changes)
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "11", "12")
	if err := s.FinishMirrorCleanup(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained`, "0")
}

// The marks of a board's lacking references stay within a chunk per
// transaction and cost nothing in proportion to the marks a write does not
// change: 80 cards naming 50 unknown sprints each mark 4,000 references
// pending, which the next complete structure read makes unexplained in
// transactions of at most clearMirrorChunk. After that, a page naming one
// more unknown sprint and the complete structure read that follows it
// write one mark each and read the same pages whether the board holds
// 4,000 unexplained marks or 8,000 (both deep enough that the indexes have
// the same height).
func TestMirrorMarksStayWithinAChunk(t *testing.T) {
	const cardSprints = 50
	pages := map[int][2]int{}
	for _, cards := range []int{80, 160} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		for r := range cards {
			var sprints []string
			for i := range cardSprints {
				sprints = append(sprints, fmt.Sprint(5000+r*cardSprints+i))
			}
			writeDuePage(t, s, root, 2, webRow(fmt.Sprint(10100+r), 5, "m", sprints...))
		}
		recorder := recordMirrorWrites(t, s)
		replaceDueStructure(t, s, root, 3, true)
		most, transactions := recorder.mostMarksInATransaction()
		if want := (cards*cardSprints + clearMirrorChunk - 1) / clearMirrorChunk; most > clearMirrorChunk || transactions != want {
			t.Fatalf("%d marks made unexplained: at most %d in one of %d transactions; want at most %d in %d",
				cards*cardSprints, most, transactions, clearMirrorChunk, want)
		}
		assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_board_unexplained WHERE named_at = 0`, fmt.Sprint(cards*cardSprints))
		recorder.reset()
		page := storePageAccessesForTest(t, s, "page", func() error {
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{webRow("10002", 5, "b", "24")}, ReadAt: mirrorReadAt(4)}), nil)
			return err
		})
		structure := storePageAccessesForTest(t, s, "structure", func() error {
			st := webStructure()
			st.ReadAt, st.Complete = mirrorReadAt(5), true
			_, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st))
			return err
		})
		if writes := recorder.rowWrites()["tracker_cache_board_unexplained"]; writes != 2 {
			t.Fatalf("one more lacking sprint and its structure read wrote %d marks; want 2", writes)
		}
		pages[cards] = [2]int{page, structure}
	}
	if pages[80] != pages[160] {
		t.Fatalf("pages read by a page naming one more lacking sprint and its structure read, with 4,000 marks %v, with 8,000 %v; want the same",
			pages[80], pages[160])
	}
}
