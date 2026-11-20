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
	page, err := s.ListMirrorSection(root, "10", MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("backlog = %+v, %v", page, err)
	}
	rows, _, err := s.ListMirrorIssueRows(root, "10001", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("issue rows = %+v, %v", rows, err)
	}
	counts, err := s.CountMirrorSections(root, "10")
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("row %s issue row %s total %v", show(page.Rows[0].Estimate), show(rows[0].Estimate), counts.Backlog.Estimate)
}

// A board's estimate definition changed by a structure read makes the
// rows stored before it unknown: their numbers are in the old unit. The
// structure's write clears the board's cursor, so the next walk reads
// every row again, and each row read again in the new unit shows and makes
// its issue history-due, since estimate changes are read per field. The
// transitions: points to time, time back to points, and a board that loses
// its estimate and gains one.
func TestMirrorEstimateChangeHidesRowsReadBefore(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	five, three := 5.0, 3.0
	row := webRow("10001", 5, "a")
	row.Estimate = &five
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: 2}, []byte("c1"))
	mustReplaceHistory(t, s, root, webHistory("10001"), MirrorWrite{Changed: true})
	if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" {
		t.Fatalf("before = %s", got)
	}
	structure := func(unit int, readAt int64) MirrorStructure {
		st := webStructure()
		st.Board.EstimateUnit, st.ReadAt = unit, readAt
		return st
	}
	read := int64(10)
	for _, step := range []struct {
		name string
		unit int
	}{{"points to time", MirrorEstimateTime}, {"time back to points", MirrorEstimatePoints},
		{"no estimate", MirrorEstimateNone}, {"an estimate again", MirrorEstimatePoints}} {
		read += 10
		mustReplaceStructure(t, s, root, structure(step.unit, read), true)
		if got := mirrorEstimates(t, s, root); got != "row unknown issue row unknown total 0" {
			t.Errorf("%s: rows read before = %s", step.name, got)
		}
		if st, _, err := s.GetMirrorSync(root, "10"); err != nil || st.Cursor != nil {
			t.Errorf("%s: sync = %+v, %v; want no cursor", step.name, st, err)
		}
		if due := mirrorDue(t, s, root); due != "[]" {
			t.Errorf("%s: due before the row is read again = %s", step.name, due)
		}
		// A page read no later than the structure (here in the same
		// millisecond) lands after it: still unknown, and its cursor is
		// refused.
		stale := row
		stale.Estimate = &three
		mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{stale}, ReadAt: read}, []byte("c-stale"))
		if got := mirrorEstimates(t, s, root); got != "row unknown issue row unknown total 0" {
			t.Errorf("%s: a row read before the structure = %s", step.name, got)
		}
		if st, _, err := s.GetMirrorSync(root, "10"); err != nil || st.Cursor != nil {
			t.Errorf("%s: a cursor read before the structure = %+v, %v; want none", step.name, st, err)
		}
		mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: read + 1}, []byte("c2"))
		if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" && step.unit != MirrorEstimateNone ||
			got != "row 5 issue row 5 total 0" && step.unit == MirrorEstimateNone {
			t.Errorf("%s: the row read again = %s", step.name, got)
		}
		due, _, err := s.ListMirrorHistoryDue(root, "")
		if err != nil || len(due) != 1 {
			t.Fatalf("%s: due after the row was read again = %+v, %v", step.name, due, err)
		}
		h := webHistory("10001")
		h.BoardsRev = due[0].BoardsRev
		mustReplaceHistory(t, s, root, h, MirrorWrite{Changed: true})
		read++
	}
	// The same definition read again resets nothing.
	read += 10
	mustReplaceStructure(t, s, root, structure(MirrorEstimatePoints, read), false)
	if st, _, err := s.GetMirrorSync(root, "10"); err != nil || string(st.Cursor) != "c2" {
		t.Errorf("an unchanged estimate: sync = %+v, %v; want the cursor kept", st, err)
	}
	if got := mirrorEstimates(t, s, root); got != "row 5 issue row 5 total 5" {
		t.Errorf("an unchanged estimate = %s", got)
	}
}
