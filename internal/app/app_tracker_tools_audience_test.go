package app

import (
	"fmt"
	"strings"
	"testing"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/provider"
	"agent-overflow/internal/trackerapp"
)

// An agent's comment on a Jira ticket is judged on the project a read
// made after the approval finds the ticket in, or just before the send
// when one ticket waits for nobody. WEB-3 moves into a service desk
// project after the plan read it (the mirror's card, for a comment
// alone): with one ticket before the call, with three while the approval
// waits. Its comment is not sent, since the agent named no audience and
// a public one would email the customer; the other tickets get theirs.
func TestTrackerAgentCommentAudienceIsReadAfterTheWait(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d tickets", n), func(t *testing.T) {
			t.Parallel()
			site := trackerBoardSite()
			site.Projects = append(site.Projects, trackerfake.JiraProject{ID: "10010", Key: "SD", Name: "Service", TypeKey: "service_desk"})
			f := newTrackerBoardFixtureOn(t, trackerBoardSetup{site: site})
			t.Cleanup(func() { _ = f.a.trackerMCPServer().Close() })
			thread, endpoint := trackerWriteThread(t, f, provider.RuntimeAuto)
			readBoard(t, f, f.conn.ID, "1", []trackerapp.Section{{Kind: trackerapp.SectionSprint, Sprint: "1"}, {Kind: trackerapp.SectionBacklog}})
			moveIntoServiceDesk := func() {
				world := f.e.Snapshot()
				s := &world.Jira[0]
				s.Token = trackerTestToken
				for i := range s.Issues {
					if is := &s.Issues[i]; is.ID == "10103" {
						is.Project, is.Key, is.FormerKeys = "SD", "SD-1", []string{"WEB-3"}
					}
				}
				if err := f.e.Seed(world); err != nil {
					t.Fatal(err)
				}
			}
			keys := []string{"WEB-1", "WEB-2", "WEB-3"}[3-n:]
			args := map[string]any{"keys": keys, "comment": "Checked."}
			var result [2]any
			if n == 1 {
				moveIntoServiceDesk()
				result = <-trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
			} else {
				done := trackerWriteCall(t, f.a, endpoint, "ticket_update", args)
				open, _ := trackerWritePrompt(t, f.a, thread.ID)
				moveIntoServiceDesk()
				if err := f.a.RespondToApproval(t.Context(), thread.ID, provider.ApprovalResponse{RequestID: open.RequestID, Occurrence: open.Occurrence, Decision: "allow"}); err != nil {
					t.Fatal(err)
				}
				result = <-done
			}
			if got := fakeComments(t, f, "10103"); len(got) != 0 {
				t.Errorf("the ticket in the service desk project got %q; want no comment", got)
			}
			for _, id := range []string{"10101", "10102"}[:n-1] {
				if got := fakeComments(t, f, id); len(got) != 1 {
					t.Errorf("%s got %q; want the agent's comment", id, got)
				}
			}
			text, _ := result[0].(string)
			if !strings.Contains(text, `"key":"WEB-3","refused":`) || !strings.Contains(text, "service desk") {
				t.Errorf("ticket_update = %v; want WEB-3 refused for its service desk project", result)
			}
		})
	}
}
