package trackerapp

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"agent-overflow/internal/harness/trackerfake"
	"agent-overflow/internal/store"
	"agent-overflow/internal/tracker"
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

	if person := must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10101", Markdown: "A person's comment"})); person.Repeat != nil {
		t.Fatalf("a person's comment answered as a repeat: %+v", person)
	}
	v := must[WriteView](t)(s.svc.AddComment(agent, conn.ID, CommentRequest{Issue: "10101", Markdown: "The agent's comment"}))
	if len(v.Parts) != 1 || v.Parts[0].State != "applied" || v.Repeat != nil {
		t.Fatalf("the agent's comment: %+v", v)
	}
	made := agentWritesOn(t, s, "10101")
	if len(made) != 1 || made[0].Comment == "" || made[0].Sent == 0 {
		t.Fatalf("the agent's comment's record = %+v", made)
	}
	again := must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-9"), conn.ID, CommentRequest{Issue: "10101", Markdown: "The agent's comment"}))
	if again.Repeat == nil || again.Repeat.Comment != made[0].Comment || again.Repeat.Issue != "10101" || len(again.Parts) != 0 {
		t.Fatalf("the repeat of the comment = %+v", again)
	}
	person := must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10101", Markdown: "The agent's comment"}))
	if person.Repeat != nil || len(person.Parts) != 1 || person.Parts[0].State != "applied" {
		t.Fatalf("a person's comment of the agent's text = %+v; want it sent", person)
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
		switch {
		case c.ID == made[0].Comment:
			if c.Thread == nil || c.Thread.ID != "thread-1" {
				t.Errorf("the agent's comment is marked %+v", c.Thread)
			}
		case c.Thread != nil:
			t.Errorf("the person's comment %s is marked %+v", c.ID, c.Thread)
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
	if again := must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-2"), conn.ID, CommentRequest{Issue: "10101", Markdown: ""})); again.Repeat != nil {
		t.Fatalf("a repeat after a refusal = %+v; want it sent", again)
	}
}

// interrupted records, as an earlier run of the app left it, a comment of
// markdown on issue sent at sent by the thread's agent and never answered.
func interrupted(t *testing.T, s *fakeScenario, conn Connection, issue, markdown string, sent time.Time) {
	t.Helper()
	row, err := s.st.GetTrackerConnection(conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := commentOf(tracker.IssueID(issue), markdown).digest(row.Kind, row.Site)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := s.st.ClaimAgentWrite(store.AgentWrite{
		Ticket: store.TicketRef{Kind: row.Kind, Site: row.Site, IssueID: issue}, Account: row.VerifiedAccountID,
		ThreadID: "thread-1", CallID: "call-0", Step: "comment", Digest: digest, Sent: sent.UnixMilli(), At: sent.UnixMilli(),
	}, 0); err != nil || !claimed {
		t.Fatalf("the interrupted row: %v, %v", claimed, err)
	}
}

// A comment an earlier run left sent is looked for: one on the ticket by
// the account, near its send, whose letters and digits are the text sent
// is the earlier attempt's, and nothing is sent; one whose text differs is
// not, and the comment is sent; a lookup that fails sends nothing.
func TestAnInterruptedAgentCommentIsLookedFor(t *testing.T) {
	t.Parallel()
	s, conn := agentWriteScenario(t)
	// The fake's clock runs an hour behind the service's: a comment the
	// fake stores now is made at the service's time.
	s.clock.advance(time.Hour)
	// The earlier attempt landed as the tracker stored it.
	landed := must[WriteView](t)(s.svc.AddComment(t.Context(), conn.ID, CommentRequest{Issue: "10101", Markdown: "Fixed: the *login* form, again!"}))
	if landed.Parts[0].State != "applied" {
		t.Fatalf("the landed comment: %+v", landed)
	}
	interrupted(t, s, conn, "10101", "Fixed the login form again.", s.svc.now())
	v := must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-1"), conn.ID, CommentRequest{Issue: "10101", Markdown: "Fixed the login form again."}))
	if v.Repeat == nil || v.Repeat.Comment == "" || len(v.Parts) != 0 {
		t.Fatalf("the repeat of a landed comment = %+v", v)
	}
	marks := agentWritesOn(t, s, "10101")
	if len(marks) != 1 || marks[0].Comment != v.Repeat.Comment || marks[0].State != store.AgentWriteApplied {
		t.Fatalf("the settled record = %+v", marks)
	}

	interrupted(t, s, conn, "10101", "Checked on Safari.", s.svc.now())
	v = must[WriteView](t)(s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-2"), conn.ID, CommentRequest{Issue: "10101", Markdown: "Checked on Safari."}))
	if v.Repeat != nil || len(v.Parts) != 1 || v.Parts[0].State != "applied" {
		t.Fatalf("a comment not found = %+v; want it sent", v)
	}

	interrupted(t, s, conn, "10102", "Looked.", s.svc.now())
	if err := s.e.AddFault(trackerfake.Fault{Origin: trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"},
		Route: "GET rest/api/3/issue/{issue}/comment", Kind: trackerfake.FaultRefuse, Status: 500, Body: json.RawMessage(`{"errorMessages":["down"]}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.svc.AddComment(ByAgent(t.Context(), "thread-1", "call-3"), conn.ID, CommentRequest{Issue: "10102", Markdown: "Looked."}); !errors.Is(err, ErrAgentRepeatUnchecked) {
		t.Fatalf("a lookup that failed = %v, want ErrAgentRepeatUnchecked", err)
	}
	s.e.ClearFaults(trackerfake.Origin{Tracker: trackerfake.TrackerJira, Site: "example.atlassian.net"}, "GET rest/api/3/issue/{issue}/comment")
	for _, issue := range []string{"10102"} {
		d, _, err := s.svc.IssueDetail(t.Context(), conn.ID, tracker.IssueID(issue), Interactive)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Comments) != 0 {
			t.Fatalf("a lookup that failed sent %+v", d.Comments)
		}
	}
}
