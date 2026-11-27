package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// supportIDs are the issues of rows a read names support, in order.
func supportIDs(rows []MirrorRow) string {
	var ids []string
	for _, r := range rows {
		if r.Support {
			ids = append(ids, r.Card.IssueID)
		}
	}
	return strings.Join(ids, ",")
}

// A support rule names a row support when the row has no estimate and the
// rule lists its type, one of its labels or its board; a row with an
// estimate never is, a row whose estimate an estimate reset made unknown
// is, and a board with no estimate has no estimates, so every row the
// rule lists is. The totals count a support row as a row with no estimate
// that is not without one, in the backlog, a sprint, a column and the
// board, also under a regrouping; with no rule nothing changes.
func TestMirrorSupportRows(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	unestimated := webStructure()
	unestimated.Board.ID, unestimated.Board.EstimateUnit = "11", MirrorEstimateNone
	unestimated.Sprints = nil
	mustReplaceStructure(t, s, root, unestimated, true)
	listed := webStructure()
	listed.Board.ID, listed.Sprints = "12", nil
	mustReplaceStructure(t, s, root, listed, true)
	row := func(id, order, typeID string, estimate float64, labels []string, sprints ...string) MirrorRow {
		r := sectionRow(id, order, "1", estimate, sprints...)
		r.Card.TypeID, r.Card.Labels = typeID, labels
		return r
	}
	rows := []MirrorRow{
		row("10001", "a", "10004", -1, nil),                                   // the type, no estimate
		row("10002", "b", "10004", 3, nil),                                    // the type, an estimate
		row("10003", "c", "10001", -1, []string{"frontend", "support"}, "21"), // a label, no estimate
		row("10004", "d", "10001", -1, []string{"frontend"}, "21"),            // another label
		row("10005", "e", "10001", -1, nil),                                   // nothing the rule lists
	}
	mustWriteRowPage(t, s, root, webPage("10", rows...), nil)
	mustWriteRowPage(t, s, root, webPage("12", rows...), nil)
	none := slices.Clone(rows)
	for i := range none {
		none[i].Estimate = nil
	}
	mustWriteRowPage(t, s, root, webPage("11", none...), nil)

	rule, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Types: []string{"10004"}, Labels: []string{"support"}, Boards: []string{"12"}})
	if err != nil {
		t.Fatal(err)
	}
	regrouped, err := NewMirrorRegroup(map[string]int{"6": MirrorGroupDone})
	if err != nil {
		t.Fatal(err)
	}
	regroupedRule, err := regrouped.WithSupport(MirrorSupportRule{Types: []string{"10004"}, Labels: []string{"support"}, Boards: []string{"12"}})
	if err != nil {
		t.Fatal(err)
	}
	rowsOf := func(board string, regroup MirrorRegroup, section MirrorSection) []MirrorRow {
		t.Helper()
		page, err := s.ListMirrorSection(root, board, regroup, section, MirrorRowKey{})
		if err != nil {
			t.Fatal(err)
		}
		return page.Rows
	}
	backlog, sprint, column := MirrorSection{Kind: MirrorSectionBacklog}, MirrorSection{Kind: MirrorSectionSprint, SprintID: "21"},
		MirrorSection{Kind: MirrorSectionColumn}
	for _, tc := range []struct {
		name, board string
		regroup     MirrorRegroup
		section     MirrorSection
		want        string
	}{
		{"no rule", "10", MirrorRegroup{}, column, ""},
		{"backlog", "10", rule, backlog, "10001"},
		{"backlog, regrouped", "10", regroupedRule, backlog, "10001"},
		{"sprint", "10", rule, sprint, "10003"},
		{"column", "10", rule, column, "10001,10003"},
		{"a listed board", "12", rule, column, "10001,10003,10004,10005"},
		{"a board with no estimate", "11", rule, column, "10001,10002,10003"},
	} {
		if got := supportIDs(rowsOf(tc.board, tc.regroup, tc.section)); got != tc.want {
			t.Errorf("%s: support rows %q, want %q", tc.name, got, tc.want)
		}
	}
	patch, ok, err := s.ReadMirrorBoardPatch(root, "10", rule, []MirrorSection{column}, []string{"10001", "10002", "10003"}, MirrorTotalsAt{})
	if err != nil || !ok || supportIDs(patch.Sections[0]) != "10001,10003" {
		t.Errorf("patch support rows %q, %v, %v; want 10001,10003", supportIDs(patch.Sections[0]), ok, err)
	}

	counts := func(board string, regroup MirrorRegroup) string {
		t.Helper()
		c, err := s.CountMirrorSections(root, board, regroup)
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("all %d, backlog %d, sprint %d, column %d", c.All.Unestimated, c.Backlog.Unestimated, c.Sprints["21"].Unestimated,
			c.Columns[0].Unestimated)
	}
	for _, tc := range []struct {
		name, board string
		regroup     MirrorRegroup
		want        string
	}{
		{"no rule", "10", MirrorRegroup{}, "all 4, backlog 2, sprint 2, column 4"},
		{"the rule", "10", rule, "all 2, backlog 1, sprint 1, column 2"},
		{"the rule, regrouped", "10", regroupedRule, "all 2, backlog 1, sprint 1, column 2"},
		{"a listed board", "12", rule, "all 0, backlog 0, sprint 0, column 0"},
		{"a board with no estimate", "11", rule, "all 0, backlog 0, sprint 0, column 0"},
	} {
		if got := counts(tc.board, tc.regroup); got != tc.want {
			t.Errorf("%s: without an estimate %s, want %s", tc.name, got, tc.want)
		}
	}

	// An estimate reset leaves every row's estimate unknown until the row is
	// read again: the row of the type with an estimate is support then.
	reset := webStructure()
	reset.Board.EstimateUnit = MirrorEstimateTime
	reset.ReadAt = mirrorReadAt(10)
	mustReplaceStructure(t, s, root, reset, true)
	if got := supportIDs(rowsOf("10", rule, column)); got != "10001,10002,10003" {
		t.Errorf("after an estimate reset: support rows %q, want 10001,10002,10003", got)
	}
	if got := counts("10", rule); got != "all 2, backlog 1, sprint 1, column 2" {
		t.Errorf("after an estimate reset: without an estimate %s, want all 2, backlog 1, sprint 1, column 2", got)
	}
}
