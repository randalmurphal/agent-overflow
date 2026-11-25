package store

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// patchRows are n rows of board 10 spread over its sections: every third
// row in sprint 21, every fifth in sprint 22, statuses cycling over the
// three columns (status 6 is done), and an estimate on every row but each
// seventh.
func patchRows(n int) []MirrorRow {
	statuses := []string{"1", "3", "6"}
	rows := make([]MirrorRow, n)
	for i := range n {
		var sprints []string
		switch {
		case i%3 == 0:
			sprints = []string{"21"}
		case i%5 == 0:
			sprints = []string{"22"}
		}
		rows[i] = webRow(fmt.Sprintf("%05d", 20000+i), 5, fmt.Sprintf("k%03d", (i*7)%n), sprints...)
		rows[i].Card.StatusID = statuses[i%3]
		if i%7 != 0 {
			estimate := float64(i % 4)
			rows[i].Estimate = &estimate
		}
	}
	return rows
}

// patchBoard writes board 10 with patchRows(n) and returns their IDs.
func patchBoard(t *testing.T, s *Store, root int64, n int) []string {
	t.Helper()
	rows := patchRows(n)
	writeMirrorBoard(t, s, root, "10", rows)
	ids := make([]string, n)
	for i, row := range rows {
		ids[i] = row.Card.IssueID
	}
	return ids
}

var patchSections = []MirrorSection{
	{Kind: MirrorSectionSprint, SprintID: "21"}, {Kind: MirrorSectionSprint, SprintID: "22"}, {Kind: MirrorSectionBacklog},
	{Kind: MirrorSectionColumn}, {Kind: MirrorSectionColumn, Column: 1}, {Kind: MirrorSectionColumn, Column: 2},
}

// requirePatchParity fails unless a patch of board 10 for named holds,
// for every section of patchSections, exactly the rows a full read of the
// section's pages shows for those issues, whole and in order, names what
// those rows name as the pages name it, and carries the totals and
// revision CountMirrorSections reads; for a caller holding those totals it
// carries none and the same rows. It holds read with each of
// testRegroups, and returns the patch read with no regrouping.
func requirePatchParity(t *testing.T, s *Store, root int64, step string, named []string) MirrorBoardPatch {
	t.Helper()
	var patch MirrorBoardPatch
	for i, rg := range testRegroups(t) {
		read := requirePatchParityWith(t, s, root, rg.regroup, step+", "+rg.name, named)
		if i == 0 {
			patch = read
		}
	}
	return patch
}

func requirePatchParityWith(t *testing.T, s *Store, root int64, regroup MirrorRegroup, step string, named []string) MirrorBoardPatch {
	t.Helper()
	patch, ok, err := s.ReadMirrorBoardPatch(root, "10", regroup, patchSections, named, MirrorTotalsAt{})
	if err != nil || !ok || patch.Counts == nil {
		t.Fatalf("%s: patch = %+v, %v, %v", step, patch, ok, err)
	}
	counts, err := s.CountMirrorSections(root, "10", regroup)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(counts) != fmt.Sprint(*patch.Counts) || patch.Revision != counts.Revision || patch.Lifetime != currentLifetime(s, root, "10") {
		t.Errorf("%s: patch totals %+v at %d/%d; counts %+v", step, *patch.Counts, patch.Lifetime, patch.Revision, counts)
	}
	held := MirrorTotalsAt{Lifetime: patch.Lifetime, Revision: counts.TotalsRevision}
	if again, ok, err := s.ReadMirrorBoardPatch(root, "10", regroup, patchSections, named, held); err != nil || !ok || again.Counts != nil ||
		!reflect.DeepEqual(again.Sections, patch.Sections) || again.Revision != patch.Revision {
		t.Errorf("%s: a patch for the totals held at %+v = %+v, %v, %v", step, held, again, ok, err)
	}
	users, statuses, parents := map[string]MirrorUser{}, map[string]MirrorStatus{}, map[string]MirrorCard{}
	var shown []MirrorRow
	for i, section := range patchSections {
		var want []MirrorRow
		for after := (MirrorRowKey{}); ; {
			page, err := s.ListMirrorSection(root, "10", regroup, section, after)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Rows {
				if slices.Contains(named, row.Card.IssueID) {
					want = append(want, row)
				}
			}
			for _, u := range page.Names.Catalog.Users {
				users[u.ID] = u
			}
			for _, st := range page.Names.Catalog.Statuses {
				statuses[st.ID] = st
			}
			for _, p := range page.Names.Parents {
				parents[p.IssueID] = p
			}
			if !page.More {
				break
			}
			after = page.Next
		}
		if !reflect.DeepEqual(patch.Sections[i], want) {
			t.Errorf("%s: section %+v:\npatch %+v\npages %+v", step, section, patch.Sections[i], want)
		}
		shown = append(shown, want...)
	}
	var want MirrorNames
	seen := map[string]bool{}
	for _, row := range shown {
		if u, ok := users[row.Card.AssigneeID]; ok && !seen["u"+u.ID] {
			seen["u"+u.ID] = true
			want.Catalog.Users = append(want.Catalog.Users, u)
		}
		if st, ok := statuses[row.Card.StatusID]; ok && !seen["s"+st.ID] {
			seen["s"+st.ID] = true
			want.Catalog.Statuses = append(want.Catalog.Statuses, st)
		}
		if p, ok := parents[row.Card.ParentID]; ok && !seen["p"+p.IssueID] {
			seen["p"+p.IssueID] = true
			want.Parents = append(want.Parents, p)
		}
	}
	sortMirrorCatalog(&want.Catalog)
	slices.SortFunc(want.Parents, func(a, b MirrorCard) int { return strings.Compare(a.IssueID, b.IssueID) })
	if !reflect.DeepEqual(patch.Names, want) {
		t.Errorf("%s: patch names %+v\npages name %+v", step, patch.Names, want)
	}
	return patch
}

