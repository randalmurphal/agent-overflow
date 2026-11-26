package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
	"agent-overflow/internal/transport"
)

// The requests the tracker tools make, pinned on the fake's request log
// per tracker, call shape, number of tickets (1 and 3) and whether the
// agent read the tickets first (warm) or the mirror holds only their
// cards and rows (cold), plus a create, a create that moves and plans
// its ticket, a read and a find. Each fixture reads its board first and
// ends that screen's pump, so every request a pin lists is the call's.

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

// pinned fails what unless its routes are want.
func pinned(t *testing.T, what string, routes, want []string) {
	t.Helper()
	if !slices.Equal(routes, want) {
		t.Errorf("%s: %d requests %q\nwant %d: %q", what, len(routes), routes, len(want), want)
	}
}

// costSite is the board site with story points the board estimates in, a
// future sprint, and its three issues in Sprint 1 and To Do, each with a
// description.
func costSite() trackerfake.JiraSite {
	s := trackerBoardSite()
	s.Fields = append(s.Fields, trackerfake.JiraField{ID: "customfield_10016", Name: "Story Points",
		Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}})
	s.Boards[0].Estimation = "customfield_10016"
	s.Sprints = append(s.Sprints, trackerfake.JiraSprint{ID: 2, Board: 1, Name: "Sprint 2", State: "future"})
	for i := range s.Issues {
		s.Issues[i].Sprints = []int64{1}
		s.Issues[i].Status = "10000"
		s.Issues[i].Description = json.RawMessage(`{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"body"}]}]}`)
	}
	return s
}

// costLinear is trackerCostLinear with estimates on and a second cycle,
// every issue in the first.
func costLinear() trackerfake.LinearWorkspace {
	ws := trackerCostLinear()
	ws.Teams[0].Estimation = "fibonacci"
	c := ws.Cycles[0]
	ws.Cycles = append(ws.Cycles, trackerfake.LinearCycle{ID: waitLID(31), Team: c.Team, Number: 2, Name: "Cycle 2", StartsAt: c.EndsAt, EndsAt: c.EndsAt.Add(14 * 24 * time.Hour)})
	for i := range ws.Issues {
		ws.Issues[i].Cycle = c.ID
	}
	return ws
}

// costFixture is a fixture whose project lists the board of kind's
// tracker, read once, and a thread that writes.
func costFixture(t *testing.T, linear bool) (*trackerBoardFixture, string, string) {
	t.Helper()
	ws := costLinear()
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: costSite(), linear: &ws})
	t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
	if !linear {
		thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
		readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
		return f, thread.ID, endpoint
	}
	conn := f.addLinear(t, ws)
	team := ws.Teams[0].ID
	listed := []trackerapp.ProjectBoard{
		{Connection: f.conn.ID, Board: "1", OpensFirst: true, Name: "WEB board", TrackerProject: "WEB"},
		{Connection: conn.ID, Board: team, Name: "Engineering", TrackerProject: "ENG"},
	}
	if _, err := f.a.SetProjectTrackerBoards(t.Context(), f.project, f.held(t, t.Context()), listed); err != nil {
		t.Fatal(err)
	}
	thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
	readBoard(t, f, conn.ID, team, []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: ws.Cycles[0].ID}, {Kind: trackerapp.SectionBacklog}})
	return f, thread.ID, endpoint
}

