package store

import (
	"cmp"
	"reflect"
	"slices"
	"testing"
)

// paceOf is board's pace read in a board view (ReadMirrorBoardView), by
// sprint.
func paceOf(t *testing.T, s *Store, root int64, board string, regroup MirrorRegroup) []MirrorSprintFinished {
	t.Helper()
	view, ok, err := s.ReadMirrorBoardView(root, board, regroup, theseSections(nil))
	if err != nil || !ok {
		t.Fatalf("board %s view: %v, %v", board, ok, err)
	}
	pace := view.Counts.Pace
	slices.SortFunc(pace, func(a, b MirrorSprintFinished) int { return cmp.Compare(a.SprintID, b.SprintID) })
	return pace
}

// paceFixture is a mirror whose board 10 has six closed sprints (41 to
// 46) and whose board 11 has a closed sprint with no completion time, with
// members whose status at each completion the pace's rule decides.
func paceFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	closed := func(id string, completed int64) MirrorSprint {
		return MirrorSprint{ID: id, OriginBoardID: "10", Name: "WEB Sprint " + id, State: MirrorSprintClosed, StartAt: 1, EndAt: 2, CompletedAt: completed}
	}
	st.Sprints = append(st.Sprints, closed("41", 50), closed("42", 300), closed("43", 400), closed("44", 450), closed("45", 500),
		closed("46", 600))
	mustReplaceStructure(t, s, root, st, true)
	other := webStructure()
	other.Board.ID, other.Board.EstimateUnit = "11", MirrorEstimateNone
	other.Sprints = append(other.Sprints, closed("40", 0), closed("46", 600))
	mustReplaceStructure(t, s, root, other, true)

	row := func(id, status string, estimate float64, sprints ...string) MirrorRow {
		r := sectionRow(id, id, status, estimate, sprints...)
		r.Card.UpdatedAt = 1_000
		return r
	}
	mustWriteRowPage(t, s, root, webPage("10",
		row("10001", "1", 3, "46"),           // done at 550, reopened at 700: done
		row("10002", "10001", 5, "46"),       // done at 650, after the completion: not done
		row("10003", "10001", -1, "46"),      // never changed, done now: done, no estimate
		row("10009", "3", 2, "46"),           // done at 550, in progress now by a change its history lacks: done
		row("10005", "6", 2, "45"),           // canceled
		row("10006", "10001", 1, "41"),       // done in a sprint before the last 5
		row("10008", "10001", 8, "21"),       // done in the active sprint
		row("10010", "10001", 2, "45", "21"), // carried on to the active sprint, history unread: not done in 45
		row("10011", "10001", 1, "44", "45"), // carried from 44 to 45, history unread: done in 45 alone
		row("10012", "10001", 1, "44", "46"), // history read, never changed: done in both
	), nil)
	mustWriteRowPage(t, s, root, webPage("11",
		row("10004", "10001", 4, "46"),
		row("10007", "10001", 1, "40"), // done in a sprint with no completion time
	), nil)
	histories := []MirrorHistory{
		{IssueID: "10001", Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: 550, From: "1", To: "10001"},
			{Kind: MirrorChangeStatus, At: 700, From: "10001", To: "1"},
		}},
		{IssueID: "10009", Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: 100, From: "1", To: "3"},
			{Kind: MirrorChangeStatus, At: 550, From: "3", To: "10001"},
		}},
		{IssueID: "10002", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 650, From: "1", To: "10001"}}},
		{IssueID: "10012"},
	}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, histories), MirrorCatalog{},
		mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	return s, root
}

// A board's pace is its last MirrorPaceSprints closed sprints by
// completion, a closed sprint with no completion time left out and one
// with no member on the board counted as zero. Each counts its members the
// board holds and those whose status when the sprint was completed was
// done: the last status change at or before the completion, else what the
// first change after it left, else the status now when the issue's
// history was read (it never changed) or this is the latest sprint, closed
// or open, the issue is in; a member carried on from the sprint whose
// history is unread is not done. A done member with no estimate counts
// and adds nothing; a board with no estimate unit sums to zero. A
// regrouping decides what counted as done, as it does for the sections.
func TestMirrorPaceCountsTheStatusAtEachCompletion(t *testing.T) {
	t.Parallel()
	s, root := paceFixture(t)

	plain := []MirrorSprintFinished{{SprintID: "20"}, {SprintID: "43"}, {SprintID: "44", Members: 2, Done: 1, Estimate: 1},
		{SprintID: "45", Members: 3, Done: 1, Estimate: 1}, {SprintID: "46", Members: 5, Done: 4, Estimate: 6}}
	if got := paceOf(t, s, root, "10", MirrorRegroup{}); !reflect.DeepEqual(got, plain) {
		t.Fatalf("pace = %+v\nwant %+v", got, plain)
	}
	regroup, err := NewMirrorRegroup(map[string]int{"6": MirrorGroupDone, "10001": MirrorGroupInProgress})
	if err != nil {
		t.Fatal(err)
	}
	regrouped := []MirrorSprintFinished{{SprintID: "20"}, {SprintID: "43"}, {SprintID: "44", Members: 2},
		{SprintID: "45", Members: 3, Done: 1, Estimate: 2}, {SprintID: "46", Members: 5}}
	if got := paceOf(t, s, root, "10", regroup); !reflect.DeepEqual(got, regrouped) {
		t.Fatalf("regrouped pace = %+v\nwant %+v", got, regrouped)
	}
	if got := paceOf(t, s, root, "11", MirrorRegroup{}); !reflect.DeepEqual(got, []MirrorSprintFinished{{SprintID: "20"}, {SprintID: "46", Members: 1, Done: 1}}) {
		t.Fatalf("pace of a board with no estimate unit = %+v; want sprint 20 empty and sprint 46's one done member summing to zero", got)
	}
}

