package store

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
)

func TestAgentWakeStderrMigration(t *testing.T) {
	path := fileDBThrough(t, agentWakeStderrMigrationVersion-1)
	db, err := sql.Open("sqlite", poolDSN(path, writerConnPragmas))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at) VALUES ('p', '/p', 'p', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at, archived) VALUES
		('agent', 'p', 'Agent', 'claude', '/p', '', 1, 1, 0)`)
	mustExec(t, db, `INSERT INTO agent_wakes (id, thread_id, kind, label, command, interval_seconds, next_at, last_outcome, state, created_at)
		VALUES ('w', 'agent', 'watch', 'poll', 'true', 60, 5, 'no output', 'armed', 1)`)
	before, err := queryIDs(db, `SELECT id || '|' || label || '|' || last_outcome || '|' || state FROM agent_wakes`)
	if err != nil {
		t.Fatal(err)
	}

	if err := applyMigration(db, migrationByVersion(t, agentWakeStderrMigrationVersion)); err != nil {
		t.Fatalf("apply v%d: %v", agentWakeStderrMigrationVersion, err)
	}

	if got, err := queryIDs(db, `SELECT id || '|' || label || '|' || last_outcome || '|' || state FROM agent_wakes`); err != nil {
		t.Fatal(err)
	} else if !slices.Equal(got, before) {
		t.Errorf("rows changed:\n got %q\nwant %q", got, before)
	}
	got, err := queryIDs(db, `SELECT name || '|' || type || '|' || "notnull" || '|' || COALESCE(dflt_value, 'NULL')
		FROM pragma_table_info('agent_wakes') WHERE name = 'last_stderr'`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"last_stderr|TEXT|1|''"}; !slices.Equal(got, want) {
		t.Errorf("last_stderr column = %q, want %q", got, want)
	}
	if stderr, err := queryIDs(db, `SELECT last_stderr FROM agent_wakes WHERE id = 'w'`); err != nil || !slices.Equal(stderr, []string{""}) {
		t.Errorf("existing row's last_stderr = %q, %v; want empty", stderr, err)
	}
}

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
