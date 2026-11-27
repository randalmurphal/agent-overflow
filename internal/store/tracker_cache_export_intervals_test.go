package store

import (
	"context"
	"reflect"
	"testing"
)

// intervalsWindow is the history window's start intervalsFixture's
// exports bind; its sprint 39 began earlier, at intervalsFrom, the
// earliest instant a count can ask.
const (
	intervalsWindow = 10_000
	intervalsFrom   = 9_000
)

// intervalsFixture is a sprint board (10, points) whose kept sprints are
// 39 (closed, begun before the window), 40 (closed) and 41 (active), and
// whose tickets each hold one case of the intervals' rules; the comment
// of each row says what its history holds. Every card was created at
// 1,000 and read at 20,000; the histories are complete from creation and
// final for that read.
func intervalsFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Sprints = []MirrorSprint{
		{ID: "39", OriginBoardID: "10", Name: "WEB Sprint 39", State: MirrorSprintClosed, StartAt: intervalsFrom, EndAt: 11_000, CompletedAt: 11_000},
		{ID: "40", OriginBoardID: "10", Name: "WEB Sprint 40", State: MirrorSprintClosed, StartAt: 12_000, EndAt: 14_000, CompletedAt: 14_000},
		{ID: "41", OriginBoardID: "10", Name: "WEB Sprint 41", State: MirrorSprintActive, StartAt: 14_000, EndAt: 16_000},
	}
	mustReplaceStructure(t, s, root, st, true)
	row := func(id, status string, estimate float64, sprints ...string) MirrorRow {
		r := sectionRow(id, id, status, estimate, sprints...)
		r.Card.CreatedAt, r.Card.UpdatedAt = 1_000, 20_000
		return r
	}
	mustWriteRowPage(t, s, root, webPage("10",
		row("10001", "3", 1),             // in progress since 5,000, before the window: its interval now alone
		row("10002", "10001", 1),         // done now, but its history ends in progress: when it got done is not known
		row("10003", "3", 1),             // in progress since 9,000, the earliest instant exactly
		row("10004", "10001", 1),         // started at 12,000 and done at 15,000
		row("10005", "3", 1, "41"),       // history unread: nothing is known
		row("10006", "1", 3),             // added to 41 at 14,500 and removed at 15,000
		row("10007", "1", 5, "40", "41"), // in 40 from 12,500, carried on to 41 at 14,000, re-estimated 2 to 5 at 13,000
		row("10008", "1", 1, "41"),       // in 41 with no sprint change: in it from its history's start
		row("10009", "1", 1),             // left 39 at 8,500, before the earliest instant
		row("10010", "1", 2, "41"),       // moved from 40 to 41 at 14,000, as Linear carries an issue over
		row("10011", "1", 2),             // its history puts it in 41 at 14,200, but it is in no sprint now
		row("10012", "3", 1),             // in progress at 4,000, to do at 6,000 and in progress again at 12,000
		row("10013", "1", 1),             // in progress from 9,500, after sprint 39 began and before the window, to 11,000
	), nil)
	two, five := 2.0, 5.0
	status := func(at int64, from, to string) MirrorChange {
		return MirrorChange{Kind: MirrorChangeStatus, At: at, From: from, To: to}
	}
	sprint := func(at int64, from, to []string) MirrorChange {
		return MirrorChange{Kind: MirrorChangeSprint, At: at, FromSprints: from, ToSprints: to}
	}
	none := []string{}
	histories := []MirrorHistory{
		{IssueID: "10001", Changes: []MirrorChange{status(2_000, "1", "3"), status(3_000, "3", "1"), status(5_000, "1", "3")}},
		{IssueID: "10002", Changes: []MirrorChange{status(12_000, "1", "3")}},
		{IssueID: "10003", Changes: []MirrorChange{status(intervalsFrom, "1", "3")}},
		{IssueID: "10004", Changes: []MirrorChange{status(12_000, "1", "3"), status(15_000, "3", "10001")}},
		{IssueID: "10006", Changes: []MirrorChange{sprint(14_500, none, []string{"41"}), sprint(15_000, []string{"41"}, none)}},
		{IssueID: "10007", Changes: []MirrorChange{sprint(12_500, none, []string{"40"}), sprint(14_000, []string{"40"}, []string{"40", "41"}),
			{Kind: MirrorChangeEstimate, At: 13_000, BoardID: "10", FromNumber: &two, ToNumber: &five}}},
		{IssueID: "10008"},
		{IssueID: "10009", Changes: []MirrorChange{sprint(8_500, []string{"39"}, none)}},
		{IssueID: "10010", Changes: []MirrorChange{sprint(12_000, none, []string{"40"}), sprint(14_000, []string{"40"}, []string{"41"})}},
		{IssueID: "10011", Changes: []MirrorChange{sprint(14_200, none, []string{"41"})}},
		{IssueID: "10012", Changes: []MirrorChange{status(4_000, "1", "3"), status(6_000, "3", "1"), status(12_000, "1", "3")}},
		{IssueID: "10013", Changes: []MirrorChange{status(9_500, "1", "3"), status(11_000, "3", "1")}},
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, histories), MirrorCatalog{},
		mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	return s, root
}

