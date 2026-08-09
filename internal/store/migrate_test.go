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

	// Test-only dependency. The shipped internal/store package stays
	// provider-free by design (it duplicates the runtime-mode value set
	// rather than importing it); importing provider here is what proves the
	// duplicate has not drifted, without putting the edge in the build graph.
	"agent-overflow/internal/provider"

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

	db, err := sql.Open("sqlite", poolDSN(":memory:", writerConnPragmas))
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// --- Schema smoke ---

func TestMigrationFreshDB(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatalf("create fresh store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close fresh store: %v", err)
		}
	})

	tables := []string{
		"migration_versions", "threads", "items", "payloads", "payload_chunks",
		"channels", "channel_messages", "discussion_definitions",
		"attachments", "thread_drafts", "message_anchors", "turns",
		"proposed_plans", "proposed_plan_comments",
		"chat_bar_favorites", "chat_model_profiles",
		"diff_review_comments", "pending_background_task_terminals",
		"projects", "usage_ledger", "ui_state", "edit_file_snapshots",
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
		"mcp_servers", "thread_checkpoints", "thread_tracked_files",
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

// TestV23CarriesCheckpointRowsIntoMessageAnchors pins the v23 slim-down:
// provider correlation (turn index + Claude wire uuids) moves from
// thread_checkpoints into message_anchors, the git-era tables drop, and
// review comments scoped to the removed turn/session diff sources are
// purged while other scopes survive.
func TestV23CarriesCheckpointRowsIntoMessageAnchors(t *testing.T) {
	db := migrateThrough(t, 22)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v23', '/v23', 'v23', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v23', 'p-v23', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('t-v23-user:0', 't-v23', 0, 0, 'user_text', 'user', 'completed', 'hi', '', 0, '', '', '', '{}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_checkpoints
		(id, thread_id, user_item_id, turn_index, provider_user_message_id, provider_parent_uuid,
		 ref_name, captured_at, workspace_path)
		VALUES ('chk-v23', 't-v23', 't-v23-user:0', 0, 'wire-uuid', 'parent-uuid', 'refs/x', 4242, '/tmp')`)
	mustExec(t, db, `INSERT INTO thread_tracked_files (thread_id, turn_index, path)
		VALUES ('t-v23', 0, 'tracked.txt')`)
	for i, scope := range []string{"turn", "session", "workspace"} {
		mustExec(t, db, `INSERT INTO diff_review_comments
			(id, thread_id, scope, source_key, file_path, side, body, created_at, updated_at)
			VALUES (?, 't-v23', ?, 'sk', 'f.go', 'file', 'note', ?, ?)`,
			fmt.Sprintf("c-%s", scope), scope, i, i)
	}

	if err := applyMigration(db, migrationByVersion(t, 23)); err != nil {
		t.Fatalf("apply v23: %v", err)
	}

	var turnIndex int
	var msgID, parentUUID string
	var createdAt int64
	if err := db.QueryRow(`SELECT turn_index, provider_user_message_id, provider_parent_uuid, created_at
		FROM message_anchors WHERE thread_id = 't-v23' AND user_item_id = 't-v23-user:0'`).
		Scan(&turnIndex, &msgID, &parentUUID, &createdAt); err != nil {
		t.Fatalf("read carried anchor: %v", err)
	}
	if turnIndex != 0 || msgID != "wire-uuid" || parentUUID != "parent-uuid" || createdAt != 4242 {
		t.Errorf("carried anchor = turn=%d msg=%q parent=%q created=%d, want 0/wire-uuid/parent-uuid/4242",
			turnIndex, msgID, parentUUID, createdAt)
	}

	for _, table := range []string{"thread_checkpoints", "thread_tracked_files"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err == nil {
			t.Errorf("%s should be dropped by v23", table)
		}
	}

	rows, err := db.Query(`SELECT scope FROM diff_review_comments WHERE thread_id = 't-v23' ORDER BY scope`)
	if err != nil {
		t.Fatalf("query comments: %v", err)
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			t.Fatalf("scan scope: %v", err)
		}
		scopes = append(scopes, scope)
	}
	if fmt.Sprint(scopes) != fmt.Sprint([]string{"workspace"}) {
		t.Errorf("surviving comment scopes = %v, want [workspace] (turn/session purged)", scopes)
	}
}

func TestMessageAnchorsPrimaryKeyThreadUserItem(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t-anc-unique")

	if _, err := s.db.Exec(`INSERT INTO message_anchors
		(thread_id, user_item_id, turn_index, created_at)
		VALUES ('t-anc-unique', 't-anc-unique-user:1', 1, 1000)`); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	if _, err := s.db.Exec(`INSERT INTO message_anchors
		(thread_id, user_item_id, turn_index, created_at)
		VALUES ('t-anc-unique', 't-anc-unique-user:1', 1, 2000)`); err == nil {
		t.Error("expected PK violation on (thread_id, user_item_id)")
	}

	if _, err := s.db.Exec(`INSERT INTO message_anchors
		(thread_id, user_item_id, turn_index, created_at)
		VALUES ('t-anc-unique', 't-anc-unique-user:2', 2, 3000)`); err != nil {
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

func TestMessageAnchorsCascadeOnThreadDelete(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t-anc-casc")

	if _, err := s.db.Exec(`INSERT INTO message_anchors
		(thread_id, user_item_id, turn_index, created_at)
		VALUES ('t-anc-casc', 't-anc-casc-user:0', 0, 1000)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 't-anc-casc'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message_anchors`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected cascade to drop anchors, still have %d", count)
	}
}

func TestMessageAnchorsCascadeOnItemDelete(t *testing.T) {
	s := newTestStore(t)
	mustCreateThreadWithUserItems(t, s, "t-anc-item")

	if _, err := s.db.Exec(`INSERT INTO message_anchors
		(thread_id, user_item_id, turn_index, created_at)
		VALUES ('t-anc-item', 't-anc-item-user:1', 1, 1000)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM items WHERE thread_id = 't-anc-item' AND id = 't-anc-item-user:1'`); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message_anchors`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected items FK cascade to drop the anchor, still have %d", count)
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

func TestV31WorkflowProposalKindWidening(t *testing.T) {
	db := migrateThrough(t, 30)
	mustExec(t, db, `INSERT INTO projects (id, path, name, slug, created_at, updated_at)
		VALUES ('p-v31', '/v31', 'v31', 'v31', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v31', 'p-v31', 'Keep me', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('existing-v31', 't-v31', 0, 0, 'assistant_text', 'assistant', 'completed',
		'existing', '', 0, '', '', '', '{}', 1, 1)`)
	const proposalInsert = `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('proposal-v31', 't-v31', 0, 1, 'workflow_proposal', 'assistant', 'completed',
		'Queue it', '', 0, '', '', '', '{"state":"pending"}', 1, 1)`
	if _, err := db.Exec(proposalInsert); err == nil {
		t.Fatal("pre-v31: items must reject kind workflow_proposal")
	}
	if err := applyRebuildMigration(db, migrationByVersion(t, 31)); err != nil {
		t.Fatalf("apply v31 rebuild: %v", err)
	}
	if _, err := db.Exec(proposalInsert); err != nil {
		t.Fatalf("post-v31: workflow_proposal rejected: %v", err)
	}
	var existing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE id = 'existing-v31'`).Scan(&existing); err != nil || existing != 1 {
		t.Fatalf("existing item survival: count=%d err=%v", existing, err)
	}
	if _, err := db.Exec(`UPDATE items SET kind = 'not_a_kind' WHERE id = 'proposal-v31'`); err == nil {
		t.Fatal("post-v31: bogus kind accepted")
	}
	var indexName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_work_items_agent_source_ref'`).Scan(&indexName); err != nil {
		t.Fatalf("post-v31: agent source-ref idempotency index missing: %v", err)
	}
}

func TestMigrationV32AddsProjectWorkflowQueueSettings(t *testing.T) {
	db := migrateThrough(t, 31)
	mustExec(t, db, `INSERT INTO projects (id, path, name, slug, created_at, updated_at)
		VALUES ('p-v32', '/v32', 'v32', 'v32', 1, 1)`)

	if err := applyMigration(db, migrationByVersion(t, 32)); err != nil {
		t.Fatalf("apply migration v32: %v", err)
	}
	var paused, concurrency int
	if err := db.QueryRow(`SELECT workflow_queue_paused, workflow_concurrency
		FROM projects WHERE id = 'p-v32'`).Scan(&paused, &concurrency); err != nil {
		t.Fatalf("read migrated project: %v", err)
	}
	if paused != 0 || concurrency != 0 {
		t.Fatalf("migrated defaults = paused %d concurrency %d, want 0/0", paused, concurrency)
	}
	mustExec(t, db, `UPDATE projects SET workflow_queue_paused = 1, workflow_concurrency = 32
		WHERE id = 'p-v32'`)
	if _, err := db.Exec(`UPDATE projects SET workflow_concurrency = 33 WHERE id = 'p-v32'`); err == nil {
		t.Fatal("workflow concurrency above 32 unexpectedly succeeded")
	}
}

// TestMigrationV30RemovesQueueFromWorkItems covers the direct-start rebuild:
// resident `queued` rows become needs-human(interrupted) with an ended_at, the
// `queued` state becomes unrepresentable, `sort_position` is gone, every other
// row survives byte-for-byte, and the rebuilt indexes are the created_at-ordered
// set the queueless read path uses.
func TestMigrationV33RemovesQueueFromWorkItems(t *testing.T) {
	db := migrateThrough(t, 32)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, disposition, digest,
		 created_at, started_at, ended_at)
		VALUES ('queued-v33', 'project-v33', 'never started', 'wf', 'project', '{}',
		 'queued', '', 4, '{"ticket":"AO-1"}', 1, '', '', '', '{}',
		 'manual', 'ref-v33', '', '', '', 40, 0, 0)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, disposition, digest,
		 created_at, started_at, ended_at)
		VALUES ('parked-v33', 'project-v33', 'keep goal', 'wf', 'shared', '{"id":"wf"}',
		 'needs-human', 'question', 9, '{}', 0, '/tmp/wt', 'ao-v33', 'main', '{}',
		 'agent', 'agent-ref-v33', 'triage-v33', '{"action":"merged"}', '{"whatHappened":"x"}',
		 4, 5, 6)`)

	if err := applyRebuildMigration(db, migrationByVersion(t, 33)); err != nil {
		t.Fatalf("apply migration v33: %v", err)
	}

	columns, err := tableColumns(db, "work_items")
	if err != nil {
		t.Fatalf("work_items columns: %v", err)
	}
	if columns["sort_position"] {
		t.Fatal("work_items.sort_position survived the direct-start rebuild")
	}
	var state, reason string
	var endedAt int64
	if err := db.QueryRow(`SELECT state, reason, ended_at FROM work_items WHERE id = 'queued-v33'`).
		Scan(&state, &reason, &endedAt); err != nil {
		t.Fatalf("read migrated queued row: %v", err)
	}
	if state != "needs-human" || reason != "interrupted" || endedAt != 40 {
		t.Fatalf("migrated queued row = (%q, %q, %d), want (needs-human, interrupted, 40)", state, reason, endedAt)
	}
	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'parked-v33' AND project_id = 'project-v33' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'shared' AND snapshot = '{"id":"wf"}'
		  AND state = 'needs-human' AND reason = 'question' AND seeds = '{}'
		  AND step_mode = 0 AND worktree_path = '/tmp/wt' AND branch = 'ao-v33'
		  AND base_branch = 'main' AND budget = '{}' AND source = 'agent'
		  AND source_ref = 'agent-ref-v33' AND triage_thread_id = 'triage-v33'
		  AND disposition = '{"action":"merged"}' AND digest = '{"whatHappened":"x"}'
		  AND created_at = 4 AND started_at = 5 AND ended_at = 6`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v33 rebuild did not preserve the untouched work item row")
	}
	if _, err := db.Exec(`UPDATE work_items SET state = 'queued' WHERE id = 'parked-v33'`); err == nil {
		t.Fatal("post-v33 work_items accepted the removed queued state")
	}
	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_project_created",
		"idx_work_items_state_created", "idx_work_items_triage_thread",
		"idx_work_items_agent_source_ref",
	} {
		readIndexSQL(t, db, index)
	}
	if _, err := db.Exec(`INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, source_ref, created_at)
		VALUES ('dup-v33', 'project-v33', 'dup', 'wf', 'shared', 'running', 'agent', 'agent-ref-v33', 7)`); err == nil {
		t.Fatal("post-v33 rebuild lost the agent source-ref idempotency index")
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
	db, err := sql.Open("sqlite", poolDSN(":memory:", writerConnPragmas))
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
// contain ':') and must stay empty — Claude has no wire turn id.
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

func TestMigrationV25BackfillsDeterministicProjectSlugs(t *testing.T) {
	db := migrateThrough(t, 24)
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

	if err := applyMigration(db, migrationByVersion(t, 25)); err != nil {
		t.Fatalf("apply migration v25: %v", err)
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

func TestMigrationV26CreatesWorkflowPersistence(t *testing.T) {
	db := migrateThrough(t, 25)
	if err := applyMigration(db, migrationByVersion(t, 26)); err != nil {
		t.Fatalf("apply migration v26: %v", err)
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
		VALUES ('item-v25', 'deleted-project', 'goal', 'wf', 'shared', 'queued', '', 0, 1, 'manual', 1)`)
	mustExec(t, db, `INSERT INTO work_item_phases
		(item_id, phase_id, attempt, status, started_at)
		VALUES ('item-v25', 'phase', 1, 'running', 2)`)
	mustExec(t, db, `INSERT INTO work_item_effects
		(item_id, phase_id, tool, payload_hash, payload, created_at)
		VALUES ('item-v25', 'phase', 'report', 'hash', '{}', 3)`)
	mustExec(t, db, `INSERT INTO automations
		(id, project_id, workflow_id, workflow_scope, name, trigger, created_at, updated_at)
		VALUES ('auto-v25', 'deleted-project', 'wf', 'project', 'Auto', '{"cron":"* * * * *"}', 4, 4)`)
	mustExec(t, db, `INSERT INTO automation_cursors
		(automation_id, source_key, cursor, updated_at)
		VALUES ('auto-v25', 'jira', 'cursor-1', 5)`)

	invalid := []struct {
		name string
		sql  string
	}{
		{"work item scope", `UPDATE work_items SET workflow_scope = 'unknown' WHERE id = 'item-v25'`},
		{"work item state", `UPDATE work_items SET state = 'unknown' WHERE id = 'item-v25'`},
		{"work item reason", `UPDATE work_items SET reason = 'unknown' WHERE id = 'item-v25'`},
		{"work item snapshot", `UPDATE work_items SET snapshot = '{' WHERE id = 'item-v25'`},
		{"work item seeds", `UPDATE work_items SET seeds = '{' WHERE id = 'item-v25'`},
		{"work item step mode", `UPDATE work_items SET step_mode = 2 WHERE id = 'item-v25'`},
		{"work item budget", `UPDATE work_items SET budget = '{' WHERE id = 'item-v25'`},
		{"work item source", `UPDATE work_items SET source = 'unknown' WHERE id = 'item-v25'`},
		{"phase status", `UPDATE work_item_phases SET status = 'unknown' WHERE item_id = 'item-v25'`},
		{"phase attempt", `UPDATE work_item_phases SET attempt = 0 WHERE item_id = 'item-v25'`},
		{"phase input envelope", `UPDATE work_item_phases SET input_envelope = '{' WHERE item_id = 'item-v25'`},
		{"phase output envelope", `UPDATE work_item_phases SET output_envelope = '{' WHERE item_id = 'item-v25'`},
		{"phase gate trace", `UPDATE work_item_phases SET gate_trace = '{' WHERE item_id = 'item-v25'`},
		{"phase intervention", `UPDATE work_item_phases SET intervention = '{' WHERE item_id = 'item-v25'`},
		{"effect json", `UPDATE work_item_effects SET payload = '{' WHERE item_id = 'item-v25'`},
		{"automation scope", `UPDATE automations SET workflow_scope = 'unknown' WHERE id = 'auto-v25'`},
		{"automation enabled", `UPDATE automations SET enabled = 2 WHERE id = 'auto-v25'`},
		{"automation trigger", `UPDATE automations SET trigger = '{' WHERE id = 'auto-v25'`},
		{"automation condition", `UPDATE automations SET condition = '{' WHERE id = 'auto-v25'`},
		{"automation seeds", `UPDATE automations SET seeds = '{' WHERE id = 'auto-v25'`},
	}
	for _, test := range invalid {
		if _, err := db.Exec(test.sql); err == nil {
			t.Errorf("%s constraint accepted invalid value", test.name)
		}
	}

	// Run records deliberately carry no project/thread foreign keys.
	mustExec(t, db, `DELETE FROM work_items WHERE id = 'item-v25'`)
	var phaseCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_item_phases WHERE item_id = 'item-v25'`).Scan(&phaseCount); err != nil {
		t.Fatalf("count retained phases: %v", err)
	}
	if phaseCount != 1 {
		t.Fatalf("phase rows after work item delete = %d, want 1 (no FK cascade)", phaseCount)
	}
}

func TestMigrationV27PreservesThreadsAndAcceptsWorkflowMode(t *testing.T) {
	db := migrateThrough(t, 26)
	mustExec(t, db, `INSERT INTO projects
		(id, path, name, slug, created_at, updated_at)
		VALUES ('project-v27', '/tmp/v27', 'V24', 'v27', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('root-v27', 'project-v27', 'Root', 'codex', '/tmp/v27', 1, 1)`)
	mustExec(t, db, `INSERT INTO channels
		(id, thread_id, type, status, max_turns, created_at, updated_at)
		VALUES ('channel-v27', 'root-v27', 'deliberation', 'open', 8, 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, model, workspace_path, worktree_path,
		 branch, pr_ref, session_ref, pending_fork_session_ref, mode,
		 reasoning_effort, fast_mode, context_window,
		 auto_compact_standard_percent, auto_compact_extended_percent,
		 runtime_mode, discussion_id, parent_thread_id, forked_from_thread_id,
		 last_token_usage, last_read_at, pinned_at, created_at, updated_at,
		 archived, disabled_mcp_servers)
		VALUES ('thread-v27', 'project-v27', 'Preserved', 'codex', 'gpt', '/tmp/v27', '/tmp/v27-wt',
		 'ao/v27', 'pr-23', 'session-23', 'fork-session-23', 'discussion',
		 'ultra', 1, 123456, 45, 55, 'auto-accept-edits', 'channel-v27',
		 'root-v27', 'root-v27', '{"inputTokens":1}', 6, 7, 2, 3, 1, '["server"]')`)
	mustExec(t, db, `INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at)
		VALUES ('item-v27', 'thread-v27', 0, 0, 'user_text', 'user', 'completed', 'keep me', 2, 2)`)
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('workflow-before-v27', 'project-v27', 'Workflow', 'codex', '/tmp/v27', 'workflow', 4, 4)`); err == nil {
		t.Fatal("pre-v27 threads table accepted workflow mode")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 27)); err != nil {
		t.Fatalf("apply migration v27: %v", err)
	}

	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads
		WHERE id = 'thread-v27' AND project_id = 'project-v27'
		  AND title = 'Preserved' AND provider = 'codex' AND model = 'gpt'
		  AND workspace_path = '/tmp/v27' AND worktree_path = '/tmp/v27-wt'
		  AND branch = 'ao/v27' AND pr_ref = 'pr-23' AND session_ref = 'session-23'
		  AND pending_fork_session_ref = 'fork-session-23' AND mode = 'discussion'
		  AND reasoning_effort = 'ultra' AND fast_mode = 1 AND context_window = 123456
		  AND auto_compact_standard_percent = 45 AND auto_compact_extended_percent = 55
		  AND runtime_mode = 'auto-accept-edits' AND discussion_id = 'channel-v27'
		  AND parent_thread_id = 'root-v27' AND forked_from_thread_id = 'root-v27'
		  AND last_token_usage = '{"inputTokens":1}' AND last_read_at = 6 AND pinned_at = 7
		  AND created_at = 2 AND updated_at = 3 AND archived = 1
		  AND disabled_mcp_servers = '["server"]'`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved thread: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v27 rebuild did not preserve the complete thread row")
	}
	var childRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 'thread-v27'`).Scan(&childRows); err != nil {
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
		t.Fatalf("foreign_keys = %d after v27, want 1", foreignKeys)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('bad-fk-v27', 'missing-project', 'Bad FK', 'codex', '/tmp/v27', 4, 4)`); err == nil {
		t.Fatal("foreign key enforcement was not restored after v27")
	}
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('workflow-after-v27', 'project-v27', 'Workflow', 'codex', '/tmp/v27', 'workflow', 4, 4)`)
}

func TestMigrationV28AddsWorkflowModesTakeoverAndTriageLink(t *testing.T) {
	db := migrateThrough(t, 27)
	mustExec(t, db, `INSERT INTO projects
		(id, path, name, slug, created_at, updated_at)
		VALUES ('project-v28', '/tmp/v28', 'V28', 'v28', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, model, workspace_path, worktree_path,
		 branch, session_ref, mode, reasoning_effort, fast_mode, context_window,
		 runtime_mode, created_at, updated_at)
		VALUES ('thread-v28', 'project-v28', 'Preserved', 'codex', 'gpt', '/tmp/v28', '/tmp/v28-wt',
		 'ao-v28', 'session-v28', 'workflow', 'ultra', 1, 123456,
		 'auto-accept-edits', 2, 3)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, created_at, started_at, ended_at)
		VALUES ('item-v28', 'project-v28', 'keep goal', 'wf', 'project', '{}',
		 'needs-human', 'question', 7, '{}', 1, '/tmp/v28-wt', 'ao-v28', 'main', '{}',
		 'manual', 'source-v28', 4, 5, 6)`)

	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
		VALUES ('studio-before-v28', 'project-v28', 'Studio', 'codex', '/tmp/v28', 'workflow-studio', 4, 4)`); err == nil {
		t.Fatal("pre-v28 threads table accepted workflow-studio")
	}
	if _, err := db.Exec(`UPDATE work_items SET reason = 'taken-over' WHERE id = 'item-v28'`); err == nil {
		t.Fatal("pre-v28 work_items accepted taken-over")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 28)); err != nil {
		t.Fatalf("apply migration v28: %v", err)
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
		WHERE id = 'thread-v28' AND project_id = 'project-v28' AND title = 'Preserved'
		  AND provider = 'codex' AND model = 'gpt' AND workspace_path = '/tmp/v28'
		  AND worktree_path = '/tmp/v28-wt' AND branch = 'ao-v28'
		  AND session_ref = 'session-v28' AND mode = 'workflow'
		  AND reasoning_effort = 'ultra' AND fast_mode = 1 AND context_window = 123456
		  AND runtime_mode = 'auto-accept-edits' AND created_at = 2 AND updated_at = 3`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved thread: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v28 rebuild did not preserve the thread row")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v28' AND project_id = 'project-v28' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'project' AND snapshot = '{}'
		  AND state = 'needs-human' AND reason = 'question' AND sort_position = 7
		  AND seeds = '{}' AND step_mode = 1 AND worktree_path = '/tmp/v28-wt'
		  AND branch = 'ao-v28' AND base_branch = 'main' AND budget = '{}'
		  AND source = 'manual' AND source_ref = 'source-v28' AND triage_thread_id = ''
		  AND created_at = 4 AND started_at = 5 AND ended_at = 6`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v28 rebuild did not preserve the work item row")
	}
	for _, mode := range []string{"workflow-studio", "workflow-triage"} {
		mustExec(t, db, `INSERT INTO threads
			(id, project_id, title, provider, workspace_path, mode, created_at, updated_at)
			VALUES (?, 'project-v28', 'Mode', 'codex', '/tmp/v28', ?, 8, 8)`, "thread-"+mode, mode)
	}
	mustExec(t, db, `UPDATE work_items SET reason = 'taken-over', triage_thread_id = 'thread-workflow-triage' WHERE id = 'item-v28'`)
	for _, index := range []string{
		"idx_threads_forked_from", "idx_threads_parent", "idx_threads_pinned_at",
		"idx_threads_project", "idx_threads_updated",
		"idx_work_items_project_state_sort", "idx_work_items_project_sort",
		"idx_work_items_triage_thread", "idx_work_item_phases_thread",
	} {
		readIndexSQL(t, db, index)
	}
}

