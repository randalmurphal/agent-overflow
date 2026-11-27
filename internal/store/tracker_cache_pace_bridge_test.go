package store

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// PaceFixtureForTest and PaceForTest hand the pace's fixture and the
// Backlog's reading of it to the external test that holds the dashboard's
// pace to it (tracker_cache_dashboard_pace_test.go), which imports
// trackerdash and so cannot be in this package.
func PaceFixtureForTest(t *testing.T) (*Store, int64) { return paceFixture(t) }

func PaceForTest(t *testing.T, s *Store, root int64, board string, regroup MirrorRegroup) []MirrorSprintFinished {
	return paceOf(t, s, root, board, regroup)
}

// ScaleMirrorForTest hands the scale fixture of one board of issues
// issues (benchChanges changes each, parented as a scrum board's:
// benchParented), its root and its board, to the
// external test that measures a dashboard over it
// (tracker_cache_dashboard_scale_test.go).
//
// With recent above zero it is the fixture with real times instead
// (benchRecentBoard), that percentage of its issues changed inside the
// window that starts at BenchWindowForTest, the others long before it; a
// count over it runs at BenchNowForTest.
func ScaleMirrorForTest(tb testing.TB, issues, recent int) (*Store, int64, string) {
	s, root, _ := openSizedMirror(tb, benchMirrorSize{boards: 1, issues: issues, parents: true, recent: recent})
	return s, root, benchBoardID(0)
}

// DashboardStatusFixtureForTest is a sprint board (10, its active sprint
// 21) and a board without sprints (11), each holding one ticket of every
// kind of status a count tells apart, created at created: Backlog (status
// 2), unclassified (99, which the catalog does not hold), left out by the
// regrouping it returns (7, In Review, in progress in the catalog), in
// progress, done and canceled. Board 10's are WEB-1 to WEB-6 in that order,
// board 11's WEB-11 to WEB-16, the estimate of each its number in the list.
func DashboardStatusFixtureForTest(t *testing.T, created int64) (*Store, int64, MirrorRegroup) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Catalog.Statuses = append(st.Catalog.Statuses,
		MirrorStatus{ID: "2", Name: "Backlog", Group: MirrorGroupBacklog}, MirrorStatus{ID: "7", Name: "In Review", Group: MirrorGroupInProgress})
	mustReplaceStructure(t, s, root, st, true)
	weeks := webStructure()
	weeks.Board.ID, weeks.Board.PlansSprints, weeks.Sprints = "11", false, nil
	mustReplaceStructure(t, s, root, weeks, true)
	for _, b := range []struct{ board, first string }{{"10", "1"}, {"11", "11"}} {
		var rows []MirrorRow
		for i, status := range []string{"2", "99", "7", "3", "10001", "6"} {
			id := fmt.Sprint(atoi(t, b.first) + i)
			r := sectionRow(id, id, status, float64(i+1), "21")
			if b.board == "11" {
				r.Card.Sprints = nil
			}
			r.Card.CreatedAt = created
			rows = append(rows, r)
		}
		mustWriteRowPage(t, s, root, webPage(b.board, rows...), nil)
	}
	regroup, err := NewMirrorRegroup(map[string]int{"7": MirrorGroupLeftOut})
	if err != nil {
		t.Fatal(err)
	}
	return s, root, regroup
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// DashboardHistoryFixtureForTest is a board without sprints (11) of four
// done tickets created at created, with histories as each tracker writes
// them, a day being day: WEB-21 a Linear history, complete from 3 minutes
// after creation, moved from To Do into progress a day after creation
// and done two days later; WEB-22 a Linear one whose first change, two
// days after creation, is from In Progress into done (it was made in
// progress within the fold); WEB-23 a Jira one, complete from creation,
// into progress after a day and done the next; WEB-24 a Jira one of no
// changes, made in done and resolved two days after creation.
func DashboardHistoryFixtureForTest(t *testing.T, created, day int64) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	weeks := webStructure()
	weeks.Board.ID, weeks.Board.PlansSprints, weeks.Sprints = "11", false, nil
	mustReplaceStructure(t, s, root, weeks, true)
	var rows []MirrorRow
	for _, id := range []string{"21", "22", "23", "24"} {
		r := sectionRow(id, id, "10001", 1)
		r.Card.CreatedAt, r.Card.UpdatedAt = created, created+3*day
		if id == "24" {
			r.Card.ResolvedAt = created + 2*day
		}
		rows = append(rows, r)
	}
	mustWriteRowPage(t, s, root, webPage("11", rows...), nil)
	linear := created + int64(3*time.Minute/time.Millisecond)
	histories := []MirrorHistory{
		{IssueID: "21", CompleteFrom: linear, Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: created + day, From: "1", To: "3"},
			{Kind: MirrorChangeStatus, At: created + 3*day, From: "3", To: "10001"},
		}},
		{IssueID: "22", CompleteFrom: linear, Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: created + 2*day, From: "3", To: "10001"}}},
		{IssueID: "23", Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: created + day, From: "1", To: "3"},
			{Kind: MirrorChangeStatus, At: created + 2*day, From: "3", To: "10001"},
		}},
		{IssueID: "24"},
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, histories), MirrorCatalog{},
		mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	return s, root
}

// IntervalsFixtureForTest is intervalsFixture, with the history window's
// start its exports bind.
func IntervalsFixtureForTest(t *testing.T) (*Store, int64, int64) {
	s, root := intervalsFixture(t)
	return s, root, intervalsWindow
}