// The intervals reach back to the earliest instant a count can ask, the
// window's start or a kept sprint's begun before it, whichever is
// earlier, and hold only what the history and the card both show. A
// ticket in progress since before then has its interval now alone; one
// that began exactly then too. A history that ends in another status than
// the card's leaves the time after its last change unknown: no row. A
// ticket whose history is unread has none. A ticket removed from a sprint
// in the window has its stretch in it; one that left a kept sprint before
// the earliest instant has none; a Jira sprint stays in the set after it
// closes, so its stretch stays open; a Linear move from one cycle to the
// next closes the first at the move; an estimate change splits a
// stretch; a stretch the history leaves open while the ticket is in no
// sprint now has an end not known, so no row. The export reads the same
// from the file and from the copy's changes.
func TestTheIntervalsHoldWhatTheHistoryShowsInTheWindow(t *testing.T) {
	t.Parallel()
	s, root := intervalsFixture(t)
	wantStatus := []string{
		"10001|3|5000|-",
		"10002|1|1000|12000",
		"10003|3|9000|-",
		"10004|1|1000|12000", "10004|3|12000|15000", "10004|10001|15000|-",
		// The others' status now, from their history's start: they never
		// changed status.
		"10006|1|1000|-", "10007|1|1000|-", "10008|1|1000|-", "10009|1|1000|-", "10010|1|1000|-", "10011|1|1000|-",
		// What it held before 9,000 ends before a count can ask.
		"10012|1|6000|12000", "10012|3|12000|-",
		// Sprint 39 began at 9,000, so a count can ask from then.
		"10013|1|1000|9500", "10013|3|9500|11000", "10013|1|11000|-",
	}
	wantSprint := []string{
		"40|10007|2|12500|13000", "40|10007|5|13000|14000", "40|10007|5|14000|-", "40|10010|2|12000|14000",
		"41|10006|3|14500|15000", "41|10007|5|14000|-", "41|10008|1|1000|-", "41|10010|2|14000|-",
	}
	for _, relations := range [][]string{{"status_intervals", "sprint_intervals", "statuses"}, {"status_intervals", "sprint_intervals", "statuses", "changes"}} {
		conn := exportConn(t)
		if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, relations, intervalsWindow, "", CardShapeForTest); err != nil || !ok {
			t.Fatalf("export %v: %v, %v", relations, ok, err)
		}
		got := exportRows(t, conn, `SELECT ticket || '|' || status || '|' || from_at || '|' || coalesce(to_at, '-') FROM status_intervals ORDER BY ticket, from_at`)
		if !reflect.DeepEqual(got, wantStatus) {
			t.Errorf("export of %v: status intervals\n%v\nwant\n%v", relations, got, wantStatus)
		}
		got = exportRows(t, conn, `SELECT sprint || '|' || ticket || '|' || coalesce(CAST(points AS INTEGER), '-') || '|' || from_at || '|' || coalesce(to_at, '-')
		  FROM sprint_intervals ORDER BY sprint, ticket, from_at`)
		if !reflect.DeepEqual(got, wantSprint) {
			t.Errorf("export of %v: sprint intervals\n%v\nwant\n%v", relations, got, wantSprint)
		}
		if got := exportRows(t, conn, `SELECT DISTINCT status FROM status_intervals WHERE status NOT IN (SELECT id FROM statuses)`); len(got) > 0 {
			t.Errorf("export of %v: statuses %v of the intervals are not copied", relations, got)
		}
	}
}

// A board with no estimate holds no points in its sprint intervals, and
// a window that opens after every change the history holds keeps only
// what a count can ask from then: the interval now, and a stretch open
// now from the history's start.
func TestTheIntervalsFollowTheWindowAndTheEstimate(t *testing.T) {
	t.Parallel()
	s, root := intervalsFixture(t)
	if _, err := s.db.Exec(`UPDATE tracker_cache_boards SET estimate_unit = ? WHERE root_id = ? AND board_id = '10'`, MirrorEstimateNone, root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracker_cache_sprints SET start_at = 30000 WHERE root_id = ? AND board_id = '10'`, root); err != nil {
		t.Fatal(err)
	}
	conn := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", MirrorRegroup{}, []string{"status_intervals", "sprint_intervals"}, 16_000, "", CardShapeForTest); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	got := exportRows(t, conn, `SELECT ticket || '|' || status || '|' || from_at || '|' || coalesce(to_at, '-') FROM status_intervals ORDER BY ticket, from_at`)
	want := []string{"10001|3|5000|-", "10003|3|9000|-", "10004|10001|15000|-", "10006|1|1000|-", "10007|1|1000|-", "10008|1|1000|-",
		"10009|1|1000|-", "10010|1|1000|-", "10011|1|1000|-", "10012|3|12000|-",
		"10013|1|11000|-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("status intervals from 16,000\n%v\nwant\n%v", got, want)
	}
	got = exportRows(t, conn, `SELECT sprint || '|' || ticket || '|' || coalesce(CAST(points AS INTEGER), '-') || '|' || from_at || '|' || coalesce(to_at, '-')
	  FROM sprint_intervals ORDER BY sprint, ticket, from_at`)
	want = []string{"40|10007|-|14000|-", "41|10007|-|14000|-", "41|10008|-|1000|-", "41|10010|-|14000|-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sprint intervals from 16,000 on a board with no estimate\n%v\nwant\n%v", got, want)
	}
}
