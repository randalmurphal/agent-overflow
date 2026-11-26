package store

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
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

// sectionRow is a row of status and sprints with estimate (negative: none).
func sectionRow(id, order, status string, estimate float64, sprints ...string) MirrorRow {
	r := webRow(id, 5, order, sprints...)
	r.Card.StatusID = status
	if estimate >= 0 {
		r.Estimate = &estimate
	}
	return r
}

// Each section holds the rows its sprint, the backlog or its column's
// statuses select, in order-key order, ties by issue ID. The backlog holds
// the rows in no open sprint of the board (a closed one, or one only
// another board shows, leaves them there) whose status is not done or
// canceled, an unknown status counting as not done. The counts and their
// estimate sums and counts of rows with no estimate agree with the pages; a
// board with no estimate unit sums and counts zero.
func TestListMirrorSectionSelectsItsRows(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	mustReplaceStructure(t, s, root, webStructure(), true)
	other := webStructure()
	other.Board.ID, other.Board.EstimateUnit = "11", MirrorEstimateNone
	other.Sprints = append(other.Sprints, MirrorSprint{ID: "31", OriginBoardID: "11", Name: "APP Sprint 1", State: MirrorSprintActive})
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, webPage("10",
		sectionRow("10001", "a", "1", 1, "21"),
		sectionRow("10002", "b", "3", 2, "22"),
		sectionRow("10003", "c", "1", -1, "20"),
		sectionRow("10004", "d", "3", -1, "31"),
		sectionRow("10005", "b", "3", 5, "21", "20"),
		sectionRow("10007", "e", "10001", -1),
		sectionRow("10008", "f", "6", -1),
		sectionRow("10009", "g", "99", 9),
	), nil)
	mustWriteRowPage(t, s, root, webPage("11", sectionRow("10006", "a", "1", 4, "21")), nil)

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
		page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, tc.section, MirrorRowKey{})
		if err != nil || sectionIDs(page) != tc.want || page.More {
			t.Errorf("%+v = %s (more %v), %v; want %s", tc.section, sectionIDs(page), page.More, err, tc.want)
		}
	}
	page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionSprint, SprintID: "21"}, MirrorRowKey{})
	if err != nil || !reflect.DeepEqual(page.Rows[1], sectionRow("10005", "b", "3", 5, "20", "21")) {
		t.Fatalf("row = %+v, %v", page.Rows, err)
	}
	if _, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{}, MirrorRowKey{}); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("no section kind: %v", err)
	}

	counts, err := s.CountMirrorSections(root, "10", MirrorRegroup{})
	want := MirrorSectionCounts{Revision: page.Revision, TotalsRevision: currentTotalsRevision(s, root, "10"),
		All: MirrorSectionTotal{8, 17, 4}, Backlog: MirrorSectionTotal{3, 9, 2},
		Sprints: map[string]MirrorSectionTotal{"20": {2, 5, 1}, "21": {2, 6, 0}, "22": {1, 2, 0}},
		Columns: map[int]MirrorSectionTotal{0: {2, 1, 1}, 1: {3, 7, 1}, 2: {2, 0, 2}}}
	if err != nil || page.Revision == 0 || !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts = %+v, %v\nwant %+v", counts, err, want)
	}
	noUnit, err := s.CountMirrorSections(root, "11", MirrorRegroup{})
	if err != nil || noUnit.All != (MirrorSectionTotal{Rows: 1}) || noUnit.Sprints["21"] != (MirrorSectionTotal{Rows: 1}) {
		t.Fatalf("counts of a board with no estimate unit = %+v, %v", noUnit, err)
	}
	empty, err := s.CountMirrorSections(root, "12", MirrorRegroup{})
	if err != nil || !reflect.DeepEqual(empty, MirrorSectionCounts{Sprints: map[string]MirrorSectionTotal{}, Columns: map[int]MirrorSectionTotal{}}) {
		t.Fatalf("counts of no board = %+v, %v", empty, err)
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
	rows := []MirrorRow{sectionRow("10001", "a", "1", -1, "21"), sectionRow("10002", "b", "3", -1), sectionRow("10003", "c", "10001", -1), sectionRow("10004", "d", "99", -1)}
	for _, board := range []string{"10", "12"} {
		mustWriteRowPage(t, s, root, webPage(board, rows...), nil)
	}
	backlog := func(board string) (string, int) {
		t.Helper()
		page, err := s.ListMirrorSection(root, board, MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
		if err != nil {
			t.Fatal(err)
		}
		counts, err := s.CountMirrorSections(root, board, MirrorRegroup{})
		if err != nil {
			t.Fatal(err)
		}
		return sectionIDs(page), counts.Backlog.Rows
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
	if _, err := s.CreateProject(newProject("p2", "/tmp/p2", "P2")); err != nil {
		t.Fatal(err)
	}
	listMirrorBoards(t, s, "p2", linear.ID, "10")
	regrouped := webStructure()
	regrouped.ReadAt = mirrorReadAt(2)
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
	n := 2*mirrorSectionPageSize + 5
	rows := make([]MirrorRow, 0, n)
	for i := range n {
		rows = append(rows, webRow(fmt.Sprintf("1%04d", n-i), 5, "m"))
	}
	writeMirrorBoard(t, s, root, "10", rows)

	var after MirrorRowKey
	var all []string
	for pageNo, wantMore := range []bool{true, true, false} {
		page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, after)
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

// A continuation fails with ErrTrackerCacheStale after any change to which
// rows a section of the board holds or their order, and continues after a
// change to neither. A start reads at the new revision.
func TestMirrorSectionContinuationIsRefusedAfterAChange(t *testing.T) {
	t.Parallel()
	structure := func(edit func(*MirrorStructure)) func(s *Store, root int64) error {
		return func(s *Store, root int64) error {
			st := webStructure()
			st.ReadAt = mirrorReadAt(2)
			edit(&st)
			_, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st))
			return err
		}
	}
	card := func(id string, edit func(*MirrorRow)) func(s *Store, root int64) error {
		return func(s *Store, root int64) error {
			r := sectionRow(id, "a", "1", -1)
			r.OrderKey = map[string]string{"10001": "a", "10002": "b", "10003": "c"}[id]
			r.Card.UpdatedAt = 6
			edit(&r)
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Rows: []MirrorRow{r}, ReadAt: mirrorReadAt(2)}), nil)
			return err
		}
	}
	catalog := func(st MirrorStatus) func(s *Store, root int64) error {
		return func(s *Store, root int64) error {
			_, err := s.UpsertMirrorCards(root, currentSweep(s, root), nil, MirrorCatalog{Statuses: []MirrorStatus{st}}, mirrorReadAt(2))
			return err
		}
	}
	for _, tc := range []struct {
		name   string
		change func(s *Store, root int64) error
		stale  bool
	}{
		{"row added", func(s *Store, root int64) error {
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("10", sectionRow("10004", "0", "1", -1))), nil)
			return err
		}, true},
		{"row removed", func(s *Store, root int64) error {
			_, err := s.WriteMirrorRows(root, currentPage(s, root, MirrorRowPage{BoardID: "10", Missing: []string{"10003"}, ReadAt: mirrorReadAt(2)}))
			return err
		}, true},
		{"row moved before the cursor", card("10003", func(r *MirrorRow) { r.OrderKey = "0" }), true},
		{"status changed", card("10001", func(r *MirrorRow) { r.Card.StatusID, r.Card.Sprints = "3", []string{"21"} }), true},
		{"sprint joined", card("10002", func(r *MirrorRow) { r.Card.Sprints = []string{"21"} }), true},
		{"sprint left", card("10001", func(r *MirrorRow) {}), true},
		{"sprint added", structure(func(st *MirrorStructure) {
			st.Sprints = append(st.Sprints, MirrorSprint{ID: "23", State: MirrorSprintFuture})
		}), true},
		{"sprint removed", structure(func(st *MirrorStructure) { st.Sprints = st.Sprints[:2] }), true},
		{"sprint started", structure(func(st *MirrorStructure) { st.Sprints[1].State = MirrorSprintActive }), true},
		{"column status added", structure(func(st *MirrorStructure) { st.Columns[0].Statuses = []string{"1", "99"} }), true},
		{"column status removed", structure(func(st *MirrorStructure) { st.Columns[2].Statuses = []string{"10001"} }), true},
		{"column status moved", structure(func(st *MirrorStructure) { st.Columns[0].Statuses, st.Columns[1].Statuses = []string{"1", "3"}, nil }), true},
		{"status of a row done", catalog(MirrorStatus{ID: "1", Name: "To Do", Group: MirrorGroupDone}), true},
		{"status added", catalog(MirrorStatus{ID: "99", Name: "Blocked", Group: MirrorGroupInProgress}), false},
		{"status of no row done", catalog(MirrorStatus{ID: "3", Name: "In Progress", Group: MirrorGroupDone}), false},
		{"status of a row still open", catalog(MirrorStatus{ID: "1", Name: "To Do", Group: MirrorGroupInProgress}), false},
		{"card retitled", card("10001", func(r *MirrorRow) { r.Card.Sprints, r.Card.Data = []string{"21"}, []byte(`{"title":"Renamed"}`) }), false},
		{"status renamed", catalog(MirrorStatus{ID: "3", Name: "Doing", Group: MirrorGroupInProgress}), false},
		{"sprint renamed", structure(func(st *MirrorStructure) { st.Sprints[0].Name = "Renamed" }), false},
		{"another board's row", func(s *Store, root int64) error {
			_, err := s.WriteMirrorRowPage(root, currentPage(s, root, webPage("11", sectionRow("10009", "a", "1", -1))), nil)
			return err
		}, false},
	} {
		s, _, root := newMirror(t)
		mustReplaceStructure(t, s, root, webStructure(), true)
		other := webStructure()
		other.Board.ID = "11"
		mustReplaceStructure(t, s, root, other, true)
		mustWriteRowPage(t, s, root, webPage("10", sectionRow("10001", "a", "1", -1, "21"), sectionRow("10002", "b", "1", -1), sectionRow("10003", "c", "1", -1)), nil)
		column := MirrorSection{Kind: MirrorSectionColumn, Column: 0}
		first, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, column, MirrorRowKey{})
		if err != nil || sectionIDs(first) != "10001,10002,10003" {
			t.Fatalf("%s: first page = %s, %v", tc.name, sectionIDs(first), err)
		}
		after := MirrorRowKey{OrderKey: "a", IssueID: "10001", Lifetime: currentLifetime(s, root, "10"), Revision: first.Revision}
		if err := tc.change(s, root); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_, err = s.ListMirrorSection(root, "10", MirrorRegroup{}, column, after)
		if stale := errors.Is(err, ErrTrackerCacheStale); stale != tc.stale || (err != nil && !stale) {
			t.Errorf("%s: continuation: %v; want stale %v", tc.name, err, tc.stale)
		}
		if again, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, column, MirrorRowKey{}); err != nil || (again.Revision == first.Revision) == tc.stale {
			t.Errorf("%s: start at revision %d after %d, %v", tc.name, again.Revision, first.Revision, err)
		}
	}
}

