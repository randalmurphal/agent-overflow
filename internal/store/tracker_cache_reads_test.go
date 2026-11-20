package store

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func sectionIDs(page MirrorSectionPage) string {
	ids := make([]string, 0, len(page.Rows))
	for _, row := range page.Rows {
		ids = append(ids, row.Card.IssueID)
	}
	return strings.Join(ids, ",")
}

// Each section holds the rows its sprint, the backlog or its column's
// statuses select, in order-key order, ties by issue ID. The backlog holds
// the rows in no open sprint of the board (a closed one, or one only
// another board shows, leaves them there) whose status is not done or
// canceled, an unknown status counting as not done. The counts agree with
// the pages.
func TestListMirrorSectionSelectsItsRows(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID = "11"
	other.Sprints = append(other.Sprints, MirrorSprint{ID: "31", OriginBoardID: "11", Name: "APP Sprint 1", State: MirrorSprintActive})
	mustReplaceStructure(t, s, root, other, true)
	row := func(id, order, status string, sprints ...string) MirrorRow {
		r := webRow(id, 5, order, sprints...)
		r.Card.StatusID = status
		return r
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{
		row("10001", "a", "1", "21"),
		row("10002", "b", "3", "22"),
		row("10003", "c", "1", "20"),
		row("10004", "d", "3", "31"),
		row("10005", "b", "3", "21", "20"),
		row("10007", "e", "10001"),
		row("10008", "f", "6"),
		row("10009", "g", "99"),
	}}, nil)
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "11", Rows: []MirrorRow{row("10006", "a", "1", "21")}}, nil)

	for _, tc := range []struct {
		section MirrorSection
		want    string
	}{
		{MirrorSection{Kind: MirrorSectionSprint, SprintID: "21"}, "10001,10005"},
		{MirrorSection{Kind: MirrorSectionSprint, SprintID: "22"}, "10002"},
		{MirrorSection{Kind: MirrorSectionSprint, SprintID: "99"}, ""},
		{MirrorSection{Kind: MirrorSectionBacklog}, "10003,10004,10009"},
		{MirrorSection{Kind: MirrorSectionColumn, Column: 0}, "10001,10003"},
		{MirrorSection{Kind: MirrorSectionColumn, Column: 1}, "10002,10005,10004"},
		{MirrorSection{Kind: MirrorSectionColumn, Column: 2}, "10007,10008"},
	} {
		page, err := s.ListMirrorSection(root, "10", tc.section, MirrorRowKey{})
		if err != nil || sectionIDs(page) != tc.want || page.More {
			t.Errorf("%+v = %s (more %v), %v; want %s", tc.section, sectionIDs(page), page.More, err, tc.want)
		}
	}
	page, err := s.ListMirrorSection(root, "10", MirrorSection{Kind: MirrorSectionSprint, SprintID: "21"}, MirrorRowKey{})
	if err != nil || !reflect.DeepEqual(page.Rows[1], row("10005", "b", "3", "20", "21")) {
		t.Fatalf("row = %+v, %v", page.Rows, err)
	}
	if _, err := s.ListMirrorSection(root, "10", MirrorSection{}, MirrorRowKey{}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("no section kind: %v", err)
	}

	counts, err := s.CountMirrorSections(root, "10")
	want := MirrorSectionCounts{Rows: 8, Backlog: 3, Sprints: map[string]int{"20": 2, "21": 2, "22": 1}, Columns: map[int]int{0: 2, 1: 3, 2: 2}}
	if err != nil || !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %+v, %v\nwant %+v", counts, err, want)
	}
	empty, err := s.CountMirrorSections(root, "12")
	if err != nil || !reflect.DeepEqual(empty, MirrorSectionCounts{Sprints: map[string]int{}, Columns: map[int]int{}}) {
		t.Fatalf("counts of no board = %+v, %v", empty, err)
	}

	issueRows, err := s.ListMirrorIssueRows(root, "10001")
	if err != nil || !reflect.DeepEqual(issueRows, []MirrorIssueRow{{BoardID: "10", OrderKey: "a"}}) {
		t.Fatalf("rows of 10001 = %+v, %v", issueRows, err)
	}
}

