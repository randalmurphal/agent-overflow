package trackerapp

import (
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/store"
	"agent-overflow/internal/harness/trackerfake"
)

// agentWriteScenario is the fake Jira site with its board synced and its
// connection verified, so the connection knows its account.
func agentWriteScenario(t *testing.T) (*fakeScenario, Connection) {
	t.Helper()
	s := newFakeScenario(t, func(at time.Time) trackerfake.Fixture {
		return trackerfake.Fixture{Jira: []trackerfake.JiraSite{jiraSeed(at)}}
	}, "example.atlassian.net")
	conn := s.addJira()
	s.writeToken(conn, "fake-jira-token-1")
	scope := Scope{Connection: conn.ID, Board: "1"}
	s.listBoards(conn.ID, scope.Board)
	s.syncBoard(t, scope)
	conn = must[Connection](t)(s.svc.Verify(t.Context(), conn.ID))
	return s, conn
}

func agentWritesOn(t *testing.T, s *fakeScenario, issue string) []store.AgentWriteMark {
	t.Helper()
	conns, err := s.st.ListTrackerConnections()
	if err != nil || len(conns) != 1 {
		t.Fatalf("connections %+v, %v", conns, err)
	}
	marks, err := s.st.AgentWritesOf(store.TicketRef{Kind: conns[0].Kind, Site: conns[0].Site, IssueID: issue})
	if err != nil {
		t.Fatal(err)
	}
	return marks
}

// An agent's comment is recorded applied with the comment it made, and a
// repeat of it finds it; the ticket's view names the agent's thread on
// the comment and on the status change the agent made, and on nothing a
// person did. A person's writes record nothing.
func TestAnAgentsWritesAreRecordedAndMarkTheTicket(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	agent := ByAgent(t.Context(), "thread-1", "call-1")

	person := must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10101", Markdown: "A person's comment"}))
	v := must[WriteView](t)(s.svc.AddComment(agent, conn.ID, CommentRequest{Issue: "10101", Markdown: "The agent's comment"}))
	if len(v.Parts) != 1 || v.Parts[0].State != "applied" || v.Parts[0].Comment == "" {
		t.Fatalf("the agent's comment: %+v", v)
	}
	repeat, ok, err := s.svc.AgentRepeatOf("thread-1", CommentDigest(conn.ID, "10101", "The agent's comment"))
	if err != nil || !ok || repeat.State != store.AgentWriteApplied || repeat.Comment != v.Parts[0].Comment || repeat.Issue != "10101" {
		t.Fatalf("the repeat of the comment = %+v, %v, %v", repeat, ok, err)
	}
	if _, ok, err := s.svc.AgentRepeatOf("thread-2", CommentDigest(conn.ID, "10101", "The agent's comment")); err != nil || ok {
		t.Fatalf("another thread's repeat = %v, %v; want none", ok, err)
	}

	moves := must[[]MoveView](t)(s.svc.Moves(t.Context(), conn.ID, "10101"))
	done := slices.IndexFunc(moves, func(m MoveView) bool { return m.To.ID == "10002" })
	s.clock.advance(time.Minute)
	if mv := must[WriteView](t)(s.svc.Move(ByAgent(t.Context(), "thread-1", "call-2"), conn.ID, MoveRequest{Issue: "10101", Move: moves[done].ID})); mv.Parts[0].State != "applied" {
		t.Fatalf("the agent's move: %+v", mv)
	}
	marks := agentWritesOn(t, s, "10101")
	if len(marks) != 2 || marks[0].Step != "move" || marks[1].Step != "comment" || marks[0].Account != "acc-user1" || marks[0].After == 0 {
		t.Fatalf("the records = %+v", marks)
	}

	d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, "10101", Interactive)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Comments {
		switch c.ID {
		case v.Parts[0].Comment:
			if c.Thread == nil || c.Thread.ID != "thread-1" {
				t.Errorf("the agent's comment is marked %+v", c.Thread)
			}
		case person.Parts[0].Comment:
			if c.Thread != nil {
				t.Errorf("the person's comment is marked %+v", c.Thread)
			}
		}
	}
	// The move to Done sets the status and the resolution: both are the
	// agent's, and nothing older is.
	var statusMarked bool
	for _, e := range d.History {
		switch {
		case e.Agent == nil:
		case e.Agent.ID != "thread-1" || e.At <= marks[0].Before:
			t.Errorf("history event %+v is marked", e)
		case e.Field == "status":
			statusMarked = true
		}
	}
	if !statusMarked {
		t.Errorf("no history event is marked: %+v", d.History)
	}
}

// A comment the provider refuses before it is sent leaves no record: the
// sent row goes with the refusal.
func TestARefusedAgentCommentLeavesNoRecord(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	v := must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-1"), conn.ID, CommentRequest{Issue: "10101", Markdown: ""}))
	if len(v.Parts) == 0 || v.Parts[len(v.Parts)-1].State != "refused" {
		t.Fatalf("an empty comment: %+v", v)
	}
	if marks := agentWritesOn(t, s, "10101"); len(marks) != 0 {
		t.Fatalf("records after a refusal: %+v", marks)
	}
	if _, ok, err := s.svc.AgentRepeatOf("thread-1", CommentDigest(conn.ID, "10101", "")); err != nil || ok {
		t.Fatalf("a repeat after a refusal = %v, %v; want none", ok, err)
	}
}
