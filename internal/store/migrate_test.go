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
		"migration_versions", "threads", "items", "payloads", "payload_chunks",
		"channels", "channel_messages", "discussion_definitions",
		"attachments", "thread_drafts", "thread_checkpoints", "turns",
		"proposed_plans", "proposed_plan_comments",
		"chat_bar_favorites", "chat_model_profiles", "new_thread_mcp_defaults",
		"diff_review_comments", "pending_background_task_terminals",
		"projects", "thread_tracked_files", "usage_ledger", "ui_state",
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

	for _, p := range []string{"claude", "codex", "claude-tui"} {
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
	for _, eff := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
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
		VALUES ('t-claude-minimal', ?, 'Bad', 'claude', '/tmp', '', 1, 1, 0, 'chat', 'minimal')
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT claude with reasoning_effort='minimal' must violate CHECK constraint")
	}
}

func TestMigrationV19PreservesRowsAndWidensCodexReasoningEfforts(t *testing.T) {
	db := migrateThrough(t, 18)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v19', '/tmp/v19', 'V19', 'blue', 7, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived, disabled_mcp_servers
		) VALUES (
			'thread-v19', 'project-v19', 'Preserve me', 'codex', 'gpt-5.5', '/tmp/v19', '/tmp/v19-wt',
			'feature/v19', 'github:42', 'session-v19', 'fork-v19', 'plan', 'xhigh',
			1, 272000, 65, 80, 'auto-accept-edits', '{"input_tokens":12}', 21,
			22, 23, 24, 1, '["server-a"]'
		)
	`)
	mustExec(t, db, `
		INSERT INTO items (
			id, thread_id, turn_index, item_index, kind, role, status, summary,
			parent_id, is_background, completion_of, tool_name, decision, meta,
			created_at, updated_at
		) VALUES (
			'item-v19', 'thread-v19', 1, 2, 'assistant_text', 'assistant', 'completed', 'kept',
			'', 0, '', '', '', '{}', 25, 26
		)
	`)
	mustExec(t, db, `
		INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window,
			auto_compact_standard_percent, auto_compact_extended_percent,
			runtime_mode, updated_at
		) VALUES ('codex', 'gpt-5.5', 'xhigh', 1, 272000, 60, 75, 'full-access', 27)
	`)

	if err := applyRebuildMigration(db, migrationByVersion(t, 19)); err != nil {
		t.Fatalf("apply migration v19: %v", err)
	}

	var title, model, prRef, tokenUsage, disabledServers string
	var fastMode, contextWindow, archived int
	if err := db.QueryRow(`
		SELECT title, model, pr_ref, last_token_usage, disabled_mcp_servers,
		       fast_mode, context_window, archived
		FROM threads WHERE id = 'thread-v19'
	`).Scan(&title, &model, &prRef, &tokenUsage, &disabledServers, &fastMode, &contextWindow, &archived); err != nil {
		t.Fatalf("read migrated thread: %v", err)
	}
	if title != "Preserve me" || model != "gpt-5.5" || prRef != "github:42" ||
		tokenUsage != `{"input_tokens":12}` || disabledServers != `["server-a"]` ||
		fastMode != 1 || contextWindow != 272000 || archived != 1 {
		t.Fatalf("migrated thread lost data: title=%q model=%q pr_ref=%q usage=%q disabled=%q fast=%d context=%d archived=%d",
			title, model, prRef, tokenUsage, disabledServers, fastMode, contextWindow, archived)
	}

	var itemSummary string
	if err := db.QueryRow(`SELECT summary FROM items WHERE thread_id = 'thread-v19' AND id = 'item-v19'`).Scan(&itemSummary); err != nil {
		t.Fatalf("read child item after thread rebuild: %v", err)
	}
	if itemSummary != "kept" {
		t.Fatalf("item summary = %q, want kept", itemSummary)
	}

	var profileFastMode, profileContextWindow, profileUpdatedAt int
	if err := db.QueryRow(`
		SELECT fast_mode, context_window, updated_at
		FROM chat_model_profiles WHERE provider = 'codex' AND model = 'gpt-5.5'
	`).Scan(&profileFastMode, &profileContextWindow, &profileUpdatedAt); err != nil {
		t.Fatalf("read migrated model profile: %v", err)
	}
	if profileFastMode != 1 || profileContextWindow != 272000 || profileUpdatedAt != 27 {
		t.Fatalf("migrated profile lost data: fast=%d context=%d updated=%d", profileFastMode, profileContextWindow, profileUpdatedAt)
	}

	for _, index := range []string{"idx_threads_project", "idx_threads_updated", "idx_chat_model_profiles_updated"} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&found); err != nil {
			t.Fatalf("index %s missing after migration: %v", index, err)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v19 rebuild")
	}

	mustExec(t, db, `UPDATE threads SET reasoning_effort = 'max' WHERE id = 'thread-v19'`)
	mustExec(t, db, `UPDATE chat_model_profiles SET reasoning_effort = 'ultra' WHERE provider = 'codex' AND model = 'gpt-5.5'`)
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
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('codex', 'gpt-5.6-sol', 'max', 0, 272000, 'full-access', 1)`,
	); err != nil {
		t.Fatalf("codex profile with max effort must satisfy CHECK: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('codex', 'gpt-5.6-terra', 'ultra', 0, 272000, 'full-access', 1)`,
	); err != nil {
		t.Fatalf("codex profile with ultra effort must satisfy CHECK: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('claude', 'claude-opus-4-7', 'ultra', 0, 1000000, 'full-access', 1)`,
	); err == nil {
		t.Fatal("claude profile with codex-only effort 'ultra' must violate coupling CHECK")
	}

	// claude-tui shares claude's effort set and is a first-class provider on
	// both chat-bar tables post-v10.
	if _, err := s.db.Exec(
		`INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		 VALUES ('model', 'claude-tui', 'claude-opus-4-8', 'Opus 4.8', 1)`,
	); err != nil {
		t.Fatalf("claude-tui model favorite must satisfy CHECK: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('claude-tui', 'claude-opus-4-8', 'max', 0, 1000000, 'full-access', 1)`,
	); err != nil {
		t.Fatalf("claude-tui profile with valid effort must satisfy CHECK: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('claude-tui', 'claude-sonnet-4-6', 'minimal', 0, 1000000, 'full-access', 1)`,
	); err == nil {
		t.Fatal("claude-tui profile with codex-only effort 'minimal' must violate coupling CHECK")
	}
}

// --- v5: terminal mode + nullable project_id ---

func TestThreadsModeCheckAcceptsTerminal(t *testing.T) {
	s := newTestStore(t)

	for _, m := range []string{"chat", "plan", "design", "discussion", "terminal"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode)
			VALUES (?, ?, 'M', 'claude', '/tmp', '', 1, 1, 0, ?)
		`, "t-mode-"+m, defaultTestProjectID, m); err != nil {
			t.Errorf("INSERT with mode=%q must satisfy CHECK: %v", m, err)
		}
	}

	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-mode-bogus', ?, 'B', 'claude', '/tmp', '', 1, 1, 0, 'terminator')
	`, defaultTestProjectID); err == nil {
		t.Fatal("INSERT with mode='terminator' must violate CHECK constraint")
	}
}

func TestThreadsProjectIDNullable(t *testing.T) {
	s := newTestStore(t)

	// A standalone "home" terminal carries no project. Post-v5 the FK is
	// nullable; the NULL must round-trip rather than coerce to ''.
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-home', NULL, 'Home Terminal', 'claude', '/home/u', '', 1, 1, 0, 'terminal')
	`); err != nil {
		t.Fatalf("INSERT with NULL project_id must be allowed post-v5: %v", err)
	}

	var projectID sql.NullString
	if err := s.db.QueryRow(`SELECT project_id FROM threads WHERE id = 't-home'`).Scan(&projectID); err != nil {
		t.Fatalf("select project_id: %v", err)
	}
	if projectID.Valid {
		t.Errorf("project_id = %q, want NULL", projectID.String)
	}
}

// TestThreadsV5RebuildPreservesChildren is the guard against silent
// cascade data loss. The v5 migration rebuilds the threads table
// (DROP + recreate) to alter the mode CHECK and drop project_id's NOT
// NULL; with foreign-key enforcement naively left on, that DROP would
// fire ON DELETE CASCADE against every child table. This test seeds a
// thread with a spread of CASCADE children across distinct tables, runs
// the real v5 rebuild, and asserts the children survive, integrity is
// clean, enforcement is restored, and CASCADE still works afterward.
func TestThreadsV5RebuildPreservesChildren(t *testing.T) {
	db := migrateThrough(t, 4)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v5', '/v5', 'v5', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v5', 'p-v5', 'Keep my children', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('turn-v5', 't-v5', 0, 1)`)
	// The item doubles as the checkpoint's composite-FK referent
	// (thread_checkpoints(thread_id, user_item_id) -> items(thread_id, id)),
	// so a single user_text item is both a CASCADE child and a valid parent.
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v5-user:0', 't-v5', 0, 0, 'user_text', 'user', 'completed', 'hi', '', 0, '', '', '', '{}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-v5', 't-v5', 't-v5-user:0', 0, 'refs/x', 1000, '/tmp')`)

	// Pre-v5 the new affordances must not exist yet — proves migrateThrough
	// really stopped at the old schema, so the survival assertion is meaningful.
	if _, err := db.Exec(`UPDATE threads SET mode = 'terminal' WHERE id = 't-v5'`); err == nil {
		t.Fatal("pre-v5: mode='terminal' must violate CHECK")
	}
	if _, err := db.Exec(`UPDATE threads SET project_id = NULL WHERE id = 't-v5'`); err == nil {
		t.Fatal("pre-v5: NULL project_id must violate NOT NULL")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 5)); err != nil {
		t.Fatalf("apply v5 rebuild: %v", err)
	}

	for _, c := range []struct{ table, where string }{
		{"threads", "id = 't-v5'"},
		{"turns", "thread_id = 't-v5'"},
		{"items", "thread_id = 't-v5'"},
		{"thread_checkpoints", "thread_id = 't-v5'"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + c.table + ` WHERE ` + c.where).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("expected %s row to survive rebuild, got %d", c.table, n)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reported a violation after rebuild")
	}
	rows.Close()

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d after rebuild, want 1 (re-enabled)", fk)
	}

	// Post-v5 affordances now work.
	if _, err := db.Exec(`UPDATE threads SET mode = 'terminal' WHERE id = 't-v5'`); err != nil {
		t.Fatalf("post-v5: mode='terminal' must be accepted: %v", err)
	}
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-standalone', NULL, 'Home', 'claude', '/home', '', 1, 1, 0, 'terminal')`)

	// CASCADE still wired after the rebuild: deleting the project drops its
	// (now terminal-mode) thread, while the project-less terminal survives.
	mustExec(t, db, `DELETE FROM projects WHERE id = 'p-v5'`)
	var withProject, standalone int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id = 't-v5'`).Scan(&withProject); err != nil {
		t.Fatalf("count t-v5 after project delete: %v", err)
	}
	if withProject != 0 {
		t.Errorf("post-v5 CASCADE broken: project delete left %d threads", withProject)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id = 't-standalone'`).Scan(&standalone); err != nil {
		t.Fatalf("count standalone after project delete: %v", err)
	}
	if standalone != 1 {
		t.Errorf("standalone terminal should survive project delete, got %d", standalone)
	}
}

