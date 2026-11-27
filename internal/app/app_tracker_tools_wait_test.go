package app

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
)

// A ticket a person changes while the approval of an agent's change to
// it waits is sent nothing and said as refused, on Jira and on Linear,
// for each kind of value no write compares at send: a title, a status
// the ticket held when planned, a sprint, an estimate and an assignee.
// The call's other ticket gets the agent's change.

// waitSite is the board site with a second user, a future sprint and
// story points the board estimates in.
func waitSite() trackerfake.JiraSite {
	site := trackerBoardSite()
	site.Users = append(site.Users, trackerfake.JiraUser{AccountID: "acc-user2", DisplayName: "User 2", Email: "user2@example.com", TimeZone: "UTC"})
	site.Fields = append(site.Fields, trackerfake.JiraField{ID: "customfield_10016", Name: "Story Points",
		Schema: trackerfake.JiraSchema{Type: "number", Custom: "com.atlassian.jira.plugin.system.customfieldtypes:float", CustomID: 10016}})
	site.Boards[0].Estimation = "customfield_10016"
	site.Sprints = append(site.Sprints, trackerfake.JiraSprint{ID: 2, Board: 1, Name: "Sprint 2", State: "future"})
	for i := range site.Issues {
		site.Issues[i].Values = map[string]json.RawMessage{"customfield_10016": json.RawMessage(`5`)}
	}
	return site
}

// waitLinear is trackerCostLinear with estimates on and a second cycle,
// after the first.
func waitLinear() trackerfake.LinearWorkspace {
	ws := trackerCostLinear()
	ws.Teams[0].Estimation = "fibonacci"
	c := ws.Cycles[0]
	ws.Cycles = append(ws.Cycles, trackerfake.LinearCycle{ID: waitLID(31), Team: c.Team, Number: 2, Name: "Cycle 2", StartsAt: c.EndsAt, EndsAt: c.EndsAt.Add(14 * 24 * time.Hour)})
	return ws
}

func waitLID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

// waitTicket is a ticket's values as the fake holds them.
type waitTicket struct {
	title, status, assignee string
	sprints                 []string
	estimate                string
}

func waitTickets(f *trackerBoardFixture) map[string]waitTicket {
	out := map[string]waitTicket{}
	snap := f.e.Snapshot()
	for _, site := range snap.Jira {
		for _, is := range site.Issues {
			t := waitTicket{title: is.Summary, status: is.Status, assignee: is.Assignee, estimate: string(is.Values["customfield_10016"])}
			for _, sp := range is.Sprints {
				t.sprints = append(t.sprints, fmt.Sprint(sp))
			}
			out[is.ID] = t
		}
	}
	for _, ws := range snap.Linear {
		for _, is := range ws.Issues {
			t := waitTicket{title: is.Title, status: is.State, assignee: is.Assignee}
			if is.Cycle != "" {
				t.sprints = []string{is.Cycle}
			}
			if is.Estimate != nil {
				t.estimate = fmt.Sprint(*is.Estimate)
			}
			out[is.ID] = t
		}
	}
	return out
}

// waitCase is one kind of value: what the agent sets on both tickets,
// what the person does to the first while the approval waits, what each
// ticket then holds, and what the answer says of the second.
type waitCase struct {
	kind   string
	set    map[string]any
	person func(t *testing.T, f *trackerBoardFixture, conn, issue string)
	holds  func(first, second waitTicket) bool
	second string
}

// personMove moves issue to the status named to, as the ticket's screen
// does.
func personMove(t *testing.T, f *trackerBoardFixture, conn, issue, to string) {
	t.Helper()
	moves, err := f.a.TrackerMoves(t.Context(), conn, issue)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(moves, func(m trackerapp.MoveView) bool { return m.To.Name == to })
	if i < 0 {
		t.Fatalf("%s has no move to %s: %+v", issue, to, moves)
	}
	personWrote(t, "the person's move")(f.a.MoveTrackerIssue(t.Context(), conn, trackerapp.MoveRequest{Issue: issue, Move: moves[i].ID}))
}

