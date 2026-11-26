package store

import (
	"errors"
	"testing"
)

func agentComment(thread, call string, at int64) AgentWrite {
	return AgentWrite{
		Ticket:   TicketRef{Kind: "jira", Site: "https://example.atlassian.net", IssueID: "10001"},
		Account:  "u1",
		ThreadID: thread,
		CallID:   call,
		Step:     "comment",
		Digest:   "d1",
		Sent:     at,
		At:       at,
	}
}

// claim claims w since, failing the test on an error.
func claim(t *testing.T, s *Store, w AgentWrite, since int64) (AgentWrite, bool) {
	t.Helper()
	got, claimed, err := s.ClaimAgentWrite(w, since)
	if err != nil {
		t.Fatal(err)
	}
	return got, claimed
}

// A comment is claimed sent; a second claim of its thread and digest in
// its window finds it, one after the window or from another thread
// claims anew. Its outcome replaces it in one transaction, and then marks
// its ticket with the thread's title, which a deleted thread leaves empty.
func TestAgentWritesRecordAndMark(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "Fix the retry")
	w := agentComment("t1", "c1", 1_000)
	first, claimed := claim(t, s, w, 0)
	if !claimed || first.ID == 0 || first.State != AgentWriteSent {
		t.Fatalf("the first claim = %+v, %v", first, claimed)
	}
	sent := first.ID
	if got, claimed := claim(t, s, agentComment("t1", "c2", 1_050), 1_000); claimed || got.ID != sent || got.State != AgentWriteSent || got.Sent != 1_000 {
		t.Fatalf("a second claim in the window = %+v, %v; want row %d sent at 1000", got, claimed, sent)
	}
	if got, claimed := claim(t, s, agentComment("t2", "c3", 1_050), 1_000); !claimed || got.ID == sent {
		t.Fatalf("a claim from another thread = %+v, %v; want its own row", got, claimed)
	}
	later, claimed := claim(t, s, agentComment("t1", "c4", 5_000), 1_001)
	if !claimed || later.ID == sent {
		t.Fatalf("a claim after the window = %+v, %v; want its own row", later, claimed)
	}
	if err := s.RecordAgentWrites(later.ID, nil); err != nil {
		t.Fatal(err)
	}
	if marks, err := s.AgentWritesOf(w.Ticket); err != nil || len(marks) != 0 {
		t.Fatalf("marks while sent = %+v, %v; want none", marks, err)
	}

	applied := w
	applied.State, applied.Comment, applied.Before, applied.After = AgentWriteApplied, "c-100", 900, 1_100
	if err := s.RecordAgentWrites(sent, []AgentWrite{applied}); err != nil {
		t.Fatal(err)
	}
	marks, err := s.AgentWritesOf(w.Ticket)
	if err != nil || len(marks) != 1 {
		t.Fatalf("marks = %+v, %v; want one", marks, err)
	}
	if m := marks[0]; m.Comment != "c-100" || m.State != AgentWriteApplied || m.ThreadTitle != "Fix the retry" || m.Before != 900 || m.After != 1_100 {
		t.Fatalf("mark = %+v", m)
	}
	if got, claimed := claim(t, s, agentComment("t1", "c5", 1_100), 0); claimed || got.State != AgentWriteApplied || got.Sent != 1_000 {
		t.Fatalf("a claim after the outcome = %+v, %v; want the applied row", got, claimed)
	}

	if err := s.DeleteThread("t1"); err != nil {
		t.Fatal(err)
	}
	if marks, err := s.AgentWritesOf(w.Ticket); err != nil || len(marks) != 1 || marks[0].ThreadTitle != "" {
		t.Fatalf("marks of a deleted thread = %+v, %v; want the mark without a title", marks, err)
	}
}

// An outcome the table refuses leaves the sent row in place, so the
// write still counts as sent; a refused or not-sent part goes with no row.
func TestAgentWritesOutcomeIsOneTransaction(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	first, _ := claim(t, s, agentComment("t1", "c1", 1_000), 0)
	sent := first.ID
	bad := agentComment("t1", "c1", 1_000)
	bad.State = "refused"
	if err := s.RecordAgentWrites(sent, []AgentWrite{bad}); !errors.Is(err, ErrInvalidAgentWrite) {
		t.Fatalf("a refused state = %v, want ErrInvalidAgentWrite", err)
	}
	if got, claimed := claim(t, s, agentComment("t1", "c2", 1_000), 0); claimed || got.ID != sent {
		t.Fatalf("after a refused outcome = %+v, %v; want the sent row kept", got, claimed)
	}
	if err := s.RecordAgentWrites(sent, nil); err != nil {
		t.Fatal(err)
	}
	if got, claimed := claim(t, s, agentComment("t1", "c3", 1_000), 0); !claimed || got.CallID != "c3" {
		t.Fatalf("after a refused part = %+v, %v; want a new claim", got, claimed)
	}
	edit := agentComment("t1", "c2", 1)
	edit.Step, edit.Ticket.IssueID = "edit", ""
	if _, _, err := s.ClaimAgentWrite(edit, 0); !errors.Is(err, ErrInvalidAgentWrite) {
		t.Fatalf("an edit without its issue = %v, want ErrInvalidAgentWrite", err)
	}
	if err := s.ResendAgentWrite(sent, 0); !errors.Is(err, ErrInvalidAgentWrite) {
		t.Fatalf("a resend with no time = %v, want ErrInvalidAgentWrite", err)
	}
}

// A create sent again is measured from its new send time; one of unknown
// outcome that a later read finds is settled applied with the issue it
// made, and then marks that ticket.
func TestAgentWriteSettleNamesTheIssue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	create := agentComment("t1", "c1", 1_000)
	create.Step, create.Ticket.IssueID, create.Project = "create", "", "WEB"
	first, _ := claim(t, s, create, 0)
	if err := s.ResendAgentWrite(first.ID, 2_000); err != nil {
		t.Fatal(err)
	}
	if got, claimed := claim(t, s, create, 1_500); claimed || got.Sent != 2_000 || got.State != AgentWriteSent {
		t.Fatalf("a claim after the resend = %+v, %v; want the row sent at 2000", got, claimed)
	}
	if err := s.SettleAgentWrite(first.ID, "10042", ""); err != nil {
		t.Fatal(err)
	}
	marks, err := s.AgentWritesOf(TicketRef{Kind: "jira", Site: "https://example.atlassian.net", IssueID: "10042"})
	if err != nil || len(marks) != 1 || marks[0].State != AgentWriteApplied || marks[0].Step != "create" {
		t.Fatalf("marks of the settled create = %+v, %v", marks, err)
	}
}

// A ticket's marks are its newest agentWritesShown writes, newest first.
func TestAgentWritesOfIsBounded(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ws := make([]AgentWrite, agentWritesShown+5)
	for i := range ws {
		ws[i] = agentComment("t1", "c", int64(i+1))
		ws[i].State = AgentWriteApplied
	}
	if err := s.RecordAgentWrites(0, ws); err != nil {
		t.Fatal(err)
	}
	marks, err := s.AgentWritesOf(ws[0].Ticket)
	if err != nil || len(marks) != agentWritesShown {
		t.Fatalf("marks = %d, %v; want %d", len(marks), err, agentWritesShown)
	}
	if marks[0].At != int64(len(ws)) || marks[len(marks)-1].At != 6 {
		t.Fatalf("marks run from %d to %d, want %d to 6", marks[0].At, marks[len(marks)-1].At, len(ws))
	}
}