func TestMigrationV29AddsDispositionAndDigest(t *testing.T) {
	db := migrateThrough(t, 28)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 sort_position, seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, created_at, started_at, ended_at)
		VALUES ('item-v29', 'project', 'keep goal', 'wf', 'shared', '{}', 'done', '',
		 3, '{}', 0, '/tmp/wt', 'item-branch', 'main', '{}', 'manual', '', '', 4, 5, 6)`)

	if err := applyMigration(db, migrationByVersion(t, 29)); err != nil {
		t.Fatalf("apply migration v29: %v", err)
	}
	columns, err := tableColumns(db, "work_items")
	if err != nil {
		t.Fatalf("work_items columns: %v", err)
	}
	for _, column := range []string{"disposition", "digest"} {
		if !columns[column] {
			t.Fatalf("work_items.%s missing", column)
		}
	}
	var indexName string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'idx_work_items_state_sort'`).Scan(&indexName); err != nil {
		t.Fatalf("state-leading work item index missing: %v", err)
	}
	var disposition, digest string
	if err := db.QueryRow(`SELECT disposition, digest FROM work_items WHERE id = 'item-v29'`).Scan(&disposition, &digest); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if disposition != "" || digest != "" {
		t.Fatalf("new defaults = disposition %q digest %q, want empty", disposition, digest)
	}
	for name, statement := range map[string]string{
		"disposition": `UPDATE work_items SET disposition = '{' WHERE id = 'item-v29'`,
		"digest":      `UPDATE work_items SET digest = '{' WHERE id = 'item-v29'`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Errorf("invalid %s JSON unexpectedly succeeded", name)
		}
	}
	mustExec(t, db, `UPDATE work_items
		SET disposition = '{"action":"merged"}', digest = '{"whatHappened":"done","whatItNeeds":"nothing"}'
		WHERE id = 'item-v29'`)
}