// TestV10ClaudeTUIProviderWidening guards the v10 rebuild that widens the
// provider CHECK on all four provider-keyed tables to admit 'claude-tui'. It
// seeds the pre-v10 schema with a claude thread (plus CASCADE children) and a
// row in each leaf table, proves claude-tui is rejected everywhere before the
// migration (so the post-migration acceptance is meaningful), applies the real
// v10 rebuild, then asserts: the seeded rows survive, FK enforcement is clean
// and restored, claude-tui is now accepted on every table, the provider/effort
// coupling still rejects a codex-only effort under claude-tui, and CASCADE
// remains wired.
func TestV10ClaudeTUIProviderWidening(t *testing.T) {
	db := migrateThrough(t, 9)

	// Pre-image: a claude thread with a spread of CASCADE children, plus one
	// row in each leaf table the rebuild touches.
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v10', '/v10', 'v10', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v10', 'p-v10', 'Keep me', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('turn-v10', 't-v10', 0, 1)`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v10-user:0', 't-v10', 0, 0, 'user_text', 'user', 'completed', 'hi', '', 0, '', '', '', '{}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-v10', 't-v10', 't-v10-user:0', 0, 'refs/x', 1000, '/tmp')`)
	mustExec(t, db, `INSERT INTO chat_model_profiles
		(provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at)
		VALUES ('claude', 'claude-opus-4-7', 'high', 0, 1000000, 'full-access', 1)`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		VALUES ('model', 'claude', 'claude-opus-4-7', 'Opus 4.7', 1)`)
	mustExec(t, db, `INSERT INTO new_thread_mcp_defaults (provider, workspace_path, disabled_servers, updated_at)
		VALUES ('claude', '/tmp', '[]', 1)`)

	// The same four inserts, opposite expectations across the migration
	// boundary: rejected by the pre-v10 CHECK, accepted after the rebuild.
	claudeTUIInserts := []struct {
		table string
		sql   string
	}{
		{"threads", `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
			VALUES ('t-tui', 'p-v10', 'TUI', 'claude-tui', '/tmp', 'claude-opus-4-8', 1, 1, 0, 'chat')`},
		{"chat_model_profiles", `INSERT INTO chat_model_profiles
			(provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at)
			VALUES ('claude-tui', 'claude-opus-4-8', 'max', 0, 1000000, 'full-access', 1)`},
		{"chat_bar_favorites", `INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
			VALUES ('model', 'claude-tui', 'claude-opus-4-8', 'Opus', 1)`},
		{"new_thread_mcp_defaults", `INSERT INTO new_thread_mcp_defaults (provider, workspace_path, disabled_servers, updated_at)
			VALUES ('claude-tui', '/tmp', '[]', 1)`},
	}
	for _, c := range claudeTUIInserts {
		if _, err := db.Exec(c.sql); err == nil {
			t.Errorf("pre-v10: %s must reject provider 'claude-tui'", c.table)
		}
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 10)); err != nil {
		t.Fatalf("apply v10 rebuild: %v", err)
	}

	// Seeded rows (thread + CASCADE children + each leaf table) survive.
	for _, c := range []struct{ table, where string }{
		{"threads", "id = 't-v10'"},
		{"turns", "thread_id = 't-v10'"},
		{"items", "thread_id = 't-v10'"},
		{"thread_checkpoints", "thread_id = 't-v10'"},
		{"chat_model_profiles", "provider = 'claude' AND model = 'claude-opus-4-7'"},
		{"chat_bar_favorites", "kind = 'model' AND provider = 'claude' AND value = 'claude-opus-4-7'"},
		{"new_thread_mcp_defaults", "provider = 'claude' AND workspace_path = '/tmp'"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + c.table + ` WHERE ` + c.where).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("expected %s row to survive v10 rebuild, got %d", c.table, n)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reported a violation after v10 rebuild")
	}
	rows.Close()

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d after v10 rebuild, want 1 (re-enabled)", fk)
	}

	// claude-tui is now accepted on every widened table.
	for _, c := range claudeTUIInserts {
		if _, err := db.Exec(c.sql); err != nil {
			t.Errorf("post-v10: %s must accept provider 'claude-tui': %v", c.table, err)
		}
	}

	// The provider/effort coupling still bites: 'minimal' is a codex-only
	// effort and must be rejected under claude-tui on both coupled tables.
	if _, err := db.Exec(`UPDATE threads SET reasoning_effort = 'minimal' WHERE id = 't-tui'`); err == nil {
		t.Error("post-v10: claude-tui thread + reasoning_effort='minimal' (codex-only) must violate coupling CHECK")
	}
	if _, err := db.Exec(`UPDATE chat_model_profiles SET reasoning_effort = 'minimal' WHERE provider = 'claude-tui'`); err == nil {
		t.Error("post-v10: claude-tui chat_model_profiles + effort='minimal' must violate coupling CHECK")
	}

	// CASCADE survives the rebuild: dropping the project removes its threads.
	mustExec(t, db, `DELETE FROM projects WHERE id = 'p-v10'`)
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id = 't-v10'`).Scan(&remaining); err != nil {
		t.Fatalf("count t-v10 after project delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("post-v10 CASCADE broken: project delete left %d threads", remaining)
	}
}

