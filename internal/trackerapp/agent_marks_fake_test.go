package trackerapp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
)

// renameOnJira renames issue on the fake site as Jira's own screen does,
// as the connection's account: the mirror does not hear of it.
func renameOnJira(t *testing.T, s *fakeScenario, issue, title string) {
	t.Helper()
	base, err := s.origin(store.TrackerKindJira, "example.atlassian.net")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"fields": map[string]any{"summary": title}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/rest/api/3/issue/"+issue, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("user1@example.com", "fake-jira-token-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename on Jira: %s", resp.Status)
	}
}

// A history mark matches what the agent's step changed: a person's title
// change on Jira's own screen, from the same account, just before the
// agent's move keeps no mark, and the move's status change is the agent's.
func TestAnAgentsMoveMarksOnlyTheStatusChange(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	renameOnJira(t, s, "10101", "First story, renamed")
	s.clock.advance(10 * time.Second)
	moves := must[[]MoveView](t)(s.svc.Moves(t.Context(), conn.ID, "10101"))
	done := slices.IndexFunc(moves, func(m MoveView) bool { return m.To.ID == "10002" })
	if mv := must[WriteView](t)(s.svc.Move(ByAgent(t.Context(), "thread-1", "call-1"), conn.ID, MoveRequest{Issue: "10101", Move: moves[done].ID})); mv.Parts[0].State != "applied" {
		t.Fatalf("the agent's move: %+v", mv)
	}
	d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, "10101", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	status := false
	for _, e := range d.History {
		switch {
		case e.Field == "status" && e.Agent != nil:
			status = true
		case e.Agent != nil && e.Field != "resolution":
			t.Errorf("the person's %s change is marked %+v", e.Field, e.Agent)
		}
	}
	if !status {
		t.Errorf("the agent's status change is not marked: %+v", d.History)
	}
}

// A move whose answer never came marks nothing, though it was made.
func TestAnAgentsUnknownMoveMarksNothing(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	moves := must[[]MoveView](t)(s.svc.Moves(t.Context(), conn.ID, "10101"))
	done := slices.IndexFunc(moves, func(m MoveView) bool { return m.To.ID == "10002" })
	origin := trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}
	if err := s.e.AddFault(trackerfake.Fault{Origin: origin, Route: "POST rest/api/3/issue/{issue}/transitions", Kind: trackerfake.FaultFailAfterApply, Times: 1}); err != nil {
		t.Fatal(err)
	}
	if mv := must[WriteView](t)(s.svc.Move(ByAgent(t.Context(), "thread-1", "call-1"), conn.ID, MoveRequest{Issue: "10101", Move: moves[done].ID})); mv.Parts[0].State != "unknown" {
		t.Fatalf("the agent's move: %+v", mv)
	}
	d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, "10101", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range d.History {
		if e.Agent != nil {
			t.Errorf("history event %+v is marked by a write of unknown outcome", e)
		}
	}
}

// A page of comments read on its own, as Show earlier comments reads one,
// marks the agent's comment as the ticket's view does.
func TestACommentsPageMarksTheAgentsComment(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-1"), conn.ID, CommentRequest{Issue: "10101", Markdown: "The agent's comment"}))
	must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10101", Markdown: "A person's comment"}))
	page := must[CommentPageView](t)(s.svc.Comments(t.Context(), conn.ID, "10101", ""))
	marked := 0
	for _, c := range page.Comments {
		if c.Thread != nil {
			marked++
			if c.Thread.ID != "thread-1" {
				t.Errorf("comment %s is marked %+v", c.ID, c.Thread)
			}
		}
	}
	if len(page.Comments) != 2 || marked != 1 {
		t.Fatalf("%d of %d comments are marked, want 1 of 2", marked, len(page.Comments))
	}
}