// TestMigrationV34WidensRuntimeModeCheckOnBothTables is the per-version test
// for the read-only runtime tier. It asserts all three properties a CHECK
// widening needs: the new value is accepted afterwards, garbage is still
// rejected (a widened CHECK must not become a permissive one), and existing
// rows survive the table rebuild intact.
//
// Both tables are covered because both must move together — a thread's
// runtime mode is written back into chat_model_profiles by
// rememberChatModelProfile, so widening only threads would relocate the CHECK
// violation rather than remove it.
func TestMigrationV34WidensRuntimeModeCheckOnBothTables(t *testing.T) {
	db := migrateThrough(t, 33)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v34', '/tmp/v34', 'V34', 'blue', 3, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived, disabled_mcp_servers
		) VALUES (
			'thread-v34', 'project-v34', 'Preserve me', 'claude', 'claude-sonnet-5', '/tmp/v34', '/tmp/v34-wt',
			'feature/v34', 'github:34', 'session-v34', 'fork-v34', 'workflow-triage', 'high',
			1, 1000000, 55, 70, 'auto-accept-edits', '{"input_tokens":34}', 21,
			22, 23, 24, 1, '["server-v34"]'
		)
	`)
	mustExec(t, db, `
		INSERT INTO items (
			id, thread_id, turn_index, item_index, kind, role, status, summary,
			parent_id, is_background, completion_of, tool_name, decision, meta,
			created_at, updated_at
		) VALUES (
			'item-v34', 'thread-v34', 1, 2, 'assistant_text', 'assistant', 'completed', 'kept',
			'', 0, '', '', '', '{}', 25, 26
		)
	`)
	mustExec(t, db, `
		INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window,
			auto_compact_standard_percent, auto_compact_extended_percent,
			runtime_mode, updated_at
		) VALUES ('claude', 'claude-sonnet-5', 'high', 1, 1000000, 60, 75, 'full-access', 27)
	`)

	// Before the migration the tier is unrepresentable on both tables.
	if _, err := db.Exec(`UPDATE threads SET runtime_mode = 'read-only' WHERE id = 'thread-v34'`); err == nil {
		t.Fatal("threads accepted 'read-only' before v34 — the CHECK was already wrong")
	}
	if _, err := db.Exec(`UPDATE chat_model_profiles SET runtime_mode = 'read-only'`); err == nil {
		t.Fatal("chat_model_profiles accepted 'read-only' before v34 — the CHECK was already wrong")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 34)); err != nil {
		t.Fatalf("apply migration v34: %v", err)
	}

	// Carry-over: every column of the pre-existing rows survives the rebuild.
	var title, model, prRef, mode, tokenUsage, disabledServers, runtimeMode string
	var fastMode, contextWindow, archived int
	if err := db.QueryRow(`
		SELECT title, model, pr_ref, mode, last_token_usage, disabled_mcp_servers,
		       runtime_mode, fast_mode, context_window, archived
		FROM threads WHERE id = 'thread-v34'
	`).Scan(&title, &model, &prRef, &mode, &tokenUsage, &disabledServers,
		&runtimeMode, &fastMode, &contextWindow, &archived); err != nil {
		t.Fatalf("read migrated thread: %v", err)
	}
	if title != "Preserve me" || model != "claude-sonnet-5" || prRef != "github:34" ||
		mode != "workflow-triage" || tokenUsage != `{"input_tokens":34}` ||
		disabledServers != `["server-v34"]` || runtimeMode != "auto-accept-edits" ||
		fastMode != 1 || contextWindow != 1000000 || archived != 1 {
		t.Fatalf("migrated thread lost data: title=%q model=%q pr_ref=%q mode=%q usage=%q disabled=%q runtime=%q fast=%d context=%d archived=%d",
			title, model, prRef, mode, tokenUsage, disabledServers, runtimeMode, fastMode, contextWindow, archived)
	}

	var itemSummary string
	if err := db.QueryRow(`SELECT summary FROM items WHERE id = 'item-v34'`).Scan(&itemSummary); err != nil {
		t.Fatalf("read child item after thread rebuild: %v", err)
	}
	if itemSummary != "kept" {
		t.Fatalf("item summary = %q, want kept", itemSummary)
	}

	var profileRuntime string
	var profileFastMode, profileContextWindow, profileUpdatedAt int
	if err := db.QueryRow(`
		SELECT runtime_mode, fast_mode, context_window, updated_at
		FROM chat_model_profiles WHERE provider = 'claude' AND model = 'claude-sonnet-5'
	`).Scan(&profileRuntime, &profileFastMode, &profileContextWindow, &profileUpdatedAt); err != nil {
		t.Fatalf("read migrated model profile: %v", err)
	}
	if profileRuntime != "full-access" || profileFastMode != 1 ||
		profileContextWindow != 1000000 || profileUpdatedAt != 27 {
		t.Fatalf("migrated profile lost data: runtime=%q fast=%d context=%d updated=%d",
			profileRuntime, profileFastMode, profileContextWindow, profileUpdatedAt)
	}

	// The new value is now accepted on both tables.
	mustExec(t, db, `UPDATE threads SET runtime_mode = 'read-only' WHERE id = 'thread-v34'`)
	mustExec(t, db, `UPDATE chat_model_profiles SET runtime_mode = 'read-only' WHERE provider = 'claude'`)

	// Widening is not loosening: an unknown value is still refused.
	if _, err := db.Exec(`UPDATE threads SET runtime_mode = 'yolo' WHERE id = 'thread-v34'`); err == nil {
		t.Fatal("threads accepted an unknown runtime_mode after v34")
	}
	if _, err := db.Exec(`UPDATE chat_model_profiles SET runtime_mode = 'yolo' WHERE provider = 'claude'`); err == nil {
		t.Fatal("chat_model_profiles accepted an unknown runtime_mode after v34")
	}

	for _, index := range []string{
		"idx_threads_project", "idx_threads_updated", "idx_threads_parent",
		"idx_threads_forked_from", "idx_threads_pinned_at", "idx_chat_model_profiles_updated",
	} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v34: %v", index, err)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v34 rebuild")
	}
}

// TestRuntimeModeCheckMatchesProvider ties the SQL CHECK literal to
// provider.AllRuntimeModes. internal/store cannot import internal/provider
// (cycle), so the value set is written out by hand in two places; this test is
// what stops those copies from drifting. A mode added to the provider package
// without a migration would otherwise fail at INSERT time in production.
func TestRuntimeModeCheckMatchesProvider(t *testing.T) {
	s := newTestStore(t)
	for _, table := range []string{"threads", "chat_model_profiles"} {
		var schema string
		if err := s.db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&schema); err != nil {
			t.Fatalf("read %s schema: %v", table, err)
		}
		if !strings.Contains(schema, runtimeModeCheckV45) {
			t.Fatalf("%s does not carry the current runtime_mode CHECK\nwant substring: %s\ngot: %s",
				table, runtimeModeCheckV45, schema)
		}
	}

	for _, mode := range provider.AllRuntimeModes {
		if !strings.Contains(runtimeModeCheckV45, "'"+string(mode)+"'") {
			t.Errorf("runtime mode %q is canonical in internal/provider but absent from the SQL CHECK — add a migration", mode)
		}
		if got := normalizeRuntimeMode(string(mode)); got != string(mode) {
			t.Errorf("normalizeRuntimeMode(%q) = %q — store's copy of the value set has drifted", mode, got)
		}
	}
}

func TestMigrationV35CreatesWorkItemUnits(t *testing.T) {
	db := migrateThrough(t, 34)
	var before int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'work_item_units'`,
	).Scan(&before); err != nil {
		t.Fatalf("count work_item_units before v35: %v", err)
	}
	if before != 0 {
		t.Fatal("work_item_units existed before v35")
	}
	if err := applyMigration(db, migrationByVersion(t, 35)); err != nil {
		t.Fatalf("apply migration v35: %v", err)
	}
	for _, index := range []string{"idx_work_item_units_attempt", "idx_work_item_units_worktree"} {
		readIndexSQL(t, db, index)
	}

	mustExec(t, db, `INSERT INTO work_item_units
		(item_id, phase_id, attempt, unit_id, unit_index, kind, provider, model, status)
		VALUES ('item-v35', 'port', 1, 'port-0', 0, 'unit', 'claude', 'sonnet', 'pending')`)
	mustExec(t, db, `INSERT INTO work_item_units
		(item_id, phase_id, attempt, unit_id, unit_index, kind, status)
		VALUES ('item-v35', 'port', 1, 'merge', 1, 'join', 'pending')`)

	// The same unit id in a later attempt is a different row; within one
	// attempt it is a collision.
	mustExec(t, db, `INSERT INTO work_item_units
		(item_id, phase_id, attempt, unit_id, unit_index, kind, status)
		VALUES ('item-v35', 'port', 2, 'port-0', 0, 'unit', 'pending')`)
	if _, err := db.Exec(`INSERT INTO work_item_units
		(item_id, phase_id, attempt, unit_id, unit_index, kind, status)
		VALUES ('item-v35', 'port', 1, 'port-0', 3, 'unit', 'pending')`); err == nil {
		t.Fatal("duplicate unit id within one attempt was accepted")
	}

	invalid := []struct {
		name string
		sql  string
	}{
		{"unit kind", `UPDATE work_item_units SET kind = 'coordinator' WHERE unit_id = 'port-0' AND attempt = 1`},
		{"unit status", `UPDATE work_item_units SET status = 'paused' WHERE unit_id = 'port-0' AND attempt = 1`},
		{"phase attempt", `UPDATE work_item_units SET attempt = 0 WHERE unit_id = 'port-0' AND attempt = 1`},
		{"unit index", `UPDATE work_item_units SET unit_index = -1 WHERE unit_id = 'port-0' AND attempt = 1`},
		{"unit attempt", `UPDATE work_item_units SET unit_attempt = 0 WHERE unit_id = 'port-0' AND attempt = 1`},
		{"unit envelope", `UPDATE work_item_units SET envelope = '{' WHERE unit_id = 'port-0' AND attempt = 1`},
	}
	for _, test := range invalid {
		if _, err := db.Exec(test.sql); err == nil {
			t.Errorf("%s constraint accepted invalid value", test.name)
		}
	}
	for _, status := range []string{"pending", "running", "done", "failed", "dropped", "taken-over"} {
		mustExec(t, db, `UPDATE work_item_units SET status = ? WHERE unit_id = 'port-0' AND attempt = 1`, status)
	}

	// Unit rows carry no foreign keys, for the same reason phase rows do not:
	// the run record outlives the item it describes.
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, created_at)
		VALUES ('item-v35', 'project-v35', 'goal', 'wf', 'shared', 'running', 'manual', 1)`)
	mustExec(t, db, `DELETE FROM work_items WHERE id = 'item-v35'`)
	var retained int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_item_units WHERE item_id = 'item-v35'`).Scan(&retained); err != nil {
		t.Fatalf("count retained units: %v", err)
	}
	if retained != 3 {
		t.Fatalf("unit rows after work item delete = %d, want 3 (no FK cascade)", retained)
	}
}