// TestV11CompactionReasoningKindWidening guards the v11 rebuild that widens the
// items.kind CHECK to admit 'compaction_reasoning'. It seeds the pre-v11 schema
// with a thread, a user item, and a thread_checkpoints row whose
// (thread_id, user_item_id) FK REFERENCES items ON DELETE CASCADE — the one FK
// the items rebuild must preserve — proves 'compaction_reasoning' is rejected
// before the migration, applies the real v11 rebuild, then asserts: the seeded
// rows survive, FK enforcement is clean and restored, the new kind is accepted,
// a bogus kind is still rejected, and the items→thread_checkpoints CASCADE
// survived the table swap.
func TestV11CompactionReasoningKindWidening(t *testing.T) {
	db := migrateThrough(t, 10)

	// Pre-image: a thread, a user item carrying a payload (to exercise the GC
	// trigger the rebuild must recreate), and a checkpoint pinned to that item
	// by the FK the rebuild must carry across.
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v11', '/v11', 'v11', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v11', 'p-v11', 'Keep me', 'claude-tui', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('pl-v11', 'thinking', '{}', X'00', 1)`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, payload_id, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v11-user:0', 't-v11', 0, 0, 'user_text', 'user', 'completed', 'hi', 'pl-v11', '', 0, '', '', '', '{}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, ref_name, captured_at, workspace_path)
		VALUES ('chk-v11', 't-v11', 't-v11-user:0', 0, 'refs/x', 1000, '/tmp')`)

	// A compaction_reasoning row: rejected by the pre-v11 CHECK, accepted after.
	const reasoningInsert = `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v11-reason:0', 't-v11', 0, 1, 'compaction_reasoning', 'assistant', 'completed',
		'Reviewing the conversation', '', 0, '', '', '', '{}', 1, 1)`
	if _, err := db.Exec(reasoningInsert); err == nil {
		t.Fatal("pre-v11: items must reject kind 'compaction_reasoning'")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 11)); err != nil {
		t.Fatalf("apply v11 rebuild: %v", err)
	}

	// Seeded item + its CASCADE child survive the rebuild.
	for _, c := range []struct{ table, where string }{
		{"items", "id = 't-v11-user:0'"},
		{"thread_checkpoints", "id = 'chk-v11'"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + c.table + ` WHERE ` + c.where).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != 1 {
			t.Errorf("expected %s row to survive v11 rebuild, got %d", c.table, n)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if rows.Next() {
		t.Error("foreign_key_check reported a violation after v11 rebuild")
	}
	rows.Close()

	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d after v11 rebuild, want 1 (re-enabled)", fk)
	}

	// The new kind is now accepted; a bogus kind is still rejected.
	if _, err := db.Exec(reasoningInsert); err != nil {
		t.Errorf("post-v11: items must accept kind 'compaction_reasoning': %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v11-bogus:0', 't-v11', 0, 2, 'not_a_kind', 'assistant', 'completed', '', '', 0, '', '', '', '{}', 1, 1)`); err == nil {
		t.Error("post-v11: items must still reject a bogus kind")
	}

	// Both payload-GC triggers were recreated (a table rebuild drops the
	// triggers attached to the old table — they must be reinstated explicitly).
	for _, trg := range []string{"trg_items_gc_payload", "trg_items_gc_input_payload"} {
		var name string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='trigger' AND name=?`, trg,
		).Scan(&name); err != nil {
			t.Errorf("post-v11: trigger %s missing after rebuild: %v", trg, err)
		}
	}

	// CASCADE + GC trigger both survive the rebuild: deleting the user item
	// removes its checkpoint (FK REFERENCES items, carried across with FK off)
	// and sweeps its now-orphaned payload (trg_items_gc_payload firing).
	mustExec(t, db, `DELETE FROM items WHERE id = 't-v11-user:0'`)
	var checkpoints int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_checkpoints WHERE id = 'chk-v11'`).Scan(&checkpoints); err != nil {
		t.Fatalf("count chk-v11 after item delete: %v", err)
	}
	if checkpoints != 0 {
		t.Errorf("post-v11 CASCADE broken: item delete left %d checkpoints", checkpoints)
	}
	var payloads int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'pl-v11'`).Scan(&payloads); err != nil {
		t.Fatalf("count pl-v11 after item delete: %v", err)
	}
	if payloads != 0 {
		t.Errorf("post-v11 payload GC broken: item delete left %d orphaned payloads", payloads)
	}
}

// TestV12ChannelMaxTurnsColumn covers the plain ALTER TABLE ADD COLUMN
// migration that backs the restart-rebuild path
// (deliberationForChannel): a pre-existing channel row must backfill
// to DefaultMaxTurns (8), new rows can set an explicit value, and the
// NOT NULL constraint is enforced.
func TestV12ChannelMaxTurnsColumn(t *testing.T) {
	db := migrateThrough(t, 11)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v12', '/v12', 'v12', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v12', 'p-v12', 'Discuss', 'claude-tui', '/tmp', '', 1, 1, 0, 'discussion')`)
	mustExec(t, db, `INSERT INTO channels (id, thread_id, type, status, created_at, updated_at)
		VALUES ('c-v12', 't-v12', 'deliberation', 'open', 1, 1)`)

	if err := applyMigration(db, migrationByVersion(t, 12)); err != nil {
		t.Fatalf("apply v12: %v", err)
	}

	var backfilled int
	if err := db.QueryRow(`SELECT max_turns FROM channels WHERE id = 'c-v12'`).Scan(&backfilled); err != nil {
		t.Fatalf("read backfilled max_turns: %v", err)
	}
	if backfilled != 8 {
		t.Fatalf("max_turns for pre-existing row = %d, want 8 (DEFAULT backfill)", backfilled)
	}

	mustExec(t, db, `INSERT INTO channels (id, thread_id, type, status, max_turns, created_at, updated_at)
		VALUES ('c-v12-custom', 't-v12', 'deliberation', 'open', 12, 2, 2)`)
	var custom int
	if err := db.QueryRow(`SELECT max_turns FROM channels WHERE id = 'c-v12-custom'`).Scan(&custom); err != nil {
		t.Fatalf("read custom max_turns: %v", err)
	}
	if custom != 12 {
		t.Fatalf("custom max_turns = %d, want 12", custom)
	}

	if _, err := db.Exec(`INSERT INTO channels (id, thread_id, type, status, max_turns, created_at, updated_at)
		VALUES ('c-v12-null', 't-v12', 'deliberation', 'open', NULL, 3, 3)`); err == nil {
		t.Fatal("expected a NOT NULL violation inserting a NULL max_turns")
	}
}

