package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openSQLiteDB(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrationFreshDB(t *testing.T) {
	s := newTestStore(t)

	// All tables should exist after fresh migration.
	tables := []string{
		"migration_versions", "threads", "items", "payloads",
		"channels", "channel_messages", "discussion_definitions", "design_artifacts",
		"attachments", "thread_drafts", "thread_checkpoints",
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

	if len(versions) != 9 {
		t.Fatalf("expected 9 migration versions, got %d", len(versions))
	}
	if versions[0].version != 1 || versions[0].name != "initial_schema" {
		t.Errorf("v1: got %d/%s", versions[0].version, versions[0].name)
	}
	if versions[1].version != 2 || versions[1].name != "parity_tables" {
		t.Errorf("v2: got %d/%s", versions[1].version, versions[1].name)
	}
	if versions[2].version != 3 || versions[2].name != "thread_fork_state" {
		t.Errorf("v3: got %d/%s", versions[2].version, versions[2].name)
	}
	if versions[3].version != 4 || versions[3].name != "subagent_correlation" {
		t.Errorf("v4: got %d/%s", versions[3].version, versions[3].name)
	}
	if versions[4].version != 5 || versions[4].name != "attachments" {
		t.Errorf("v5: got %d/%s", versions[4].version, versions[4].name)
	}
	if versions[5].version != 6 || versions[5].name != "thread_drafts" {
		t.Errorf("v6: got %d/%s", versions[5].version, versions[5].name)
	}
	if versions[6].version != 7 || versions[6].name != "thread_checkpoints" {
		t.Errorf("v7: got %d/%s", versions[6].version, versions[6].name)
	}
	if versions[7].version != 8 || versions[7].name != "thread_checkpoints_unique_thread_turn" {
		t.Errorf("v8: got %d/%s", versions[7].version, versions[7].name)
	}
	if versions[8].version != 9 || versions[8].name != "payload_gc_on_item_delete" {
		t.Errorf("v9: got %d/%s", versions[8].version, versions[8].name)
	}
}

func TestMigrationIdempotent(t *testing.T) {
	db := openSQLiteDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("first migration run: %v", err)
	}

	// Run again — all migrations already applied, should be a no-op.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second migration run: %v", err)
	}

	// Verify no duplicate version rows.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM migration_versions").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 9 {
		t.Errorf("expected 9 version rows after idempotent re-run, got %d", count)
	}
}

