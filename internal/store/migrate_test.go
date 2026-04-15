package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrationFreshDB(t *testing.T) {
	s := newTestStore(t)

	// All tables should exist after fresh migration.
	tables := []string{
		"migration_versions", "threads", "items", "payloads",
		"channels", "channel_messages", "discussion_definitions", "design_artifacts",
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
}

func TestMigrationVersionTracking(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query("SELECT version, name FROM migration_versions ORDER BY version")
	if err != nil {
		t.Fatalf("query migration_versions: %v", err)
	}
	defer rows.Close()

	type versionRow struct {
		version int
		name    string
	}
	var versions []versionRow
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.version, &v.name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	if len(versions) != 2 {
		t.Fatalf("expected 2 migration versions, got %d", len(versions))
	}
	if versions[0].version != 1 || versions[0].name != "initial_schema" {
		t.Errorf("v1: got %d/%s", versions[0].version, versions[0].name)
	}
	if versions[1].version != 2 || versions[1].name != "parity_tables" {
		t.Errorf("v2: got %d/%s", versions[1].version, versions[1].name)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	// Open a DB, run migrations, close, reopen — should not fail.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("first migration run: %v", err)
	}

	// Run again — all migrations already applied, should be a no-op.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second migration run: %v", err)
	}

	// Verify only 2 version rows (not duplicated).
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM migration_versions").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 version rows after idempotent re-run, got %d", count)
	}

	db.Close()
}

func TestMigrationV2NewColumns(t *testing.T) {
	s := newTestStore(t)

	// Verify the new thread columns exist by inserting a thread with new fields.
	_, err := s.db.Exec(`
		INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at,
			interaction_mode, branch, worktree_path, project_path, discussion_id, parent_thread_id)
		VALUES ('t-test', 'Test', 'claude', '/tmp', '', 1000, 1000,
			'design', 'feature-x', '/tmp/wt', '/project', 'disc-1', NULL)
	`)
	if err != nil {
		t.Fatalf("insert with new columns: %v", err)
	}

	var mode, branch, wtPath, projPath string
	var discID sql.NullString
	err = s.db.QueryRow(`
		SELECT interaction_mode, COALESCE(branch, ''), COALESCE(worktree_path, ''),
			project_path, discussion_id
		FROM threads WHERE id = 't-test'
	`).Scan(&mode, &branch, &wtPath, &projPath, &discID)
	if err != nil {
		t.Fatalf("query new columns: %v", err)
	}
	if mode != "design" {
		t.Errorf("interaction_mode = %q, want design", mode)
	}
	if branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", branch)
	}
	if wtPath != "/tmp/wt" {
		t.Errorf("worktree_path = %q, want /tmp/wt", wtPath)
	}
	if projPath != "/project" {
		t.Errorf("project_path = %q, want /project", projPath)
	}
	if !discID.Valid || discID.String != "disc-1" {
		t.Errorf("discussion_id = %v, want disc-1", discID)
	}
}

func TestMigrationV2NewTables(t *testing.T) {
	s := newTestStore(t)

	// Insert into channels.
	_, err := s.db.Exec(`
		INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)
	`)
	if err != nil {
		t.Fatalf("insert thread: %v", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO channels (id, thread_id, type, status, created_at, updated_at)
		VALUES ('ch-1', 't1', 'deliberation', 'open', 1000, 1000)
	`)
	if err != nil {
		t.Fatalf("insert channel: %v", err)
	}

	// Insert into channel_messages.
	_, err = s.db.Exec(`
		INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, content, created_at)
		VALUES ('msg-1', 'ch-1', 1, 'agent', 'a1', 'hello', 1000)
	`)
	if err != nil {
		t.Fatalf("insert channel message: %v", err)
	}

	// Insert into discussion_definitions.
	_, err = s.db.Exec(`
		INSERT INTO discussion_definitions (id, name, scope, definition, created_at, updated_at)
		VALUES ('dd-1', 'Code Review', 'global', '{}', 1000, 1000)
	`)
	if err != nil {
		t.Fatalf("insert discussion definition: %v", err)
	}

	// Insert into design_artifacts.
	_, err = s.db.Exec(`
		INSERT INTO design_artifacts (id, thread_id, title, kind, html_path, created_at)
		VALUES ('da-1', 't1', 'Landing Page', 'render', '/tmp/da-1.html', 1000)
	`)
	if err != nil {
		t.Fatalf("insert design artifact: %v", err)
	}
}

func TestMigrationChannelMessageUniqueness(t *testing.T) {
	s := newTestStore(t)

	// Set up thread + channel.
	s.db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`)
	s.db.Exec(`INSERT INTO channels (id, thread_id, type, status, created_at, updated_at)
		VALUES ('ch-1', 't1', 'deliberation', 'open', 1000, 1000)`)

	// First message at sequence 1 succeeds.
	_, err := s.db.Exec(`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, content, created_at)
		VALUES ('m1', 'ch-1', 1, 'agent', 'a1', 'first', 1000)`)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Duplicate sequence should fail.
	_, err = s.db.Exec(`INSERT INTO channel_messages (id, channel_id, sequence, from_type, from_id, content, created_at)
		VALUES ('m2', 'ch-1', 1, 'agent', 'a2', 'dupe', 1000)`)
	if err == nil {
		t.Error("expected unique constraint violation for duplicate channel+sequence")
	}
}
