package store

import (
	"fmt"
	"testing"
)

// mirrorEstimates is what reads show of issue 10001's estimate on board
// 10: its backlog row, its issue row and the backlog total.
func mirrorEstimates(t *testing.T, s *Store, root int64) string {
	t.Helper()
	show := func(e *float64) string {
		if e == nil {
			return "unknown"
		}
		return fmt.Sprint(*e)
	}
	page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("backlog = %+v, %v", page, err)
	}
	rows, _, err := s.ListMirrorIssueRows(root, "10001", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("issue rows = %+v, %v", rows, err)
	}
	counts, err := s.CountMirrorSections(root, "10", MirrorRegroup{})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("row %s issue row %s total %v", show(page.Rows[0].Estimate), show(rows[0].Estimate), counts.Backlog.Estimate)
}

// mirrorCursor is board 10's stored cursor, "none" for none.
func mirrorCursor(t *testing.T, s *Store, root int64) string {
	t.Helper()
	st, _, err := s.GetMirrorSync(root, "10", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if st.Cursor == nil {
		return "none"
	}
	return string(st.Cursor)
}

// A board's estimate definition changed by a structure read makes the
// rows stored before it unknown: their numbers are in the old unit. The
// structure's write clears the board's cursor and reports the reset, so
// the next walk reads every row again, and each row read again in the new
// unit shows and makes its issue history-due, since estimate changes are
// read per field. A page of a walk captured before the reset, or read no
// later than it, stores its rows and not its cursor. The transitions:
// points to time, time back to points, points of another field (a Jira
// board that switches story point fields), and a board that loses its
// estimate and gains one.
func TestMirrorEstimateChangeHidesRowsReadBefore(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	five, three := 5.0, 3.0
	row := webRow("10001", 5, "a")
	row.Estimate = &five
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(2)}, []byte("c1"))
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" {
		t.Fatalf("before = %s", got)
	}
	structure := func(unit int, field string, readAt int64) MirrorStructure {
		st := webStructure()
		st.Board.EstimateUnit, st.Board.EstimateField, st.ReadAt = unit, field, mirrorReadAt(readAt)
		return st
	}
	read := int64(10)
	for _, step := range []struct {
		name  string
		unit  int
		field string
	}{{"points to time", MirrorEstimateTime, "timeoriginalestimate"}, {"time back to points", MirrorEstimatePoints, ""},
		{"points of another field", MirrorEstimatePoints, "customfield_10026"},
		{"no estimate", MirrorEstimateNone, ""}, {"an estimate again", MirrorEstimatePoints, ""}} {
		read += 10
		captured := currentPage(s, root, MirrorRowPage{BoardID: "10"})
		got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, structure(step.unit, step.field, read)))
		if err != nil || got != (MirrorStructureWrite{MirrorWrite: MirrorWrite{Changed: true}, EstimateResetAt: read}) {
			t.Fatalf("%s: structure = %+v, %v; want changed and reset at %d", step.name, got, err, read)
		}
		if got := mirrorEstimates(t, s, root); got != "row unknown issue row unknown total 0" {
			t.Errorf("%s: rows read before = %s", step.name, got)
		}
		if c := mirrorCursor(t, s, root); c != "none" {
			t.Errorf("%s: cursor %s; want none", step.name, c)
		}
		if due := mirrorDue(t, s, root); due != "[]" {
			t.Errorf("%s: due before the row is read again = %s", step.name, due)
		}
		stale := row
		stale.Estimate = &three
		for name, page := range map[string]MirrorRowPage{
			// A walk captured before the reset, read before or after it.
			"a page of the old walk read before the structure": {Lifetime: captured.Lifetime, EstimateResetAt: captured.EstimateResetAt, ReadAt: mirrorReadAt(read - 1)},
			"a page of the old walk read after the structure":  {Lifetime: captured.Lifetime, EstimateResetAt: captured.EstimateResetAt, ReadAt: mirrorReadAt(read + 1)},
			// A walk captured after the reset, read in its millisecond.
			"a page read in the structure's millisecond": {ReadAt: mirrorReadAt(read)},
		} {
			page.BoardID, page.Rows = "10", []MirrorRow{stale}
			changes := mustWriteRowPage(t, s, root, page, []byte("c-stale"))
			if !changes.CursorRefused {
				t.Errorf("%s: %s = %+v; want its cursor refused", step.name, name, changes)
			}
			if c := mirrorCursor(t, s, root); c != "none" {
				t.Errorf("%s: %s left cursor %s; want none", step.name, name, c)
			}
		}
		// The old walk's later page stored its row, read after the reset.
		if got := mirrorEstimates(t, s, root); got != "row 3 issue row 3 total 3" && step.unit != MirrorEstimateNone ||
			got != "row 3 issue row 3 total 0" && step.unit == MirrorEstimateNone {
			t.Errorf("%s: the rows of the refused pages = %s", step.name, got)
		}
		read += 2
		changes := mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(read)}, []byte("c2"))
		if changes.CursorRefused || mirrorCursor(t, s, root) != "c2" {
			t.Errorf("%s: a page of the new walk = %+v, cursor %s; want c2 stored", step.name, changes, mirrorCursor(t, s, root))
		}
		if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" && step.unit != MirrorEstimateNone ||
			got != "row 5 issue row 5 total 0" && step.unit == MirrorEstimateNone {
			t.Errorf("%s: the row read again = %s", step.name, got)
		}
		// Its history is final through its unchanged update time: only the
		// boards revision makes it due, in both reads of the rule.
		if got := mirrorDue(t, s, root); got == "[]" {
			t.Fatalf("%s: nothing due after the row was read again", step.name)
		}
		due, _, err := s.ListMirrorHistoryDue(root, testNow)
		if err != nil || len(due) != 1 {
			t.Fatalf("%s: due after the row was read again = %+v, %v", step.name, due, err)
		}
		h := webHistory("10001")
		h.BoardsRev = due[0].BoardsRev
		// The same history, read for the new boards revision: nothing shown
		// changes.
		mustReplaceHistory(t, s, root, h, MirrorWrite{})
		read++
	}
	// The same definition read again resets nothing.
	read += 10
	reset := currentEstimateReset(s, root, "10")
	if got, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, structure(MirrorEstimatePoints, "", read))); err != nil ||
		got != (MirrorStructureWrite{EstimateResetAt: reset}) {
		t.Errorf("an unchanged estimate = %+v, %v; want unchanged, reset still %d", got, err, reset)
	}
	if c := mirrorCursor(t, s, root); c != "c2" {
		t.Errorf("an unchanged estimate: cursor %s; want c2 kept", c)
	}
	if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" {
		t.Errorf("an unchanged estimate = %s", got)
	}
}

// A walk's page that arrives after a reset is refused whether or not the
// board has a sync row: the reset's fence is on the board. Here rows were
// stored with no cursor (a reconcile's) and no tick recorded, so the
// board has no sync row when its estimate unit changes.
func TestMirrorEstimateResetRefusesACursorWithNoSyncRow(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	five := 5.0
	row := webRow("10001", 5, "a")
	row.Estimate = &five
	mustReplaceStructure(t, s, root, webStructure(), true)
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(2)})); err != nil {
		t.Fatal(err)
	}
	walk := currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(10)})
	assertRows(t, s.db, `SELECT count(*) FROM tracker_cache_sync`, "0")
	st := webStructure()
	st.Board.EstimateUnit, st.ReadAt = MirrorEstimateTime, mirrorReadAt(20)
	mustReplaceStructure(t, s, root, st, true)
	changes, err := s.WriteMirrorRowPage(root, walk, []byte("c-old-unit"))
	if err != nil || !changes.CursorRefused {
		t.Fatalf("the pending page = %+v, %v; want its cursor refused", changes, err)
	}
	if c := mirrorCursor(t, s, root); c != "none" {
		t.Fatalf("cursor %s after a reset; want none", c)
	}
}