func personWrote(t *testing.T, what string) func(trackerapp.WriteView, error) {
	return func(v trackerapp.WriteView, err error) {
		t.Helper()
		if err != nil || len(v.Parts) == 0 || slices.ContainsFunc(v.Parts, func(p trackerapp.PartView) bool { return p.State != "applied" }) {
			t.Fatalf("%s = %+v, %v", what, v, err)
		}
	}
}

func personEdit(t *testing.T, f *trackerBoardFixture, conn, issue string, e trackerapp.FieldEditInput) {
	t.Helper()
	personWrote(t, "the person's edit")(f.a.EditTrackerIssue(t.Context(), conn, trackerapp.EditRequest{Issue: issue, Fields: []trackerapp.FieldEditInput{e}}))
}

func number(n float64) *float64 { return &n }

func TestTrackerWriteTicketChangedWhileWaiting(t *testing.T) {
	t.Parallel()
	type onTracker struct {
		name   string
		linear bool
		keys   []string
		ids    [2]string
		board  string
		cases  []waitCase
	}
	title := waitCase{kind: "title", set: map[string]any{"title": "The agent's title"},
		person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
			personEdit(t, f, conn, issue, trackerapp.FieldEditInput{Field: "title", Typed: []string{"The person's title"}})
		},
		holds:  func(a, b waitTicket) bool { return a.title == "The person's title" && b.title == "The agent's title" },
		second: `"applied":["title"]`}
	jira := onTracker{name: "jira", keys: []string{"WEB-1", "WEB-2"}, ids: [2]string{"10101", "10102"}, board: "1", cases: []waitCase{
		title,
		{kind: "status already", set: map[string]any{"status": "To Do"},
			// WEB-2 and WEB-3 are in To Do when planned; WEB-2 leaves it.
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personMove(t, f, conn, issue, "In Progress")
			},
			holds:  func(_, _ waitTicket) bool { return true },
			second: `"already":["status"]`},
		{kind: "sprint", set: map[string]any{"sprint": "Sprint 2"},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personWrote(t, "the person's move to the backlog")(f.a.MoveTrackerIssuesToSprint(t.Context(), conn, trackerapp.SprintMoveRequest{Board: "1", Issues: []string{issue}}))
			},
			holds:  func(a, b waitTicket) bool { return !slices.Contains(a.sprints, "2") && slices.Contains(b.sprints, "2") },
			second: `"applied":["sprint"]`},
		{kind: "estimate", set: map[string]any{"estimate": 8},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personWrote(t, "the person's estimate")(f.a.SetTrackerEstimate(t.Context(), conn, trackerapp.EstimateRequest{Board: "1", Issue: issue, Value: number(3)}))
			},
			holds:  func(a, b waitTicket) bool { return a.estimate == "3" && b.estimate == "8" },
			second: `"applied":["estimate"]`},
		{kind: "assignee", set: map[string]any{"assignee": "User 2"},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personEdit(t, f, conn, issue, trackerapp.FieldEditInput{Field: "assignee", Value: &trackerapp.FieldValueView{Kind: "user", Choices: []trackerapp.ChoiceView{{ID: "acc-user1"}}}})
			},
			holds:  func(a, b waitTicket) bool { return a.assignee == "acc-user1" && b.assignee == "acc-user2" },
			second: `"applied":["assignee"]`},
	}}
	linear := onTracker{name: "linear", linear: true, keys: []string{"example/ENG-1", "example/ENG-2"}, ids: [2]string{waitLID(60), waitLID(62)}, board: waitLID(10), cases: []waitCase{
		title,
		{kind: "status already", set: map[string]any{"status": "Todo"},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personMove(t, f, conn, issue, "Backlog")
			},
			holds:  func(_, _ waitTicket) bool { return true },
			second: `"already":["status"]`},
		{kind: "sprint", set: map[string]any{"sprint": "Cycle 2"},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personWrote(t, "the person's move out of the cycle")(f.a.MoveTrackerIssuesToSprint(t.Context(), conn, trackerapp.SprintMoveRequest{Board: waitLID(10), Issues: []string{issue}}))
			},
			holds: func(a, b waitTicket) bool {
				return len(a.sprints) == 0 && slices.Equal(b.sprints, []string{waitLID(31)})
			},
			second: `"applied":["sprint"]`},
		{kind: "estimate", set: map[string]any{"estimate": 5},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personWrote(t, "the person's estimate")(f.a.SetTrackerEstimate(t.Context(), conn, trackerapp.EstimateRequest{Board: waitLID(10), Issue: issue, Value: number(3)}))
			},
			holds:  func(a, b waitTicket) bool { return a.estimate == "3" && b.estimate == "5" },
			second: `"applied":["estimate"]`},
		{kind: "assignee", set: map[string]any{"assignee": "User 3"},
			person: func(t *testing.T, f *trackerBoardFixture, conn, issue string) {
				personEdit(t, f, conn, issue, trackerapp.FieldEditInput{Field: "assignee", Value: &trackerapp.FieldValueView{Kind: "user", Choices: []trackerapp.ChoiceView{{ID: waitLID(1)}}}})
			},
			holds:  func(a, b waitTicket) bool { return a.assignee == waitLID(1) && b.assignee == waitLID(3) },
			second: `"applied":["assignee"]`},
	}}
	for _, tr := range []onTracker{jira, linear} {
		for _, tc := range tr.cases {
			t.Run(tr.name+"/"+tc.kind, func(t *testing.T) {
				t.Parallel()
				setup := trackerBoardSetup{site: waitSite()}
				if tr.linear {
					ws := waitLinear()
					setup.linear = &ws
				}
				f := newTrackerBoardFixtureOn(t, setup)
				t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
				conn := f.conn.ID
				sections := []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}}
				if tr.linear {
					conn = f.addLinear(t, *setup.linear).ID
					listed := []trackerapp.ProjectBoard{
						{Connection: f.conn.ID, Board: "1", OpensFirst: true, Name: "WEB board", TrackerProject: "WEB"},
						{Connection: conn, Board: tr.board, Name: "Engineering", TrackerProject: "ENG"},
					}
					if _, err := f.a.SetProjectTrackerBoards(t.Context(), f.project, f.held(t, t.Context()), listed); err != nil {
						t.Fatal(err)
					}
					sections = []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: setup.linear.Cycles[0].ID}, {Kind: trackerapp.SectionBacklog}}
				}
				thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
				readBoard(t, f, conn, tr.board, sections)
				keys, ids := tr.keys, tr.ids
				if tc.kind == "status already" && !tr.linear {
					keys, ids = []string{"WEB-2", "WEB-3"}, [2]string{"10102", "10103"}
				}
				done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": keys, "set": tc.set})
				open, _ := trackerWritePrompt(t, f.a, thread.ID)
				tc.person(t, f, conn, ids[0])
				held := waitTickets(f)[ids[0]]
				if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
					t.Fatal(err)
				}
				result := <-done
				now := waitTickets(f)
				if !tc.holds(now[ids[0]], now[ids[1]]) || !waitSame(now[ids[0]], held) {
					t.Errorf("the tickets hold %+v and %+v; the first as the person left it, %+v, and the second with the agent's change", now[ids[0]], now[ids[1]], held)
				}
				text := result[0].(string)
				first := fmt.Sprintf(`{"key":%q,"refused":`, keys[0])
				if result[1].(bool) || !strings.Contains(text, first) || !strings.Contains(text, tc.second) {
					t.Errorf("ticket_update = %v; want %s refused and %s", result, keys[0], tc.second)
				}
			})
		}
	}
}

// waitSame reports whether a and b hold the same values.
func waitSame(a, b waitTicket) bool {
	return a.title == b.title && a.status == b.status && a.assignee == b.assignee && a.estimate == b.estimate && slices.Equal(a.sprints, b.sprints)
}
