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
		At:       at,
	}
}

// A comment is recorded sent, found by a repeat of its call, replaced by
// its outcome in one transaction, and then marks its ticket with the
// thread's title, which a deleted thread leaves empty.
func TestAgentWritesRecordAndMark(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThreadForSearch(t, s, "t1", "Fix the retry")
	w := agentComment("t1", "c1", 1_000)
	sent, err := s.RecordAgentWriteSent(w)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.AgentWriteRepeat("t1", "d1", 1_000)
	if err != nil || !ok || got.ID != sent || got.State != AgentWriteSent {
		t.Fatalf("repeat of a sent comment = %+v, %v, %v; want row %d sent", got, ok, err, sent)
	}
	if _, ok, err := s.AgentWriteRepeat("t1", "d1", 1_001); err != nil || ok {
		t.Fatalf("repeat after its window = %v, %v; want none", ok, err)
	}
	if _, ok, err := s.AgentWriteRepeat("t2", "d1", 0); err != nil || ok {
		t.Fatalf("repeat from another thread = %v, %v; want none", ok, err)
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
	if got, ok, err := s.AgentWriteRepeat("t1", "d1", 0); err != nil || !ok || got.State != AgentWriteApplied {
		t.Fatalf("repeat after the outcome = %+v, %v, %v; want the applied row", got, ok, err)
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
	sent, err := s.RecordAgentWriteSent(agentComment("t1", "c1", 1_000))
	if err != nil {
		t.Fatal(err)
	}
	bad := agentComment("t1", "c1", 1_000)
	bad.State = "refused"
	if err := s.RecordAgentWrites(sent, []AgentWrite{bad}); !errors.Is(err, ErrInvalidAgentWrite) {
		t.Fatalf("a refused state = %v, want ErrInvalidAgentWrite", err)
	}
	if got, ok, err := s.AgentWriteRepeat("t1", "d1", 0); err != nil || !ok || got.ID != sent {
		t.Fatalf("after a refused outcome = %+v, %v, %v; want the sent row kept", got, ok, err)
	}
	if err := s.RecordAgentWrites(sent, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.AgentWriteRepeat("t1", "d1", 0); err != nil || ok {
		t.Fatalf("after a refused part = %v, %v; want no row", ok, err)
	}
	edit := agentComment("t1", "c2", 1)
	edit.Step, edit.Ticket.IssueID = "edit", ""
	if _, err := s.RecordAgentWriteSent(edit); !errors.Is(err, ErrInvalidAgentWrite) {
		t.Fatalf("an edit without its issue = %v, want ErrInvalidAgentWrite", err)
	}
}

// A create of unknown outcome that a later read finds is settled applied
// with the issue it made, and then marks that ticket.
func TestAgentWriteSettleNamesTheIssue(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	create := agentComment("t1", "c1", 1_000)
	create.Step, create.Ticket.IssueID, create.Project = "create", "", "WEB"
	sent, err := s.RecordAgentWriteSent(create)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SettleAgentWrite(sent, "10042", ""); err != nil {
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
