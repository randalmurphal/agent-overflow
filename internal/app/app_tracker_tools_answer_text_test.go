package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
)

// What an agent is told when Jira refuses or a read fails after the
// approval is Jira's sentence or the app's, never the request: no
// method, URL or status line; the URL names the site's host. A refusal
// Jira says only of fields gives each field's sentence by the field's
// name. Each
// case arms its failure while the approval waits, so it meets the read
// after the approval or the sends.
func TestTrackerAgentAnswersNameNoRequest(t *testing.T) {
	t.Parallel()
	sprint := map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "set": map[string]any{"sprint": "Sprint 2"}, "comment": "Planned."}
	refuse := func(route string, status int, body string) func(*testing.T, *trackerBoardFixture) {
		return func(t *testing.T, f *trackerBoardFixture) {
			if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: route, Kind: trackerfake.FaultRefuse,
				Status: status, Body: json.RawMessage(body)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name string
		args map[string]any
		fail func(*testing.T, *trackerBoardFixture)
		says string
	}{
		{"Jira refuses the sprint move", sprint,
			refuse("POST rest/agile/1.0/sprint/{sprint}/issue", 400, `{"errorMessages":["The sprint cannot take these issues."],"errors":{}}`),
			`"refused":"sprint: The sprint cannot take these issues."`},
		{"Jira refuses the sprint move by a field alone", sprint,
			refuse("POST rest/agile/1.0/sprint/{sprint}/issue", 400, `{"errorMessages":[],"errors":{"issues":"These issues cannot be moved."}}`),
			`"refused":"sprint: the tracker refused the write (HTTP 400); issues: These issues cannot be moved."`},
		{"Jira refuses the title by its field alone", map[string]any{"keys": []string{"WEB-1", "WEB-2"}, "set": map[string]any{"title": "Renamed"}},
			refuse("PUT rest/api/3/issue/{issue}", 400, `{"errorMessages":[],"errors":{"summary":"The summary is too long."}}`),
			`"refused":"title: the tracker refused the write (HTTP 400); title: The summary is too long."`},
		{"the read after the approval fails", sprint,
			refuse("POST rest/api/3/issue/bulkfetch", 500, `{"errorMessages":["Internal server error"],"errors":{}}`),
			"Nothing was sent: the tickets could not be read again after the user approved. example.atlassian.net: Jira answered HTTP 500"},
		{"the read after the approval is refused", sprint,
			refuse("POST rest/api/3/issue/bulkfetch", 400, `{"errorMessages":["The request cannot be read."],"errors":{}}`),
			"Nothing was sent: the tickets could not be read again after the user approved. example.atlassian.net: Jira answered HTTP 400"},
		{"the move is no longer offered", map[string]any{"keys": []string{"WEB-2", "WEB-3"}, "set": map[string]any{"status": "Done"}},
			func(t *testing.T, f *trackerBoardFixture) {
				world := f.e.Snapshot()
				world.Jira[0].Token = trackerTestToken
				p := &world.Jira[0].Projects[0]
				n := len(p.Transitions)
				p.Transitions = slices.DeleteFunc(p.Transitions, func(tr trackerfake.JiraTransition) bool { return tr.To == "10002" })
				if n == len(p.Transitions) {
					t.Fatalf("no move to Done among %+v", p.Transitions)
				}
				if err := f.e.Seed(world); err != nil {
					t.Fatal(err)
				}
			},
			`"refused":"status: it changed on the tracker after it was read; read the ticket again before you change it."`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: waitSite()})
			t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
			thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
			readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
			done := trackerWriteCall(t, f.a, endpoint, "ticket_update", c.args)
			open, _ := trackerWritePrompt(t, f.a, thread.ID)
			c.fail(t, f)
			if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Shown: true, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			result := <-done
			text, _ := result[0].(string)
			if !strings.Contains(text, c.says) || strings.Contains(text, "http") || strings.Contains(text, "://") {
				t.Errorf("ticket_update = %s\nwant it to say %q and name no request", text, c.says)
			}
		})
	}
}