// patchEstimates counts the estimates a patch's rows show.
func patchEstimates(patch MirrorBoardPatch) int {
	n := 0
	for _, rows := range patch.Sections {
		for _, row := range rows {
			if row.Estimate != nil {
				n++
			}
		}
	}
	return n
}

// A patch holds, for each section, exactly what a full read of the
// section shows for the named issues now, rows whole and names alike,
// with the totals the counts read and the revision of the snapshot,
// through every change that alters what a row shows: an estimate set, the
// board's estimate definition changed (rows read before it show none)
// and the rows read again, a row moving sprint, a row leaving the board,
// a status renamed and a parent appearing. A board the mirror does not
// show has none.
func TestMirrorBoardPatchMatchesTheSections(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	rows := patchRows(230)
	writeMirrorBoard(t, s, root, "10", rows)
	// In sprint 21, done in no sprint, done in sprint 22, in progress, in
	// sprint 22, in the backlog, and an issue the mirror lacks.
	named := []string{rows[0].Card.IssueID, rows[2].Card.IssueID, rows[3].Card.IssueID, rows[5].Card.IssueID,
		rows[7].Card.IssueID, rows[100].Card.IssueID, rows[229].Card.IssueID, "missing"}
	read := int64(1)
	write := func(page MirrorRowPage) {
		t.Helper()
		read++
		page.BoardID, page.ReadAt = "10", mirrorReadAt(read)
		mustWriteRowPage(t, s, root, page, nil)
	}
	structure := func(change func(*MirrorStructure)) {
		t.Helper()
		read++
		st := webStructure()
		st.Board.EstimateUnit, st.ReadAt = MirrorEstimateTime, mirrorReadAt(read)
		change(&st)
		mustReplaceStructure(t, s, root, st, true)
	}
	if patch := requirePatchParity(t, s, root, "as written", named); patchEstimates(patch) == 0 {
		t.Fatal("no named row shows an estimate")
	}

	eight := 8.0
	rows[0].Estimate = &eight
	write(MirrorRowPage{Rows: []MirrorRow{rows[0]}})
	requirePatchParity(t, s, root, "an estimate set", named)

	structure(func(*MirrorStructure) {})
	if patch := requirePatchParity(t, s, root, "the estimate definition changed", named); patchEstimates(patch) != 0 {
		t.Fatalf("rows read before the estimate definition changed show %d estimates", patchEstimates(patch))
	}
	write(MirrorRowPage{Rows: []MirrorRow{rows[0], rows[3], rows[7], rows[100]}})
	if patch := requirePatchParity(t, s, root, "the rows read again", named); patchEstimates(patch) == 0 {
		t.Fatal("the rows read again show no estimate")
	}

	rows[3].Card.UpdatedAt, rows[3].Card.Sprints = 6, []string{"22"}
	write(MirrorRowPage{Rows: []MirrorRow{rows[3]}})
	requirePatchParity(t, s, root, "a row moving sprint", named)

	write(MirrorRowPage{Missing: []string{rows[5].Card.IssueID}})
	requirePatchParity(t, s, root, "a row leaving the board", named)

	structure(func(st *MirrorStructure) { st.Catalog.Statuses[1].Name = "Doing" })
	if patch := requirePatchParity(t, s, root, "a status renamed", named); !slices.Contains(patch.Names.Catalog.Statuses,
		MirrorStatus{ID: "3", Name: "Doing", Group: MirrorGroupInProgress}) {
		t.Fatalf("the patch names %+v; want status 3 renamed", patch.Names.Catalog.Statuses)
	}

	rows[7].Card.UpdatedAt, rows[7].Card.ParentID = 7, "30000"
	write(MirrorRowPage{Rows: []MirrorRow{rows[7]}})
	requirePatchParity(t, s, root, "a parent the mirror lacks", named)
	write(MirrorRowPage{Rows: []MirrorRow{webRow("30000", 5, "z")}})
	if patch := requirePatchParity(t, s, root, "a parent appearing", named); len(patch.Names.Parents) != 1 {
		t.Fatalf("the patch names parents %+v; want 30000", patch.Names.Parents)
	}

	if _, ok, err := s.ReadMirrorBoardPatch(root, "11", MirrorRegroup{}, patchSections, named, MirrorTotalsAt{}); ok || err != nil {
		t.Fatalf("a board with no structure: %v, %v", ok, err)
	}
	if _, _, err := s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, patchSections, make([]string, mirrorMaxLookup+1), MirrorTotalsAt{}); err == nil {
		t.Fatal("an over-long list was read")
	}
}

