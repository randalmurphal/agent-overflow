package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
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
	for _, tc := range []struct {
		name, board string
		regroup     MirrorRegroup
		unit        int
		want        string
	}{
		{"no rule", "10", MirrorRegroup{}, MirrorEstimatePoints, ""},
		{"the rule", "10", rule, MirrorEstimatePoints, "10001,10003"},
		{"a board with no estimate", "11", rule, MirrorEstimateNone, "10001,10002,10003"},
	} {
		of, ok, err := s.ListMirrorSupport(root, tc.board, tc.regroup, []string{"10001", "10002", "10003", "10004", "99999"})
		slices.Sort(of.Support)
		if got := strings.Join(of.Support, ","); err != nil || !ok || got != tc.want || of.Unit != tc.unit {
			t.Errorf("%s: support of the issues %q in unit %d, %v, %v; want %q in %d", tc.name, got, of.Unit, ok, err, tc.want, tc.unit)
		}
	}
	if _, ok, err := s.ListMirrorSupport(root, "13", rule, []string{"10001"}); ok || err != nil {
		t.Errorf("a board the mirror does not show: %v, %v; want none", ok, err)
	}
	patch, ok, err := s.ReadMirrorBoardPatch(root, "10", rule, []MirrorSection{column}, []string{"10001", "10002", "10003"}, MirrorTotalsAt{})
	if err != nil || !ok || supportIDs(patch.Sections[0]) != "10001,10003" {
		t.Errorf("patch support rows %q, %v, %v; want 10001,10003", supportIDs(patch.Sections[0]), ok, err)
	}

	// A card read again with a label the rule lists is support from then.
	relabeled := rows[4]
	relabeled.Card.UpdatedAt, relabeled.Card.Labels = 6, []string{"support"}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{relabeled}, ReadAt: mirrorReadAt(2)}, nil)
	if got := supportIDs(rowsOf("10", rule, column)); got != "10001,10003,10005" {
		t.Errorf("after a card's labels changed: support rows %q, want 10001,10003,10005", got)
	}
	relabeled.Card.UpdatedAt, relabeled.Card.Labels = 7, nil
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{relabeled}, ReadAt: mirrorReadAt(3)}, nil)

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

// A client that holds a board's totals is given them again by its next
// patch when a card's type or labels change what is support, though its
// estimate, status and sprints did not change: the totals revision moves
// with every input of the totals.
func TestASupportInputChangeMovesTheTotals(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	plain := sectionRow("10001", "a", "1", -1)
	mustWriteRowPage(t, s, root, webPage("10", plain), nil)
	rule, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Types: []string{"10004"}, Labels: []string{"support"}})
	if err != nil {
		t.Fatal(err)
	}
	backlog := []MirrorSection{{Kind: MirrorSectionBacklog}}
	held := func() MirrorTotalsAt {
		t.Helper()
		patch, ok, err := s.ReadMirrorBoardPatch(root, "10", rule, backlog, []string{"10001"}, MirrorTotalsAt{})
		if err != nil || !ok || patch.Counts == nil {
			t.Fatalf("the first patch: %+v, %v, %v", patch, ok, err)
		}
		return MirrorTotalsAt{Lifetime: patch.Lifetime, Revision: patch.Counts.TotalsRevision}
	}
	for i, change := range []struct {
		name        string
		edit        func(r *MirrorRow)
		unestimated int
	}{
		{"labeled support", func(r *MirrorRow) { r.Card.Labels = []string{"support"} }, 0},
		{"the label removed", func(r *MirrorRow) { r.Card.Labels = nil }, 1},
		{"typed support", func(r *MirrorRow) { r.Card.TypeID = "10004" }, 0},
	} {
		at := held()
		row := plain
		row.Card.UpdatedAt = int64(10 + i)
		change.edit(&row)
		plain = row
		mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{row}, ReadAt: mirrorReadAt(int64(2 + i))}, nil)
		patch, ok, err := s.ReadMirrorBoardPatch(root, "10", rule, backlog, []string{"10001"}, at)
		if err != nil || !ok {
			t.Fatalf("%s: %v, %v", change.name, ok, err)
		}
		if patch.Counts == nil {
			t.Fatalf("%s: the patch for a client holding the totals at revision %d carries none", change.name, at.Revision)
		}
		if got := patch.Counts.Backlog.Unestimated; got != change.unestimated {
			t.Fatalf("%s: %d without an estimate, want %d", change.name, got, change.unestimated)
		}
	}
}

