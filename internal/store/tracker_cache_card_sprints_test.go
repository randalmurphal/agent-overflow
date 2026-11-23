package store

import (
	"fmt"
	"maps"
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

// manySprintCards are n board cards of update at, each naming per sprints
// from first on: issue i names first+i*10_000 onward.
func manySprintCards(n, per int, at int64, first int) []MirrorCard {
	out := make([]MirrorCard, n)
	for i := range out {
		out[i] = webCard(fmt.Sprint(20001+i), at, sprintsOf(first+i*10_000, per)...)
	}
	return out
}

// writeBoardCards writes cards as rows of board 10 read from at.
func writeBoardCards(t *testing.T, s *Store, root int64, at int64, cards []MirrorCard) {
	t.Helper()
	page := MirrorRowPage{BoardID: "10", ReadAt: mirrorReadAt(at)}
	for i, c := range cards {
		page.Rows = append(page.Rows, MirrorRow{Card: c, OrderKey: fmt.Sprintf("a%04d", i)})
	}
	if _, err := s.WriteMirrorRowPage(root, currentPage(s, root, page), nil); err != nil {
		t.Fatal(err)
	}
}

// totalSprints is how many memberships root holds.
func totalSprints(t *testing.T, s *Store, root int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM tracker_cache_issue_sprints WHERE root_id = ?`, root).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 50 cards of 1,200 memberships each are written, replaced half by other
// sprints, and then by none, in statements of at most mirrorSprintChunk
// memberships added or clearMirrorChunk removed: 60,000 memberships are
// added in 60 statements and removed in 120, a half replaced in at most
// one removal per chunk of the 60,000 read and 30 additions, and each
// write leaves exactly the lists the cards name.
func TestManyMembershipsAreWrittenInBoundedStatements(t *testing.T) {
	if mirrorSprintChunk != 1000 || clearMirrorChunk != 500 {
		t.Fatalf("the chunks are %d and %d; the test is sized for 1000 and 500", mirrorSprintChunk, clearMirrorChunk)
	}
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	recorder := recordMirrorWrites(t, s)
	check := func(what string, cards []MirrorCard, most, statements int, exact bool) {
		t.Helper()
		for _, c := range cards {
			if got := storedSprints(t, s, root, c.IssueID); !slices.Equal(got, sortedSprints(c.Sprints)) {
				t.Fatalf("%s: issue %s holds %d sprints; want %d", what, c.IssueID, len(got), len(c.Sprints))
			}
		}
		gotMost, gotStatements := recorder.mostSprintWritesInAStatement()
		t.Logf("%s: %d statements wrote memberships, at most %d each", what, gotStatements, gotMost)
		if gotMost > most || gotStatements > statements || exact && gotStatements != statements {
			t.Fatalf("%s: %d statements wrote memberships, at most %d each; want %d of at most %d", what, gotStatements, gotMost, statements, most)
		}
		recorder.reset()
	}
	cards := manySprintCards(50, 1200, 2, 100_000)
	writeBoardCards(t, s, root, 2, cards)
	check("the first write", cards, mirrorSprintChunk, 60, true)
	// Each card keeps its last 600 sprints and names 600 others.
	half := manySprintCards(50, 1200, 3, 100_600)
	writeBoardCards(t, s, root, 3, half)
	check("half replaced", half, mirrorSprintChunk, 120+30, false)
	if n := totalSprints(t, s, root); n != 60_000 {
		t.Fatalf("%d memberships after half replaced; want 60,000", n)
	}
	writeBoardCards(t, s, root, 4, manySprintCards(50, 0, 4, 0))
	check("all removed", nil, clearMirrorChunk, 120, true)
	if n := totalSprints(t, s, root); n != 0 {
		t.Fatalf("%d memberships after every card named none", n)
	}
}

// A card read again naming the same sprints is compared by its digest
// alone: its write reads and writes no membership and runs the statements
// a card of one sprint runs, but for one more mark statement per
// mirrorSprintChunk references (its sprints, status and assignee). Its VM
// steps grow with its sprints only through the mark statement, which
// probes each reference once, so linearly. Each size is measured on a
// mirror of its own.
func TestAnIdenticalMembershipSetIsOneComparison(t *testing.T) {
	type cost struct{ statements, steps, sprintWrites int }
	measure := func(per int) cost {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		cards := manySprintCards(1, per, 2, 100_000)
		writeBoardCards(t, s, root, 2, cards)
		recorder := recordMirrorWrites(t, s)
		var c cost
		c.steps = countMirrorVMSteps(t, func() {
			c.statements = countMirrorStatements(t, func() {
				changes, err := s.UpsertMirrorCards(root, currentSweep(s, root), cards, MirrorCatalog{}, mirrorReadAt(3))
				if err != nil || len(changes.Cards) != 0 {
					t.Fatalf("an identical card read again: %+v, %v", changes, err)
				}
			})
		})
		c.sprintWrites = recorder.rowWrites()["tracker_cache_issue_sprints"]
		if got := recorder.rowWrites(); !maps.Equal(got, map[string]int{"tracker_cache_issues": 1}) {
			t.Fatalf("%d sprints: an identical card wrote %v; want its read time alone", per, got)
		}
		return c
	}
	sizes := []int{1, 10, 100, 1000}
	costs := make([]cost, len(sizes))
	for i, per := range sizes {
		costs[i] = measure(per)
	}
	t.Logf("identical redelivery of one card by its sprints %v: %+v", sizes, costs)
	for i, c := range costs {
		marks := (sizes[i]+2-1)/mirrorSprintChunk - (sizes[0]+2-1)/mirrorSprintChunk
		if c.sprintWrites != 0 || c.statements != costs[0].statements+marks {
			t.Fatalf("%d sprints: %+v; want no membership write and the %d statements of one sprint and %d more marks", sizes[i], c, costs[0].statements, marks)
		}
	}
	// Linear: the steps from 100 to 1,000 sprints grow at most 12 times
	// those from 10 to 100, where a comparison per membership pair grows
	// 100 times.
	if grow, base := costs[3].steps-costs[2].steps, costs[2].steps-costs[1].steps; base <= 0 || grow > 12*base {
		t.Fatalf("steps %+v grow faster than the sprints", costs)
	}
}

// The digest is canonical: the order a card names its sprints in does not
// change it, any other set does, and no set's digest is the sweep's mark.
func TestTheSprintDigestIsTheSet(t *testing.T) {
	t.Parallel()
	if mirrorSprintSet(nil) != "" || mirrorSprintSet([]string{}) != "" {
		t.Fatal("no sprints have a digest")
	}
	a, b := mirrorSprintSet([]string{"21", "3", "100"}), mirrorSprintSet([]string{"100", "21", "3"})
	if a != b || len(a) != 32 {
		t.Fatalf("the digests of one set in two orders: %q, %q", a, b)
	}
	for _, other := range [][]string{{"21", "3"}, {"21", "3", "1000"}, {"213", "100"}, {"2", "13", "100"}} {
		if mirrorSprintSet(other) == a {
			t.Fatalf("%v digests to the set's", other)
		}
	}
	if a == mirrorSprintSetPartial {
		t.Fatal("a set digests to the sweep's mark")
	}
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
