package store

import (
	"fmt"
	"slices"
	"testing"
)

// sprintsOf is n sprint IDs from first on.
func sprintsOf(first, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprint(first + i)
	}
	return out
}

// storedSprints is the sprints the mirror holds of issue, sorted.
func storedSprints(t *testing.T, s *Store, root int64, issue string) []string {
	t.Helper()
	ids, err := queryIDs(s.db, `SELECT sprint_id FROM tracker_cache_issue_sprints WHERE root_id = ? AND issue_id = ? ORDER BY sprint_id`, root, issue)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// sortedSprints is sprints sorted as storedSprints reads them.
func sortedSprints(sprints []string) []string {
	out := slices.Clone(sprints)
	slices.Sort(out)
	return out
}

// A card names any number of sprints, on each card writer: a board page
// and a write of cards read by ID each store an issue of 60 sprints and
// its sibling of one, and a later write replaces the 60 with others.
func TestACardNamesAnyNumberOfSprints(t *testing.T) {
	t.Parallel()
	for name, write := range map[string]func(s *Store, root int64, at int64, cards ...MirrorCard) error{
		"board rows": func(s *Store, root int64, at int64, cards ...MirrorCard) error {
			page := MirrorRowPage{BoardID: "10", ReadAt: mirrorReadAt(at)}
			for i, c := range cards {
				page.Rows = append(page.Rows, MirrorRow{Card: c, OrderKey: fmt.Sprint("a", i)})
			}
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), nil)
			return err
		},
		"cards by ID": func(s *Store, root int64, at int64, cards ...MirrorCard) error {
			_, err := s.UpsertMirrorCards(root, currentSweep(s, root), cards, MirrorCatalog{}, mirrorReadAt(at))
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, _, root := newMirror(t)
			mustReplaceStructure(t, s, root, webStructure(), true)
			// The issues are on the board, so the mirror keeps them for a
			// write of cards by ID.
			mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 1, "a"), webRow("10002", 1, "b")), nil)
			many := sprintsOf(1000, 60)
			if err := write(s, root, 2, webCard("10001", 2, many...), webCard("10002", 2, "21")); err != nil {
				t.Fatalf("an issue of 60 sprints and a sibling: %v", err)
			}
			if got := storedSprints(t, s, root, "10001"); !slices.Equal(got, sortedSprints(many)) {
				t.Fatalf("the issue of 60 sprints holds %d: %v", len(got), got)
			}
			if got := storedSprints(t, s, root, "10002"); !slices.Equal(got, []string{"21"}) {
				t.Fatalf("its sibling holds %v", got)
			}
			others := sprintsOf(2000, 60)
			if err := write(s, root, 3, webCard("10001", 3, others...), webCard("10002", 3)); err != nil {
				t.Fatal(err)
			}
			if got := storedSprints(t, s, root, "10001"); !slices.Equal(got, sortedSprints(others)) {
				t.Fatalf("after the replace the issue holds %d: %v", len(got), got)
			}
			if got := storedSprints(t, s, root, "10002"); len(got) != 0 {
				t.Fatalf("after leaving its sprint the sibling holds %v", got)
			}
		})
	}
}