// costRoute is a route of the fake as a pin names it: Jira's by what it
// does, Linear's operation by its name.
var costRoute = map[string]string{
	"GET rest/api/3/field":                                        "fields",
	"GET rest/api/3/issue/{issue}":                                "issue",
	"GET rest/api/3/issue/{issue}/comment":                        "comments",
	"POST rest/api/3/issue/{issue}/comment":                       "comment",
	"POST rest/api/3/issue/bulkfetch":                             "bulkfetch",
	"GET rest/api/3/issue/{issue}/transitions":                    "transitions",
	"POST rest/api/3/issue/{issue}/transitions":                   "transition",
	"PUT rest/api/3/issue/{issue}":                                "edit",
	"GET rest/software/1.0/board/{board}/issue":                   "rows",
	"GET rest/agile/1.0/board/{board}/sprint":                     "sprints",
	"POST rest/agile/1.0/sprint/{sprint}/issue":                   "to-sprint",
	"POST rest/agile/1.0/backlog/{board}/issue":                   "to-backlog",
	"PUT rest/agile/1.0/issue/{issue}/estimation":                 "estimate",
	"GET rest/api/3/issue/createmeta/{project}/issuetypes":        "types",
	"GET rest/api/3/issue/createmeta/{project}/issuetypes/{type}": "screen",
	"POST rest/api/3/issue":                                       "create",
	"POST rest/api/3/jql/match":                                   "match",
	"POST rest/api/3/search/jql":                                  "search",
}

// costNames are routes as costRoute names them, joined by spaces.
func costNames(routes []string) string {
	out := make([]string, len(routes))
	for i, r := range routes {
		out[i] = cmp.Or(costRoute[r], r)
	}
	return strings.Join(out, " ")
}

// TestTrackerToolRequestCounts pins the requests of each call shape. The
// read after an approval (planCall.recheck) is the bulkfetch or Issues a
// call of three tickets with set makes after its plan's reads; a call of
// one ticket or a comment alone waits for no approval or judges nothing.
func TestTrackerToolRequestCounts(t *testing.T) {
	t.Parallel()
	type shape struct {
		name string
		set  func(linear bool) map[string]any
		// comment is the comment the call adds, if any.
		comment string
	}
	status := func(linear bool) map[string]any {
		if linear {
			return map[string]any{"status": "Backlog"}
		}
		return map[string]any{"status": "Done"}
	}
	sprint := func(linear bool) map[string]any {
		if linear {
			return map[string]any{"sprint": "Cycle 2"}
		}
		return map[string]any{"sprint": "Sprint 2"}
	}
	shapes := []shape{
		{name: "comment", comment: "Checked."},
		{name: "status", set: status},
		{name: "sprint", set: sprint},
		{name: "backlog", set: func(bool) map[string]any { return map[string]any{"sprint": "backlog"} }},
		{name: "title", set: func(bool) map[string]any { return map[string]any{"title": "Renamed"} }},
		{name: "estimate", set: func(bool) map[string]any { return map[string]any{"estimate": 3} }},
		{name: "mixed", comment: "Done.", set: func(linear bool) map[string]any {
			m := map[string]any{"title": "Renamed", "estimate": 3}
			maps.Copy(m, status(linear))
			maps.Copy(m, sprint(linear))
			return m
		}},
	}
	// want is each pin, by tracker, shape, temperature and N.
	want := costPins
	for _, linear := range []bool{false, true} {
		tracker, keys := "jira", []string{"WEB-1", "WEB-2", "WEB-3"}
		if linear {
			tracker, keys = "linear", []string{"example/ENG-1", "example/ENG-2", "example/ENG-3"}
		}
		for _, sh := range shapes {
			for _, warm := range []bool{false, true} {
				temp := "cold"
				if warm {
					temp = "warm"
				}
				for _, n := range []int{1, 3} {
					name := fmt.Sprintf("%s/%s/%s/%d", tracker, sh.name, temp, n)
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						f, thread, endpoint := costFixture(t, linear)
						if warm {
							for _, k := range keys[:n] {
								toolRoutes(t, f, thread, endpoint, "ticket_read", map[string]any{"key": k})
							}
						}
						args := map[string]any{"keys": keys[:n]}
						if sh.set != nil {
							args["set"] = sh.set(linear)
						}
						if sh.comment != "" {
							args["comment"] = sh.comment
						}
						_, routes := toolRoutes(t, f, thread, endpoint, "ticket_update", args)
						if got := costNames(routes); got != want[name] {
							t.Errorf("%d requests: %s\nwant %s", len(routes), got, want[name])
						}
					})
				}
			}
		}
		project := "WEB"
		if linear {
			project = "ENG"
		}
		for _, c := range []struct {
			name string
			tool string
			args map[string]any
		}{
			{"create", "ticket_create", map[string]any{"project": project, "title": "New page"}},
			{"create with status and sprint", "ticket_create", map[string]any{"project": project, "title": "New page", "set": func() map[string]any {
				m := status(linear)
				maps.Copy(m, sprint(linear))
				return m
			}()}},
			{"read", "ticket_read", map[string]any{"key": keys[0]}},
			{"find", "ticket_find", map[string]any{"text": "story"}},
		} {
			name := tracker + "/" + c.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f, thread, endpoint := costFixture(t, linear)
				_, routes := toolRoutes(t, f, thread, endpoint, c.tool, c.args)
				got := costNames(routes)
				if c.tool == "ticket_read" {
					// The same read again: the body the first stored, checked.
					_, again := toolRoutes(t, f, thread, endpoint, c.tool, c.args)
					got += " | " + costNames(again)
				}
				if got != want[name] {
					t.Errorf("%s\nwant %s", got, want[name])
				}
			})
		}
	}
}

