package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// Schema and runner invariants for the squashed v1 migration.
//
// Transition tests for the pre-v0.0.1 migration chain (v1..v51) were
// dropped when those migrations were squashed; cmd/db-rebake rewrites
// existing databases to a single (1, 'initial_schema') row, so there
// is nothing to step forward through any more.

func openSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- Schema smoke ---

func TestMigrationFreshDB(t *testing.T) {
	s := newTestStore(t)

	tables := []string{
		"migration_versions", "threads", "items", "payloads",
		"channels", "channel_messages", "discussion_definitions",
		"attachments", "thread_drafts", "thread_checkpoints", "turns",
		"proposed_plans", "proposed_plan_comments",
		"chat_bar_favorites", "chat_model_profiles",
		"diff_review_comments", "pending_background_task_terminals",
		"projects", "thread_tracked_files",
	}
	for _, table := range tables {
		var name string
		err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}

	for _, table := range []string{
		"design_snapshots", "design_artifacts",
		"new_thread_drafts", "draft_attachments",
		"mcp_servers",
	} {
		var dropped string
		err := s.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?",
			table,
		).Scan(&dropped)
		if err == nil {
			t.Errorf("%s should be absent on fresh DB, got %q", table, dropped)
		}
	}
}

func TestMigrationVersionTrackingPostSquash(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query("SELECT version, name FROM migration_versions ORDER BY version")
	if err != nil {
		t.Fatalf("query migration_versions: %v", err)
	}
	defer rows.Close()

	type row struct {
		version int
		name    string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.version, &r.name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	if len(got) == 0 {
		t.Fatalf("expected migration_versions to be populated, got %d rows", len(got))
	}
	if got[0].version != 1 || got[0].name != "initial_schema" {
		t.Fatalf("expected first row (1, initial_schema), got (%d, %q)", got[0].version, got[0].name)
	}
	for i := 1; i < len(got); i++ {
		if got[i].version != got[i-1].version+1 {
			t.Fatalf("migration_versions has a gap: row %d has version %d after %d", i, got[i].version, got[i-1].version)
		}
	}
}

// --- Runner mechanics ---

func TestMigrationIdempotent(t *testing.T) {
	db := openSQLiteDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("first migration run: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second migration run: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM migration_versions").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != len(migrations) {
		t.Errorf("expected %d version rows after idempotent re-run, got %d", len(migrations), count)
	}
}

func TestCurrentMigrationVersionEmptyTable(t *testing.T) {
	db := openSQLiteDB(t)
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	version, err := currentMigrationVersion(db)
	if err != nil {
		t.Fatalf("current migration version: %v", err)
	}
	if version != 0 {
		t.Fatalf("expected version 0 for empty tracking table, got %d", version)
	}
}

func TestConfigureDatabaseEnablesWAL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-probe.db")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestConfigureDatabaseWarnsOnFallback(t *testing.T) {
	db := openSQLiteDB(t)

	var logBuf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(previous) })

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configureDatabase: %v", err)
	}

	output := logBuf.String()
	if !strings.Contains(output, "journal_mode=WAL returned") {
		t.Errorf("expected fallback warning in log, got: %q", output)
	}
	if strings.Contains(output, `returned "wal"`) {
		t.Errorf("unexpected: in-memory DB reported WAL journaling: %q", output)
	}
}

func TestForeignKeysEnabledAfterMigrations(t *testing.T) {
	s := newTestStore(t)
	var fk int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys pragma: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}
}

// --- threads CHECK constraints ---

func TestThreadsModeCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-bogus', ?, 'Bogus', 'claude', '/tmp', '', 1, 1, 0, 'plann')
	`, defaultTestProjectID)
	if err == nil {
		t.Fatal("INSERT with mode='plann' must violate CHECK constraint")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-ok', ?, 'Ok', 'claude', '/tmp', '', 1, 1, 0, 'plan')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("valid INSERT: %v", err)
	}

	if _, err := s.db.Exec(`UPDATE threads SET mode = 'xyz' WHERE id = 't-ok'`); err == nil {
		t.Fatal("UPDATE to bogus mode must fail")
	}
}

func TestThreadsProviderCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-bogus-prov', ?, 'Bogus', 'xyz', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID)
	if err == nil {
		t.Fatal("INSERT with provider='xyz' must violate CHECK constraint")
	}

	for _, p := range []string{"claude", "codex"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-ok-"+p, defaultTestProjectID, "Ok", p, "/tmp", "", 1, 1, 0, "chat"); err != nil {
			t.Errorf("INSERT with provider=%q: %v", p, err)
		}
	}

	if _, err := s.db.Exec(`UPDATE threads SET provider = 'nope' WHERE id = 't-ok-claude'`); err == nil {
		t.Error("UPDATE to bogus provider must fail")
	}
}

func TestThreadsRuntimeModeCheckRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, runtime_mode)
		VALUES ('t-bogus-runtime', ?, 'Bogus', 'claude', '/tmp', '', 1, 1, 0, 'chat', 'yolo')
	`, defaultTestProjectID)
	if err == nil {
		t.Fatal("INSERT with runtime_mode='yolo' must violate CHECK constraint")
	}

	for _, m := range []string{"approval-required", "auto-accept-edits", "full-access"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, runtime_mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-ok-"+m, defaultTestProjectID, "Ok", "claude", "/tmp", "", 1, 1, 0, "chat", m); err != nil {
			t.Errorf("INSERT with runtime_mode=%q: %v", m, err)
		}
	}

	if _, err := s.db.Exec(`UPDATE threads SET runtime_mode = 'nope' WHERE id = 't-ok-full-access'`); err == nil {
		t.Error("UPDATE to bogus runtime_mode must fail")
	}
}

func TestThreadsRuntimeModeDefaultSeedsFullAccess(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-default-rm', ?, 'Default', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("INSERT without runtime_mode: %v", err)
	}

	var got string
	if err := s.db.QueryRow(`SELECT runtime_mode FROM threads WHERE id = 't-default-rm'`).Scan(&got); err != nil {
		t.Fatalf("select runtime_mode: %v", err)
	}
	if got != "full-access" {
		t.Errorf("runtime_mode default = %q, want full-access", got)
	}
}

func TestThreadsReasoningEffortCheck(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, reasoning_effort)
		VALUES ('t-bogus-eff', ?, 'Bogus', 'claude', '/tmp', '', 1, 1, 0, 'chat', 'ultranope')
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT with reasoning_effort='ultranope' must violate CHECK constraint")
	}

	for _, eff := range []string{"low", "medium", "high", "xhigh", "max"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, reasoning_effort)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-claude-eff-"+eff, defaultTestProjectID, "Ok", "claude", "/tmp", "", 1, 1, 0, "chat", eff); err != nil {
			t.Errorf("INSERT claude reasoning_effort=%q: %v", eff, err)
		}
	}
	for _, eff := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, reasoning_effort)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-codex-eff-"+eff, defaultTestProjectID, "Ok", "codex", "/tmp", "", 1, 1, 0, "chat", eff); err != nil {
			t.Errorf("INSERT codex reasoning_effort=%q: %v", eff, err)
		}
	}
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, reasoning_effort)
		VALUES ('t-codex-max', ?, 'Bad', 'codex', '/tmp', '', 1, 1, 0, 'chat', 'max')
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT codex with reasoning_effort='max' must violate CHECK constraint")
	}
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, reasoning_effort)
		VALUES ('t-claude-minimal', ?, 'Bad', 'claude', '/tmp', '', 1, 1, 0, 'chat', 'minimal')
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT claude with reasoning_effort='minimal' must violate CHECK constraint")
	}
}

func TestThreadsContextWindowCheck(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, context_window)
		VALUES ('t-cw-bad', ?, 'Bad', 'claude', '/tmp', '', 1, 1, 0, 'chat', -1)
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT with context_window=-1 must violate CHECK constraint")
	}

	for _, cw := range []int{200000, 272000, 500000, 1000000, 1050000} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, context_window)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, fmt.Sprintf("t-cw-%d", cw), defaultTestProjectID, "Ok", "claude", "/tmp", "", 1, 1, 0, "chat", cw); err != nil {
			t.Errorf("INSERT with context_window=%d: %v", cw, err)
		}
	}
}