// The patch reads in one snapshot, one statement per part: the board, the
// column statuses, the two count statements, a statement per section, and
// the names (users and statuses, parents); for a caller holding the
// totals, neither the column statuses nor the counts. It runs alone: it
// counts the package's statements.
func TestMirrorBoardPatchStatements(t *testing.T) {
	s, _, root := newMirror(t)
	ids := patchBoard(t, s, root, 30)
	sections := patchSections[:3]
	var patch MirrorBoardPatch
	statements := countMirrorStatements(t, func() {
		var ok bool
		var err error
		if patch, ok, err = s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, sections, ids[:1], MirrorTotalsAt{}); err != nil || !ok {
			t.Fatalf("%v, %v", ok, err)
		}
	})
	// board 1, column statuses 1, counts 2, sections 3, names 1 (no parent).
	if want := 1 + 1 + 2 + len(sections) + 1; statements != want {
		t.Fatalf("a patch of one issue in %d sections ran %d statements; want %d", len(sections), statements, want)
	}
	held := MirrorTotalsAt{Lifetime: patch.Lifetime, Revision: patch.Counts.TotalsRevision}
	statements = countMirrorStatements(t, func() {
		if again, ok, err := s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, sections, ids[:1], held); err != nil || !ok || again.Counts != nil {
			t.Fatalf("%+v, %v, %v", again, ok, err)
		}
	})
	// board 1, sections 3, names 1.
	if want := 1 + len(sections) + 1; statements != want {
		t.Fatalf("a patch of one issue in %d sections for the totals held ran %d statements; want %d", len(sections), statements, want)
	}
	statements = countMirrorStatements(t, func() {
		if _, err := s.ListMirrorIssueBoards(root, ids, ids); err != nil {
			t.Fatal(err)
		}
	})
	if statements != 1 {
		t.Fatalf("the boards of %d issues took %d statements", len(ids), statements)
	}
}

// The boards of a list of issues come from one probe per issue of the
// board rows' primary key, with their children's from one probe per issue
// of the parent index, and a patch's section rows from one probe per named
// issue: neither walks the board.
func TestMirrorPatchReadsProbeByKey(t *testing.T) {
	t.Parallel()
	s, _, _ := newMirror(t)
	for kind, query := range mirrorPatchSQL {
		plan := explainMirror(t, s, query)
		if !strings.Contains(plan, "SEARCH b USING PRIMARY KEY (root_id=? AND issue_id=? AND board_id=?)") || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("section %d patch plan:\n%s\nwant a key probe per named issue and no sort", kind, plan)
		}
	}
	plan := explainMirror(t, s, mirrorIssueBoardsSQL)
	if !strings.Contains(plan, "SEARCH b USING PRIMARY KEY (root_id=? AND issue_id=?)") ||
		!strings.Contains(plan, "SEARCH c USING INDEX idx_tracker_cache_issues_parent (root_id=? AND parent_id=?)") {
		t.Errorf("issue boards plan:\n%s\nwant a key prefix probe per issue and a parent index probe per issue", plan)
	}
}

// The boards of issues are their own rows' and their children's: a row
// shows its parent's key and title. The parent list reads children alone,
// so a parent that lost its last row still names its children's boards.
func TestMirrorIssueBoards(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	child := webRow("10003", 5, "c")
	child.Card.ParentID = "10002"
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a"), child})
	boards := func(issues, parents []string) []MirrorIssueBoard {
		t.Helper()
		got, err := s.ListMirrorIssueBoards(root, issues, parents)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b MirrorIssueBoard) int {
			return strings.Compare(a.IssueID+" "+a.BoardID, b.IssueID+" "+b.BoardID)
		})
		return got
	}
	all := []string{"10001", "10002", "10009"}
	if got, want := boards(all, all), []MirrorIssueBoard{{"10001", "10"}, {"10001", "11"}, {"10002", "10"}, {"10003", "11"}}; !slices.Equal(got, want) {
		t.Fatalf("boards %v, want %v", got, want)
	}
	if got, want := boards(nil, []string{"10002"}), []MirrorIssueBoard{{"10003", "11"}}; !slices.Equal(got, want) {
		t.Fatalf("the children of 10002: %v, want %v", got, want)
	}
	if _, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10002"}, ReadAt: mirrorReadAt(30)})); err != nil {
		t.Fatal(err)
	}
	if got, want := boards([]string{"10002"}, []string{"10002"}), []MirrorIssueBoard{{"10003", "11"}}; !slices.Equal(got, want) {
		t.Fatalf("a parent with no row left: %v, want its child's %v", got, want)
	}
}