// The pace rides on the section totals, so every write that moves it
// moves the totals revision: a member's row added and removed, its
// estimate, its history, and its status's group; a history of an issue in
// no closed sprint of the board moves neither.
func TestMirrorPaceMovesTheTotalsRevision(t *testing.T) {
	t.Parallel()
	s, _, root := newMirror(t)
	st := webStructure()
	mustReplaceStructure(t, s, root, st, true)
	three, five := 3.0, 5.0
	member := func(id, status string, estimate *float64, sprints ...string) MirrorRow {
		r := webRow(id, 1_000, id, sprints...)
		r.Card.StatusID, r.Estimate = status, estimate
		return r
	}
	read := int64(10)
	page := func(p MirrorRowPage) func() {
		return func() {
			read++
			p.BoardID, p.ReadAt = "10", mirrorReadAt(read)
			if _, err := s.WriteMirrorRows(root, currentPage(s, root, p)); err != nil {
				t.Fatal(err)
			}
		}
	}
	history := func(h MirrorHistory) func() {
		return func() {
			read++
			if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, []MirrorHistory{h}), MirrorCatalog{},
				mirrorReadOf(root, read)); err != nil {
				t.Fatal(err)
			}
		}
	}
	page(MirrorRowPage{Rows: []MirrorRow{member("10001", "10001", &three, "20"), member("10003", "1", nil, "21")}})()
	for _, step := range []struct {
		name  string
		write func()
		want  MirrorSprintFinished
		moves bool
	}{
		{"a member's row added", page(MirrorRowPage{Rows: []MirrorRow{member("10002", "1", nil, "20")}}),
			MirrorSprintFinished{SprintID: "20", Members: 2, Done: 1, Estimate: 3}, true},
		{"a member's estimate", page(MirrorRowPage{Rows: []MirrorRow{member("10001", "10001", &five, "20")}}),
			MirrorSprintFinished{SprintID: "20", Members: 2, Done: 1, Estimate: 5}, true},
		{"a member's history", history(MirrorHistory{IssueID: "10002", Changes: []MirrorChange{{Kind: MirrorChangeStatus, At: 900, From: "3", To: "10001"},
			{Kind: MirrorChangeStatus, At: 960, From: "10001", To: "1"}}}),
			MirrorSprintFinished{SprintID: "20", Members: 2, Done: 2, Estimate: 5}, true},
		{"a history of an issue in no closed sprint", history(MirrorHistory{IssueID: "10003", Changes: []MirrorChange{
			{Kind: MirrorChangeStatus, At: 900, From: "3", To: "1"}}}),
			MirrorSprintFinished{SprintID: "20", Members: 2, Done: 2, Estimate: 5}, false},
		{"a member's row removed", page(MirrorRowPage{Missing: []string{"10002"}}),
			MirrorSprintFinished{SprintID: "20", Members: 1, Done: 1, Estimate: 5}, true},
		{"its status's group", func() {
			read++
			st.Catalog.Statuses[2].Group = MirrorGroupInProgress
			st.ReadAt = mirrorReadAt(read)
			if _, err := s.ReplaceMirrorStructure(root, currentStructure(s, root, st)); err != nil {
				t.Fatal(err)
			}
		}, MirrorSprintFinished{SprintID: "20", Members: 1}, true},
	} {
		before, err := s.CountMirrorSections(root, "10", MirrorRegroup{})
		if err != nil {
			t.Fatal(err)
		}
		step.write()
		after, err := s.CountMirrorSections(root, "10", MirrorRegroup{})
		if err != nil {
			t.Fatal(err)
		}
		if moved := after.TotalsRevision != before.TotalsRevision; moved != step.moves ||
			!reflect.DeepEqual(after.Pace, []MirrorSprintFinished{step.want}) {
			t.Errorf("%s: totals revision %d -> %d, pace %+v; want it to move %v and pace %+v", step.name, before.TotalsRevision,
				after.TotalsRevision, after.Pace, step.moves, step.want)
		}
	}
}
