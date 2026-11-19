package store

import (
	"slices"
	"strings"
	"testing"
)

// A watch keeps the tail of its last stderr until a cancel freezes it or a
// registration replaces the watch; a loop has none to keep.
func TestAgentWakeKeepsItsStderrTail(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	watch, err := s.RegisterAgentWake(AgentWake{ID: "w", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "true", IntervalSeconds: 60, NextAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	loop, err := s.RegisterAgentWake(AgentWake{ID: "l", ThreadID: "t", Kind: AgentWakeLoop, Label: "board", Note: "n", IntervalSeconds: 60, NextAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.NoteAgentWakeStderr(watch.ID, "poll: checked\n"); err != nil || !ok {
		t.Fatalf("note stderr: %v %v", ok, err)
	}
	if ok, err := s.NoteAgentWakeStderr(loop.ID, "loops have none"); err != nil || ok {
		t.Fatalf("note stderr on a loop: %v %v, want no change", ok, err)
	}
	if _, err := s.NoteAgentWakeStderr(watch.ID, strings.Repeat("x", MaxAgentWakeStderrBytes+1)); err == nil {
		t.Fatal("a stderr past the cap was accepted")
	}
	row, _, err := s.GetAgentWake(watch.ID)
	if err != nil || row.LastStderr != "poll: checked\n" {
		t.Fatalf("watch stderr = %q, %v", row.LastStderr, err)
	}
	again, err := s.RegisterAgentWake(AgentWake{ID: "w2", ThreadID: "t", Kind: AgentWakeWatch, Label: "poll", Command: "false", IntervalSeconds: 60, NextAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if again.LastStderr != "" {
		t.Fatalf("re-registered watch kept stderr %q", again.LastStderr)
	}
	if _, cancelled, err := s.CancelAgentWake("t", "poll", "", "cancelled"); err != nil || !cancelled {
		t.Fatalf("cancel: %v %v", cancelled, err)
	}
	if ok, err := s.NoteAgentWakeStderr(watch.ID, "late"); err != nil || ok {
		t.Fatalf("note stderr on a cancelled watch: %v %v, want no change", ok, err)
	}
}

func TestListOpenThreadReminders(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	mustCreateThread(t, s, "t")
	mustCreateThread(t, s, "u")
	insert := func(token, caller, kind, state string, due int64) {
		t.Helper()
		if err := s.InsertThreadRequest(ThreadRequest{
			Token: token, CallerThreadID: caller, Kind: kind, DueAt: due, State: state, CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("insert %s: %v", token, err)
		}
	}
	insert("later", "t", ThreadRequestRemind, ThreadRequestAccepted, 3000)
	insert("sooner", "t", ThreadRequestRemind, ThreadRequestAccepted, 1000)
	insert("middle", "t", ThreadRequestRemind, ThreadRequestAccepted, 2000)
	insert("fired", "t", ThreadRequestRemind, ThreadRequestFinished, 500)
	insert("other", "u", ThreadRequestRemind, ThreadRequestAccepted, 100)
	insert("send", "t", ThreadRequestSend, ThreadRequestAccepted, 0)

	rows, total, err := s.ListOpenThreadReminders("t", 2)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, row := range rows {
		tokens = append(tokens, row.Token)
	}
	if total != 3 || !slices.Equal(tokens, []string{"sooner", "middle"}) {
		t.Fatalf("reminders = %q of %d, want [sooner middle] of 3", tokens, total)
	}
	if rows, total, err := s.ListOpenThreadReminders("none", 2); err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("a thread with no reminders = %v of %d, %v", rows, total, err)
	}
}