// Every column of an issue that the section totals read is one whose
// change advances the totals revision of the issue's boards, through a
// trigger: a patch for a client holding the totals reads none, so a total
// input the triggers do not name would leave the client's totals stale.
// The keys never change.
func TestEveryIssueColumnTheTotalsReadMovesThem(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	read := map[string]bool{}
	for _, statement := range []string{mirrorCountRowsSQL, mirrorRegroupedCountRowsSQL, mirrorCountSprintsSQL, mirrorCountSprintsLeftOutSQL} {
		for _, m := range regexp.MustCompile(`\bi\.([a-z_]+)`).FindAllStringSubmatch(statement, -1) {
			read[m[1]] = true
		}
	}
	delete(read, "root_id")
	delete(read, "issue_id")
	if !read["status_id"] || !read["type_id"] || !read["labels"] {
		t.Fatalf("the totals read the issue columns %v; the scan lost status, type or labels", read)
	}
	rows, err := s.db.Query(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'tracker_cache_issues'`)
	if err != nil {
		t.Fatal(err)
	}
	moved := map[string]bool{}
	of := regexp.MustCompile(`(?i)AFTER UPDATE OF ([a-z_, ]+) ON tracker_cache_issues`)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		m := of.FindStringSubmatch(text)
		if m == nil || !strings.Contains(text, "totals_revision = totals_revision + 1") {
			continue
		}
		for _, column := range strings.Split(m[1], ",") {
			moved[strings.TrimSpace(column)] = true
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for column := range read {
		if !moved[column] {
			t.Errorf("the totals read tracker_cache_issues.%s, which no trigger that advances the totals revision names", column)
		}
	}
}

// The board pass of the section totals hands the sprint pass the support
// rows that are members of one of the counted board's sprints and no
// other: a support row outside every sprint, or only in another board's,
// is not part of that board's sprint pass.
func TestTheSprintPassIsGivenOnlySupportSprintMembers(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	// Board 12 shares board 10's tickets and has no sprint of its own.
	shared := webStructure()
	shared.Board.ID, shared.Sprints = "12", nil
	mustReplaceStructure(t, s, root, shared, true)
	typed := sectionRow("10001", "a", "1", -1)
	typed.Card.TypeID = "10004"
	labeled := sectionRow("10002", "b", "1", -1, "21")
	labeled.Card.Labels = []string{"support"}
	rows := []MirrorRow{typed, labeled, sectionRow("10003", "c", "1", -1, "21")}
	mustWriteRowPage(t, s, root, webPage("10", rows...), nil)
	mustWriteRowPage(t, s, root, webPage("12", rows...), nil)
	rule, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Types: []string{"10004"}, Labels: []string{"support"}})
	if err != nil {
		t.Fatal(err)
	}
	handed := func(board string) []string {
		t.Helper()
		var got []string
		err := scanMirrorRows(s.db, mirrorCountRowsSQL, []any{root, board, int64(0), nil, rule.supportArg()}, func(r rowScanner) error {
			var backlog bool
			var status sql.RawBytes
			var estimate sql.NullFloat64
			var support sql.NullString
			if err := r.Scan(&backlog, &status, &estimate, &support); err != nil {
				return err
			}
			got = append(got, fmt.Sprintf("%v %q", support.Valid, support.String))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got, want := handed("10"), []string{`true ""`, `true "10002"`, `false ""`}; !slices.Equal(got, want) {
		t.Fatalf("board 10's pass's support column %q; want %q", got, want)
	}
	if got, want := handed("12"), []string{`true ""`, `true ""`, `false ""`}; !slices.Equal(got, want) {
		t.Fatalf("board 12's pass's support column %q; want %q", got, want)
	}
	c, err := s.CountMirrorSections(root, "10", rule)
	if err != nil || c.All.Unestimated != 1 || c.Sprints["21"].Unestimated != 1 || c.Backlog.Unestimated != 0 {
		t.Fatalf("board 10's counts %+v, %v; want one without an estimate, in sprint 21", c, err)
	}
	if c, err := s.CountMirrorSections(root, "12", rule); err != nil || c.All.Unestimated != 1 || len(c.Sprints) != 0 {
		t.Fatalf("board 12's counts %+v, %v; want one without an estimate and no sprint", c, err)
	}
}
