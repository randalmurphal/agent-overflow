package store

import (
	"context"
	"database/sql/driver"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// creditRules is one snapshot from the start: 10% of a finished ticket's
// points to a reviewer who commented and logged time, a point for every 4
// hours of support; with approved, approvals count too.
func creditRules(approved bool) []MirrorCreditRule {
	return []MirrorCreditRule{{ReviewShare: 10, ReviewNeed: 2, Commented: true, Approved: approved, LoggedTime: true, HoursPerPoint: 4}}
}

// creditRegroup is board 10's support (each row of it with no estimate)
// and rules as its project counts credit.
func creditRegroup(t *testing.T, rules []MirrorCreditRule) MirrorRegroup {
	t.Helper()
	r, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Boards: []string{"10"}})
	if err == nil {
		r, err = r.WithCredit(rules)
	}
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// act is one act of issue's source by person at at.
func act(id, person string, at, seconds int64) MirrorAct {
	return MirrorAct{ID: id, Person: person, At: at, Seconds: seconds}
}

// creditFixture is board 10 with closed sprint 20 (100 to its completion
// at 950) and active sprint 21 (from 1000):
//   - 10001, 5 points, done in 20 at 800 while User 1's; reassigned to
//     User 2 at 850. User 2 commented (a reviewer now, not then), User 3
//     commented and logged time, User 4 commented after the completion
//     and User 1 commented.
//   - 10002, 3 points, done in 20, its evidence never read.
//   - 10003, a support ticket in no sprint: User 2 logged 2 hours at 500,
//     in 20's dates, and 2 at 960, between the sprints.
//   - 10004, a support ticket updated at 990 whose worklogs are not read.
//   - 10005, 2 points, done now in 21, User 3 only commented.
func creditFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	s, _, root := newMirror(t)
	st := webStructure()
	st.Catalog.Users = append(st.Catalog.Users, MirrorUser{ID: "acc-user2", Name: "User 2", Data: []byte(`{}`)},
		MirrorUser{ID: "acc-user3", Name: "User 3", Data: []byte(`{}`)}, MirrorUser{ID: "acc-user4", Name: "User 4", Data: []byte(`{}`)})
	mustReplaceStructure(t, s, root, st, true)
	row := func(id, status string, estimate float64, updated int64, sprints ...string) MirrorRow {
		r := sectionRow(id, id, status, estimate, sprints...)
		r.Card.UpdatedAt = updated
		return r
	}
	first := row("10001", "10001", 5, 900, "20")
	first.Card.AssigneeID = "acc-user2"
	mustWriteRowPage(t, s, root, webPage("10", first, row("10002", "10001", 3, 900, "20"), row("10003", "1", -1, 970),
		row("10004", "1", -1, 990), row("10005", "10001", 2, 1500, "21")), nil)
	histories := []MirrorHistory{{IssueID: "10001", Changes: []MirrorChange{
		{Kind: MirrorChangeStatus, At: 800, From: "1", To: "10001"},
		{Kind: MirrorChangeAssignee, At: 850, From: "acc-user1", To: "acc-user2"},
	}}}
	if _, _, err := s.ReplaceMirrorHistories(root, currentSweep(s, root), dueHistories(s, root, histories), MirrorCatalog{},
		mirrorReadOf(root, 10)); err != nil {
		t.Fatal(err)
	}
	replaceActs(t, s, root, 20, nil,
		MirrorIssueActs{IssueID: "10001", Source: MirrorActComment, ReadFor: 900, Acts: []MirrorAct{act("c1", "acc-user2", 500, 0),
			act("c2", "acc-user3", 600, 0), act("c3", "acc-user4", 960, 0), act("c4", "acc-user1", 700, 0)}},
		MirrorIssueActs{IssueID: "10001", Source: MirrorActWorklog, ReadFor: 900, Acts: []MirrorAct{act("w1", "acc-user3", 700, 3600)}},
		MirrorIssueActs{IssueID: "10003", Source: MirrorActWorklog, ReadFor: 970, Acts: []MirrorAct{act("w2", "acc-user2", 500, 7200),
			act("w3", "acc-user2", 960, 7200)}},
		MirrorIssueActs{IssueID: "10005", Source: MirrorActComment, ReadFor: 1500, Acts: []MirrorAct{act("c5", "acc-user3", 1500, 0)}},
		MirrorIssueActs{IssueID: "10005", Source: MirrorActWorklog, ReadFor: 1500})
	return s, root
}

// creditOf is board 10's credit relation as an export with regroup
// fills it, one line a row in the relation's columns, and the people it
// names.
func creditOf(t *testing.T, s *Store, root int64, regroup MirrorRegroup) (rows, people []string) {
	t.Helper()
	conn := exportConn(t)
	if _, ok, err := s.ExportMirrorBoard(context.Background(), conn, root, "10", regroup, []string{"credit"}, 0, CardShapeForTest); err != nil || !ok {
		t.Fatalf("export: %v, %v", ok, err)
	}
	rows = exportRows(t, conn, `SELECT concat_ws(' ', coalesce(sprint, '-'), coalesce(at, '-'), ticket, kind, coalesce(person, '-'),
	    coalesce(round(value, 3), '-'), coalesce(round(share, 3), '-'), coalesce(points, '-'), coalesce(review_share, '-'), coalesce(review_need, '-'),
	    coalesce(did, '-'), coalesce(qualified, '-'), coalesce(round(hours, 3), '-'), coalesce(hours_per_point, '-'), coalesce(note, '-'))
	  FROM main.credit ORDER BY sprint, ticket, kind, person`)
	people = exportRows(t, conn, `SELECT id FROM main.people ORDER BY id`)
	return rows, people
}