// A continuation of a board the mirror stops showing is stale; a start
// reads nothing.
func TestMirrorSectionOfAnUnshownBoard(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	writeMirrorBoard(t, s, root, "10", []MirrorRow{webRow("10001", 5, "a")})
	first, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{})
	if err != nil || sectionIDs(first) != "10001" {
		t.Fatalf("first page = %s, %v", sectionIDs(first), err)
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "11")
	if _, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, first.Next); !errors.Is(err, ErrTrackerCacheStale) {
		t.Fatalf("continuation of an unlisted board: %v", err)
	}
	if page, err := s.ListMirrorSection(root, "10", MirrorRegroup{}, MirrorSection{Kind: MirrorSectionBacklog}, MirrorRowKey{}); err != nil || len(page.Rows) != 0 {
		t.Fatalf("start of an unlisted board = %+v, %v", page, err)
	}
}

// An issue's rows read a page at a time, by board, from the boards the
// mirror shows.
func TestListMirrorIssueRowsPages(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	boards := make([]string, 0, mirrorIssueRowsPage+2)
	for i := range mirrorIssueRowsPage + 2 {
		boards = append(boards, strconv.Itoa(100+i))
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, boards...)
	for _, board := range boards {
		writeMirrorBoard(t, s, root, board, []MirrorRow{webRow("10001", 5, "a")})
	}
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, boards[1:]...)
	first, more, err := s.ListMirrorIssueRows(root, "10001", "")
	if err != nil || !more || len(first) != mirrorIssueRowsPage || first[0].BoardID != "101" {
		t.Fatalf("first page = %d rows from %+v, more %v, %v", len(first), first[0], more, err)
	}
	rest, more, err := s.ListMirrorIssueRows(root, "10001", first[len(first)-1].BoardID)
	if err != nil || more || !reflect.DeepEqual(rest, []MirrorIssueRow{{BoardID: boards[len(boards)-1], OrderKey: "a"}}) {
		t.Fatalf("second page = %+v, %v, %v", rest, more, err)
	}
}

