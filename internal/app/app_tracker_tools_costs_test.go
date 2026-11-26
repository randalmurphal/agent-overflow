package app

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// The requests the tracker tools' writes make, pinned per tracker: a
// comment alone, a status change, a sprint move of three tickets and a
// create, on Jira also one that names a field. Each board the writes judge is read first and its pump ended,
// so every request a pin lists is the call's.

// toolRoutes runs one tool call, answering the approval it raises, and
// answers its text and the routes it asked of the fake since the last.
func toolRoutes(t *testing.T, f *trackerBoardFixture, threadID, endpoint, tool string, args any) (string, []string) {
	t.Helper()
	since := 0
	if log := f.e.Requests(0).Requests; len(log) > 0 {
		since = log[len(log)-1].Seq
	}
	done := trackerWriteCall(t, f.a, endpoint, tool, args)
	var result [2]any
	waitFor(t, "the call's answer or its approval", func() bool {
		select {
		case result = <-done:
			return true
		default:
		}
		for _, req := range f.a.triage.PendingInteractiveRequests(threadID).Approvals {
			if err := f.a.RespondToApproval(t.Context(), threadID, provider.ApprovalResponse{RequestID: req.RequestID, Occurrence: req.Occurrence, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
		}
		return false
	})
	if result[1].(bool) {
		t.Fatalf("%s = %s", tool, result[0])
	}
	var routes []string
	for _, r := range f.e.Requests(since).Requests {
		routes = append(routes, r.Route)
	}
	return result[0].(string), routes
}

// readBoard reads board of connection once for the project, as a screen
// that opens it does, and ends the screen's pump.
func readBoard(t *testing.T, f *trackerBoardFixture, connection, board string, sections []trackerapp.Section) {
	t.Helper()
	ctx, state := newTestConnection(t, transport.ConnPrincipal{})
	sub, err := f.a.SubscribeTrackerBoard(ctx, f.project, connection, board, sections, true)
	if err != nil {
		t.Fatal(err)
	}
	f.boardEvent(t, "for the board's first read", func(e trackerapp.BoardEvent) bool { return e.ReadAgain && e.Seq > sub.Seq })
	state.RunCleanups()
}

func pinned(t *testing.T, what string, routes, want []string) {
	t.Helper()
	if !slices.Equal(routes, want) {
		t.Errorf("%s: %d requests %q\nwant %d: %q", what, len(routes), routes, len(want), want)
	}
}

func TestTrackerWriteRequestsOnJira(t *testing.T) {
	t.Parallel()
	f := newTrackerBoardFixture(t)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})

	const (
		issue       = "GET rest/api/3/issue/{issue}"
		comments    = "GET rest/api/3/issue/{issue}/comment"
		boardRows   = "GET rest/software/1.0/board/{board}/issue"
		types       = "GET rest/api/3/issue/createmeta/{project}/issuetypes"
		screen      = "GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}"
		create      = "POST rest/api/3/issue"
		match       = "POST rest/api/3/jql/match"
		bulkfetch   = "POST rest/api/3/issue/bulkfetch"
		transitions = "GET rest/api/3/issue/{issue}/transitions"
		transition  = "POST rest/api/3/issue/{issue}/transitions"
	)
	_, routes := toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1"}, "comment": "Checked."})
	pinned(t, "a comment alone", routes, []string{
		issue, // the project, whose comment audience the comment takes
		"POST rest/api/3/issue/{issue}/comment",
	})
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-2"}, "set": map[string]any{"status": "Done"}})
	pinned(t, "a status change", routes, []string{
		issue, comments, // the ticket's detail, which the plan judges the status on
		transitions, transition, // the move: the moves it can make, then the one
		boardRows, issue, comments, // the ticket's row and detail read again after the write
	})
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"WEB-1", "WEB-2", "WEB-3"}, "set": map[string]any{"sprint": "backlog"}})
	pinned(t, "a sprint move of three tickets", routes, []string{
		issue, comments, // WEB-1's detail
		bulkfetch,       // WEB-2's: the body the status change read, checked by its update time
		issue, comments, // WEB-3's detail: in no sprint, it is already in the backlog
		boardRows, "POST rest/agile/1.0/backlog/{board}/issue", // WEB-1's move, from its sprint
		boardRows, issue, comments, // WEB-1 read again
		boardRows, "POST rest/agile/1.0/backlog/{board}/issue", // WEB-2's
		boardRows, issue, comments, // WEB-2 read again
	})
	// The create that names a field reads the create screen once, for both
	// the tool's field names and the create's plan; the next create, within
	// the minute the client keeps a screen, reads it none.
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "Dated page", "set": map[string]any{"Due date": "2026-12-01"}})
	pinned(t, "a create that names a field", routes, []string{
		types, screen, // the type, then its screen
		create, match, bulkfetch, // the create, then its place on the board and its row
	})
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_create", map[string]any{"project": "WEB", "title": "New page", "description": "Steps.", "set": map[string]any{"status": "In Progress"}})
	pinned(t, "a create", routes, []string{
		types, create, match, bulkfetch,
		issue, comments, // the new ticket's detail, for the status after the create
		transitions, transition,
		boardRows, issue, comments, // read again after the move
	})
}

