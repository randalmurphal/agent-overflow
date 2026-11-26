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

// costSite is the board site with story points the board estimates in, a
// future sprint, and n issues, at least its three, in Sprint 1 and To Do,
// each with a description.
func costSite(n int) trackerfake.JiraSite {
	s := trackerBoardSite()
	for i := len(s.Issues) + 1; i <= n; i++ {
		is := s.Issues[1]
		is.ID, is.Key, is.Summary = fmt.Sprintf("%d", 10100+i), fmt.Sprintf("WEB-%d", i), fmt.Sprintf("Task %d", i)
		s.Issues = append(s.Issues, is)
	}
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

// costLinear is trackerCostLinear with estimates on, a second cycle and
// n issues, at least its three, every issue in the first.
func costLinear(n int) trackerfake.LinearWorkspace {
	ws := trackerCostLinear()
	for i := len(ws.Issues) + 1; i <= n; i++ {
		is := ws.Issues[1]
		is.ID, is.Number, is.Title = fmt.Sprintf("00000000-0000-4000-8000-%012x", 0x100+i), float64(i), fmt.Sprintf("Issue %d", i)
		ws.Issues = append(ws.Issues, is)
	}
	ws.Teams[0].Estimation = "fibonacci"
	c := ws.Cycles[0]
	ws.Cycles = append(ws.Cycles, trackerfake.LinearCycle{ID: waitLID(31), Team: c.Team, Number: 2, Name: "Cycle 2", StartsAt: c.EndsAt, EndsAt: c.EndsAt.Add(14 * 24 * time.Hour)})
	for i := range ws.Issues {
		ws.Issues[i].Cycle = c.ID
	}
	return ws
}

// costFixture is a fixture of n issues on each tracker whose project
// lists the board of kind's tracker, read once, and a thread that writes.
func costFixture(t *testing.T, linear bool, n int) (*trackerBoardFixture, string, string) {
	t.Helper()
	ws := costLinear(n)
	f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: costSite(n), linear: &ws})
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

// costWrites are the names of costRoute and Linear's operations that
// change the tracker; every other request is a read.
var costWrites = map[string]bool{
	"comment": true, "transition": true, "edit": true, "to-sprint": true, "to-backlog": true, "estimate": true, "create": true,
	"UpdateIssue": true, "UpdateIssues": true, "CreateComment": true, "CreateIssue": true,
}

// costNames are routes as costRoute names them, joined by spaces, with
// only the order a call's correctness needs: its writes in the order they
// are sent, and the reads between two writes counted, in name order, since
// a call reads several tickets at once. A run of one name is counted:
// "comments×3 issue×3 transition×3 rows".
func costNames(routes []string) string {
	var names, reads []string
	endReads := func() {
		slices.Sort(reads)
		names, reads = append(names, reads...), reads[:0]
	}
	for _, r := range routes {
		name := cmp.Or(costRoute[r], r)
		if !costWrites[name] {
			reads = append(reads, name)
			continue
		}
		endReads()
		names = append(names, name)
	}
	endReads()
	var out []string
	for i := 0; i < len(names); {
		n := 1
		for i+n < len(names) && names[i+n] == names[i] {
			n++
		}
		if n == 1 {
			out = append(out, names[i])
		} else {
			out = append(out, fmt.Sprintf("%s×%d", names[i], n))
		}
		i += n
	}
	return strings.Join(out, " ")
}

// TestTrackerToolRequestCounts pins the requests of each call shape. The
// read after an approval (planCall.recheck) is the bulkfetch or Issues a
// call of three tickets with set makes after its plan's reads; a call of
// one ticket or a comment alone waits for no approval or judges nothing.
// A Jira comment's lookup (Server.readAudience) is the bulkfetch just
// before the comments, after every other write.
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
	// want is each pin, by tracker, shape, temperature and N; ran is
	// each pin's name a case ran.
	want, ran := costPins, map[string]bool{}
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
					ran[name] = true
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						f, thread, endpoint := costFixture(t, linear, 3)
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
			// again is the call made after it on the same client, if any.
			again map[string]any
		}{
			{name: "create", tool: "ticket_create", args: map[string]any{"project": project, "title": "New page"}},
			{name: "create, then another", tool: "ticket_create", args: map[string]any{"project": project, "title": "New page"},
				again: map[string]any{"project": project, "title": "Another page"}},
			{name: "create with status and sprint", tool: "ticket_create", args: map[string]any{"project": project, "title": "New page", "set": func() map[string]any {
				m := status(linear)
				maps.Copy(m, sprint(linear))
				return m
			}()}},
			// The same read again: the body the first stored, checked.
			{name: "read", tool: "ticket_read", args: map[string]any{"key": keys[0]}, again: map[string]any{"key": keys[0]}},
			{name: "find", tool: "ticket_find", args: map[string]any{"text": "story"}},
		} {
			name := tracker + "/" + c.name
			ran[name] = true
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				f, thread, endpoint := costFixture(t, linear, 3)
				_, routes := toolRoutes(t, f, thread, endpoint, c.tool, c.args)
				got := costNames(routes)
				if c.again != nil {
					_, again := toolRoutes(t, f, thread, endpoint, c.tool, c.again)
					got += " | " + costNames(again)
				}
				if got != want[name] {
					t.Errorf("%s\nwant %s", got, want[name])
				}
				if c.tool == "ticket_create" {
					var titles []string
					for _, title := range []any{c.args["title"], c.again["title"]} {
						if title != nil {
							titles = append(titles, title.(string))
						}
					}
					if got := createdTitles(f, linear); !slices.Equal(got, titles) {
						t.Errorf("the tracker holds the created tickets %q; want %q", got, titles)
					}
				}
			})
		}
	}
	// A pin no case names would pass unrun.
	t.Cleanup(func() {
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if !ran[name] {
				t.Errorf("pin %s names no case", name)
			}
		}
	})
}