// The rows of several open tickets' issues read together, by issue and
// then board, each with its estimate and its board's unit; a row on a board
// no project lists any more is left out, and so is an issue the mirror has
// no row of.
func TestListMirrorTicketRows(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11", "12")
	five := 5.0
	estimated := webRow("10001", 5, "a")
	estimated.Estimate = &five
	writeMirrorBoard(t, s, root, "10", []MirrorRow{estimated, webRow("10002", 5, "b")})
	writeMirrorBoard(t, s, root, "11", []MirrorRow{webRow("10001", 5, "a")})
	writeMirrorBoard(t, s, root, "12", []MirrorRow{webRow("10001", 5, "a")})
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "11")
	rows, err := s.ListMirrorTicketRows(root, []string{"10002", "10001", "10009"})
	want := []MirrorTicketRow{
		{IssueID: "10001", BoardID: "10", Estimate: &five, EstimateUnit: MirrorEstimatePoints},
		{IssueID: "10001", BoardID: "11", EstimateUnit: MirrorEstimatePoints},
		{IssueID: "10002", BoardID: "10", EstimateUnit: MirrorEstimatePoints},
	}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if _, err := s.ListMirrorTicketRows(root, make([]string, MirrorMaxBodies+1)); !errors.Is(err, ErrInvalidTrackerCache) {
		t.Fatalf("a read of more issues than a view build takes: %v", err)
	}
	// Reads of other roots run the statement compiled for the first: the
	// package's tests fail on a cached statement compiled again.
	for _, other := range []int64{root + 1, root, root + 2, root} {
		if _, err := s.ListMirrorTicketRows(other, []string{"10001"}); err != nil {
			t.Fatalf("rows of root %d: %v", other, err)
		}
	}
}

