package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/store"
)

// findFixture is a project thread with the tracker tools over the fake
// trackers, and the call that asks ticket_find.
type findFixture struct {
	f        *trackerBoardFixture
	endpoint string
}

func newFindFixture(t *testing.T, setup trackerBoardSetup) *findFixture {
	t.Helper()
	f := newTrackerBoardFixtureOn(t, setup)
	if setup.linear != nil {
		f.addLinear(t, *setup.linear)
	}
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread := store.Thread{ID: uuid.NewString(), ProjectID: f.project, Provider: string(provider.Claude), WorkspacePath: t.TempDir()}
	if err := f.a.store.CreateThread(thread); err != nil {
		t.Fatal(err)
	}
	token := uuid.NewString()
	f.a.sessionManager().put(thread.ID, session{Token: token, Provider: thread.Provider})
	return &findFixture{f: f, endpoint: trackerMCPEndpoint(t, f.a, thread, token)}
}

// lastSeq is the fake's latest request.
func (ff *findFixture) lastSeq() int {
	l := ff.f.e.Requests(0).Requests
	if len(l) == 0 {
		return 0
	}
	return l[len(l)-1].Seq
}

// find calls ticket_find with args and answers the keys it found, in
// order, and the routes of the requests the call made.
func (ff *findFixture) find(t *testing.T, args map[string]any) (keys, routes []string) {
	t.Helper()
	since := ff.lastSeq()
	text, isError := trackerMCPStreamedCall(t, ff.f.a, ff.endpoint, "ticket_find", args)
	if isError {
		t.Fatalf("ticket_find %v = %s", args, text)
	}
	var page struct {
		Tickets []struct{ Key string }
		Next    string
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil {
		t.Fatalf("ticket_find %v = %s: %v", args, text, err)
	}
	for _, tk := range page.Tickets {
		keys = append(keys, tk.Key)
	}
	for _, r := range ff.f.e.Requests(since).Requests {
		routes = append(routes, r.Route)
	}
	return keys, routes
}

// refused calls ticket_find with args and answers its refusal.
func (ff *findFixture) refused(t *testing.T, args map[string]any) string {
	t.Helper()
	text, isError := trackerMCPStreamedCall(t, ff.f.a, ff.endpoint, "ticket_find", args)
	if !isError {
		t.Fatalf("ticket_find %v answered %s, want a refusal", args, text)
	}
	return text
}

// findSite is trackerBoardSite with an In Review status (in progress, by
// Jira's category), a second person and a closed sprint, and six issues
// of User 1's and User 2's, updated in key order.
func findSite() trackerfake.JiraSite {
	s := trackerBoardSite()
	day := s.Issues[0].Created
	s.Statuses = []trackerfake.JiraStatus{
		{ID: "10000", Name: "To Do", Category: "new"}, {ID: "10001", Name: "In Progress", Category: "indeterminate"},
		{ID: "10002", Name: "Done", Category: "done"}, {ID: "10003", Name: "In Review", Category: "indeterminate"},
	}
	s.Users = append(s.Users, trackerfake.JiraUser{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com", TimeZone: "UTC"})
	s.Sprints = append(s.Sprints, trackerfake.JiraSprint{ID: 2, Board: 1, Name: "Sprint 0", State: "closed",
		Start: day.Add(-28 * 24 * time.Hour), End: day.Add(-14 * 24 * time.Hour), Complete: day.Add(-14 * 24 * time.Hour)})
	issue := func(n int, summary, status, assignee string, sprints ...int64) trackerfake.JiraIssue {
		at := day.Add(time.Duration(n) * time.Minute)
		return trackerfake.JiraIssue{ID: fmt.Sprint(10100 + n), Key: fmt.Sprintf("WEB-%d", n), Project: "WEB", Type: "10002", Summary: summary,
			Status: status, Assignee: assignee, Sprints: sprints, Created: day, Updated: at}
	}
	s.Issues = []trackerfake.JiraIssue{
		issue(1, "First story", "10001", "acc-user1", 1),
		issue(2, "Second task", "10000", "acc-user1", 1),
		issue(3, "Backlog task", "10000", "acc-user1"),
		issue(4, "Their story", "10001", "acc-user2", 1),
		issue(5, "Reviewed story", "10003", "acc-user1", 2),
		issue(6, "Finished story", "10002", "acc-user1", 1),
	}
	return s
}

// oneJiraSearch reports whether routes are one search, after the field
// catalog a cold client reads first.
func oneJiraSearch(routes []string) bool {
	routes = slices.DeleteFunc(slices.Clone(routes), func(r string) bool { return r == "GET rest/api/3/field" })
	return slices.Equal(routes, []string{"POST rest/api/3/search/jql"})
}

// An agent finds tickets on Jira by who has them, their status or its
// group, and the active sprint, alone, together or with text, with one
// search each.
func TestTrackerAgentFindsJiraTicketsByFilters(t *testing.T) {
	t.Parallel()
	ff := newFindFixture(t, trackerBoardSetup{site: findSite()})
	for _, c := range []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"mine": true}, []string{"WEB-6", "WEB-5", "WEB-3", "WEB-2", "WEB-1"}},
		{map[string]any{"mine": true, "sprint": "active"}, []string{"WEB-6", "WEB-2", "WEB-1"}},
		{map[string]any{"mine": true, "status": "in progress"}, []string{"WEB-5", "WEB-1"}},
		{map[string]any{"mine": true, "status": "In Progress", "sprint": "active"}, []string{"WEB-1"}},
		{map[string]any{"status": "not done", "sprint": "active"}, []string{"WEB-4", "WEB-2", "WEB-1"}},
		{map[string]any{"status": "done"}, []string{"WEB-6"}},
		{map[string]any{"status": "to do", "mine": true}, []string{"WEB-3", "WEB-2"}},
		{map[string]any{"status": "in review"}, []string{"WEB-5"}},
		{map[string]any{"text": "task", "mine": true}, []string{"WEB-3", "WEB-2"}},
		{map[string]any{"text": "story", "sprint": "active", "status": "not done"}, []string{"WEB-4", "WEB-1"}},
	} {
		keys, routes := ff.find(t, c.args)
		if !slices.Equal(keys, c.want) {
			t.Errorf("ticket_find %v = %v, want %v", c.args, keys, c.want)
		}
		if !oneJiraSearch(routes) {
			t.Errorf("ticket_find %v sent %v, want one search", c.args, routes)
		}
	}
}