// createdTitles are the titles of the tickets on f's tracker past the
// three costFixture seeds, in the order they were created.
func createdTitles(f *trackerBoardFixture, linear bool) []string {
	world := f.e.Snapshot()
	var out []string
	if linear {
		for _, is := range world.Linear[0].Issues[3:] {
			out = append(out, is.Title)
		}
		return out
	}
	for _, is := range world.Jira[0].Issues[3:] {
		out = append(out, is.Summary)
	}
	return out
}

// costPins are TestTrackerToolRequestCounts' pins, by tracker, shape,
// temperature and number of tickets. A Jira status planned cold takes
// its moves from the detail read (no transitions read); warm, the plan
// reads the cards, which keep no read, so the move reads them. An
// estimate reads the board's rows to check the issue is on it first.
// Every write ends with one refresh (rows, Issues). A read is one read
// and then the same read again, after "|"; a create followed by a create
// of another ticket on the same client is the two creates.
var costPins = map[string]string{
	"jira/comment/cold/1":                  "bulkfetch comment",
	"jira/comment/cold/3":                  "bulkfetch comment×3",
	"jira/comment/warm/1":                  "bulkfetch comment",
	"jira/comment/warm/3":                  "bulkfetch comment×3",
	"jira/status/cold/1":                   "comments issue transition rows",
	"jira/status/cold/3":                   "bulkfetch comments×3 issue×3 transition×3 rows",
	"jira/status/warm/1":                   "bulkfetch transitions transition rows",
	"jira/status/warm/3":                   "bulkfetch×2 transitions transition transitions transition transitions transition rows",
	"jira/sprint/cold/1":                   "bulkfetch rows sprints to-sprint rows",
	"jira/sprint/cold/3":                   "bulkfetch×2 rows sprints to-sprint rows",
	"jira/sprint/warm/1":                   "bulkfetch rows sprints to-sprint rows",
	"jira/sprint/warm/3":                   "bulkfetch×2 rows sprints to-sprint rows",
	"jira/backlog/cold/1":                  "bulkfetch rows to-backlog rows",
	"jira/backlog/cold/3":                  "bulkfetch×2 rows to-backlog rows",
	"jira/backlog/warm/1":                  "bulkfetch rows to-backlog rows",
	"jira/backlog/warm/3":                  "bulkfetch×2 rows to-backlog rows",
	"jira/title/cold/1":                    "bulkfetch issue edit rows",
	"jira/title/cold/3":                    "bulkfetch×2 issue edit issue edit issue edit rows",
	"jira/title/warm/1":                    "bulkfetch issue edit rows",
	"jira/title/warm/3":                    "bulkfetch×2 issue edit issue edit issue edit rows",
	"jira/estimate/cold/1":                 "bulkfetch rows estimate rows",
	"jira/estimate/cold/3":                 "bulkfetch×2 rows estimate rows estimate rows estimate rows",
	"jira/estimate/warm/1":                 "bulkfetch rows estimate rows",
	"jira/estimate/warm/3":                 "bulkfetch×2 rows estimate rows estimate rows estimate rows",
	"jira/mixed/cold/1":                    "comments issue transition issue edit rows estimate rows sprints to-sprint bulkfetch comment rows",
	"jira/mixed/cold/3":                    "bulkfetch comments×3 issue×3 transition×3 issue edit issue edit issue edit rows estimate rows estimate rows estimate rows sprints to-sprint bulkfetch comment×3 rows",
	"jira/mixed/warm/1":                    "bulkfetch transitions transition issue edit rows estimate rows sprints to-sprint bulkfetch comment rows",
	"jira/mixed/warm/3":                    "bulkfetch×2 transitions transition transitions transition transitions transition issue edit issue edit issue edit rows estimate rows estimate rows estimate rows sprints to-sprint bulkfetch comment×3 rows",
	"jira/create":                          "screen types create bulkfetch match",
	"jira/create, then another":            "screen types create bulkfetch match | types create bulkfetch match",
	"jira/create with status and sprint":   "screen types create bulkfetch comments issue match transition rows sprints to-sprint rows",
	"jira/read":                            "comments issue | bulkfetch",
	"jira/find":                            "search",
	"linear/comment/cold/1":                "CreateComment",
	"linear/comment/cold/3":                "CreateComment×3",
	"linear/comment/warm/1":                "CreateComment",
	"linear/comment/warm/3":                "CreateComment×3",
	"linear/status/cold/1":                 "IssueDetails UpdateIssue Issues",
	"linear/status/cold/3":                 "IssueDetails Issues UpdateIssue×3 Issues",
	"linear/status/warm/1":                 "IssueDetails UpdateIssue Issues",
	"linear/status/warm/3":                 "IssueDetails Issues UpdateIssue×3 Issues",
	"linear/sprint/cold/1":                 "CyclePositions Issues UpdateIssues Issues",
	"linear/sprint/cold/3":                 "CyclePositions Issues×2 UpdateIssues Issues",
	"linear/sprint/warm/1":                 "CyclePositions Issues UpdateIssues Issues",
	"linear/sprint/warm/3":                 "CyclePositions Issues×2 UpdateIssues Issues",
	"linear/backlog/cold/1":                "IssuePositions Issues UpdateIssues Issues",
	"linear/backlog/cold/3":                "IssuePositions Issues×2 UpdateIssues Issues",
	"linear/backlog/warm/1":                "IssuePositions Issues UpdateIssues Issues",
	"linear/backlog/warm/3":                "IssuePositions Issues×2 UpdateIssues Issues",
	"linear/title/cold/1":                  "Issues UpdateIssue Issues",
	"linear/title/cold/3":                  "Issues×2 UpdateIssue×3 Issues",
	"linear/title/warm/1":                  "Issues UpdateIssue Issues",
	"linear/title/warm/3":                  "Issues×2 UpdateIssue×3 Issues",
	"linear/estimate/cold/1":               "IssueTeam Issues UpdateIssue Issues",
	"linear/estimate/cold/3":               "IssueTeam Issues×2 UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue Issues",
	"linear/estimate/warm/1":               "IssueTeam Issues UpdateIssue Issues",
	"linear/estimate/warm/3":               "IssueTeam Issues×2 UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue Issues",
	"linear/mixed/cold/1":                  "IssueDetails UpdateIssue×2 IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment Issues",
	"linear/mixed/cold/3":                  "IssueDetails Issues UpdateIssue×6 IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment×3 Issues",
	"linear/mixed/warm/1":                  "IssueDetails UpdateIssue×2 IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment Issues",
	"linear/mixed/warm/3":                  "IssueDetails Issues UpdateIssue×6 IssueTeam UpdateIssue IssueTeam UpdateIssue IssueTeam UpdateIssue CyclePositions UpdateIssues CreateComment×3 Issues",
	"linear/create":                        "CreateIssue Issues",
	"linear/create, then another":          "CreateIssue Issues | CreateIssue Issues",
	"linear/create with status and sprint": "CreateIssue CyclePositions IssueDetails Issues UpdateIssues Issues",
	"linear/read":                          "IssueDetails | IssueDetails",
	"linear/find":                          "SearchIssues fields search",
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

// costCounts are routes as costRoute names them, counted by name, in
// name order: "edit×20 rows".
func costCounts(routes []string) string {
	n := map[string]int{}
	for _, r := range routes {
		n[cmp.Or(costRoute[r], r)]++
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(n)) {
		if n[name] == 1 {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("%s×%d", name, n[name]))
		}
	}
	return strings.Join(out, " ")
}