func TestMigrationV36AddsUnitFailedReason(t *testing.T) {
	db := migrateThrough(t, 35)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, disposition, digest,
		 created_at, started_at, ended_at)
		VALUES ('item-v36', 'project-v36', 'keep goal', 'wf', 'shared', '{"id":"wf"}',
		 'needs-human', 'question', '{"ticket":"AO-2"}', 1, '/tmp/wt', 'ao-v36', 'main', '{}',
		 'agent', 'agent-ref-v36', 'triage-v36', '{"action":"merged"}', '{"whatHappened":"x"}',
		 4, 5, 6)`)
	if _, err := db.Exec(`UPDATE work_items SET reason = 'unit-failed' WHERE id = 'item-v36'`); err == nil {
		t.Fatal("work_items accepted 'unit-failed' before v36 — the CHECK was already wrong")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 36)); err != nil {
		t.Fatalf("apply migration v36: %v", err)
	}

	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v36' AND project_id = 'project-v36' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'shared' AND snapshot = '{"id":"wf"}'
		  AND state = 'needs-human' AND reason = 'question' AND seeds = '{"ticket":"AO-2"}'
		  AND step_mode = 1 AND worktree_path = '/tmp/wt' AND branch = 'ao-v36'
		  AND base_branch = 'main' AND budget = '{}' AND source = 'agent'
		  AND source_ref = 'agent-ref-v36' AND triage_thread_id = 'triage-v36'
		  AND disposition = '{"action":"merged"}' AND digest = '{"whatHappened":"x"}'
		  AND created_at = 4 AND started_at = 5 AND ended_at = 6`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v36 rebuild did not preserve the work item row")
	}
	mustExec(t, db, `UPDATE work_items SET reason = 'unit-failed' WHERE id = 'item-v36'`)
	// Widening is not loosening.
	if _, err := db.Exec(`UPDATE work_items SET reason = 'unit-exploded' WHERE id = 'item-v36'`); err == nil {
		t.Fatal("work_items accepted an unknown reason after v36")
	}
	if _, err := db.Exec(`UPDATE work_items SET state = 'queued' WHERE id = 'item-v36'`); err == nil {
		t.Fatal("v36 rebuild reintroduced the queued state")
	}
	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_project_created",
		"idx_work_items_state_created", "idx_work_items_triage_thread",
		"idx_work_items_agent_source_ref",
	} {
		readIndexSQL(t, db, index)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v36 rebuild")
	}
}