// A status Jira does not have is refused naming it, and the connection
// keeps no problem from it, since Jira answered; a group Jira does not
// have is refused before anything is sent; a call with nothing to search
// for, or a sprint other than active, is refused saying what it takes.
func TestTrackerAgentFindRefusesWhatJiraCannotSearch(t *testing.T) {
	t.Parallel()
	ff := newFindFixture(t, trackerBoardSetup{site: findSite()})
	since := ff.lastSeq()
	if got, want := ff.refused(t, map[string]any{"status": "Nope", "mine": true}),
		`[tracker_failed_tracker] example.atlassian.net: Jira refused the search (HTTP 400); check that it has a status named "Nope"`; got != want {
		t.Errorf("an unknown status = %s\nwant %s", got, want)
	}
	conns, err := ff.f.a.TrackerConnections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.Problem != nil {
			t.Errorf("connection %s has the problem %+v after a refused search", c.ID, *c.Problem)
		}
	}
	if got, want := ff.refused(t, map[string]any{"status": "backlog"}),
		"[tracker_request_invalid] example.atlassian.net: nothing was sent: Jira groups statuses only as to do, in progress and done, not backlog"; got != want {
		t.Errorf("a group Jira has not = %s\nwant %s", got, want)
	}
	if got, want := ff.refused(t, map[string]any{}), "[tracker_request_invalid] Give text, mine, status or sprint."; got != want {
		t.Errorf("an empty search = %s\nwant %s", got, want)
	}
	if got, want := ff.refused(t, map[string]any{"text": "  "}), "[tracker_request_invalid] Give text, mine, status or sprint."; got != want {
		t.Errorf("a search for spaces = %s\nwant %s", got, want)
	}
	if got, want := ff.refused(t, map[string]any{"sprint": "Sprint 1"}), `[tracker_request_invalid] sprint takes "active" only.`; got != want {
		t.Errorf("a sprint by name = %s\nwant %s", got, want)
	}
	var sent []string
	for _, r := range ff.f.e.Requests(since).Requests {
		sent = append(sent, r.Route)
	}
	if !oneJiraSearch(sent) {
		t.Errorf("the refused calls sent %v; want the one search Jira refused", sent)
	}
}

