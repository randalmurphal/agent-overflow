package store

import (
	"database/sql"
	"strings"
	"testing"
)

// TestMigrationV36PendingBackgroundTaskTerminals verifies the schema
// shape of the new stash table: PK on (thread_id, task_id), nullable
// exit_code/end_time, defaulted tool_use_id/output_file, and the two
// indexes the tray query and startup recovery rely on.
func TestMigrationV36PendingBackgroundTaskTerminals(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query(`PRAGMA table_info(pending_background_task_terminals)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()

	type col struct {
		name    string
		typ     string
		notnull int
		dflt    sql.NullString
		pk      int
	}
	got := map[string]col{}
	for rows.Next() {
		var (
			cid     int
			name    string
			typ     string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		got[name] = col{name: name, typ: typ, notnull: notnull, dflt: dflt, pk: pk}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}

	want := []struct {
		name    string
		typ     string
		notnull int
		pk      int
	}{
		{"thread_id", "TEXT", 1, 1},
		{"task_id", "TEXT", 1, 2},
		{"tool_use_id", "TEXT", 1, 0},
		{"status", "TEXT", 1, 0},
		{"exit_code", "INTEGER", 0, 0},
		{"output_file", "TEXT", 1, 0},
		{"end_time", "INTEGER", 0, 0},
		{"source", "TEXT", 1, 0},
		{"created_at", "INTEGER", 1, 0},
	}
	for _, w := range want {
		c, ok := got[w.name]
		if !ok {
			t.Errorf("missing column %s", w.name)
			continue
		}
		if c.typ != w.typ {
			t.Errorf("column %s type = %q, want %q", w.name, c.typ, w.typ)
		}
		if c.notnull != w.notnull {
			t.Errorf("column %s notnull = %d, want %d", w.name, c.notnull, w.notnull)
		}
		if c.pk != w.pk {
			t.Errorf("column %s pk = %d, want %d", w.name, c.pk, w.pk)
		}
	}

	var idxSQL sql.NullString
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`,
		"idx_pending_terminals_tool_use",
	).Scan(&idxSQL); err != nil {
		t.Fatalf("look up index: %v", err)
	}
	if !idxSQL.Valid {
		t.Error("index idx_pending_terminals_tool_use missing")
	}

	var partialIndex sql.NullString
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_pending_terminals_tool_use'`,
	).Scan(&partialIndex); err != nil {
		t.Fatalf("look up partial index: %v", err)
	}
	if !strings.Contains(partialIndex.String, "WHERE tool_use_id <> ''") {
		t.Errorf("expected partial index predicate; got %q", partialIndex.String)
	}
}