func TestMigrationExistingVersionedDBSkipsAppliedV1(t *testing.T) {
	db := openSQLiteDB(t)

	if _, err := db.Exec(createMigrationVersionsTableSQL); err != nil {
		t.Fatalf("create migration_versions table: %v", err)
	}
	if _, err := db.Exec(migrations[0].SQL); err != nil {
		t.Fatalf("apply legacy v1 schema: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
		migrations[0].Version,
		migrations[0].Name,
	); err != nil {
		t.Fatalf("record v1 version: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("run migrations on existing versioned db: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM migration_versions WHERE version = 1").Scan(&count); err != nil {
		t.Fatalf("count v1 version rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 v1 row, got %d", count)
	}

	var appliedVersions int
	if err := db.QueryRow("SELECT COUNT(*) FROM migration_versions").Scan(&appliedVersions); err != nil {
		t.Fatalf("count migration rows: %v", err)
	}
	if appliedVersions != 9 {
		t.Fatalf("expected 9 applied migrations, got %d", appliedVersions)
	}
}

func TestMigrationExistingLegacyDBSeedsTrackingAndAppliesV2(t *testing.T) {
	db := openSQLiteDB(t)

	if _, err := db.Exec(migrations[0].SQL); err != nil {
		t.Fatalf("apply legacy schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('legacy-thread', 'Legacy', 'claude', '/tmp', '', 1000, 1000)
	`); err != nil {
		t.Fatalf("insert legacy thread: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("run migrations on legacy db: %v", err)
	}

	rows, err := db.Query("SELECT version, name FROM migration_versions ORDER BY version")
	if err != nil {
		t.Fatalf("query migration versions: %v", err)
	}
	defer rows.Close()

	var versions []int
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatalf("scan migration row: %v", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read migration rows: %v", err)
	}

	if len(versions) != 9 {
		t.Fatalf("expected 9 applied migrations, got %d", len(versions))
	}
	expected := []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	for i, want := range expected {
		if versions[i] != want {
			t.Fatalf("unexpected migration versions: %v", versions)
		}
	}

	var threadCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads WHERE id = 'legacy-thread'").Scan(&threadCount); err != nil {
		t.Fatalf("count legacy thread: %v", err)
	}
	if threadCount != 1 {
		t.Fatalf("expected legacy thread to survive migration, got %d rows", threadCount)
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

func TestBackfillLegacyMigrationVersionsNoLegacySchema(t *testing.T) {
	db := openSQLiteDB(t)

	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}

	if err := backfillLegacyMigrationVersions(db); err != nil {
		t.Fatalf("backfill legacy migration versions: %v", err)
	}

	version, err := currentMigrationVersion(db)
	if err != nil {
		t.Fatalf("current migration version: %v", err)
	}
	if version != 0 {
		t.Fatalf("expected version 0 when no legacy schema is present, got %d", version)
	}
}

func TestMigrationExistingLegacyParityDBBackfillsVersionHistory(t *testing.T) {
	db := openSQLiteDB(t)

	if _, err := db.Exec(migrations[0].SQL); err != nil {
		t.Fatalf("apply legacy v1 schema: %v", err)
	}
	if _, err := db.Exec(migrations[1].SQL); err != nil {
		t.Fatalf("apply legacy v2 schema: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO threads (
			id, title, provider, workspace_path, model, created_at, updated_at,
			interaction_mode, branch, worktree_path, project_path, discussion_id, parent_thread_id
		) VALUES (
			'legacy-parity-thread', 'Legacy Parity', 'claude', '/tmp/workspace', 'sonnet', 1000, 1000,
			'design', 'feature/parity', '/tmp/worktree', '/tmp/project', 'discussion-1', NULL
		)
	`); err != nil {
		t.Fatalf("insert legacy parity thread: %v", err)
	}

	if err := runMigrations(db); err != nil {
		t.Fatalf("run migrations on legacy parity db: %v", err)
	}

	rows, err := db.Query("SELECT version, name FROM migration_versions ORDER BY version")
	if err != nil {
		t.Fatalf("query migration versions: %v", err)
	}
	defer rows.Close()

	type versionRow struct {
		version int
		name    string
	}
	var versions []versionRow
	for rows.Next() {
		var row versionRow
		if err := rows.Scan(&row.version, &row.name); err != nil {
			t.Fatalf("scan migration version: %v", err)
		}
		versions = append(versions, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read migration versions: %v", err)
	}

	if len(versions) != 9 {
		t.Fatalf("expected 9 migration rows after legacy backfill + new migrations, got %d", len(versions))
	}
	expectedVersions := []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
	for i, want := range expectedVersions {
		if versions[i].version != want {
			t.Fatalf("unexpected migration versions: %+v", versions)
		}
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads WHERE id = 'legacy-parity-thread'").Scan(&count); err != nil {
		t.Fatalf("count legacy parity thread: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected legacy parity thread to survive migration, got %d rows", count)
	}
}

func TestTableExistsReturnsFalseForMissingTable(t *testing.T) {
	db := openSQLiteDB(t)

	exists, err := tableExists(db, "missing_table")
	if err != nil {
		t.Fatalf("table exists lookup: %v", err)
	}
	if exists {
		t.Fatal("expected missing_table to be absent")
	}
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

// TestMigrationV9SweepsPreExistingOrphanPayloads seeds a database up to v8
// (where payloads could orphan), inserts an orphan, then runs v9 and
// verifies the orphan is deleted. Without the sweep, v9 would only fix
// future deletes while leaving historical garbage behind.
func TestMigrationV9SweepsPreExistingOrphanPayloads(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version == 9 {
			break
		}
		if _, err := db.Exec(m.SQL); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
			m.Version, m.Name,
		); err != nil {
			t.Fatalf("record v%d: %v", m.Version, err)
		}
	}

	// Seed: a thread, an item, and TWO payloads — one linked, one orphan.
	if _, err := db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('p-linked', 'diff', '{}', x'00', 1000)`); err != nil {
		t.Fatalf("seed linked payload: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('p-orphan', 'diff', '{}', x'00', 1001)`); err != nil {
		t.Fatalf("seed orphan payload: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, payload_id, parent_tool_use_id, created_at)
		VALUES ('i1', 't1', 0, 0, 'diff', 'assistant', 'x', 'p-linked', '', 1000)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Now apply v9.
	if err := applyMigration(db, migrations[8]); err != nil {
		t.Fatalf("apply v9: %v", err)
	}

	// The orphan must be gone; the linked one must survive.
	var linked, orphan int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p-linked'`).Scan(&linked); err != nil {
		t.Fatalf("count linked: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p-orphan'`).Scan(&orphan); err != nil {
		t.Fatalf("count orphan: %v", err)
	}
	if linked != 1 {
		t.Errorf("linked payload should survive v9 sweep, got %d", linked)
	}
	if orphan != 0 {
		t.Errorf("orphan payload should be swept by v9, got %d", orphan)
	}
}

func TestMigrationV8ThreadCheckpointsUniqueThreadTurn(t *testing.T) {
	s := newTestStore(t)

	// Thread FK must exist before we can insert a checkpoint.
	if _, err := s.db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-1', 't1', 0, 'refs/agent-overflow/x/0', 'deadbeef', 1000, '/tmp')`); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	// A different ref_name at the SAME (thread, turn) must violate the new
	// composite UNIQUE — this is the whole point of v8.
	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-2', 't1', 0, 'refs/agent-overflow/x/different', '', 2000, '/tmp')`); err == nil {
		t.Error("expected unique constraint violation for duplicate (thread_id, turn_index)")
	}

	// A different (thread, turn) pair must still be allowed.
	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-3', 't1', 1, 'refs/agent-overflow/x/1', '', 2000, '/tmp')`); err != nil {
		t.Errorf("second turn insert should succeed: %v", err)
	}
}

func TestMigrationV7ThreadCheckpointsCascadesOnThreadDelete(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-1', 't1', 0, 'refs/agent-overflow/x/0', '', 1000, '/tmp')`); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	if _, err := s.db.Exec(`DELETE FROM threads WHERE id = 't1'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM thread_checkpoints`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected cascading delete to drop checkpoints, still have %d", count)
	}
}