// findLinear is a Linear workspace of team ENG with an active and a past
// cycle and seven issues of User 1's (the viewer) and User 2's, updated
// in number order.
func findLinear() trackerfake.LinearWorkspace {
	ws := trackerToolsLinear("ENG")
	lid := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
	now := time.Now().UTC().Truncate(time.Second)
	team := ws.Teams[0].ID
	ws.States = []trackerfake.LinearState{
		{ID: lid(20), Team: team, Name: "Backlog", Type: "backlog", Position: 0},
		{ID: lid(21), Team: team, Name: "Todo", Type: "unstarted", Position: 1},
		{ID: lid(22), Team: team, Name: "In Progress", Type: "started", Position: 2},
		{ID: lid(23), Team: team, Name: "In Review", Type: "started", Position: 3},
		{ID: lid(24), Team: team, Name: "Done", Type: "completed", Position: 4},
		{ID: lid(25), Team: team, Name: "Canceled", Type: "canceled", Position: 5},
	}
	ws.Cycles = []trackerfake.LinearCycle{
		{ID: lid(30), Team: team, Number: 2, StartsAt: now.Add(-7 * 24 * time.Hour), EndsAt: now.Add(7 * 24 * time.Hour)},
		{ID: lid(31), Team: team, Number: 1, StartsAt: now.Add(-21 * 24 * time.Hour), EndsAt: now.Add(-7 * 24 * time.Hour), CompletedAt: now.Add(-7 * 24 * time.Hour)},
	}
	issue := func(n int, title string, state, assignee, cycle int) trackerfake.LinearIssue {
		is := trackerfake.LinearIssue{ID: lid(60 + n), Team: team, Number: float64(n), Title: title, State: lid(state), Creator: lid(1),
			CreatedAt: now.Add(-10 * 24 * time.Hour), UpdatedAt: now.Add(-9*24*time.Hour + time.Duration(n)*time.Minute)}
		if assignee != 0 {
			is.Assignee = lid(assignee)
		}
		if cycle != 0 {
			is.Cycle = lid(cycle)
		}
		return is
	}
	ws.Issues = []trackerfake.LinearIssue{
		issue(1, "Quartz lantern", 22, 1, 30),
		issue(2, "Second task", 21, 1, 30),
		issue(3, "Backlog task", 20, 1, 0),
		issue(4, "Their lantern", 22, 2, 30),
		issue(5, "Reviewed lantern", 23, 1, 31),
		issue(6, "Finished lantern", 24, 1, 30),
		issue(7, "Dropped lantern", 25, 1, 30),
	}
	ws.Comments = nil
	return ws
}

// An agent finds Linear tickets by the same filters, with or without
// text, with one request to Linear each; a state name Linear does not
// have finds nothing.
func TestTrackerAgentFindsLinearTicketsByFilters(t *testing.T) {
	t.Parallel()
	ws := findLinear()
	ff := newFindFixture(t, trackerBoardSetup{site: trackerBoardSite(), linear: &ws})
	for _, c := range []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"mine": true}, []string{"ENG-7", "ENG-6", "ENG-5", "ENG-3", "ENG-2", "ENG-1"}},
		{map[string]any{"mine": true, "sprint": "active"}, []string{"ENG-7", "ENG-6", "ENG-2", "ENG-1"}},
		{map[string]any{"mine": true, "status": "in progress"}, []string{"ENG-5", "ENG-1"}},
		{map[string]any{"mine": true, "status": "not done"}, []string{"ENG-5", "ENG-3", "ENG-2", "ENG-1"}},
		{map[string]any{"status": "backlog"}, []string{"ENG-3"}},
		{map[string]any{"status": "canceled"}, []string{"ENG-7"}},
		{map[string]any{"status": "in review"}, []string{"ENG-5"}},
		{map[string]any{"status": "Nope"}, nil},
		{map[string]any{"text": "lantern", "mine": true, "sprint": "active", "status": "not done"}, []string{"ENG-1"}},
	} {
		keys, routes := ff.find(t, c.args)
		var linear []string
		for _, k := range keys {
			if key, ok := strings.CutPrefix(k, "example/"); ok {
				linear = append(linear, key)
			}
		}
		if !slices.Equal(linear, c.want) {
			t.Errorf("ticket_find %v found %v on Linear, want %v (all: %v)", c.args, linear, c.want, keys)
		}
		var asked []string
		for _, r := range routes {
			if !strings.HasPrefix(r, "POST ") && !strings.HasPrefix(r, "GET ") {
				asked = append(asked, r)
			}
		}
		if len(asked) != 1 {
			t.Errorf("ticket_find %v asked Linear %v, want one request", c.args, asked)
		}
	}
	// Jira, which has no backlog group, is named unsearched beside Linear's
	// answer, and is sent nothing.
	since := ff.lastSeq()
	text, isError := trackerMCPStreamedCall(t, ff.f.a, ff.endpoint, "ticket_find", map[string]any{"status": "backlog"})
	if want := `{"tickets":[{"key":"example/ENG-3","title":"Backlog task","status":"Backlog","assignee":"User 1"}],` +
		`"unsearched":[{"site":"example.atlassian.net","error":"nothing was sent: Jira groups statuses only as to do, in progress and done, not backlog"}]}`; isError || text != want {
		t.Errorf("a backlog search on Jira and Linear = %s (error %v)\nwant %s", text, isError, want)
	}
	for _, r := range ff.f.e.Requests(since).Requests {
		if strings.HasPrefix(r.Route, "POST rest/") {
			t.Errorf("a backlog search sent Jira %s", r.Route)
		}
	}
}
