package trackerapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// creditSprints closed sprints of creditTickets done tickets each are the
// credit scenario's board: a half year of two-week sprints.
const (
	creditSprints = 13
	creditTickets = 40
	// creditComments and creditWorklogs are each ticket's, fewer than
	// Jira's bulk fetch embeds, so no ticket's list is read by page.
	creditComments = 3
	creditWorklogs = 6
)

// finishedSprint adds sprint id to site, closed at completed, with
// creditTickets done tickets in it, each commented on by User 2 and
// logged on by User 1.
func finishedSprint(site *trackerfake.JiraSite, id int64, completed time.Time) {
	site.Sprints = append(site.Sprints, trackerfake.JiraSprint{ID: id, Board: 1, Name: fmt.Sprintf("Sprint %d", id), State: "closed",
		Start: completed.Add(-14 * 24 * time.Hour), End: completed, Complete: completed})
	for i := range creditTickets {
		n := id*100 + int64(i)
		is := trackerfake.JiraIssue{ID: fmt.Sprint(100000 + n), Key: fmt.Sprintf("WEB-%d", n), Project: "WEB", Type: "10002",
			Summary: fmt.Sprintf("Ticket %d", n), Sprints: []int64{id}, Assignee: "acc-user1", Reporter: "acc-user1", Status: "10002",
			Created: completed.Add(-10 * 24 * time.Hour), Updated: completed.Add(-time.Hour)}
		for c := range creditComments {
			is.Comments = append(is.Comments, trackerfake.JiraComment{ID: fmt.Sprintf("%d%d", n, c), Author: "acc-user2",
				Body: jiraDoc("Reviewed the change and left notes on the edge cases."), Created: completed.Add(-time.Duration(c+2) * time.Hour)})
		}
		for w := range creditWorklogs {
			is.Worklogs = append(is.Worklogs, trackerfake.JiraWorklog{ID: fmt.Sprintf("%d%d", n, w), Author: "acc-user1",
				Started: completed.Add(-time.Duration(w+1) * 24 * time.Hour), TimeSpent: 3600})
		}
		site.Issues = append(site.Issues, is)
	}
}

// creditScenario is a Jira board of creditSprints closed sprints inside
// the history window, its project counting credit, and the board read;
// the linked loop (runLinked) reads its history.
func creditScenario(t *testing.T) (*fakeScenario, Connection, Scope) {
	t.Helper()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		for k := range creditSprints {
			finishedSprint(&site, int64(101+k), at.Add(-time.Duration(7+14*k)*24*time.Hour))
		}
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	s.countCredit(t, conn.ID, true)
	s.readBoard(t, scope)
	return s, conn, scope
}

// readBoard reads scope's structure and rows, as a subscribe does.
func (s *fakeScenario) readBoard(t *testing.T, scope Scope) {
	t.Helper()
	for _, unit := range []func(context.Context, Scope, Urgency) (Outcome, error){s.svc.tickStructure, s.svc.tickReconcile, s.svc.tickDelta} {
		if out, err := unit(t.Context(), scope, Background); err != nil || out.Failure != nil || out.More() {
			t.Fatalf("board read: %+v, %v", out, err)
		}
	}
}

// evidenceRequests are the requests since seq that read evidence: bulk
// fetches of comments, and comment and worklog lists.
func (s *fakeScenario) evidenceRequests(since int) []string {
	var out []string
	for _, r := range s.e.Requests(since).Requests {
		switch {
		case r.Route == "POST rest/api/3/issue/bulkfetch" && strings.Contains(string(r.Body), `"comment"`),
			r.Route == "GET rest/api/3/issue/{issue}/comment", r.Route == "GET rest/api/3/issue/{issue}/worklog":
			out = append(out, r.Route)
		}
	}
	return out
}

// storeCount answers query, a count, on the fixture's store file.
func (f *fixture) storeCount(t *testing.T, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+f.file+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, errors.Join(err, db.Close()))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return n
}

// The first use of credit on a board of a half year's finished tickets
// reads each ticket's comments and worklogs once, in bulk fetches of
// evidenceBatch; an idle round reads nothing, and a newly finished sprint
// reads its tickets alone.
func TestFakeEvidenceReadsEachFinishedTicketOnce(t *testing.T) {
	t.Parallel()
	s, conn, scope := creditScenario(t)
	since := s.lastSeq()
	s.runLinked(t, conn)
	first := creditSprints * creditTickets
	if got := s.evidenceRequests(since); len(got) != (first+evidenceBatch-1)/evidenceBatch {
		t.Fatalf("the first use read evidence by %d requests: %v", len(got), got)
	}
	if n := s.storeCount(t, `SELECT count(*) FROM tracker_cache_issue_acts`); n != first*(creditComments+creditWorklogs) {
		t.Fatalf("%d acts stored", n)
	}
	if n := s.storeCount(t, `SELECT count(*) FROM tracker_cache_issue_act_reads`); n != 2*first {
		t.Fatalf("%d act receipts stored", n)
	}
	root := must[int64](t)(rootOf(s, conn))
	if due, more, err := s.st.ListMirrorActsDue(root, []store.MirrorActsBoard{{BoardID: string(scope.Board)}}, true, 0); err != nil || len(due) != 0 || more {
		t.Fatalf("due after the first use: %v, %v, %v", due, more, err)
	}

	since = s.lastSeq()
	s.runLinked(t, conn)
	if got := s.evidenceRequests(since); len(got) != 0 {
		t.Fatalf("an idle round read evidence: %v", got)
	}

	world := s.e.Snapshot()
	world.Jira[0].Token = "fake-jira-token-1"
	finishedSprint(&world.Jira[0], 200, s.clock.Now().Add(-time.Minute))
	if err := s.e.Seed(world); err != nil {
		t.Fatal(err)
	}
	s.readBoard(t, scope)
	since = s.lastSeq()
	s.runLinked(t, conn)
	if got := s.evidenceRequests(since); len(got) != 1 {
		t.Fatalf("a finished sprint read evidence by %d requests: %v", len(got), got)
	}
	if n := s.storeCount(t, `SELECT count(*) FROM tracker_cache_issue_act_reads`); n != 2*(first+creditTickets) {
		t.Fatalf("%d act receipts stored", n)
	}
}