func TestMigrationV38AddsCallLinkage(t *testing.T) {
	db := migrateThrough(t, 37)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, disposition, digest,
		 created_at, started_at, ended_at)
		VALUES ('item-v38', 'project-v38', 'keep goal', 'wf', 'shared', '{"id":"wf"}',
		 'needs-human', 'unit-failed', '{"ticket":"AO-3"}', 1, '/tmp/wt', 'ao-v38', 'main', '{}',
		 'agent', 'agent-ref-v38', 'triage-v38', '{"action":"merged"}', '{"whatHappened":"x"}',
		 7, 8, 9)`)
	if _, err := db.Exec(`UPDATE work_items SET source = 'call' WHERE id = 'item-v38'`); err == nil {
		t.Fatal("work_items accepted the 'call' source before v38 — the CHECK was already wrong")
	}
	if _, err := db.Exec(`UPDATE work_items SET reason = 'child-failed' WHERE id = 'item-v38'`); err == nil {
		t.Fatal("work_items accepted 'child-failed' before v38 — the CHECK was already wrong")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 38)); err != nil {
		t.Fatalf("apply migration v38: %v", err)
	}

	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v38' AND project_id = 'project-v38' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'shared' AND snapshot = '{"id":"wf"}'
		  AND state = 'needs-human' AND reason = 'unit-failed' AND seeds = '{"ticket":"AO-3"}'
		  AND step_mode = 1 AND worktree_path = '/tmp/wt' AND branch = 'ao-v38'
		  AND base_branch = 'main' AND budget = '{}' AND source = 'agent'
		  AND source_ref = 'agent-ref-v38' AND triage_thread_id = 'triage-v38'
		  AND disposition = '{"action":"merged"}' AND digest = '{"whatHappened":"x"}'
		  AND parent_item_id = '' AND parent_phase_id = '' AND parent_attempt = 0
		  AND call_depth = 0
		  AND created_at = 7 AND started_at = 8 AND ended_at = 9`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v38 rebuild did not preserve the work item row, or defaulted its linkage wrong")
	}

	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source,
		 parent_item_id, parent_phase_id, parent_attempt, call_depth, created_at)
		VALUES ('child-v38', 'project-v38', 'called run', 'child-wf', 'shared', 'running', 'call',
		 'item-v38', 'audit', 1, 1, 10)`)
	mustExec(t, db, `UPDATE work_items SET reason = 'child-failed' WHERE id = 'item-v38'`)
	// Widening is not loosening.
	if _, err := db.Exec(`UPDATE work_items SET reason = 'child-exploded' WHERE id = 'item-v38'`); err == nil {
		t.Fatal("work_items accepted an unknown reason after v38")
	}
	if _, err := db.Exec(`UPDATE work_items SET source = 'summoned' WHERE id = 'child-v38'`); err == nil {
		t.Fatal("work_items accepted an unknown source after v38")
	}

	// Linkage is all-or-nothing: half a parent reference would make the run tree
	// unreadable in exactly the direction recovery walks it.
	partial := []struct {
		name, query string
	}{
		{"phase without item", `UPDATE work_items SET parent_phase_id = 'audit' WHERE id = 'item-v38'`},
		{"item without phase", `UPDATE work_items SET parent_phase_id = '' WHERE id = 'child-v38'`},
		{"item without attempt", `UPDATE work_items SET parent_attempt = 0 WHERE id = 'child-v38'`},
		{"item without depth", `UPDATE work_items SET call_depth = 0 WHERE id = 'child-v38'`},
		{"depth without item", `UPDATE work_items SET call_depth = 2 WHERE id = 'item-v38'`},
	}
	for _, tc := range partial {
		if _, err := db.Exec(tc.query); err == nil {
			t.Fatalf("work_items accepted partial call linkage (%s)", tc.name)
		}
	}

	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_project_created",
		"idx_work_items_state_created", "idx_work_items_triage_thread",
		"idx_work_items_agent_source_ref", "idx_work_items_parent",
	} {
		readIndexSQL(t, db, index)
	}
	assertPlanUses(t, db, "idx_work_items_parent",
		`EXPLAIN QUERY PLAN SELECT id FROM work_items
		 WHERE parent_item_id = ? AND parent_item_id <> ''`, "item-v38")
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v38 rebuild")
	}
}

func TestMigrationV39AddsOriginThreadAndPausedReason(t *testing.T) {
	db := migrateThrough(t, 38)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, disposition, digest,
		 parent_item_id, parent_phase_id, parent_attempt, call_depth,
		 created_at, started_at, ended_at)
		VALUES ('item-v39', 'project-v39', 'keep goal', 'wf', 'shared', '{"id":"wf"}',
		 'needs-human', 'child-failed', '{"ticket":"AO-9"}', 1, '/tmp/wt', 'ao-v39', 'main', '{}',
		 'agent', 'agent-ref-v39', 'triage-v39', '{"action":"merged"}', '{"whatHappened":"x"}',
		 '', '', 0, 0, 7, 8, 9)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source,
		 parent_item_id, parent_phase_id, parent_attempt, call_depth, created_at)
		VALUES ('child-v39', 'project-v39', 'called run', 'child-wf', 'shared', 'running', 'call',
		 'item-v39', 'audit', 1, 1, 10)`)
	if _, err := db.Exec(`UPDATE work_items SET reason = 'paused' WHERE id = 'item-v39'`); err == nil {
		t.Fatal("work_items accepted 'paused' before v39 — the CHECK was already wrong")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 39)); err != nil {
		t.Fatalf("apply migration v39: %v", err)
	}

	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v39' AND project_id = 'project-v39' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'shared' AND snapshot = '{"id":"wf"}'
		  AND state = 'needs-human' AND reason = 'child-failed' AND seeds = '{"ticket":"AO-9"}'
		  AND step_mode = 1 AND worktree_path = '/tmp/wt' AND branch = 'ao-v39'
		  AND base_branch = 'main' AND budget = '{}' AND source = 'agent'
		  AND source_ref = 'agent-ref-v39' AND triage_thread_id = 'triage-v39'
		  AND disposition = '{"action":"merged"}' AND digest = '{"whatHappened":"x"}'
		  AND parent_item_id = '' AND call_depth = 0 AND origin_thread_id = ''
		  AND created_at = 7 AND started_at = 8 AND ended_at = 9`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v39 rebuild did not preserve the work item row, or defaulted its binding wrong")
	}
	var child int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'child-v39' AND parent_item_id = 'item-v39' AND parent_phase_id = 'audit'
		  AND parent_attempt = 1 AND call_depth = 1 AND origin_thread_id = ''`).Scan(&child); err != nil {
		t.Fatalf("read preserved child: %v", err)
	}
	if child != 1 {
		t.Fatal("v39 rebuild did not preserve the call linkage of a child run")
	}

	mustExec(t, db, `UPDATE work_items SET reason = 'paused' WHERE id = 'item-v39'`)
	mustExec(t, db, `UPDATE work_items SET origin_thread_id = 'thread-v39' WHERE id = 'item-v39'`)
	// Widening is not loosening.
	if _, err := db.Exec(`UPDATE work_items SET reason = 'snoozed' WHERE id = 'item-v39'`); err == nil {
		t.Fatal("work_items accepted an unknown reason after v39")
	}
	// A called run can never carry a binding: children never notify as
	// themselves, so the structure refuses what the policy forbids.
	if _, err := db.Exec(`UPDATE work_items SET origin_thread_id = 'thread-v39' WHERE id = 'child-v39'`); err == nil {
		t.Fatal("work_items accepted an origin thread on a called run")
	}
	if _, err := db.Exec(`UPDATE work_items SET parent_item_id = 'child-v39', parent_phase_id = 'p',
		parent_attempt = 1, call_depth = 1 WHERE id = 'item-v39'`); err == nil {
		t.Fatal("work_items accepted call linkage on a bound run")
	}

	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_project_created",
		"idx_work_items_state_created", "idx_work_items_triage_thread",
		"idx_work_items_agent_source_ref", "idx_work_items_parent",
		"idx_work_items_origin_thread",
	} {
		readIndexSQL(t, db, index)
	}
	assertPlanUses(t, db, "idx_work_items_origin_thread",
		`EXPLAIN QUERY PLAN SELECT id FROM work_items
		 WHERE origin_thread_id = ? AND origin_thread_id <> ''`, "thread-v39")
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v39 rebuild")
	}
}

func TestMigrationV40AddsAutomationFireRecord(t *testing.T) {
	db := migrateThrough(t, 39)
	mustExec(t, db, `INSERT INTO automations
		(id, project_id, workflow_id, workflow_scope, name, enabled, trigger,
		 condition, seeds, notes, created_at, updated_at)
		VALUES ('auto-v40', 'project-v40', 'nightly', 'project', 'Nightly audit', 1,
		 '{"kind":"cron","expr":"0 3 * * *"}', '', '{"scope":"api"}', 'carry over', 5, 6)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, source_ref, created_at)
		VALUES ('run-v40', 'project-v40', 'Nightly audit', 'nightly', 'project', 'needs-human',
		 'automation', 'auto-v40', 7)`)
	if _, err := db.Exec(`UPDATE automations SET skip_count = 1 WHERE id = 'auto-v40'`); err == nil {
		t.Fatal("automations already had skip_count before v40")
	}

	if err := applyMigration(db, migrationByVersion(t, 40)); err != nil {
		t.Fatalf("apply migration v40: %v", err)
	}

	// Existing rows keep their definition and start at a zeroed fire record.
	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM automations
		WHERE id = 'auto-v40' AND name = 'Nightly audit' AND notes = 'carry over'
		  AND seeds = '{"scope":"api"}' AND created_at = 5 AND updated_at = 6
		  AND last_fired_at = 0 AND last_run_item_id = '' AND skip_count = 0
		  AND last_skip_at = 0 AND last_skip_reason = ''`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved automation: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v40 did not preserve the automation row or defaulted its fire record wrong")
	}

	mustExec(t, db, `UPDATE automations SET last_fired_at = 11, last_run_item_id = 'run-v40'
		WHERE id = 'auto-v40'`)
	mustExec(t, db, `UPDATE automations SET skip_count = skip_count + 1, last_skip_at = 12,
		last_skip_reason = 'run run-v40 is still running' WHERE id = 'auto-v40'`)
	var skips int64
	var reason string
	if err := db.QueryRow(`SELECT skip_count, last_skip_reason FROM automations
		WHERE id = 'auto-v40'`).Scan(&skips, &reason); err != nil {
		t.Fatalf("read fire record: %v", err)
	}
	if skips != 1 || reason != "run run-v40 is still running" {
		t.Fatalf("fire record = (%d, %q)", skips, reason)
	}

	readIndexSQL(t, db, "idx_work_items_automation_source_ref")
	assertPlanUses(t, db, "idx_work_items_automation_source_ref",
		`EXPLAIN QUERY PLAN SELECT id, state FROM work_items
		 WHERE source = 'automation' AND source_ref = ? AND source_ref <> ''
		   AND state IN ('running','needs-human')`, "auto-v40")
}

// seedRunningToolCallFixtures inserts one thread with a matching and a
// non-matching tool_call row for the v41/v42 running-tool-call partial
// indexes. isBackground selects which index's membership is under test.
func seedRunningToolCallFixtures(t *testing.T, db *sql.DB, isBackground int) {
	t.Helper()
	mustExec(t, db, `INSERT INTO projects
		(id, path, name, slug, created_at, updated_at)
		VALUES ('project-idx', '/tmp/idx', 'Idx', 'idx', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads
		(id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('thread-idx', 'project-idx', 'Idx', 'claude', '/tmp/idx', 1, 1)`)
	mustExec(t, db, fmt.Sprintf(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, is_background, created_at, updated_at)
		VALUES ('tc-live', 'thread-idx', 0, 0, 'tool_call', 'assistant', 'running', %d, 2, 2)`, isBackground))
	mustExec(t, db, fmt.Sprintf(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, is_background, created_at, updated_at)
		VALUES ('tc-settled', 'thread-idx', 0, 1, 'tool_call', 'assistant', 'completed', %d, 2, 2)`, isBackground))
}

func TestMigrationV41AddsRunningBackgroundToolCallIndex(t *testing.T) {
	db := migrateThrough(t, 40)
	seedRunningToolCallFixtures(t, db, 1)

	if err := applyMigration(db, migrationByVersion(t, 41)); err != nil {
		t.Fatalf("apply migration v41: %v", err)
	}

	readIndexSQL(t, db, "idx_items_running_bg_tool_calls")
	// Membership: only the still-running background launch is in the index.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items INDEXED BY idx_items_running_bg_tool_calls
		 WHERE kind = 'tool_call' AND status = 'running' AND is_background = 1
		   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0`).Scan(&count); err != nil {
		t.Fatalf("count via index: %v", err)
	}
	if count != 1 {
		t.Fatalf("index membership = %d, want 1", count)
	}
	// The startup recovery scan (ListRecoverableClaudeBackgroundLaunches'
	// outer predicate) must be served by the partial index, not a table scan.
	assertPlanUses(t, db, "idx_items_running_bg_tool_calls",
		`EXPLAIN QUERY PLAN SELECT items.id FROM items
		  JOIN threads ON threads.id = items.thread_id
		 WHERE threads.provider IN ('claude', 'claude-tui')
		   AND items.kind = 'tool_call'
		   AND items.status = 'running'
		   AND items.is_background = 1
		   AND COALESCE(json_extract(items.meta, '$.live_background_active'), 1) != 0`)
}