// Credit counts each finished ticket for its assignee when it moved to
// done, each other person who acted on it by the sprint's end as a
// reviewer (counted when they did the kinds the rule needs, 0 otherwise),
// a ticket whose evidence is not read as one review row not counted, and
// support time by the sprint whose dates hold its start, else the next to
// start, a support ticket whose worklogs are not read as one row not
// counted. Its people are named.
func TestMirrorCreditCountsFinishedReviewAndSupport(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	rows, people := creditOf(t, s, root, creditRegroup(t, creditRules(false)))
	want := []string{
		"20 800 10001 finished acc-user1 5.0 1.0 5.0 - - - - - - -",
		"20 800 10001 review acc-user2 0.0 0.0 5.0 10 2 commented 0 - - -",
		"20 800 10001 review acc-user3 0.5 0.1 5.0 10 2 commented, logged time 1 - - -",
		"20 - 10002 finished acc-user1 3.0 1.0 3.0 - - - - - - -",
		"20 - 10002 review - - - 3.0 10 2 - - - - who worked on it is not read yet",
		"20 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 - 10003 support acc-user2 0.5 0.5 - - - - - 2.0 4 -",
		"21 990 10004 support - - - - - - - - - - who worked on it is not read yet",
		"21 - 10005 finished acc-user1 2.0 1.0 2.0 - - - - - - -",
		"21 - 10005 review acc-user3 0.0 0.0 2.0 10 2 commented 0 - - -",
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("credit\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
	}
	if !reflect.DeepEqual(people, []string{"acc-user1", "acc-user2", "acc-user3"}) {
		t.Fatalf("people %v; want the assignees and the people credit names", people)
	}
}

// With credit, the Backlog's pace adds each sprint's credit to what it
// finished: the review and support credit in the board's estimate and in
// tickets, and the tickets whose credit is not read; what it finished is
// what the pace without credit counts.
func TestMirrorPaceCountsCredit(t *testing.T) {
	t.Parallel()
	s, root := creditFixture(t)
	want := []MirrorSprintFinished{{SprintID: "20", Members: 2, Done: 2, Estimate: 8, Review: 0.5, ReviewShares: 0.1, Support: 0.5, SupportShares: 0.5,
		Unread: 1}}
	if got := paceOf(t, s, root, "10", creditRegroup(t, creditRules(false))); !reflect.DeepEqual(got, want) {
		t.Fatalf("pace = %+v\nwant %+v", got, want)
	}
	ps, proot := paceFixture(t)
	for _, board := range []string{"10", "11"} {
		plain := paceOf(t, ps, proot, board, MirrorRegroup{})
		credited := paceOf(t, ps, proot, board, creditRegroup(t, creditRules(false)))
		for i := range credited {
			credited[i].Unread = 0
		}
		if !reflect.DeepEqual(credited, plain) {
			t.Fatalf("board %s: pace with credit = %+v\nwithout = %+v; want what each sprint finished alike", board, credited, plain)
		}
	}
}

// A project that counts no credit reads the pace with today's statement,
// a support rule or not, and its pace holds no credit. Not parallel: it
// records the statements a read runs.
func TestAPaceWithoutCreditRunsTodaysStatement(t *testing.T) {
	s, root := creditFixture(t)
	support, err := MirrorRegroup{}.WithSupport(MirrorSupportRule{Boards: []string{"10"}})
	if err != nil {
		t.Fatal(err)
	}
	none, err := support.WithCredit(nil)
	if err != nil {
		t.Fatal(err)
	}
	checked := cachedStmtRan
	t.Cleanup(func() { cachedStmtRan = checked })
	for name, regroup := range map[string]MirrorRegroup{"no regrouping": {}, "support": support, "credit cleared": none} {
		var ran []string
		cachedStmtRan = func(stmt driver.Stmt, query string) {
			checked(stmt, query)
			ran = append(ran, query)
		}
		pace, ok, err := s.GetMirrorBoardPace(root, "10", regroup)
		cachedStmtRan = checked
		if err != nil || !ok {
			t.Fatalf("%s: pace %v, %v", name, ok, err)
		}
		if !slices.Contains(ran, mirrorPaceSQL) || slices.ContainsFunc(ran, func(q string) bool { return strings.Contains(q, "cr_sp") }) {
			t.Fatalf("%s: ran\n%s\nwant the pace's statement without credit", name, strings.Join(ran, "\n---\n"))
		}
		if want := []MirrorSprintFinished{{SprintID: "20", Members: 2, Done: 2, Estimate: 8}}; !reflect.DeepEqual(pace.Finished, want) {
			t.Fatalf("%s: pace = %+v; want %+v", name, pace.Finished, want)
		}
	}
}