// trackerCostLinear is trackerToolsLinear with cycles on and three issues
// of team ENG in its active cycle.
func trackerCostLinear() trackerfake.LinearWorkspace {
	ws := trackerToolsLinear("ENG")
	ws.Teams[0].CyclesEnabled = true
	lid := func(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }
	now := time.Now().UTC().Truncate(time.Second)
	ws.Cycles = []trackerfake.LinearCycle{{ID: lid(30), Team: lid(10), Number: 1, Name: "Cycle 1", StartsAt: now.Add(-48 * time.Hour), EndsAt: now.Add(12 * 24 * time.Hour)}}
	ws.Issues[0].Cycle = lid(30)
	for n := 2; n <= 3; n++ {
		is := ws.Issues[0]
		is.ID, is.Number, is.Title, is.History = lid(60+n), float64(n), fmt.Sprintf("Issue %d", n), nil
		ws.Issues = append(ws.Issues, is)
	}
	return ws
}

func TestTrackerWriteRequestsOnLinear(t *testing.T) {
	t.Parallel()
	ws := trackerCostLinear()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: trackerBoardSite(), linear: &ws})
	linear := f.addLinear(t, ws)
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	team := ws.Teams[0].ID
	listed := []trackerapp.ProjectBoard{
		{Connection: f.conn.ID, Board: "1", OpensFirst: true, Name: "WEB board", TrackerProject: "WEB"},
		{Connection: linear.ID, Board: team, Name: "Engineering", TrackerProject: "ENG"},
	}
	if _, err := f.a.SetProjectTrackerBoards(t.Context(), f.project, f.held(t, t.Context()), listed); err != nil {
		t.Fatal(err)
	}
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	readBoard(t, f, linear.ID, team, []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: ws.Cycles[0].ID}, {Kind: trackerapp.SectionBacklog}})

	_, routes := toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"example/ENG-1"}, "comment": "Checked."})
	pinned(t, "a comment alone", routes, []string{"CreateComment"})
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"example/ENG-1"}, "set": map[string]any{"status": "Backlog"}})
	pinned(t, "a status change", routes, []string{
		"IssueDetails",               // the ticket's detail, which the plan judges the status on
		"IssueStates", "UpdateIssue", // the team's states, then the move
		"Issues", "IssueDetails", // the ticket's row and detail read again after the write
	})
	_, routes = toolRoutes(t, f, thread.ID, endpoint, "ticket_update", map[string]any{"keys": []string{"example/ENG-1", "example/ENG-2", "example/ENG-3"}, "set": map[string]any{"sprint": "backlog"}})
	out := []string{"IssueDetails", "IssueDetails", "IssueDetails"} // each ticket's detail
	for range 3 {
		// Each ticket's move out of its cycle: the positions it is placed
		// by, the move, and its row and detail read again.
		out = append(out, "IssuePositions", "UpdateIssues", "Issues", "IssueDetails")
	}
	pinned(t, "a sprint move of three tickets", routes, out)
	text, routes := toolRoutes(t, f, thread.ID, endpoint, "ticket_create", map[string]any{"project": "ENG", "title": "New page", "description": "Steps.", "set": map[string]any{"status": "Todo"}})
	if !strings.Contains(text, "ENG-4") {
		t.Errorf("the create = %s", text)
	}
	pinned(t, "a create", routes, []string{
		"CreateIssue", "Issues", // the create, then its row
		"IssueDetails",               // the new ticket's detail, for the status after the create
		"IssueStates", "UpdateIssue", // the move
		"Issues", "IssueDetails", // read again after the move
	})
}