// The backlog follows the group its own mirror holds for a status: a
// status moved to done by a structure read leaves it, page and count
// alike. A board that plans no sprints has every row not done in its
// backlog.
func TestMirrorBacklogFollowsStatusGroups(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	kanban := webStructure()
	kanban.Board.ID, kanban.Board.PlansSprints, kanban.Sprints = "12", false, nil
	mustReplaceStructure(t, s, root, kanban, true)
	row := func(id, order, status string, sprints ...string) MirrorRow {
		r := webRow(id, 5, order, sprints...)
		r.Card.StatusID = status
		return r
	}
	rows := []MirrorRow{row("10001", "a", "1", "21"), row("10002", "b", "3"), row("10003", "c", "10001"), row("10004", "d", "99")}
	for _, board := range []string{"10", "12"} {
		mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: board, Rows: rows}, nil)
	}
	backlog := func(board string) (string, int) {
		t.Helper()
		page, err := s.ListMirrorSection(root, board, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
		if err != nil {
			t.Fatal(err)
		}
		counts, err := s.CountMirrorSections(root, board)
		if err != nil {
			t.Fatal(err)
		}
		return sectionIDs(page), counts.Backlog
	}
	for board, want := range map[string]string{"10": "10002,10004", "12": "10001,10002,10004"} {
		if got, n := backlog(board); got != want || n != len(strings.Split(want, ",")) {
			t.Errorf("board %s backlog = %s (%d counted); want %s", board, got, n, want)
		}
	}
	// Another mirror's group for the same status ID does not count.
	linear := mustCreateTrackerConnection(t, s, NewTrackerConnection{Kind: TrackerKindLinear, Site: "workspace-a", CreatedAt: 12})
	otherRoot, _, err := s.GetOrCreateMirrorRoot(linear.ID)
	if err != nil {
		t.Fatal(err)
	}
	regrouped := webStructure()
	regrouped.Catalog.Statuses[1].Group = MirrorGroupDone
	mustReplaceStructure(t, s, otherRoot, regrouped, true)
	if got, n := backlog("10"); got != "10002,10004" || n != 2 {
		t.Errorf("after another mirror's status 3 became done: backlog = %s (%d counted)", got, n)
	}
	mustReplaceStructure(t, s, root, regrouped, true)
	if got, n := backlog("10"); got != "10004" || n != 1 {
		t.Errorf("after status 3 became done: backlog = %s (%d counted)", got, n)
	}
}

// A section reads in pages that continue after the last row, ties in the
// order key included, and say when another follows.
func TestListMirrorSectionPagesByKeyset(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	n := 2*mirrorSectionPageSize + 5
	rows := make([]MirrorRow, 0, n)
	for i := range n {
		rows = append(rows, webRow(fmt.Sprintf("1%04d", n-i), 5, "m"))
	}
	mustWriteRowPage(t, s, root, MirrorRowPage{BoardID: "10", Rows: rows}, nil)

	var after MirrorRowKey
	var all []string
	for pageNo, wantMore := range []bool{true, true, false} {
		page, err := s.ListMirrorSection(root, "10", MirrorSection{Kind: MirrorSectionBacklog}, after)
		if err != nil || page.More != wantMore {
			t.Fatalf("page %d: more %v, %v; want %v", pageNo, page.More, err, wantMore)
		}
		all = append(all, strings.Split(sectionIDs(page), ",")...)
		after = page.Next
	}
	if len(all) != n {
		t.Fatalf("read %d rows; want %d", len(all), n)
	}
	for i, id := range all {
		if want := fmt.Sprintf("1%04d", i+1); id != want {
			t.Fatalf("row %d = %s; want %s", i, id, want)
		}
	}
}

func TestListMirrorCardsIsBounded(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	ids := make([]string, mirrorMaxLookup+1)
	for i := range ids {
		ids[i] = strconv.Itoa(i)
	}
	if _, err := s.ListMirrorCards(root, ids); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("lookup over the bound: %v", err)
	}
	if cards, err := s.ListMirrorCards(root, ids[:mirrorMaxLookup]); err != nil || cards != nil {
		t.Fatalf("lookup at the bound = %v, %v", cards, err)
	}
}

// A section page walks the board's order index from the keyset with no
// sort, and the history-due read probes its partial index.
func TestMirrorReadsUseTheirIndexes(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, trackerMigrationVersion)
	for kind, query := range mirrorSectionSQL {
		args := []any{"r", "b", "", ""}
		switch kind {
		case MirrorSectionSprint:
			args = append(args, "s")
		case MirrorSectionColumn:
			args = append(args, 0)
		}
		plan := queryPlanText(t, db, "EXPLAIN QUERY PLAN "+query, args...)
		if !strings.Contains(plan, "idx_tracker_cache_board_issues_order (root_id=? AND board_id=? AND (order_key,issue_id)>(?,?))") ||
			strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("section %d plan:\n%s\nwant a range of idx_tracker_cache_board_issues_order and no sort", kind, plan)
		}
	}
	plan := queryPlanText(t, db, "EXPLAIN QUERY PLAN "+mirrorHistoryDueSQL, "r", "")
	if !strings.Contains(plan, "idx_tracker_cache_issues_history_due") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("history due plan:\n%s", plan)
	}
	plan = queryPlanText(t, db, "EXPLAIN QUERY PLAN "+mirrorBoardlessSQL, "r", "")
	if !strings.Contains(plan, "idx_tracker_cache_issues_boardless") || strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("unkept sweep plan:\n%s", plan)
	}
}