func TestThreadsLastTokenUsageCheck(t *testing.T) {
	s := newTestStore(t)

	cases := []struct {
		name      string
		value     string
		wantError bool
	}{
		{"empty string", "", false},
		{"empty object", "{}", false},
		{"populated json", `{"input_tokens":1234,"output_tokens":7}`, false},
		{"garbage", "not json", true},
		{"truncated", `{"a":`, true},
	}
	for i, tc := range cases {
		id := fmt.Sprintf("t-ltu-%d", i)
		_, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, model, workspace_path,
				mode, reasoning_effort, created_at, updated_at, archived, last_token_usage)
			VALUES (?, ?, ?, 'claude', '', '/tmp', 'chat', 'high', 1, 1, 0, ?)
		`, id, defaultTestProjectID, tc.name, tc.value)
		if tc.wantError && err == nil {
			t.Errorf("%s: INSERT with last_token_usage=%q must violate CHECK", tc.name, tc.value)
		}
		if !tc.wantError && err != nil {
			t.Errorf("%s: INSERT with last_token_usage=%q failed: %v", tc.name, tc.value, err)
		}
	}
}

// --- items CHECK constraints ---

func TestItemsDecisionCheckRejectsTimeout(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-dec', ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-timeout', 't-dec', 0, 0, 'tool_call', 'assistant', 'completed',
			'', '', 0, '', 'Bash', 'timeout', '{}', 1, 1)
	`); err == nil {
		t.Fatal("INSERT with decision='timeout' must violate CHECK (unreachable value, removed v0.0.1)")
	}

	for i, d := range []string{"", "approved", "declined", "amended", "lost"} {
		id := fmt.Sprintf("i-dec-%d", i)
		if _, err := s.db.Exec(`
			INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
				summary, parent_id, is_background, completion_of, tool_name, decision,
				meta, created_at, updated_at)
			VALUES (?, 't-dec', 0, ?, 'tool_call', 'assistant', 'completed',
				'', '', 0, '', 'Bash', ?, '{}', 1, 1)
		`, id, i, d); err != nil {
			t.Errorf("INSERT with decision=%q: %v", d, err)
		}
	}
}

func TestItemsKindCheckAcceptsTerminalInteraction(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-ti', ?, 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-waited', 't-ti', 0, 0, 'terminal_interaction', 'assistant', 'completed',
			'Waited for background terminal', '', 0, '', '', '', '{}', 1, 1)
	`); err != nil {
		t.Fatalf("INSERT with kind='terminal_interaction': %v", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-bogus-kind', 't-ti', 1, 0, 'not_a_kind', 'assistant', 'completed',
			'Bogus', '', 0, '', '', '', '{}', 1, 1)
	`); err == nil {
		t.Error("INSERT with bogus kind must violate CHECK")
	}
}

func TestItemsStatusCheckAcceptsKilled(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-k', ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-killed', 't-k', 0, 0, 'tool_completion', 'assistant', 'killed',
			'Stopped by user', '', 1, 'i-launch', 'Bash', '', '{}', 1, 1)
	`); err != nil {
		t.Fatalf("INSERT with status='killed': %v", err)
	}

	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-stopped', 't-k', 1, 0, 'tool_completion', 'assistant', 'stopped',
			'Bogus', '', 0, '', 'Bash', '', '{}', 1, 1)
	`); err == nil {
		t.Error("INSERT with status='stopped' must violate CHECK")
	}
}

// --- UNIQUE constraints ---

func TestProjectsPathUnique(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p1', '/tmp/conflict', '/tmp/conflict', 1, 1)
	`); err != nil {
		t.Fatalf("first INSERT: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p2', '/tmp/conflict', '/tmp/conflict', 1, 1)
	`); err == nil {
		t.Fatal("INSERT with duplicate path must violate UNIQUE constraint")
	}
}

func TestChannelMessagesSequenceUnique(t *testing.T) {
	s := newTestStore(t)

	_, _ = s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', ?, 'T', 'claude', '/tmp', '', 1000, 1000)`, defaultTestProjectID)
	_, _ = s.db.Exec(`INSERT INTO channels (id, thread_id, type, status, created_at, updated_at)
		VALUES ('ch-1', 't1', 'deliberation', 'open', 1000, 1000)`)

	if _, err := s.db.Exec(`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, content, created_at)
		VALUES ('m1', 'ch-1', 1, 'agent', 'a1', 'first', 1000)`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, content, created_at)
		VALUES ('m2', 'ch-1', 1, 'agent', 'a2', 'dupe', 1000)`); err == nil {
		t.Error("expected UNIQUE violation for duplicate channel+sequence")
	}
}

func TestThreadCheckpointsUniqueThreadUserItem(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t-chk-unique")

	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-a', 't-chk-unique', 't-chk-unique-user:1', 1, 'refs/a', 1000, '/tmp')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-b', 't-chk-unique', 't-chk-unique-user:1', 1, 'refs/b', 2000, '/tmp')`); err == nil {
		t.Error("expected UNIQUE violation on (thread_id, user_item_id)")
	}

	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-c', 't-chk-unique', 't-chk-unique-user:2', 2, 'refs/c', 3000, '/tmp')`); err != nil {
		t.Errorf("different user_item_id must succeed: %v", err)
	}
}