// An update of as many tickets as the tool takes (maxUpdateKeys) on one
// connection is planned and sent whole, through the card read (a title)
// and through the detail read (a status), on Jira and on Linear: the
// tracker reads stay batched, and the views of what they read are built
// a bounded number of tickets at a time.
func TestTrackerToolUpdatesAsManyTicketsAsItTakes(t *testing.T) {
	t.Parallel()
	const n = 20
	for _, c := range []struct {
		name   string
		linear bool
		set    map[string]any
		want   string
	}{
		{"jira/title", false, map[string]any{"title": "Renamed"}, "bulkfetch×2 edit×20 issue×20 rows"},
		{"jira/status", false, map[string]any{"status": "Done"}, "bulkfetch comments×20 issue×20 rows transition×20"},
		{"linear/title", true, map[string]any{"title": "Renamed"}, "Issues×3 UpdateIssue×20"},
		{"linear/status", true, map[string]any{"status": "Backlog"}, "IssueDetails×2 Issues×2 UpdateIssue×20"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f, thread, endpoint := costFixture(t, c.linear, n)
			keys := make([]string, n)
			for i := range keys {
				keys[i] = fmt.Sprintf("WEB-%d", i+1)
				if c.linear {
					keys[i] = fmt.Sprintf("example/ENG-%d", i+1)
				}
			}
			text, routes := toolRoutes(t, f, thread, endpoint, "ticket_update", map[string]any{"keys": keys, "set": c.set})
			if strings.Count(text, `"applied"`) != n {
				t.Fatalf("ticket_update = %s", text)
			}
			if got := costCounts(routes); got != c.want {
				t.Errorf("%d requests: %s\nwant %s", len(routes), got, c.want)
			}
		})
	}
}