func TestMigrationV42AddsRunningForegroundToolCallIndex(t *testing.T) {
	db := migrateThrough(t, 41)
	seedRunningToolCallFixtures(t, db, 0)

	if err := applyMigration(db, migrationByVersion(t, 42)); err != nil {
		t.Fatalf("apply migration v42: %v", err)
	}

	readIndexSQL(t, db, "idx_items_running_fg_tool_calls")
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items INDEXED BY idx_items_running_fg_tool_calls
		 WHERE kind = 'tool_call' AND status = 'running' AND is_background = 0
		   AND parent_id = ''`).Scan(&count); err != nil {
		t.Fatalf("count via index: %v", err)
	}
	if count != 1 {
		t.Fatalf("index membership = %d, want 1", count)
	}
	// HasRunningTopLevelForegroundToolCall's probe must qualify for the index.
	assertPlanUses(t, db, "idx_items_running_fg_tool_calls",
		`EXPLAIN QUERY PLAN SELECT 1 FROM items
		 WHERE thread_id = ? AND kind = 'tool_call' AND status = 'running'
		   AND is_background = 0 AND parent_id = '' LIMIT 1`, "thread-idx")
}

func TestMigrationV43AddsUnitCallLinkage(t *testing.T) {
	db := migrateThrough(t, 42)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
		 seeds, step_mode, worktree_path, branch, base_branch, budget,
		 source, source_ref, triage_thread_id, origin_thread_id, disposition, digest,
		 parent_item_id, parent_phase_id, parent_attempt, call_depth,
		 created_at, started_at, ended_at)
		VALUES ('item-v43', 'project-v43', 'keep goal', 'wf', 'shared', '{"id":"wf"}',
		 'needs-human', 'unit-failed', '{"ticket":"AO-43"}', 1, '/tmp/wt', 'ao-v43', 'main', '{}',
		 'manual', 'ref-v43', 'triage-v43', 'thread-v43', '{"action":"merged"}', '{"whatHappened":"x"}',
		 '', '', 0, 0, 7, 8, 9)`)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source,
		 parent_item_id, parent_phase_id, parent_attempt, call_depth, created_at)
		VALUES ('child-v43', 'project-v43', 'called run', 'child-wf', 'shared', 'running', 'call',
		 'item-v43', 'audit', 1, 1, 10)`)
	if _, err := db.Exec(`UPDATE work_items SET parent_unit_id = 'u' WHERE id = 'child-v43'`); err == nil {
		t.Fatal("work_items already had parent_unit_id before v43")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 43)); err != nil {
		t.Fatalf("apply migration v43: %v", err)
	}

	// The rebuild derives its copy list textually from v39's, so the column v39
	// itself added is exactly the one a careless derivation would drop.
	var preserved int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'item-v43' AND project_id = 'project-v43' AND goal = 'keep goal'
		  AND workflow_id = 'wf' AND workflow_scope = 'shared' AND snapshot = '{"id":"wf"}'
		  AND state = 'needs-human' AND reason = 'unit-failed' AND seeds = '{"ticket":"AO-43"}'
		  AND step_mode = 1 AND worktree_path = '/tmp/wt' AND branch = 'ao-v43'
		  AND base_branch = 'main' AND budget = '{}' AND source = 'manual'
		  AND source_ref = 'ref-v43' AND triage_thread_id = 'triage-v43'
		  AND origin_thread_id = 'thread-v43'
		  AND disposition = '{"action":"merged"}' AND digest = '{"whatHappened":"x"}'
		  AND parent_item_id = '' AND parent_unit_id = '' AND call_depth = 0
		  AND created_at = 7 AND started_at = 8 AND ended_at = 9`).Scan(&preserved); err != nil {
		t.Fatalf("read preserved work item: %v", err)
	}
	if preserved != 1 {
		t.Fatal("v43 rebuild did not preserve the work item row, or dropped its thread binding")
	}
	// An existing call-phase child keeps its linkage and defaults to no unit,
	// which is what makes "called by a phase" the meaning of the empty value.
	var child int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE id = 'child-v43' AND parent_item_id = 'item-v43' AND parent_phase_id = 'audit'
		  AND parent_attempt = 1 AND call_depth = 1 AND parent_unit_id = ''`).Scan(&child); err != nil {
		t.Fatalf("read preserved child: %v", err)
	}
	if child != 1 {
		t.Fatal("v43 rebuild did not preserve the call linkage of a child run")
	}

	mustExec(t, db, `UPDATE work_items SET parent_unit_id = 'port-section' WHERE id = 'child-v43'`)
	// A unit id with no parent item would name a unit of no run: the linkage is
	// all-or-nothing in the same direction the other four CHECKs make it.
	if _, err := db.Exec(`UPDATE work_items SET parent_unit_id = 'port-section' WHERE id = 'item-v43'`); err == nil {
		t.Fatal("work_items accepted a parent unit on a run nothing called")
	}
	if _, err := db.Exec(`INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, parent_unit_id, created_at)
		VALUES ('orphan-v43', 'project-v43', 'orphan', 'wf', 'shared', 'running', 'call', 'u', 11)`); err == nil {
		t.Fatal("work_items accepted an orphan unit linkage")
	}
	// Two units of one attempt are told apart only by this column, which is the
	// whole reason it exists.
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source,
		 parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth, created_at)
		VALUES ('sibling-v43', 'project-v43', 'called run', 'child-wf', 'shared', 'running', 'call',
		 'item-v43', 'audit', 'port-other', 1, 1, 12)`)
	var siblings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM work_items
		WHERE parent_item_id = 'item-v43' AND parent_item_id <> ''
		  AND parent_phase_id = 'audit' AND parent_attempt = 1
		  AND parent_unit_id <> ''`).Scan(&siblings); err != nil {
		t.Fatalf("read unit children: %v", err)
	}
	if siblings != 2 {
		t.Fatalf("unit children of one attempt = %d, want 2", siblings)
	}

	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_project_created",
		"idx_work_items_state_created", "idx_work_items_triage_thread",
		"idx_work_items_agent_source_ref", "idx_work_items_parent",
		"idx_work_items_origin_thread", "idx_work_items_automation_source_ref",
	} {
		readIndexSQL(t, db, index)
	}
	assertPlanUses(t, db, "idx_work_items_parent",
		`EXPLAIN QUERY PLAN SELECT id FROM work_items
		 WHERE parent_item_id = ? AND parent_item_id <> ''`, "item-v43")
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v43 rebuild")
	}
}

// TestMigrationV45WidensRuntimeModeCheckWithAuto is the per-version test for
// the AI-reviewed `auto` tier. Same three properties every CHECK widening
// needs — the new value is accepted, garbage is still refused, and existing
// rows survive the rebuild — plus one specific to this value: `read-only`,
// added by v34, must still be accepted. v45 derives its SQL from v34's text,
// so a derivation that replaced the wrong occurrence (or only one of the two
// tables) would show up here as a tier silently disappearing.
func TestMigrationV45WidensRuntimeModeCheckWithAuto(t *testing.T) {
	db := migrateThrough(t, 44)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v45', '/tmp/v45', 'V45', 'green', 4, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived, disabled_mcp_servers
		) VALUES (
			'thread-v45', 'project-v45', 'Preserve me', 'codex', 'gpt-5.5-codex', '/tmp/v45', '/tmp/v45-wt',
			'feature/v45', 'github:45', 'session-v45', 'fork-v45', 'plan', 'xhigh',
			1, 400000, 45, 65, 'read-only', '{"input_tokens":45}', 31,
			32, 33, 34, 1, '["server-v45"]'
		)
	`)
	mustExec(t, db, `
		INSERT INTO items (
			id, thread_id, turn_index, item_index, kind, role, status, summary,
			parent_id, is_background, completion_of, tool_name, decision, meta,
			created_at, updated_at
		) VALUES (
			'item-v45', 'thread-v45', 1, 2, 'assistant_text', 'assistant', 'completed', 'kept',
			'', 0, '', '', '', '{}', 35, 36
		)
	`)
	mustExec(t, db, `
		INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window,
			auto_compact_standard_percent, auto_compact_extended_percent,
			runtime_mode, updated_at
		) VALUES ('codex', 'gpt-5.5-codex', 'xhigh', 1, 400000, 50, 70, 'read-only', 37)
	`)

	// Before the migration the tier is unrepresentable on both tables.
	if _, err := db.Exec(`UPDATE threads SET runtime_mode = 'auto' WHERE id = 'thread-v45'`); err == nil {
		t.Fatal("threads accepted 'auto' before v45 — the CHECK was already wrong")
	}
	if _, err := db.Exec(`UPDATE chat_model_profiles SET runtime_mode = 'auto'`); err == nil {
		t.Fatal("chat_model_profiles accepted 'auto' before v45 — the CHECK was already wrong")
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, 45)); err != nil {
		t.Fatalf("apply migration v45: %v", err)
	}

	var title, model, prRef, mode, tokenUsage, disabledServers, runtimeMode string
	var fastMode, contextWindow, archived int
	if err := db.QueryRow(`
		SELECT title, model, pr_ref, mode, last_token_usage, disabled_mcp_servers,
		       runtime_mode, fast_mode, context_window, archived
		FROM threads WHERE id = 'thread-v45'
	`).Scan(&title, &model, &prRef, &mode, &tokenUsage, &disabledServers,
		&runtimeMode, &fastMode, &contextWindow, &archived); err != nil {
		t.Fatalf("read migrated thread: %v", err)
	}
	if title != "Preserve me" || model != "gpt-5.5-codex" || prRef != "github:45" ||
		mode != "plan" || tokenUsage != `{"input_tokens":45}` ||
		disabledServers != `["server-v45"]` || runtimeMode != "read-only" ||
		fastMode != 1 || contextWindow != 400000 || archived != 1 {
		t.Fatalf("migrated thread lost data: title=%q model=%q pr_ref=%q mode=%q usage=%q disabled=%q runtime=%q fast=%d context=%d archived=%d",
			title, model, prRef, mode, tokenUsage, disabledServers, runtimeMode, fastMode, contextWindow, archived)
	}

	var itemSummary string
	if err := db.QueryRow(`SELECT summary FROM items WHERE id = 'item-v45'`).Scan(&itemSummary); err != nil {
		t.Fatalf("read child item after thread rebuild: %v", err)
	}
	if itemSummary != "kept" {
		t.Fatalf("item summary = %q, want kept", itemSummary)
	}

	var profileRuntime string
	var profileUpdatedAt int
	if err := db.QueryRow(`
		SELECT runtime_mode, updated_at FROM chat_model_profiles
		WHERE provider = 'codex' AND model = 'gpt-5.5-codex'
	`).Scan(&profileRuntime, &profileUpdatedAt); err != nil {
		t.Fatalf("read migrated model profile: %v", err)
	}
	if profileRuntime != "read-only" || profileUpdatedAt != 37 {
		t.Fatalf("migrated profile lost data: runtime=%q updated=%d", profileRuntime, profileUpdatedAt)
	}

	// Every canonical tier is representable on both tables afterwards — not
	// just the new one. The v34 tier is the one a bad derivation would drop.
	for _, mode := range provider.AllRuntimeModes {
		mustExec(t, db, `UPDATE threads SET runtime_mode = ? WHERE id = 'thread-v45'`, string(mode))
		mustExec(t, db, `UPDATE chat_model_profiles SET runtime_mode = ? WHERE provider = 'codex'`, string(mode))
	}

	// Widening is not loosening.
	for _, bad := range []string{"yolo", "auto_review", "AUTO"} {
		if _, err := db.Exec(`UPDATE threads SET runtime_mode = ? WHERE id = 'thread-v45'`, bad); err == nil {
			t.Errorf("threads accepted runtime_mode %q after v45", bad)
		}
		if _, err := db.Exec(`UPDATE chat_model_profiles SET runtime_mode = ? WHERE provider = 'codex'`, bad); err == nil {
			t.Errorf("chat_model_profiles accepted runtime_mode %q after v45", bad)
		}
	}

	for _, index := range []string{
		"idx_threads_project", "idx_threads_updated", "idx_threads_parent",
		"idx_threads_forked_from", "idx_threads_pinned_at", "idx_chat_model_profiles_updated",
	} {
		var found string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v45: %v", index, err)
		}
	}

	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after v45 rebuild")
	}
}