// The linked issues no polled shown board has a row of read a page at a
// time, each once however many threads link it; one with a row on a
// polled shown board is left out, and one whose only row is on a board
// no project lists any more, or on a shown board not polled, is not.
func TestListMirrorOffBoardLinkedIssuesPages(t *testing.T) {
	t.Parallel()
	s, jira, root := newMirror(t)
	for i := range mirrorLinkedPage + 2 {
		linkIssue(t, s, fmt.Sprint("t", i), strconv.Itoa(10000+i))
	}
	linkIssue(t, s, "again", "10000")
	mustReplaceStructure(t, s, root, webStructure(), true)
	mustWriteRowPage(t, s, root, webPage("10", webRow("10001", 5, "a")), nil)
	other := webStructure()
	other.Board.ID = "11"
	mustReplaceStructure(t, s, root, other, true)
	mustWriteRowPage(t, s, root, webPage("11", webRow("10002", 5, "a")), nil)
	listMirrorBoards(t, s, defaultTestProjectID, jira.ID, "10", "12")
	first, more, err := s.ListMirrorOffBoardLinkedIssues(root, "", []string{"10", "11"})
	if err != nil || !more || len(first) != mirrorLinkedPage || first[0] != "10000" || first[1] != "10002" || slices.Contains(first, "10001") {
		t.Fatalf("first page = %d from %v, more %v, %v", len(first), first[:2], more, err)
	}
	rest, more, err := s.ListMirrorOffBoardLinkedIssues(root, first[len(first)-1], []string{"10", "11"})
	if err != nil || more || !reflect.DeepEqual(rest, []string{strconv.Itoa(10001 + mirrorLinkedPage)}) {
		t.Fatalf("second page = %v, %v, %v", rest, more, err)
	}
	// With board 10 not polled, its row keeps nothing from the read.
	for _, polled := range [][]string{nil, {"11", "12"}} {
		first, _, err := s.ListMirrorOffBoardLinkedIssues(root, "", polled)
		if err != nil || first[0] != "10000" || first[1] != "10001" || first[2] != "10002" {
			t.Fatalf("polling %v: first page from %v, %v", polled, first[:3], err)
		}
	}
}