// costPins are TestTrackerToolRequestCounts' pins, by tracker, shape,
// temperature and number of tickets. A Jira status planned cold takes
// its moves from the detail read (no transitions read); warm, the plan
// reads the cards, which keep no read, so the move reads them. An
// estimate reads the board's rows to check the issue is on it first.
// Every write ends with one refresh (rows, Issues). A read is one read
// and then the same read again, after "|".
var costPins = map[string]string{
	"jira/comment/cold/1":                  "bulkfetch comment",
	"jira/comment/cold/3":                  "bulkfetch comment comment comment",
	"jira/comment/warm/1":                  "bulkfetch comment",
	"jira/comment/warm/3":                  "bulkfetch comment comment comment",
	"jira/status/cold/1":                   "issue comments transition rows",
	"jira/status/cold/3":                   "issue issue issue comments comments comments bulkfetch transition transition transition rows",
	"jira/status/warm/1":                   "bulkfetch transitions transition rows",
	"jira/status/warm/3":                   "bulkfetch bulkfetch transitions transition transitions transition transitions transition rows",
	"jira/sprint/cold/1":                   "bulkfetch rows sprints to-sprint rows",
	"jira/sprint/cold/3":                   "bulkfetch bulkfetch rows sprints to-sprint rows",
	"jira/sprint/warm/1":                   "bulkfetch rows sprints to-sprint rows",
	"jira/sprint/warm/3":                   "bulkfetch bulkfetch rows sprints to-sprint rows",
	"jira/backlog/cold/1":                  "bulkfetch rows to-backlog rows",
	"jira/backlog/cold/3":                  "bulkfetch bulkfetch rows to-backlog rows",
	"jira/backlog/warm/1":                  "bulkfetch rows to-backlog rows",
	"jira/backlog/warm/3":                  "bulkfetch bulkfetch rows to-backlog rows",
	"jira/title/cold/1":                    "bulkfetch issue edit rows",
	"jira/title/cold/3":                    "bulkfetch bulkfetch issue edit issue edit issue edit rows",
	"jira/title/warm/1":                    "bulkfetch issue edit rows",
	"jira/title/warm/3":                    "bulkfetch bulkfetch issue edit issue edit issue edit rows",
	"jira/estimate/cold/1":                 "bulkfetch rows estimate rows",
	"jira/estimate/cold/3":                 "bulkfetch bulkfetch rows estimate rows estimate rows estimate rows",
	"jira/estimate/warm/1":                 "bulkfetch rows estimate rows",
	"jira/estimate/warm/3":                 "bulkfetch bulkfetch rows estimate rows estimate rows estimate rows",
	"jira/mixed/cold/1":                    "issue comments bulkfetch transition issue edit rows estimate rows sprints to-sprint comment rows",
	"jira/mixed/cold/3":                    "issue issue issue comments comments comments bulkfetch transition transition transition issue edit issue edit issue edit rows estimate rows estimate rows estimate rows sprints to-sprint comment comment comment rows",
	"jira/mixed/warm/1":                    "bulkfetch bulkfetch transitions transition issue edit rows estimate rows sprints to-sprint comment rows",
	"jira/mixed/warm/3":                    "bulkfetch bulkfetch transitions transition transitions transition transitions transition issue edit issue edit issue edit rows estimate rows estimate rows estimate rows sprints to-sprint comment comment comment rows",
	"jira/create":                          "types screen create match bulkfetch",
	"jira/create with status and sprint":   "types screen create match bulkfetch issue comments transition rows sprints to-sprint rows",
	"jira/read":                            "issue comments | bulkfetch",
	"jira/find":                            "search",
	"linear/comment/cold/1":                "CreateComment",
	"linear/comment/cold/3":                "CreateComment CreateComment CreateComment",
	"linear/comment/warm/1":                "CreateComment",
	"linear/comment/warm/3":                "CreateComment CreateComment CreateComment",
	"linear/status/cold/1":                 "IssueDetails UpdateIssue Issues",
	"linear/status/cold/3":                 "IssueDetails Issues UpdateIssue UpdateIssue UpdateIssue Issues",
	"linear/status/warm/1":                 "IssueDetails UpdateIssue Issues",
	"linear/status/warm/3":                 "IssueDetails Issues UpdateIssue UpdateIssue UpdateIssue Issues",
	"linear/sprint/cold/1":                 "Issues CyclePositions UpdateIssues Issues",
	"linear/sprint/cold/3":                 "Issues Issues CyclePositions UpdateIssues Issues",
	"linear/sprint/warm/1":                 "Issues CyclePositions UpdateIssues Issues",
	"linear/sprint/warm/3":                 "Issues Issues CyclePositions UpdateIssues Issues",
	"linear/backlog/cold/1":                "Issues IssuePositions UpdateIssues Issues",
	"linear/backlog/cold/3":                "Issues Issues IssuePositions UpdateIssues Issues",
	"linear/backlog/warm/1":                "Issues IssuePositions UpdateIssues Issues",
	"linear/backlog/warm/3":                "Issues Issues IssuePositions UpdateIssues Issues",
	"linear/title/cold/1":                  "Issues UpdateIssue Issues",
	"linear/title/cold/3":                  "Issues Issues UpdateIssue UpdateIssue UpdateIssue Issues",
	"linear/title/warm/1":                  "Issues UpdateIssue Issues",
	"linear/title/warm/3":                  "Issues Issues UpdateIssue UpdateIssue UpdateIssue Issues",
	"linear/estimate/cold/1":               "Issues IssueTeam UpdateIssue Issues",
	"linear/estimate/cold/3":               "Issues Issues IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue Issues",
	"linear/estimate/warm/1":               "Issues IssueTeam UpdateIssue Issues",
	"linear/estimate/warm/3":               "Issues Issues IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue Issues",
	"linear/mixed/cold/1":                  "IssueDetails UpdateIssue UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment Issues",
	"linear/mixed/cold/3":                  "IssueDetails Issues UpdateIssue UpdateIssue UpdateIssue UpdateIssue UpdateIssue UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment CreateComment CreateComment Issues",
	"linear/mixed/warm/1":                  "IssueDetails UpdateIssue UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment Issues",
	"linear/mixed/warm/3":                  "IssueDetails Issues UpdateIssue UpdateIssue UpdateIssue UpdateIssue UpdateIssue UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment CreateComment CreateComment Issues",
	"linear/create":                        "CreateIssue Issues",
	"linear/create with status and sprint": "CreateIssue Issues IssueDetails CyclePositions UpdateIssues Issues",
	"linear/read":                          "IssueDetails | IssueDetails",
	"linear/find":                          "fields search SearchIssues",
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