func TestMigrationV47AddsThreadWorktreeSetupState(t *testing.T) {
	db := migrateThrough(t, 46)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v47', '/tmp/v47', 'V47', 'green', 4, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived, disabled_mcp_servers
		) VALUES (
			'thread-v47', 'project-v47', 'Pre-migration row', 'codex', 'gpt-5.5-codex', '/tmp/v47', '/tmp/v47-wt',
			'feature/v47', '', '', '', 'chat', 'xhigh',
			0, 400000, 0, 0, 'auto', '', 31,
			NULL, 33, 34, 0, NULL
		)
	`)

	if err := applyMigration(db, migrationByVersion(t, 47)); err != nil {
		t.Fatalf("apply migration v47: %v", err)
	}

	// A row that predates the column reads as "nothing to say", never NULL:
	// the column is NOT NULL so every reader has one spelling of absence.
	var state string
	if err := db.QueryRow(
		`SELECT worktree_setup_state FROM threads WHERE id = 'thread-v47'`,
	).Scan(&state); err != nil {
		t.Fatalf("read worktree_setup_state: %v", err)
	}
	if state != "" {
		t.Fatalf("backfilled worktree_setup_state = %q, want empty", state)
	}

	for _, good := range []string{"running", "failed", ""} {
		if _, err := db.Exec(
			`UPDATE threads SET worktree_setup_state = ? WHERE id = 'thread-v47'`, good,
		); err != nil {
			t.Fatalf("threads rejected worktree_setup_state %q: %v", good, err)
		}
	}
	for _, bad := range []string{"succeeded", "cancelled", "RUNNING", "pending"} {
		if _, err := db.Exec(
			`UPDATE threads SET worktree_setup_state = ? WHERE id = 'thread-v47'`, bad,
		); err == nil {
			t.Errorf("threads accepted worktree_setup_state %q after v47", bad)
		}
	}
	if _, err := db.Exec(
		`UPDATE threads SET worktree_setup_state = NULL WHERE id = 'thread-v47'`,
	); err == nil {
		t.Error("threads accepted a NULL worktree_setup_state after v47")
	}

	// ADD COLUMN, not a rebuild: the threads indexes are untouched.
	for _, index := range []string{
		"idx_threads_project", "idx_threads_updated", "idx_threads_parent",
		"idx_threads_forked_from", "idx_threads_pinned_at",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v47: %v", index, err)
		}
	}
}

func TestMigrationV49DropsPerThreadMCPState(t *testing.T) {
	db := migrateThrough(t, 48)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v49', '/tmp/v49', 'V49', 'blue', 5, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived, disabled_mcp_servers
		) VALUES (
			'thread-v49', 'project-v49', 'Pre-migration row', 'claude', 'claude-opus-4-8', '/tmp/v49', NULL,
			NULL, '', '', '', 'chat', 'high',
			0, 1000000, 0, 0, 'full-access', '', 31,
			NULL, 33, 34, 0, '["stale-server"]'
		)
	`)
	mustExec(t, db, `
		INSERT INTO new_thread_mcp_defaults (provider, workspace_path, disabled_servers, updated_at)
		VALUES ('claude', '/tmp/v49', '["stale-server"]', 1)
	`)

	if err := applyMigration(db, migrationByVersion(t, 49)); err != nil {
		t.Fatalf("apply migration v49: %v", err)
	}

	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('threads') WHERE name = 'disabled_mcp_servers'`,
	).Scan(&count); err != nil {
		t.Fatalf("probe threads columns: %v", err)
	}
	if count != 0 {
		t.Error("disabled_mcp_servers column still present after v49")
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'new_thread_mcp_defaults'`,
	).Scan(&count); err != nil {
		t.Fatalf("probe sqlite_master: %v", err)
	}
	if count != 0 {
		t.Error("new_thread_mcp_defaults table still present after v49")
	}

	// The thread row itself survives the column drop.
	var title string
	if err := db.QueryRow(
		`SELECT title FROM threads WHERE id = 'thread-v49'`,
	).Scan(&title); err != nil {
		t.Fatalf("read migrated thread: %v", err)
	}
	if title != "Pre-migration row" {
		t.Fatalf("migrated thread title = %q", title)
	}

	// DROP COLUMN, not a rebuild: the threads indexes are untouched.
	for _, index := range []string{
		"idx_threads_project", "idx_threads_updated", "idx_threads_parent",
		"idx_threads_forked_from", "idx_threads_pinned_at",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v49: %v", index, err)
		}
	}
}

