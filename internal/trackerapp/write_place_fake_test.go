package trackerapp

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/tracker"
)

// lagScenario is the Jira scenario with a search index that shows a new
// issue 30 s after its create (measured: 0.6 s, 0.7 to 1.6 s and 23 s),
// board 1 synced.
func lagScenario(t *testing.T) (*fakeScenario, Scope) {
	t.Helper()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		site := jiraSeed(at)
		site.IndexLag = 30 * time.Second
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{site}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	s.syncBoard(t, scope)
	s.clock.advance(time.Minute)
	return s, scope
}

// boardRowIDs are the issues the mirror shows on scope's board, in every
// column.
func (s *fakeScenario) boardRowIDs(t *testing.T, scope Scope) []string {
	t.Helper()
	var out []string
	for col := range 4 {
		page := must[SectionPage](t)(s.svc.SectionRows(scope, StatusGroups{}, Section{Kind: SectionColumn, Column: col}, nil))
		for _, r := range page.Rows {
			out = append(out, r.Card.ID)
		}
	}
	return out
}

// listed reports whether the board's own list holds issue now.
func (s *fakeScenario) listed(t *testing.T, scope Scope, issue tracker.IssueID) bool {
	t.Helper()
	l := must[tracker.Listing](t)(s.provider(scope.Connection).BoardIssues(t.Context(), scope.Board, time.Now().Add(-HistoryWindow)))
	return slices.Contains(l.IDs, issue)
}

// The issue a person creates is on the board when the create returns,
// while the board's list still misses it; an edit of it and a reconcile
// made before the index catches up keep the row; and once the list holds
// it, the delta finds the row it delivers identical and tells nothing.
func TestFakeJiraCreateShowsAtOnce(t *testing.T) {
	t.Parallel()
	s, scope := lagScenario(t)
	v := must[WriteView](t)(s.svc.Create(t.Context(), scope.Connection, CreateRequest{Board: "1", Project: "10000", Type: "10002", Title: "New task"}))
	created := tracker.IssueID(v.Created)
	if created == "" || len(v.Stale) > 0 || v.NotOnBoard != "" {
		t.Fatalf("view %+v", v)
	}
	if !slices.Contains(s.boardRowIDs(t, scope), string(created)) {
		t.Fatal("the new issue is not on the board when the create returns")
	}
	if s.listed(t, scope, created) {
		t.Fatal("the fake's list shows the new issue at once; the test needs it to lag")
	}
	edit := must[WriteView](t)(s.svc.Edit(t.Context(), scope.Connection, EditRequest{Issue: string(created), Fields: []FieldEditInput{{Field: "title", Typed: []string{"New task, renamed"}}}}))
	if len(edit.Stale) > 0 || !slices.Contains(s.boardRowIDs(t, scope), string(created)) {
		t.Fatalf("an edit before the index caught up removed the row: %+v", edit)
	}
	out := must[Outcome](t)(s.svc.tickReconcile(t.Context(), scope, Background))
	if out.Failure != nil || len(out.Changes().Removed) > 0 || !slices.Contains(s.boardRowIDs(t, scope), string(created)) {
		t.Fatalf("a reconcile before the index caught up removed the row: %+v", out)
	}
	s.clock.advance(30 * time.Second)
	if !s.listed(t, scope, created) {
		t.Fatal("the fake's list does not show the new issue once indexed")
	}
	out = must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
	if out.Failure != nil || !out.Changes().Empty() {
		t.Fatalf("the delta after the index caught up changed %+v", out.Changes())
	}
	s.compare(t, scope, s.jiraCards())
}

// An epic created from a scrum board is not on that board, as Jira's own
// board shows it: no row, and the write says which board leaves it out.
func TestFakeJiraEpicIsNotOnTheScrumBoard(t *testing.T) {
	t.Parallel()
	s, scope := lagScenario(t)
	v := must[WriteView](t)(s.svc.Create(t.Context(), scope.Connection, CreateRequest{Board: "1", Project: "10000", Type: "10000", Title: "An epic"}))
	if v.Created == "" || v.NotOnBoard != "1" || len(v.Stale) > 0 || slices.Contains(s.boardRowIDs(t, scope), v.Created) {
		t.Fatalf("view %+v, rows %v", v, s.boardRowIDs(t, scope))
	}
}

// With no answer from the match, nothing is guessed: the row is owed the
// board's rows read, which fails with the match and leaves the row
// stale, and the board's delta brings it once the list holds it.
func TestFakeJiraCreateWithNoMatch(t *testing.T) {
	t.Parallel()
	s, scope := lagScenario(t)
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	fault := trackerfake.Fault{Origin: origin, Route: "POST rest/api/3/jql/match", Kind: trackerfake.FaultRefuse, Status: 400,
		Body: json.RawMessage(`{"errorMessages":["The match is unavailable."]}`)}
	if err := s.e.AddFault(fault); err != nil {
		t.Fatal(err)
	}
	v := must[WriteView](t)(s.svc.Create(t.Context(), scope.Connection, CreateRequest{Board: "1", Project: "10000", Type: "10002", Title: "New task"}))
	if v.Created == "" || v.NotOnBoard != "" || len(v.Stale) != 1 || slices.Contains(s.boardRowIDs(t, scope), v.Created) {
		t.Fatalf("view %+v", v)
	}
	if r := v.Stale[0].Resources; len(r) != 1 || r[0].Kind != "row" || r[0].Issue != v.Created || r[0].Board != "1" {
		t.Fatalf("stale %+v", v.Stale)
	}
	s.e.ClearFaults(origin, fault.Route)
	s.clock.advance(30 * time.Second)
	out := must[Outcome](t)(s.svc.tickDelta(t.Context(), scope, Background))
	if !slices.Contains(out.Changes().Rows, tracker.IssueID(v.Created)) {
		t.Fatalf("the delta after the index caught up did not bring the row: %+v", out.Changes())
	}
}

// A create's requests and store statements through the fake: the create
// (its create metadata and the create), then one match and one bulk fetch
// that place and read the new row, where the rows read through the board's
// list was one request; with no match, the board's rows read follows and
// confirms its miss with a match that fails too. The second create reads
// no create metadata: the client keeps the screen the first read.
func TestFakeJiraCreateCosts(t *testing.T) {
	t.Parallel()
	s, scope := lagScenario(t)
	create := func(t *testing.T) {
		if v := must[WriteView](t)(s.svc.Create(t.Context(), scope.Connection, CreateRequest{Board: "1", Project: "10000", Type: "10002", Title: "New task"})); v.Created == "" {
			t.Fatalf("view %+v", v)
		}
	}
	s.measureTicks(t, []tickCost{{name: "create, placed", requests: 4, stmts: 2, run: create}})
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: "POST rest/api/3/jql/match", Kind: trackerfake.FaultRefuse, Status: 400,
		Body: json.RawMessage(`{"errorMessages":["The match is unavailable."]}`)}); err != nil {
		t.Fatal(err)
	}
	s.measureTicks(t, []tickCost{{name: "create, no match", requests: 4, stmts: 1, run: create}})
}