// A page of linked issues walks each linked issue's own rows and tests
// each against the polled boards, so its cost is the issues' memberships,
// not the polled boards: as the polled set grows from 1 to 50 boards, the
// page's steps grow by the polled list's build alone, for linked issues
// with no row and for ones with a row on a shown board not polled.
func TestLinkedIssuesCostTheirMembershipsNotThePolledBoards(t *testing.T) {
	t.Parallel()
	const links = 100
	s, _, root := newMirror(t)
	other := webStructure()
	other.Board.ID = "11"
	mustReplaceStructure(t, s, root, other, true)
	for i := range links {
		linkIssue(t, s, fmt.Sprint("t", i), strconv.Itoa(30000+i))
	}
	polled := func(n int) string {
		boards := make([]string, n)
		for i := range boards {
			boards[i] = strconv.Itoa(100 + i)
		}
		list, err := encodeMirrorIDs(boards)
		if err != nil {
			t.Fatal(err)
		}
		return list
	}
	for _, membership := range []string{"none", "one row each, not polled"} {
		if membership != "none" {
			mustWriteRowPage(t, s, root, webPage("11", mirrorRows(30000, links, "a")...), nil)
		}
		few := statementVMSteps(t, s, mirrorOffBoardLinkedSQL, root, "", polled(1))
		many := statementVMSteps(t, s, mirrorOffBoardLinkedSQL, root, "", polled(50))
		t.Logf("%s: %d steps polling 1 board, %d polling 50", membership, few, many)
		if growth := many - few; growth > 50*links {
			t.Errorf("%s: polling 49 more boards costs %d more steps over %d linked issues; want the list's build alone, under %d",
				membership, growth, links, 50*links)
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
	if cards, err := s.ListMirrorCards(root, ids[:mirrorMaxLookup]); err != nil || len(cards) != 0 {
		t.Fatalf("lookup at the bound = %v, %v", cards, err)
	}
}

// Every mirror read that takes a list of IDs reads a repeated ID once: its
// answer is the answer for the list with each ID once, at its first place.
func TestMirrorListReadsTakeEachIDOnce(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	ids := patchBoard(t, s, root, 30)
	onBoth := patchRows(30)[3]
	onBoth.OrderKey = "a"
	writeMirrorBoard(t, s, root, "11", []MirrorRow{onBoth})
	once, twice := []string{ids[3], ids[0], ids[1]}, []string{ids[3], ids[0], ids[3], ids[1], ids[0], ids[3]}
	given := slices.Clone(twice)
	for _, read := range []struct {
		name string
		read func(ids []string) (any, error)
	}{
		{"ReadMirrorBoardPatch", func(ids []string) (any, error) {
			patch, _, err := s.ReadMirrorBoardPatch(root, "10", MirrorRegroup{}, patchSections, ids, MirrorTotalsAt{})
			return patch, err
		}},
		{"ListMirrorIssueBoards", func(ids []string) (any, error) { return s.ListMirrorIssueBoards(root, ids, ids) }},
		{"ListMirrorCards", func(ids []string) (any, error) { return s.ListMirrorCards(root, ids) }},
		{"GetMirrorCatalog users", func([]string) (any, error) {
			return s.GetMirrorCatalog(root, []string{"acc-user1", "acc-user1"}, []string{"1"})
		}},
		{"GetMirrorCatalog statuses", func([]string) (any, error) {
			return s.GetMirrorCatalog(root, nil, []string{"3", "1", "3"})
		}},
	} {
		want, err := read.read(once)
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		got, err := read.read(twice)
		if err != nil {
			t.Fatalf("%s: %v", read.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s of a list with repeats = %+v\nwant %+v", read.name, got, want)
		}
	}
	if boards, err := s.ListMirrorIssueBoards(root, twice, twice); err != nil || len(boards) != 4 || boards[0] != (MirrorIssueBoard{ids[3], "10"}) {
		t.Errorf("boards of %v = %v, %v; want ids[3] on 10 and 11 first, then the others once", twice, boards, err)
	}
	if catalog, err := s.GetMirrorCatalog(root, []string{"acc-user1", "acc-user1"}, []string{"3", "1", "3"}); err != nil ||
		len(catalog.Users) != 1 || len(catalog.Statuses) != 2 {
		t.Errorf("catalog = %+v, %v; want one user and two statuses", catalog, err)
	}
	if !slices.Equal(twice, given) {
		t.Errorf("the reads changed their list to %v", twice)
	}
}