func TestMigrationV50AddsSessionImportState(t *testing.T) {
	db := migrateThrough(t, 49)
	mustExec(t, db, `
		INSERT INTO projects (id, path, name, color, sort_position, created_at, updated_at, archived)
		VALUES ('project-v50', '/tmp/v50', 'V50', 'red', 6, 10, 11, 0)
	`)
	mustExec(t, db, `
		INSERT INTO threads (
			id, project_id, title, provider, model, workspace_path, worktree_path,
			branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
			fast_mode, context_window, auto_compact_standard_percent,
			auto_compact_extended_percent, runtime_mode, last_token_usage, last_read_at,
			pinned_at, created_at, updated_at, archived
		) VALUES (
			'thread-v50', 'project-v50', 'Pre-migration row', 'claude', 'claude-opus-4-8', '/tmp/v50', NULL,
			NULL, '', '', '', 'chat', 'high',
			0, 1000000, 0, 0, 'full-access', '', 31,
			NULL, 33, 34, 0
		)
	`)

	if err := applyMigration(db, migrationByVersion(t, 50)); err != nil {
		t.Fatalf("apply migration v50: %v", err)
	}

	// A row that predates the column reads as "AO created this thread",
	// never NULL: the column is NOT NULL so absence has one spelling.
	var source string
	if err := db.QueryRow(
		`SELECT import_source FROM threads WHERE id = 'thread-v50'`,
	).Scan(&source); err != nil {
		t.Fatalf("read import_source: %v", err)
	}
	if source != "" {
		t.Fatalf("backfilled import_source = %q, want empty", source)
	}

	for _, good := range []string{"claude", "codex", ""} {
		if _, err := db.Exec(
			`UPDATE threads SET import_source = ? WHERE id = 'thread-v50'`, good,
		); err != nil {
			t.Fatalf("threads rejected import_source %q: %v", good, err)
		}
	}
	for _, bad := range []string{"claude-tui", "Claude", "gemini"} {
		if _, err := db.Exec(
			`UPDATE threads SET import_source = ? WHERE id = 'thread-v50'`, bad,
		); err == nil {
			t.Errorf("threads accepted import_source %q after v50", bad)
		}
	}
	if _, err := db.Exec(
		`UPDATE threads SET import_source = NULL WHERE id = 'thread-v50'`,
	); err == nil {
		t.Error("threads accepted a NULL import_source after v50")
	}

	mustExec(t, db, `
		INSERT INTO thread_import_state (thread_id, provider, source_path, source_session_id, imported_at)
		VALUES ('thread-v50', 'claude', '/tmp/v50/sess.jsonl', 'sess-1', 90)
	`)
	var leaf, lastUUID string
	var offset, imported, refreshed int64
	var lastTurnIndex, lastItemIndex int
	if err := db.QueryRow(
		`SELECT leaf_uuid, last_source_uuid, last_source_offset,
		        last_turn_index, last_item_index, imported_at, refreshed_at
		   FROM thread_import_state WHERE thread_id = 'thread-v50'`,
	).Scan(&leaf, &lastUUID, &offset, &lastTurnIndex, &lastItemIndex, &imported, &refreshed); err != nil {
		t.Fatalf("read thread_import_state: %v", err)
	}
	if leaf != "" || lastUUID != "" || offset != 0 || refreshed != 0 || imported != 90 {
		t.Fatalf("cursor defaults = %q/%q/%d/%d/%d", leaf, lastUUID, offset, imported, refreshed)
	}
	// -1, not 0, on BOTH halves: "imported nothing yet" must sort below
	// every real position or the divergence guard fires on turn 0 / item 0.
	// The pair is what makes the guard exact — item_index restarts at 0 in
	// every turn, so a lone item index names no position in a thread.
	if lastTurnIndex != -1 || lastItemIndex != -1 {
		t.Fatalf("cursor defaults = (%d, %d), want (-1, -1)", lastTurnIndex, lastItemIndex)
	}

	if _, err := db.Exec(`
		INSERT INTO thread_import_state (thread_id, provider, source_path, source_session_id, imported_at)
		VALUES ('thread-missing', 'claude', '/tmp/x.jsonl', 'sess-2', 1)
	`); err == nil {
		t.Error("thread_import_state accepted a row for an unknown thread")
	}
	if _, err := db.Exec(`
		INSERT INTO thread_import_state (thread_id, provider, source_path, source_session_id, imported_at)
		VALUES ('thread-v50-2', 'gemini', '/tmp/x.jsonl', 'sess-3', 1)
	`); err == nil {
		t.Error("thread_import_state accepted an unknown provider")
	}

	// Deleting the thread takes its cursor with it.
	mustExec(t, db, `DELETE FROM threads WHERE id = 'thread-v50'`)
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_import_state`).Scan(&remaining); err != nil {
		t.Fatalf("count thread_import_state: %v", err)
	}
	if remaining != 0 {
		t.Errorf("thread_import_state rows survived thread deletion: %d", remaining)
	}

	// ADD COLUMN + CREATE TABLE, not a rebuild: the threads indexes stand.
	for _, index := range []string{
		"idx_threads_project", "idx_threads_updated", "idx_threads_parent",
		"idx_threads_forked_from", "idx_threads_pinned_at",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v50: %v", index, err)
		}
	}
}

func TestMigrationV51AddsPhaseParkCause(t *testing.T) {
	db := migrateThrough(t, 50)
	// The attempt row alone: work_item_phases declares no foreign key, and the
	// column being added is entirely local to it.
	mustExec(t, db, `INSERT INTO work_item_phases
		(item_id, phase_id, attempt, status, started_at, ended_at)
		VALUES ('item-v51', 'work', 1, 'parked', 50, 51)`)

	if err := applyMigration(db, migrationByVersion(t, 51)); err != nil {
		t.Fatalf("apply migration v51: %v", err)
	}

	// A row that predates the column reads as "no engine-diagnosed cause",
	// never NULL: the column is NOT NULL so absence has one spelling, and an
	// attempt that parked before this migration genuinely has none recorded.
	var cause string
	if err := db.QueryRow(
		`SELECT park_cause FROM work_item_phases WHERE item_id = 'item-v51'`,
	).Scan(&cause); err != nil {
		t.Fatalf("read park_cause: %v", err)
	}
	if cause != "" {
		t.Fatalf("backfilled park_cause = %q, want empty", cause)
	}

	// The text is free-form — it is a Go error's message, not an enum — so the
	// only rules are NOT NULL and that arbitrary content round-trips.
	const engineCause = `provision worktree for item "item-v51": branch "ao/wave-3" already exists`
	if _, err := db.Exec(
		`UPDATE work_item_phases SET park_cause = ? WHERE item_id = 'item-v51'`, engineCause,
	); err != nil {
		t.Fatalf("work_item_phases rejected a park cause: %v", err)
	}
	if err := db.QueryRow(
		`SELECT park_cause FROM work_item_phases WHERE item_id = 'item-v51'`,
	).Scan(&cause); err != nil {
		t.Fatalf("re-read park_cause: %v", err)
	}
	if cause != engineCause {
		t.Fatalf("park_cause round-trip = %q, want %q", cause, engineCause)
	}
	if _, err := db.Exec(
		`UPDATE work_item_phases SET park_cause = NULL WHERE item_id = 'item-v51'`,
	); err == nil {
		t.Error("work_item_phases accepted a NULL park_cause after v51")
	}

	// ADD COLUMN, not a rebuild: the phase indexes and the attempt uniqueness
	// that every loop count is derived from are untouched.
	for _, index := range []string{
		"idx_work_item_phases_item_started", "idx_work_item_phases_thread",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v51: %v", index, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO work_item_phases
		(item_id, phase_id, attempt, status, started_at)
		VALUES ('item-v51', 'work', 1, 'running', 60)`); err == nil {
		t.Error("post-v51 work_item_phases accepted a duplicate attempt")
	}
}
func TestMigrationV52AddsWorkItemWakeSignature(t *testing.T) {
	db := migrateThrough(t, 51)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, created_at)
		VALUES ('item-v52', 'project', 'ship it', 'build', 'shared', 'needs-human', 'manual', 50)`)

	if err := applyMigration(db, migrationByVersion(t, 52)); err != nil {
		t.Fatalf("apply migration v52: %v", err)
	}

	// A run that predates the column has had nothing delivered as far as this
	// record is concerned, which is the state every wake delivers from. NOT NULL
	// so "nothing recorded" has exactly one spelling.
	var signature string
	if err := db.QueryRow(
		`SELECT wake_signature FROM work_items WHERE id = 'item-v52'`,
	).Scan(&signature); err != nil {
		t.Fatalf("read wake_signature: %v", err)
	}
	if signature != "" {
		t.Fatalf("backfilled wake_signature = %q, want empty", signature)
	}

	const delivered = `kind=rest run="item-v52" state="needs-human" reason="gate" phase="review" attempt=3`
	if _, err := db.Exec(
		`UPDATE work_items SET wake_signature = ? WHERE id = 'item-v52'`, delivered,
	); err != nil {
		t.Fatalf("work_items rejected a wake signature: %v", err)
	}
	if err := db.QueryRow(
		`SELECT wake_signature FROM work_items WHERE id = 'item-v52'`,
	).Scan(&signature); err != nil {
		t.Fatalf("re-read wake_signature: %v", err)
	}
	if signature != delivered {
		t.Fatalf("wake_signature round-trip = %q, want %q", signature, delivered)
	}
	if _, err := db.Exec(
		`UPDATE work_items SET wake_signature = NULL WHERE id = 'item-v52'`,
	); err == nil {
		t.Error("work_items accepted a NULL wake_signature after v52")
	}

	// ADD COLUMN, not a rebuild: the work_items indexes and the CHECKs that make
	// call linkage all-or-nothing survive untouched.
	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_parent",
		"idx_work_items_origin_thread",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v52: %v", index, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, created_at,
		 parent_item_id, origin_thread_id)
		VALUES ('item-v52-bound', 'project', 'ship it', 'build', 'shared', 'running', 'call', 60,
		 'item-v52', 'thread-1')`); err == nil {
		t.Error("post-v52 work_items let a called run bind a thread")
	}
}

func TestMigrationV53AddsWorkItemPendingGuidance(t *testing.T) {
	db := migrateThrough(t, 52)
	mustExec(t, db, `INSERT INTO work_items
		(id, project_id, goal, workflow_id, workflow_scope, state, source, created_at)
		VALUES ('item-v53', 'project', 'ship it', 'build', 'shared', 'running', 'manual', 50)`)

	if err := applyMigration(db, migrationByVersion(t, 53)); err != nil {
		t.Fatalf("apply migration v53: %v", err)
	}

	// A run that predates the column has nothing pending, which is the state
	// every phase entry delivers from. NOT NULL so "nothing pending" has exactly
	// one spelling and the engine never has to tell empty from absent.
	var guidance string
	if err := db.QueryRow(
		`SELECT pending_guidance FROM work_items WHERE id = 'item-v53'`,
	).Scan(&guidance); err != nil {
		t.Fatalf("read pending_guidance: %v", err)
	}
	if guidance != "" {
		t.Fatalf("backfilled pending_guidance = %q, want empty", guidance)
	}

	const pending = `[{"text":"narrow the review to the auth path","at":51,"by":"human"}]`
	if _, err := db.Exec(
		`UPDATE work_items SET pending_guidance = ? WHERE id = 'item-v53'`, pending,
	); err != nil {
		t.Fatalf("work_items rejected pending guidance: %v", err)
	}
	if err := db.QueryRow(
		`SELECT pending_guidance FROM work_items WHERE id = 'item-v53'`,
	).Scan(&guidance); err != nil {
		t.Fatalf("re-read pending_guidance: %v", err)
	}
	if guidance != pending {
		t.Fatalf("pending_guidance round-trip = %q, want %q", guidance, pending)
	}
	if _, err := db.Exec(
		`UPDATE work_items SET pending_guidance = NULL WHERE id = 'item-v53'`,
	); err == nil {
		t.Error("work_items accepted a NULL pending_guidance after v53")
	}

	// ADD COLUMN, not a rebuild: the indexes and the call-linkage CHECKs survive.
	for _, index := range []string{
		"idx_work_items_project_state_created", "idx_work_items_parent",
		"idx_work_items_origin_thread",
	} {
		var found string
		if err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&found); err != nil {
			t.Fatalf("index %s missing after v53: %v", index, err)
		}
	}
}
