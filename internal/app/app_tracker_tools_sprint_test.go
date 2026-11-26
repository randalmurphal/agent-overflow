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

// A sprint move of several tickets is one Jira request, refused whole:
// by the provider's check of a sprint that closed while the approval
// waited, which changes none of the tickets, so the read after the
// approval passes them; or by Jira's 400 to the request. Each ticket's
// answer says the refusal, and the comment after it is not sent for any
// of them.
func TestTrackerAgentSprintMoveRefusedWholeIsSaidForEachTicket(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		refuse  func(t *testing.T, f *trackerBoardFixture)
		refused string
	}{
		{"sprint closed", func(t *testing.T, f *trackerBoardFixture) {
			world := f.e.Snapshot()
			world.Jira[0].Token = trackerTestToken
			for i := range world.Jira[0].Sprints {
				if sp := &world.Jira[0].Sprints[i]; sp.ID == 2 {
					sp.State = "closed"
				}
			}
			if err := f.e.Seed(world); err != nil {
				t.Fatal(err)
			}
		}, "it changed on the tracker after it was read; read the ticket again before you change it."},
		{"Jira refuses", func(t *testing.T, f *trackerBoardFixture) {
			if err := f.e.AddFault(trackerfake.Fault{Origin: trackerBoardOrigin, Route: "POST rest/agile/1.0/sprint/{sprint}/issue", Kind: trackerfake.FaultRefuse,
				Status: 400, Body: json.RawMessage(`{"errorMessages":["The sprint cannot take these issues."],"errors":{}}`)}); err != nil {
				t.Fatal(err)
			}
		}, "The sprint cannot take these issues."},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: waitSite()})
			t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
			thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
			readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
			keys := []string{"WEB-1", "WEB-2", "WEB-3"}
			done := trackerWriteCall(t, f.a, endpoint, "ticket_update", map[string]any{"keys": keys, "set": map[string]any{"sprint": "Sprint 2"}, "comment": "Planned."})
			open, _ := trackerWritePrompt(t, f.a, thread.ID)
			c.refuse(t, f)
			if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			result := <-done
			text, _ := result[0].(string)
			var answer struct {
				Tickets []struct {
					Key, Refused string
					NotSent      []string `json:"not_sent"`
				}
			}
			if err := json.Unmarshal([]byte(text), &answer); err != nil || len(answer.Tickets) != len(keys) {
				t.Fatalf("ticket_update = %s (%v)", text, err)
			}
			for i, a := range answer.Tickets {
				if a.Key != keys[i] || !strings.HasPrefix(a.Refused, "sprint: ") || !strings.Contains(a.Refused, c.refused) || !slices.Equal(a.NotSent, []string{"comment"}) {
					t.Errorf("ticket %d = %+v; want %s's sprint refused saying %q and its comment not sent", i, a, keys[i], c.refused)
				}
			}
			for _, is := range f.e.Snapshot().Jira[0].Issues {
				if slices.Contains(is.Sprints, 2) || len(is.Comments) != 0 {
					t.Errorf("%s holds sprints %v and %d comments; want neither changed", is.Key, is.Sprints, len(is.Comments))
				}
			}
		})
	}
}