// A write whose cards name more memberships than one sprint statement
// takes runs the sprint and mark statements a group of cards at a time
// (mirrorSprintGroups), each group of at most mirrorSprintChunk
// memberships or one card of more, and stores every membership: three
// cards of 400 and one of 1,200 are three groups, two statements of each
// statement kind more than one group, and replacing them all leaves
// exactly the new lists.
func TestCardSprintsAreWrittenInChunks(t *testing.T) {
	if mirrorSprintChunk != 1000 {
		t.Fatalf("the chunk is %d; the test's cards are sized for 1000", mirrorSprintChunk)
	}
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	ids := []string{"10001", "10002", "10003", "10004"}
	sizes := []int{400, 400, 400, 1200}
	var rows []MirrorRow
	for i, id := range ids {
		rows = append(rows, webRow(id, 1, fmt.Sprint("a", i)))
	}
	mustWriteRowPage(t, s, root, webPage("10", rows...), nil)
	cards := func(at int64, first int) []MirrorCard {
		out := make([]MirrorCard, len(ids))
		for i, id := range ids {
			out[i] = webCard(id, at, sprintsOf(first+i*10_000, sizes[i])...)
		}
		return out
	}
	if groups := mirrorSprintGroups(rowsOf(cards(2, 100_000))); len(groups) != 3 || len(groups[0]) != 2 || len(groups[1]) != 1 || len(groups[2]) != 1 {
		t.Fatalf("the groups %v", groupSizes(groups))
	}
	one := cards(2, 100_000)[:1]
	baseline := countMirrorStatements(t, func() {
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), one, MirrorCatalog{}, mirrorReadAt(2)); err != nil {
			t.Fatal(err)
		}
	})
	all := cards(3, 200_000)
	statements := countMirrorStatements(t, func() {
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), all, MirrorCatalog{}, mirrorReadAt(3)); err != nil {
			t.Fatal(err)
		}
	})
	if statements != baseline+2*3 {
		t.Fatalf("a write of three groups ran %d statements, one card's %d; want 6 more", statements, baseline)
	}
	for i, c := range all {
		if got := storedSprints(t, s, root, c.IssueID); !slices.Equal(got, sortedSprints(c.Sprints)) {
			t.Fatalf("issue %s holds %d sprints; want %d", c.IssueID, len(got), sizes[i])
		}
	}
	replaced := cards(4, 300_000)
	if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), replaced, MirrorCatalog{}, mirrorReadAt(4)); err != nil {
		t.Fatal(err)
	}
	for _, c := range replaced {
		if got := storedSprints(t, s, root, c.IssueID); !slices.Equal(got, sortedSprints(c.Sprints)) {
			t.Fatalf("after the replace issue %s holds %d sprints; want %d", c.IssueID, len(got), len(c.Sprints))
		}
	}
	total := 0
	for _, c := range replaced {
		total += len(storedSprints(t, s, root, c.IssueID))
	}
	if total != 2400 {
		t.Fatalf("%d memberships; want 2400", total)
	}
}

func rowsOf(cards []MirrorCard) []mirrorCardRow {
	out := make([]mirrorCardRow, len(cards))
	for i, c := range cards {
		out[i] = newMirrorCardRow(c)
	}
	return out
}

func groupSizes(groups [][]mirrorCardRow) []int {
	out := make([]int, len(groups))
	for i, g := range groups {
		out[i] = len(g)
	}
	return out
}

// A catalog names any number of users and statuses, written
// mirrorCatalogChunk of a kind a statement: a write of cards whose
// catalog names 1,200 users and 600 statuses stores them all, in three
// statements more than a catalog of one of each.
func TestACatalogNamesAnyNumberOfEntries(t *testing.T) {
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 1, "a")), nil)
	catalog := func(users, statuses int) MirrorCatalog {
		var c MirrorCatalog
		for i := range users {
			c.Users = append(c.Users, MirrorUser{ID: fmt.Sprint("acc-many-", i), Name: fmt.Sprint("User ", i)})
		}
		for i := range statuses {
			c.Statuses = append(c.Statuses, MirrorStatus{ID: fmt.Sprint(50_000 + i), Name: fmt.Sprint("Status ", i), Group: MirrorGroupToDo})
		}
		return c
	}
	small := countMirrorStatements(t, func() {
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 2)}, catalog(1, 1), mirrorReadAt(2)); err != nil {
			t.Fatal(err)
		}
	})
	large := countMirrorStatements(t, func() {
		if _, err := s.UpsertMirrorCards(root, currentSweep(s, root), []MirrorCard{webCard("10001", 3)}, catalog(1200, 600), mirrorReadAt(3)); err != nil {
			t.Fatalf("a catalog of 1,200 users and 600 statuses: %v", err)
		}
	})
	if large != small+3 {
		t.Fatalf("the large catalog ran %d statements, the small %d; want 3 more", large, small)
	}
	// Read by key range, as every statement the mirror tests run must
	// (TestMirrorStatementsReadByKey).
	assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_users WHERE root_id = %d AND user_id >= 'acc-many-' AND user_id < 'acc-many.'`, root), "1200")
	assertRows(t, s.db, fmt.Sprintf(`SELECT count(*) FROM tracker_cache_statuses WHERE root_id = %d AND status_id >= '50000' AND status_id < '51000'`, root), "600")
}
