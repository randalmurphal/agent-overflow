package store

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"testing"
)

// currentTotalsRevision is board's totals revision in root now.
func currentTotalsRevision(s *Store, root int64, board string) int64 {
	var rev int64
	if err := s.db.QueryRow(`SELECT totals_revision FROM tracker_cache_boards WHERE root_id = ? AND board_id = ? AND retired = 0`,
		root, board).Scan(&rev); err != nil {
		panic(err)
	}
	return rev
}

// pagedTotals reads every page of section of board and totals its rows as
// the counts do: a row with no estimate, and every row of a board with
// no estimate unit, adds no estimate.
func pagedTotals(t *testing.T, s *Store, root int64, board string, section MirrorSection, estimated bool) MirrorSectionTotal {
	t.Helper()
	var total MirrorSectionTotal
	for after := (MirrorRowKey{}); ; {
		page, err := s.ListMirrorSection(root, board, section, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Rows {
			total.Rows++
			if estimated && row.Estimate != nil {
				total.Estimate += *row.Estimate
			}
		}
		if !page.More {
			return total
		}
		after = page.Next
	}
}

// requireCountsMatchPages fails unless every total of board equals what
// the pages of its section show, sprint by sprint of the board, the
// backlog and column by column, a section with no rows absent, and All
// is every row of the board.
func requireCountsMatchPages(t *testing.T, s *Store, root int64, board, step string) {
	t.Helper()
	counts, err := s.CountMirrorSections(root, board)
	if err != nil {
		t.Fatal(err)
	}
	st, ok, err := s.GetMirrorBoard(root, board)
	if err != nil || !ok {
		t.Fatalf("board %s: %v, %v", board, ok, err)
	}
	estimated := st.Board.EstimateUnit != MirrorEstimateNone
	want := MirrorSectionCounts{Revision: counts.Revision, TotalsRevision: currentTotalsRevision(s, root, board),
		Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}
	want.Backlog = pagedTotals(t, s, root, board, MirrorSection{Kind: MirrorSectionBacklog}, estimated)
	for _, sp := range st.Sprints {
		if total := pagedTotals(t, s, root, board, MirrorSection{Kind: MirrorSectionSprint, SprintID: sp.ID}, estimated); total.Rows > 0 {
			want.Sprints[sp.ID] = total
		}
	}
	for position := range st.Columns {
		if total := pagedTotals(t, s, root, board, MirrorSection{Kind: MirrorSectionColumn, Column: position}, estimated); total.Rows > 0 {
			want.Columns[position] = total
		}
	}
	var estimates []float64
	err = scanMirrorRows(s.db, `SELECT `+mirrorRowEstimate("?3")+` FROM tracker_cache_board_issues b WHERE b.root_id = ?1 AND b.board_id = ?2`,
		[]any{root, board, currentEstimateReset(s, root, board)}, func(r rowScanner) error {
			var e *float64
			err := r.Scan(&e)
			want.All.Rows++
			if e != nil {
				estimates = append(estimates, *e)
			}
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	if estimated {
		for _, e := range estimates {
			want.All.Estimate += e
		}
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("%s: board %s counts %+v\npages show %+v", step, board, counts, want)
	}
}

// totalsRow is a row of status and sprints with estimate (negative: none),
// read at update time 5.
func totalsRow(id, order, status string, estimate float64, sprints ...string) MirrorRow {
	return sectionRow(id, order, status, estimate, sprints...)
}

// Every total of a board is the rows its section's pages return, over
// every branch of the section rules: active, future and closed sprints, a
// row in two sprints, a sprint of another board and one the mirror does
// not keep, done, canceled and unknown statuses and a status in no
// column, a board with no estimate unit, a board with no sprints, an
// estimate reset and the rows read again after it, a status moved to done
// and a sprint closed.
func TestMirrorCountsMatchThePages(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID, other.Board.EstimateUnit = "11", MirrorEstimateNone
	other.Sprints = append(other.Sprints, MirrorSprint{ID: "31", OriginBoardID: "11", Name: "APP Sprint 1", State: MirrorSprintActive})
	mustReplaceStructure(t, s, root, other, true)
	kanban := webStructure()
	kanban.Board.ID, kanban.Board.PlansSprints, kanban.Sprints = "12", false, nil
	mustReplaceStructure(t, s, root, kanban, true)
	rows := []MirrorRow{
		totalsRow("10001", "a", "1", 1, "21"),
		totalsRow("10002", "b", "3", 2, "22"),
		totalsRow("10003", "c", "1", -1, "20"),
		totalsRow("10004", "d", "3", 3, "31"),
		totalsRow("10005", "b", "3", 5, "21", "20"),
		totalsRow("10006", "e", "10001", 4, "22", "20"),
		totalsRow("10007", "f", "10001", -1),
		totalsRow("10008", "g", "6", 6, "21"),
		totalsRow("10009", "h", "99", 9),
		totalsRow("10010", "i", "7", 0.5, "98"),
		totalsRow("10011", "j", "1", 1.5),
	}
	for _, board := range []string{"10", "11", "12"} {
		mustWriteRowPage(t, s, root, webPage(board, rows...), nil)
	}
	check := func(step string) {
		t.Helper()
		for _, board := range []string{"10", "11", "12"} {
			requireCountsMatchPages(t, s, root, board, step)
		}
	}
	check("as written")
	if counts, err := s.CountMirrorSections(root, "10"); err != nil || counts.Backlog.Rows != 5 || len(counts.Sprints) != 3 {
		t.Fatalf("board 10's counts %+v, %v; want 5 rows in the backlog and 3 sprints", counts, err)
	}

	read := int64(10)
	structure := func(st MirrorStructure) {
		t.Helper()
		read++
		st.ReadAt = mirrorReadAt(read)
		mustReplaceStructure(t, s, root, st, true)
	}
	reset := webStructure()
	reset.Board.EstimateUnit = MirrorEstimateTime
	structure(reset)
	check("the estimate unit changed")
	read++
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows[:4], ReadAt: mirrorReadAt(read)}, nil)
	check("some rows read again in the new unit")

	moved := reset
	moved.Catalog.Statuses = slices.Clone(moved.Catalog.Statuses)
	moved.Catalog.Statuses[1].Group = MirrorGroupDone
	moved.Sprints = slices.Clone(moved.Sprints)
	moved.Sprints[1].State = MirrorSprintClosed
	structure(moved)
	check("a status moved to done and a sprint closed")
}

// The totals revision moves with every write that can change a total and
// with none that cannot: a title, a rank and a row read again unchanged
// move it not; an estimate, a status, a sprint joined, a row added and
// one removed, an estimate unit, a sprint's state, a column's statuses,
// a status's group and a row read again after an estimate reset do. The
// counts change only where it moved.
func TestMirrorTotalsRevisionMovesWithTheTotals(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	rows := []MirrorRow{totalsRow("10001", "a", "1", 1, "21"), totalsRow("10002", "b", "3", 2), totalsRow("10003", "c", "1", 3, "22")}
	read := int64(10)
	page := func(rows ...MirrorRow) func() {
		return func() {
			read++
			mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows, ReadAt: mirrorReadAt(read)}, nil)
		}
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows, ReadAt: mirrorReadAt(read)}, nil)
	// st is the board's structure as each structure step leaves it.
	st := webStructure()
	structure := func(change func()) func() {
		return func() {
			read++
			change()
			st.ReadAt = mirrorReadAt(read)
			if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
				t.Fatal(err)
			}
		}
	}
	edit := func(i int, change func(*MirrorRow)) func() {
		return func() {
			rows[i].Card.UpdatedAt++
			change(&rows[i])
			page(rows[i])()
		}
	}
	seven := 7.0
	for _, step := range []struct {
		name  string
		write func()
		moves bool
	}{
		{"a title", edit(0, func(r *MirrorRow) { r.Card.Data = []byte(`{"title":"Renamed"}`) }), false},
		{"a rank", edit(1, func(r *MirrorRow) { r.OrderKey = "z" }), false},
		{"a row read again unchanged", func() { page(rows...)() }, false},
		{"an estimate", edit(1, func(r *MirrorRow) { r.Estimate = &seven }), true},
		{"a status", edit(0, func(r *MirrorRow) { r.Card.StatusID = "3" }), true},
		{"a sprint joined", edit(1, func(r *MirrorRow) { r.Card.Sprints = []string{"21"} }), true},
		{"a row added", page(totalsRow("10004", "d", "1", 4)), true},
		{"a row removed", func() {
			read++
			if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10004"}, ReadAt: mirrorReadAt(read)})); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"a sprint closed", structure(func() { st.Sprints[1].State = MirrorSprintClosed }), true},
		{"a column's statuses", structure(func() { st.Columns[0].Statuses, st.Columns[1].Statuses = []string{"1", "3"}, nil }), true},
		{"a status's group", structure(func() { st.Catalog.Statuses[0].Group = MirrorGroupDone }), true},
		{"an estimate unit", structure(func() { st.Board.EstimateUnit = MirrorEstimateTime }), true},
		{"a row read again after the estimate reset", func() { page(rows[2])() }, true},
	} {
		before, err := s.CountMirrorSections(root, "10")
		if err != nil {
			t.Fatal(err)
		}
		step.write()
		after, err := s.CountMirrorSections(root, "10")
		if err != nil {
			t.Fatal(err)
		}
		moved := after.TotalsRevision != before.TotalsRevision
		same := before.All == after.All && before.Backlog == after.Backlog && maps.Equal(before.Sprints, after.Sprints) &&
			maps.Equal(before.Columns, after.Columns)
		if moved != step.moves || same == step.moves {
			t.Errorf("%s: totals revision %d -> %d, totals the same %v; want it to move %v\nbefore %+v\nafter %+v",
				step.name, before.TotalsRevision, after.TotalsRevision, same, step.moves, before, after)
		}
		requireCountsMatchPages(t, s, root, "10", fmt.Sprint("after ", step.name))
	}
}