// --- Helpers ---

// migrateThrough opens an in-memory DB configured like production
// (SetMaxOpenConns(1), so the single shared :memory: connection is reused
// across calls and the rebuild migration's pinned-connection FK toggle
// lands on it) and applies every migration up to and including target. It
// is the pre-image builder for migration-transition tests: seed against
// the schema at `target`, then drive the next migration.
func migrateThrough(t *testing.T, target int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure database: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version > target {
			break
		}
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			t.Fatalf("apply migration v%d: %v", m.Version, err)
		}
	}
	return db
}

func migrationByVersion(t *testing.T, v int) Migration {
	t.Helper()
	for _, m := range migrations {
		if m.Version == v {
			return m
		}
	}
	t.Fatalf("no migration with version %d", v)
	return Migration{}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

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

// v13: the partial in-flight index backing the boot-time crashed-turn
// sweep. Assert both the index and its partial predicate so a rebuild
// migration that recreates turns can't silently drop the WHERE clause.
func TestTurnsInflightPartialIndex(t *testing.T) {
	s := newTestStore(t)

	var indexSQL string
	if err := s.db.QueryRow(
		"SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_turns_inflight'",
	).Scan(&indexSQL); err != nil {
		t.Fatalf("idx_turns_inflight not found: %v", err)
	}
	if !strings.Contains(indexSQL, "completed_at IS NULL") {
		t.Fatalf("idx_turns_inflight lost its partial predicate: %s", indexSQL)
	}
}

// TestMigrationV20BackfillsProviderTurnID pins the provider_turn_id
// backfill rule: Codex turns rows use the wire turn id verbatim as
// their PK (never contains ':'), so they backfill provider_turn_id =
// turn_id; Claude rows synthesize `<threadID>:<turnIndex>` PKs (always
// contain ':') and must stay '' — Claude has no wire turn id.
func TestMigrationV20BackfillsProviderTurnID(t *testing.T) {
	db := migrateThrough(t, 19)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v20', '/tmp/v20', 'V20', 'blue', 7, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('thread-v20', 'project-v20', 'T', 'codex', '/tmp/v20', 1, 1)
	`)
	mustExec(t, db, `
		INSERT INTO turns (turn_id, thread_id, turn_index, started_at, completed_at)
		VALUES ('codex-wire-turn-1', 'thread-v20', 0, 1, 2),
		       ('thread-v20:1', 'thread-v20', 1, 3, 4)
	`)

	if err := applyMigration(db, migrationByVersion(t, 20)); err != nil {
		t.Fatalf("apply migration v20: %v", err)
	}

	rows := map[string]string{}
	res, err := db.Query(`SELECT turn_id, provider_turn_id FROM turns WHERE thread_id = 'thread-v20'`)
	if err != nil {
		t.Fatalf("query turns: %v", err)
	}
	defer res.Close()
	for res.Next() {
		var turnID, providerTurnID string
		if err := res.Scan(&turnID, &providerTurnID); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows[turnID] = providerTurnID
	}
	if err := res.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if rows["codex-wire-turn-1"] != "codex-wire-turn-1" {
		t.Errorf("codex row provider_turn_id = %q, want backfilled wire id", rows["codex-wire-turn-1"])
	}
	if rows["thread-v20:1"] != "" {
		t.Errorf("claude row provider_turn_id = %q, want empty", rows["thread-v20:1"])
	}
}

func TestMigrationV22BackfillsDeterministicProjectSlugs(t *testing.T) {
	db := migrateThrough(t, 21)
	for _, row := range []struct {
		id        string
		path      string
		name      string
		createdAt int64
	}{
		{id: "p-b", path: "/tmp/p-b", name: "Same Project", createdAt: 10},
		{id: "p-later", path: "/tmp/p-later", name: "same___project", createdAt: 20},
		{id: "p-a", path: "/tmp/p-a", name: "SAME PROJECT!!", createdAt: 10},
	} {
		mustExec(t, db, `
			INSERT INTO projects (id, path, name, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?)
		`, row.id, row.path, row.name, row.createdAt, row.createdAt)
	}

	if err := applyMigration(db, migrationByVersion(t, 22)); err != nil {
		t.Fatalf("apply migration v22: %v", err)
	}

	want := map[string]string{
		"p-a":     "same-project",
		"p-b":     "same-project-2",
		"p-later": "same-project-3",
	}
	rows, err := db.Query(`SELECT id, slug FROM projects ORDER BY id`)
	if err != nil {
		t.Fatalf("query project slugs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			t.Fatalf("scan project slug: %v", err)
		}
		if slug != want[id] {
			t.Errorf("project %s slug = %q, want %q", id, slug, want[id])
		}
		delete(want, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate project slugs: %v", err)
	}
	if len(want) != 0 {
		t.Fatalf("missing migrated projects: %v", want)
	}

	var indexSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_projects_slug'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("idx_projects_slug not found: %v", err)
	}
	if !strings.Contains(indexSQL, "UNIQUE INDEX") {
		t.Fatalf("idx_projects_slug is not unique: %s", indexSQL)
	}
	if _, err := db.Exec(`UPDATE projects SET slug = 'same-project' WHERE id = 'p-b'`); err == nil {
		t.Fatal("duplicate slug update succeeded, want unique constraint error")
	}
}

func TestMigrationV23CreatesWorkflowPersistence(t *testing.T) {
	db := migrateThrough(t, 22)
	if err := applyMigration(db, migrationByVersion(t, 23)); err != nil {
		t.Fatalf("apply migration v23: %v", err)
	}

	for _, table := range []string{
		"work_items", "work_item_phases", "work_item_effects",
		"automations", "automation_cursors",
	} {
		var count int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&count); err != nil {
			t.Fatalf("find table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s count = %d, want 1", table, count)
		}
	}
	columns, err := tableColumns(db, "usage_ledger")
	if err != nil {
		t.Fatalf("usage_ledger columns: %v", err)
	}
	if !columns["work_item_id"] {
		t.Fatal("usage_ledger.work_item_id missing")
	}
	itemColumns, err := tableColumns(db, "work_items")
	if err != nil {
		t.Fatalf("work_items columns: %v", err)
	}
	for _, column := range []string{"step_mode", "worktree_path", "branch", "base_branch"} {
		if !itemColumns[column] {
			t.Fatalf("work_items.%s missing", column)
		}
	}
	for _, index := range []string{
		"idx_work_items_project_state_sort", "idx_work_items_project_sort",
		"idx_work_item_phases_item_started", "idx_automations_project",
		"idx_usage_ledger_work_item",
	} {
		readIndexSQL(t, db, index)
	}

	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, reason,
		 sort_position, step_mode, source, created_at)
		VALUES ('item-v22', 'deleted-project', 'goal', 'wf', 'shared', 'queued', '', 0, 1, 'manual', 1)`)
	mustExec(t, db, `INSERT INTO work_item_phases
		(item_id, phase_id, attempt, status, started_at)
		VALUES ('item-v22', 'phase', 1, 'running', 2)`)
	mustExec(t, db, `INSERT INTO work_item_effects
		(item_id, phase_id, tool, payload_hash, payload, created_at)
		VALUES ('item-v22', 'phase', 'report', 'hash', '{}', 3)`)
	mustExec(t, db, `INSERT INTO automations
		(id, project_id, workflow_id, workflow_scope, name, trigger, created_at, updated_at)
		VALUES ('auto-v22', 'deleted-project', 'wf', 'project', 'Auto', '{"cron":"* * * * *"}', 4, 4)`)
	mustExec(t, db, `INSERT INTO automation_cursors
		(automation_id, source_key, cursor, updated_at)
		VALUES ('auto-v22', 'jira', 'cursor-1', 5)`)

	invalid := []struct {
		name string
		sql  string
	}{
		{"work item scope", `UPDATE work_items SET workflow_scope = 'unknown' WHERE id = 'item-v22'`},
		{"work item state", `UPDATE work_items SET state = 'unknown' WHERE id = 'item-v22'`},
		{"work item reason", `UPDATE work_items SET reason = 'unknown' WHERE id = 'item-v22'`},
		{"work item snapshot", `UPDATE work_items SET snapshot = '{' WHERE id = 'item-v22'`},
		{"work item seeds", `UPDATE work_items SET seeds = '{' WHERE id = 'item-v22'`},
		{"work item step mode", `UPDATE work_items SET step_mode = 2 WHERE id = 'item-v22'`},
		{"work item budget", `UPDATE work_items SET budget = '{' WHERE id = 'item-v22'`},
		{"work item source", `UPDATE work_items SET source = 'unknown' WHERE id = 'item-v22'`},
		{"phase status", `UPDATE work_item_phases SET status = 'unknown' WHERE item_id = 'item-v22'`},
		{"phase attempt", `UPDATE work_item_phases SET attempt = 0 WHERE item_id = 'item-v22'`},
		{"phase input envelope", `UPDATE work_item_phases SET input_envelope = '{' WHERE item_id = 'item-v22'`},
		{"phase output envelope", `UPDATE work_item_phases SET output_envelope = '{' WHERE item_id = 'item-v22'`},
		{"phase gate trace", `UPDATE work_item_phases SET gate_trace = '{' WHERE item_id = 'item-v22'`},
		{"phase intervention", `UPDATE work_item_phases SET intervention = '{' WHERE item_id = 'item-v22'`},
		{"effect json", `UPDATE work_item_effects SET payload = '{' WHERE item_id = 'item-v22'`},
		{"automation scope", `UPDATE automations SET workflow_scope = 'unknown' WHERE id = 'auto-v22'`},
		{"automation enabled", `UPDATE automations SET enabled = 2 WHERE id = 'auto-v22'`},
		{"automation trigger", `UPDATE automations SET trigger = '{' WHERE id = 'auto-v22'`},
		{"automation condition", `UPDATE automations SET condition = '{' WHERE id = 'auto-v22'`},
		{"automation seeds", `UPDATE automations SET seeds = '{' WHERE id = 'auto-v22'`},
	}
	for _, test := range invalid {
		if _, err := db.Exec(test.sql); err == nil {
			t.Errorf("%s constraint accepted invalid value", test.name)
		}
	}

	// Run records deliberately carry no project/thread foreign keys.
	mustExec(t, db, `DELETE FROM work_items WHERE id = 'item-v22'`)
	var phaseCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_item_phases WHERE item_id = 'item-v22'`).Scan(&phaseCount); err != nil {
		t.Fatalf("count retained phases: %v", err)
	}
	if phaseCount != 1 {
		t.Fatalf("phase rows after work item delete = %d, want 1 (no FK cascade)", phaseCount)
	}
}

func TestMigrationV24PreservesThreadsAndAcceptsWorkflowMode(t *testing.T) {
	db := migrateThrough(t, 23)
	mustExec(t, db, `INSERT INTO projects
		(id, path, name, slug, created_at, updated_at)
		VALUES ('project-v24', '/tmp/v24', 'V24', 'v24', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('root-v24', 'project-v24', 'Root', 'codex', '/tmp/v24', 1, 1)`)
	mustExec(t, db, `INSERT INTO channels
		(id, thread_id, type, status, max_turns, created_at, updated_at)
		VALUES ('channel-v24', 'root-v24', 'deliberation', 'open', 8, 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, model, workspace_path, worktree_path,
		 branch, pr_ref, session_ref, pending_fork_session_ref, mode,
		 reasoning_effort, fast_mode, context_window,
		 auto_compact_standard_percent, auto_compact_extended_percent,
		 runtime_mode, discussion_id, parent_thread_id, forked_from_thread_id,
		 last_token_usage, last_read_at, pinned_at, created_at, updated_at,
		 archived, disabled_mcp_servers)
		VALUES ('thread-v24', 'project-v24', 'Preserved', 'codex', 'gpt', '/tmp/v24', '/tmp/v24-wt',
		 'ao/v24', 'pr-23', 'session-23', 'fork-session-23', 'discussion',
		 'ultra', 1, 123456, 45, 55, 'auto-accept-edits', 'channel-v24',
		 'root-v24', 'root-v24', '{"inputTokens":1}', 6, 7, 2, 3, 1, '["server"]')`)
	mustExec(t, db, `INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at)
		VALUES ('item-v24', 'thread-v24', 0, 0, 'user_text', 'user', 'completed', 'keep me', 2, 2)`)
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('workflow-before-v24', 'project-v24', 'Workflow', 'codex', '/tmp/v24', 'workflow', 4, 4)`); err == nil {
		t.Fatal("pre-v24 threads table accepted workflow mode")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 24)); err != nil {
		t.Fatalf("apply migration v24: %v", err)
	}

	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads
		WHERE id = 'thread-v24' AND project_id = 'project-v24'
		  AND title = 'Preserved' AND provider = 'codex' AND model = 'gpt'
		  AND workspace_path = '/tmp/v24' AND worktree_path = '/tmp/v24-wt'
		  AND branch = 'ao/v24' AND pr_ref = 'pr-23' AND session_ref = 'session-23'
		  AND pending_fork_session_ref = 'fork-session-23' AND mode = 'discussion'
		  AND reasoning_effort = 'ultra' AND fast_mode = 1 AND context_window = 123456
		  AND auto_compact_standard_percent = 45 AND auto_compact_extended_percent = 55
		  AND runtime_mode = 'auto-accept-edits' AND discussion_id = 'channel-v24'
		  AND parent_thread_id = 'root-v24' AND forked_from_thread_id = 'root-v24'
		  AND last_token_usage = '{"inputTokens":1}' AND last_read_at = 6 AND pinned_at = 7
		  AND created_at = 2 AND updated_at = 3 AND archived = 1
		  AND disabled_mcp_servers = '["server"]'`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved thread: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v24 rebuild did not preserve the complete thread row")
	}
	var childRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 'thread-v24'`).Scan(&childRows); err != nil {
		t.Fatalf("count child rows: %v", err)
	}
	if childRows != 1 {
		t.Fatalf("thread child rows after rebuild = %d, want 1", childRows)
	}
	for _, index := range []string{
		"idx_threads_forked_from", "idx_threads_parent", "idx_threads_pinned_at",
		"idx_threads_project", "idx_threads_updated",
	} {
		readIndexSQL(t, db, index)
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d after v24, want 1", foreignKeys)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('bad-fk-v24', 'missing-project', 'Bad FK', 'codex', '/tmp/v24', 4, 4)`); err == nil {
		t.Fatal("foreign key enforcement was not restored after v24")
	}
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('workflow-after-v24', 'project-v24', 'Workflow', 'codex', '/tmp/v24', 'workflow', 4, 4)`)
}

func TestMigrationV25AddsWorkflowModesTakeoverAndTriageLink(t *testing.T) {
	db := migrateThrough(t, 24)
	mustExec(t, db, `INSERT INTO projects
		(id, path, name, slug, created_at, updated_at)
		VALUES ('project-v25', '/tmp/v25', 'V25', 'v25', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, model, workspace_path, worktree_path,
		 branch, session_ref, mode, reasoning_effort, fast_mode, context_window,
		 runtime_mode, created_at, updated_at)
		VALUES ('thread-v25', 'project-v25', 'Preserved', 'codex', 'gpt', '/tmp/v25', '/tmp/v25-wt',
		 'ao-v25', 'session-v25', 'workflow', 'ultra', 1, 123456,
		 'auto-accept-edits', 2, 3)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, created_at, started_at, ended_at)
		VALUES ('item-v25', 'project-v25', 'keep goal', 'wf', 'project', '{}',
		 'needs-human', 'question', 7, '{}', 1, '/tmp/v25-wt', 'ao-v25', 'main', '{}',
		 'manual', 'source-v25', 4, 5, 6)`)

	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('studio-before-v25', 'project-v25', 'Studio', 'codex', '/tmp/v25', 'workflow-studio', 4, 4)`); err == nil {
		t.Fatal("pre-v25 threads table accepted workflow-studio")
	}
	if _, err := db.Exec(`UPDATE work_items SET reason = 'taken-over' WHERE id = 'item-v25'`); err == nil {
		t.Fatal("pre-v25 work_items accepted taken-over")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 25)); err != nil {
		t.Fatalf("apply migration v25: %v", err)
	}

	columns, err := tableColumns(db, "work_items")
	if err != nil {
		t.Fatalf("work_items columns: %v", err)
	}
	if !columns["triage_thread_id"] {
		t.Fatal("work_items.triage_thread_id missing")
	}
	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads
		WHERE id = 'thread-v25' AND project_id = 'project-v25' AND title = 'Preserved'
		  AND provider = 'codex' AND model = 'gpt' AND workspace_path = '/tmp/v25'
		  AND worktree_path = '/tmp/v25-wt' AND branch = 'ao-v25'
		  AND session_ref = 'session-v25' AND mode = 'workflow'
		  AND reasoning_effort = 'ultra' AND fast_mode = 1 AND context_window = 123456
		  AND runtime_mode = 'auto-accept-edits' AND created_at = 2 AND updated_at = 3`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved thread: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v25 rebuild did not preserve the thread row")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v25' AND project_id = 'project-v25' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'project' AND snapshot = '{}'
		  AND state = 'needs-human' AND reason = 'question' AND sort_position = 7
		  AND seeds = '{}' AND step_mode = 1 AND worktree_path = '/tmp/v25-wt'
		  AND branch = 'ao-v25' AND base_branch = 'main' AND budget = '{}'
		  AND source = 'manual' AND source_ref = 'source-v25' AND triage_thread_id = ''
		  AND created_at = 4 AND started_at = 5 AND ended_at = 6`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v25 rebuild did not preserve the work item row")
	}
	for _, mode := range []string{"workflow-studio", "workflow-triage"} {
		mustExec(t, db, `INSERT INTO threads
			(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
			VALUES (?, 'project-v25', 'Mode', 'codex', '/tmp/v25', ?, 8, 8)`, "thread-"+mode, mode)
	}
	mustExec(t, db, `UPDATE work_items SET reason = 'taken-over', triage_thread_id = 'thread-workflow-triage' WHERE id = 'item-v25'`)
	for _, index := range []string{
		"idx_threads_forked_from", "idx_threads_parent", "idx_threads_pinned_at",
		"idx_threads_project", "idx_threads_updated",
		"idx_work_items_project_state_sort", "idx_work_items_project_sort",
		"idx_work_items_triage_thread", "idx_work_item_phases_thread",
	} {
		readIndexSQL(t, db, index)
	}
}