// --- FK CASCADE ---

func TestProjectThreadsCascade(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-cascade', '/cascade', 'cascade', 1, 1)
	`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-cascade', 'p-cascade', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`); err != nil {
		t.Fatalf("insert thread: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = 'p-cascade'`); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM threads`).Scan(&count); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected cascade to drop threads, got %d rows", count)
	}
}

func TestThreadTurnsCascade(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-turns', ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('turn-1', 't-turns', 0, 1)`); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 't-turns'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM turns WHERE thread_id = 't-turns'`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected CASCADE to drop turn rows, still have %d", remaining)
	}
}

func TestThreadCheckpointsCascadeOnThreadDelete(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadForCheckpoint(t, s, "t-chk-casc")

	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-1', 't-chk-casc', 't-chk-casc-user:0', 0, 'refs/x', 1000, '/tmp')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 't-chk-casc'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_checkpoints`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected cascade to drop checkpoints, still have %d", count)
	}
}

// --- Index presence and query-planner usage ---

func TestPayloadIDIndexExistsAndIsPartial(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_items_payload_id'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_items_payload_id: %v", err)
	}
	if !strings.Contains(sqlText, "payload_id IS NOT NULL") {
		t.Errorf("expected partial predicate, got: %s", sqlText)
	}
}

func TestMetaTaskIDIndexUsedByPlanner(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_items_meta_task_id'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_items_meta_task_id: %v", err)
	}
	if !strings.Contains(sqlText, "json_extract(meta, '$.task_id')") {
		t.Errorf("expected index expression on json_extract(meta, '$.task_id'), got: %s", sqlText)
	}
	if !strings.Contains(sqlText, "WHERE json_extract(meta, '$.task_id') IS NOT NULL") {
		t.Errorf("expected partial predicate, got: %s", sqlText)
	}

	assertPlanUses(t, s.db, "idx_items_meta_task_id",
		`EXPLAIN QUERY PLAN
		 SELECT id FROM items
		  WHERE thread_id = ? AND json_extract(meta, '$.task_id') = ?`,
		"t", "task-1")
}

func TestTurnsTableShape(t *testing.T) {
	s := newTestStore(t)

	var indexSQL string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='turns_thread_index'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read turns_thread_index: %v", err)
	}
	if !strings.Contains(indexSQL, "turn_index DESC") {
		t.Errorf("index sql = %q, want turn_index DESC", indexSQL)
	}

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-turn', ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('t-bad', 't-turn', -1, 1)
	`); err == nil {
		t.Fatal("expected CHECK(turn_index >= 0) to reject negative index")
	}
}

func TestTurnsThreadCompletedIndexUsedByPlanner(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_turns_thread_completed'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_turns_thread_completed: %v", err)
	}
	if !strings.Contains(sqlText, "thread_id") || !strings.Contains(sqlText, "completed_at DESC") {
		t.Errorf("expected (thread_id, completed_at DESC), got: %s", sqlText)
	}
	if !strings.Contains(sqlText, "completed_at IS NOT NULL") {
		t.Errorf("expected partial predicate, got: %s", sqlText)
	}

	assertPlanUses(t, s.db, "idx_turns_thread_completed",
		`EXPLAIN QUERY PLAN
		 SELECT MAX(completed_at)
		   FROM turns
		  WHERE thread_id = ?
		    AND completed_at IS NOT NULL`,
		"thread-1")
}

func TestLiveBackgroundIndexMatchesTrayLaunchPredicate(t *testing.T) {
	s := newTestStore(t)

	sqlText := readIndexSQL(t, s.db, "idx_items_live_background")
	for _, want := range []string{
		"is_background = 1",
		"status = 'running'",
		"parent_id = ''",
		"live_background_active",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("idx_items_live_background missing %q in SQL: %s", want, sqlText)
		}
	}

	assertPlanUses(t, s.db, "idx_items_live_background",
		`EXPLAIN QUERY PLAN
		 SELECT id FROM items
		  WHERE thread_id = ?
		    AND is_background = 1
		    AND status = 'running'
		    AND parent_id = ''
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0`,
		"t")
}

func TestLiveCodexSubagentIndexUsedByPlanner(t *testing.T) {
	s := newTestStore(t)

	sqlText := readIndexSQL(t, s.db, "idx_items_live_codex_subagent")
	for _, want := range []string{
		"status = 'completed'",
		"tool_name = 'collab_agent'",
		"live_background_active",
		"$.input.tool",
		"spawn_agent",
		"spawnAgent",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("idx_items_live_codex_subagent missing %q in SQL: %s", want, sqlText)
		}
	}

	assertPlanUses(t, s.db, "idx_items_live_codex_subagent",
		`EXPLAIN QUERY PLAN
		 SELECT id FROM items
		  WHERE thread_id = ?
		    AND kind = 'tool_call'
		    AND status = 'completed'
		    AND tool_name = 'collab_agent'
		    AND is_background = 1
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    AND json_extract(meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')`,
		"t")
}