func TestUpsertPendingBackgroundTerminalReplacesByPK(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("thread-1", "claude")); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	exit := int64(0)
	endTime := int64(1000)
	first := PendingBackgroundTaskTerminal{
		ThreadID:   "thread-1",
		TaskID:     "task-1",
		ToolUseID:  "toolu_first",
		Status:     "completed",
		ExitCode:   &exit,
		OutputFile: "/tmp/first.output",
		EndTime:    &endTime,
		Source:     "task_updated",
		CreatedAt:  1,
	}
	if err := s.UpsertPendingBackgroundTerminal(first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}

	// Replay the same task_updated with richer data — INSERT OR REPLACE
	// should preserve the PK and overwrite mutable fields.
	exit2 := int64(0)
	endTime2 := int64(2000)
	second := PendingBackgroundTaskTerminal{
		ThreadID:   "thread-1",
		TaskID:     "task-1",
		ToolUseID:  "toolu_second",
		Status:     "completed",
		ExitCode:   &exit2,
		OutputFile: "/tmp/second.output",
		EndTime:    &endTime2,
		Source:     "task_updated",
		CreatedAt:  2,
	}
	if err := s.UpsertPendingBackgroundTerminal(second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pending_background_task_terminals
		   WHERE thread_id = ? AND task_id = ?`,
		"thread-1", "task-1",
	).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 after replace", count)
	}

	got, found, err := s.GetPendingBackgroundTerminal("thread-1", "task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found {
		t.Fatalf("expected row to exist")
	}
	if got.ToolUseID != "toolu_second" {
		t.Errorf("tool_use_id = %q, want toolu_second (replaced)", got.ToolUseID)
	}
	if got.OutputFile != "/tmp/second.output" {
		t.Errorf("output_file = %q, want /tmp/second.output (replaced)", got.OutputFile)
	}
	if got.CreatedAt != 2 {
		t.Errorf("created_at = %d, want 2 (replaced)", got.CreatedAt)
	}
}

func TestUpsertPendingBackgroundTerminalRejectsEmptyKeys(t *testing.T) {
	s := newTestStore(t)

	cases := []PendingBackgroundTaskTerminal{
		{TaskID: "t", Status: "completed", Source: "task_updated", CreatedAt: 1},
		{ThreadID: "th", Status: "completed", Source: "task_updated", CreatedAt: 1},
		{ThreadID: "th", TaskID: "t", Source: "task_updated", CreatedAt: 1},
		{ThreadID: "th", TaskID: "t", Status: "completed", CreatedAt: 1},
	}
	for i, c := range cases {
		if err := s.UpsertPendingBackgroundTerminal(c); err == nil {
			t.Errorf("case %d: expected error, got nil", i)
		}
	}
}

func TestTakePendingBackgroundTerminalAtomicRead(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("thread-1", "claude")); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	exit := int64(0)
	t1 := PendingBackgroundTaskTerminal{
		ThreadID: "thread-1", TaskID: "task-a",
		Status: "completed", ExitCode: &exit, Source: "task_updated", CreatedAt: 10,
	}
	if err := s.UpsertPendingBackgroundTerminal(t1); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, found, err := s.TakePendingBackgroundTerminal("thread-1", "task-a")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if !found {
		t.Fatalf("expected row to exist")
	}
	if got.TaskID != "task-a" || got.Status != "completed" {
		t.Errorf("got = %+v, want task-a/completed", got)
	}

	// Second take should miss — the row has been deleted.
	if _, found, err := s.TakePendingBackgroundTerminal("thread-1", "task-a"); err != nil {
		t.Fatalf("take after delete: %v", err)
	} else if found {
		t.Errorf("expected row to be gone after first take")
	}

	// Get should also miss.
	if _, found, err := s.GetPendingBackgroundTerminal("thread-1", "task-a"); err != nil {
		t.Fatalf("get after delete: %v", err)
	} else if found {
		t.Errorf("expected row to be gone after take")
	}
}

func TestTakePendingBackgroundTerminalEmptyKeysReturnNotFound(t *testing.T) {
	s := newTestStore(t)
	for _, args := range [][2]string{
		{"", "task-1"},
		{"thread-1", ""},
		{"", ""},
	} {
		_, found, err := s.TakePendingBackgroundTerminal(args[0], args[1])
		if err != nil {
			t.Errorf("take %v: %v", args, err)
		}
		if found {
			t.Errorf("take %v: expected not found", args)
		}
	}
}

// TestDeleteThreadCascadesPendingTerminals pins the FK CASCADE
// invariant: deleting a thread MUST sweep its stashed terminals so
// the table can't outlive the thread it references.
func TestDeleteThreadCascadesPendingTerminals(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateThread(makeThread("thread-1", "claude")); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if err := s.UpsertPendingBackgroundTerminal(PendingBackgroundTaskTerminal{
		ThreadID: "thread-1", TaskID: "task-a", Status: "completed",
		Source: "task_updated", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("seed stash: %v", err)
	}

	if err := s.DeleteThread("thread-1"); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	if _, found, err := s.GetPendingBackgroundTerminal("thread-1", "task-a"); err != nil {
		t.Fatalf("get post-delete: %v", err)
	} else if found {
		t.Error("expected FK CASCADE to sweep stash row")
	}
}
