package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// patchBoard writes board 10 with n rows spread over its sections: every
// third row in sprint 21, every fifth in sprint 22, statuses cycling over
// the three columns (status 6 is done).
func patchBoard(t *testing.T, s *Store, root int64, n int) []string {
	t.Helper()
	statuses := []string{"1", "3", "6"}
	rows := make([]MirrorRow, n)
	ids := make([]string, n)
	for i := range n {
		var sprints []string
		switch {
		case i%3 == 0:
			sprints = []string{"21"}
		case i%5 == 0:
			sprints = []string{"22"}
		}
		id := fmt.Sprintf("%05d", 20000+i)
		rows[i] = webRow(id, 5, fmt.Sprintf("k%03d", (i*7)%n), sprints...)
		rows[i].Card.StatusID = statuses[i%3]
		ids[i] = id
	}
	writeMirrorBoard(t, s, root, "10", rows)
	return ids
}

var patchSections = []MirrorSection{
	{Kind: MirrorSectionSprint, SprintID: "21"}, {Kind: MirrorSectionSprint, SprintID: "22"}, {Kind: MirrorSectionBacklog},
	{Kind: MirrorSectionColumn}, {Kind: MirrorSectionColumn, Column: 1}, {Kind: MirrorSectionColumn, Column: 2},
}

// A patch holds, for each section, exactly the named issues among the
// section's pages, in their order, with the totals the counts read and
// the revision of the snapshot; a board the mirror does not show has none.
func TestMirrorBoardPatchMatchesTheSections(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	ids := patchBoard(t, s, root, 230)
	named := []string{ids[0], ids[3], ids[5], ids[7], ids[100], ids[229], "missing"}
	patch, ok, err := s.ReadMirrorBoardPatch(root, "10", patchSections, named)
	if err != nil || !ok {
		t.Fatalf("patch = %v, %v", ok, err)
	}
	counts, err := s.CountMirrorSections(root, "10")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(counts) != fmt.Sprint(patch.Counts) || patch.Revision != counts.Revision || patch.Lifetime != currentLifetime(s, root, "10") {
		t.Fatalf("patch totals %+v at %d/%d; counts %+v", patch.Counts, patch.Lifetime, patch.Revision, counts)
	}
	for i, section := range patchSections {
		var want []string
		for after := (MirrorRowKey{}); ; {
			page, err := s.ListMirrorSection(root, "10", section, after)
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range page.Rows {
				if slices.Contains(named, row.Card.IssueID) {
					want = append(want, row.Card.IssueID+"@"+row.OrderKey)
				}
			}
			if !page.More {
				break
			}
			after = page.Next
		}
		var got []string
		for _, row := range patch.Sections[i] {
			got = append(got, row.Card.IssueID+"@"+row.OrderKey)
		}
		if !slices.Equal(got, want) {
			t.Errorf("section %+v: patch %v, pages %v", section, got, want)
		}
	}
	if len(patch.Names.Catalog.Statuses) == 0 || len(patch.Names.Catalog.Users) != 1 {
		t.Fatalf("names %+v", patch.Names)
	}
	if _, ok, err := s.ReadMirrorBoardPatch(root, "11", patchSections, named); ok || err != nil {
		t.Fatalf("a board with no structure: %v, %v", ok, err)
	}
	if _, _, err := s.ReadMirrorBoardPatch(root, "10", patchSections, make([]string, mirrorMaxLookup+1)); err == nil {
		t.Fatal("an over-long list was read")
	}
}

// The patch reads in one snapshot, one statement per part: the board, the
// column statuses, the two count walks, a statement per section, and the
// names (users and statuses, parents). It runs alone: it counts the
// package's statements.
func TestMirrorBoardPatchStatements(t *testing.T) {
	s, _, root := newMirror(t)
	ids := patchBoard(t, s, root, 30)
	sections := patchSections[:3]
	statements := countMirrorStatements(t, func() {
		if _, ok, err := s.ReadMirrorBoardPatch(root, "10", sections, ids[:1]); err != nil || !ok {
			t.Fatalf("%v, %v", ok, err)
		}
	})
	// board 1, column statuses 1, counts 2, sections 3, names 1 (no parent).
	if want := 1 + 1 + 2 + len(sections) + 1; statements != want {
		t.Fatalf("a patch of one issue in %d sections ran %d statements; want %d", len(sections), statements, want)
	}
	statements = countMirrorStatements(t, func() {
		if _, err := s.ListMirrorIssueBoards(root, ids); err != nil {
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
// shows its parent's key and title.
func TestMirrorIssueBoards(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	child := webRow("10003", 5, "c")
	child.Card.ParentID = "10002"
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a"), webRow("10002", 5, "b")})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a"), child})
	got, err := s.ListMirrorIssueBoards(root, []string{"10001", "10002", "10009"})
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b MirrorIssueBoard) int {
		return strings.Compare(a.IssueID+" "+a.BoardID, b.IssueID+" "+b.BoardID)
	})
	want := []MirrorIssueBoard{{"10001", "10"}, {"10001", "11"}, {"10002", "10"}, {"10003", "11"}}
	if !slices.Equal(got, want) {
		t.Fatalf("boards %v, want %v", got, want)
	}
}