func TestThreadsPinnedAtPartialIndex(t *testing.T) {
	s := newTestStore(t)

	var indexSQL sql.NullString
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_threads_pinned_at'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("look up idx_threads_pinned_at: %v", err)
	}
	if !indexSQL.Valid {
		t.Fatalf("idx_threads_pinned_at missing")
	}
	if !strings.Contains(indexSQL.String, "WHERE pinned_at IS NOT NULL") {
		t.Errorf("expected partial predicate, got %q", indexSQL.String)
	}
}

// --- Triggers (payload GC) ---

func TestTrgItemsGCPayloadSweepsOrphans(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-gc', ?, 'T', 'claude', '/tmp', '', ?, ?, 0, 'chat')`,
		defaultTestProjectID, now, now); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('p-gc', 'thinking', '{}', x'00', ?)`, now); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, payload_id, created_at, updated_at)
		VALUES ('i-gc', 't-gc', 0, 0, 'thinking', 'assistant', '', 'p-gc', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM items WHERE id = 'i-gc'`); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p-gc'`).Scan(&count); err != nil {
		t.Fatalf("count payload: %v", err)
	}
	if count != 0 {
		t.Errorf("trg_items_gc_payload should sweep orphaned payload, got %d", count)
	}
}

func TestTrgItemsGCInputPayloadSweepsOrphans(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-gci', ?, 'T', 'claude', '/tmp', '', ?, ?, 0, 'chat')`,
		defaultTestProjectID, now, now); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('p-tool-input', 'tool_call_input', '{}', x'00', ?)`, now); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, input_payload_id, created_at, updated_at)
		VALUES ('i-tool', 't-gci', 0, 0, 'tool_call', 'assistant', 'Edit', 'p-tool-input', ?, ?)`,
		now, now); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM items WHERE id = 'i-tool'`); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p-tool-input'`).Scan(&count); err != nil {
		t.Fatalf("count payload: %v", err)
	}
	if count != 0 {
		t.Errorf("trg_items_gc_input_payload should sweep orphaned input payload, got %d", count)
	}
}

// --- chat_bar / chat_model_profiles CHECK ---

func TestChatBarFavoritesAndProfilesCHECK(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(
		`INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		 VALUES ('model', '', 'gpt-5.5', 'GPT 5.5', 1)`,
	); err == nil {
		t.Fatal("model favorite without provider must violate CHECK")
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('claude', 'claude-opus-4-7', 'bogus', 0, 1000000, 'full-access', 1)`,
	); err == nil {
		t.Fatal("profile with invalid effort must violate CHECK")
	}
}

// --- Helpers ---

func readIndexSQL(t *testing.T, db *sql.DB, indexName string) string {
	t.Helper()
	var sqlText string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = ?`,
		indexName,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read %s sql: %v", indexName, err)
	}
	return sqlText
}

func assertPlanUses(t *testing.T, db *sql.DB, indexName, query string, args ...any) {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused sql.NullInt64
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if !strings.Contains(plan.String(), indexName) {
		t.Errorf("query plan did not use %s: %s", indexName, plan.String())
	}
}
