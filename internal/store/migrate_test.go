package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
		"channels", "channel_messages", "discussion_definitions",
		"attachments", "thread_drafts", "thread_checkpoints", "turns",
		"proposed_plans", "proposed_plan_comments",
		"chat_bar_favorites", "chat_model_profiles",
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

	// design_snapshots was dropped in v43; a fresh DB must not have it.
	var dropped string
	err := s.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='design_snapshots'",
	).Scan(&dropped)
	if err == nil {
		t.Errorf("design_snapshots should be absent on fresh DB after v43, got %q", dropped)
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

	if len(versions) != len(migrations) {
		t.Fatalf("expected %d migration versions, got %d", len(migrations), len(versions))
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
	if versions[9].version != 10 || versions[9].name != "items_unique_turn_item_index" {
		t.Errorf("v10: got %d/%s", versions[9].version, versions[9].name)
	}
	if versions[10].version != 11 || versions[10].name != "threads_interaction_mode_check" {
		t.Errorf("v11: got %d/%s", versions[10].version, versions[10].name)
	}
	if versions[11].version != 12 || versions[11].name != "threads_runtime_mode" {
		t.Errorf("v12: got %d/%s", versions[11].version, versions[11].name)
	}
	if versions[12].version != 13 || versions[12].name != "projects_and_thread_reshape" {
		t.Errorf("v13: got %d/%s", versions[12].version, versions[12].name)
	}
	if versions[13].version != 14 || versions[13].name != "items_tool_call_lifecycle" {
		t.Errorf("v14: got %d/%s", versions[13].version, versions[13].name)
	}
	if versions[14].version != 15 || versions[14].name != "chat_rewrite_unified_items" {
		t.Errorf("v15: got %d/%s", versions[14].version, versions[14].name)
	}
	if versions[15].version != 16 || versions[15].name != "items_payload_id_index" {
		t.Errorf("v16: got %d/%s", versions[15].version, versions[15].name)
	}
	if versions[16].version != 17 || versions[16].name != "items_meta_task_id_index" {
		t.Errorf("v17: got %d/%s", versions[16].version, versions[16].name)
	}
	if versions[17].version != 18 || versions[17].name != "turns_table" {
		t.Errorf("v18: got %d/%s", versions[17].version, versions[17].name)
	}
	if versions[18].version != 19 || versions[18].name != "highlighted_content_columns" {
		t.Errorf("v19: got %d/%s", versions[18].version, versions[18].name)
	}
	if versions[19].version != 20 || versions[19].name != "thread_last_read_at" {
		t.Errorf("v20: got %d/%s", versions[19].version, versions[19].name)
	}
	if versions[20].version != 21 || versions[20].name != "items_live_background_index" {
		t.Errorf("v21: got %d/%s", versions[20].version, versions[20].name)
	}
	if versions[21].version != 22 || versions[21].name != "items_status_killed" {
		t.Errorf("v22: got %d/%s", versions[21].version, versions[21].name)
	}
	if versions[22].version != 23 || versions[22].name != "items_kind_terminal_interaction" {
		t.Errorf("v23: got %d/%s", versions[22].version, versions[22].name)
	}
	if versions[23].version != 24 || versions[23].name != "items_kind_notification" {
		t.Errorf("v24: got %d/%s", versions[23].version, versions[23].name)
	}
	if versions[24].version != 25 || versions[24].name != "remove_rendered_chat_html" {
		t.Errorf("v25: got %d/%s", versions[24].version, versions[24].name)
	}
	if versions[25].version != 26 || versions[25].name != "turns_thread_completed_index" {
		t.Errorf("v26: got %d/%s", versions[25].version, versions[25].name)
	}
	if versions[26].version != 27 || versions[26].name != "drop_empty_tool_call_result_payloads" {
		t.Errorf("v27: got %d/%s", versions[26].version, versions[26].name)
	}
	if versions[27].version != 28 || versions[27].name != "checkpoint_turn_counts" {
		t.Errorf("v28: got %d/%s", versions[27].version, versions[27].name)
	}
	if versions[28].version != 29 || versions[28].name != "thread_pinned_at" {
		t.Errorf("v29: got %d/%s", versions[28].version, versions[28].name)
	}
	if versions[29].version != 30 || versions[29].name != "proposed_plan_review" {
		t.Errorf("v30: got %d/%s", versions[29].version, versions[29].name)
	}
	if versions[30].version != 31 || versions[30].name != "thread_drafts_pending_plan_implementation" {
		t.Errorf("v31: got %d/%s", versions[30].version, versions[30].name)
	}
	if versions[31].version != 32 || versions[31].name != "thread_checkpoints_tool_paths" {
		t.Errorf("v32: got %d/%s", versions[31].version, versions[31].name)
	}
	if versions[32].version != 33 || versions[32].name != "chat_bar_favorites_and_profiles" {
		t.Errorf("v33: got %d/%s", versions[32].version, versions[32].name)
	}
	if versions[38].version != 39 || versions[38].name != "drop_dead_checkpoint_turn_columns" {
		t.Errorf("v39: got %d/%s", versions[38].version, versions[38].name)
	}
	if versions[39].version != 40 || versions[39].name != "message_keyed_checkpoints" {
		t.Errorf("v40: got %d/%s", versions[39].version, versions[39].name)
	}
}

func TestMigrationV39DropsDeadCheckpointTurnColumns(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 39 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}
	s := &Store{db: db}
	now := time.Now().UnixMilli()
	if err := s.CreateProject(Project{
		ID:        defaultTestProjectID,
		Path:      "/tmp/test",
		Name:      "Default Test Project",
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.CreateThread(makeThread("t-mig-v39", "claude")); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO thread_checkpoints (
		id, thread_id, turn_index, checkpoint_turn_count, turn_id, ref_name,
		baseline_sha, status, files, tool_paths, assistant_message_id,
		completed_at, captured_at, workspace_path
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"chk-v39", "t-mig-v39", 1, 1, "turn-v39",
		"refs/agent-overflow/checkpoints/v39/turn/1", "base-sha",
		"ready", `[{"path":"src/main.go","kind":"modified","additions":2,"deletions":1}]`,
		`["src/main.go"]`, "msg-v39", int64(456), int64(123), "/tmp/v39",
	); err != nil {
		t.Fatalf("seed pre-v39 checkpoint: %v", err)
	}
	var v39 *Migration
	for i := range migrations {
		if migrations[i].Version == 39 {
			v39 = &migrations[i]
			break
		}
	}
	if v39 == nil {
		t.Fatal("v39 migration missing from list")
	}
	if err := applyMigration(db, *v39); err != nil {
		t.Fatalf("apply v39: %v", err)
	}
	for _, column := range []string{"turn_index", "turn_id", "assistant_message_id", "completed_at"} {
		if columnExists(t, db, "thread_checkpoints", column) {
			t.Fatalf("thread_checkpoints.%s should not exist after v39", column)
		}
	}

	var id, refName, baselineSHA, toolPaths, files, workspace string
	var capturedAt int64
	if err := db.QueryRow(`SELECT id, ref_name, baseline_sha, tool_paths, files, captured_at, workspace_path
		FROM thread_checkpoints WHERE thread_id = ? AND checkpoint_turn_count = ?`,
		"t-mig-v39", 1,
	).Scan(&id, &refName, &baselineSHA, &toolPaths, &files, &capturedAt, &workspace); err != nil {
		t.Fatalf("read checkpoint after v39: %v", err)
	}
	if id != "chk-v39" || refName != "refs/agent-overflow/checkpoints/v39/turn/1" ||
		baselineSHA != "base-sha" || toolPaths != `["src/main.go"]` ||
		!strings.Contains(files, "src/main.go") || capturedAt != 123 || workspace != "/tmp/v39" {
		t.Fatalf("checkpoint row after v39 = id=%q ref=%q base=%q paths=%q files=%q captured=%d workspace=%q",
			id, refName, baselineSHA, toolPaths, files, capturedAt, workspace)
	}
}

func TestMigrationV33ChatBarFavoritesAndProfiles(t *testing.T) {
	s := newTestStore(t)

	for _, table := range []string{"chat_bar_favorites", "chat_model_profiles"} {
		var name string
		if err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&name); err != nil {
			t.Fatalf("%s table missing: %v", table, err)
		}
	}

	if _, err := s.db.Exec(
		`INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		 VALUES ('model', '', 'gpt-5.5', 'GPT 5.5', 1)`,
	); err == nil {
		t.Fatal("model favorite without provider inserted; want CHECK failure")
	}
	if _, err := s.db.Exec(
		`INSERT INTO chat_model_profiles (
			provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at
		) VALUES ('claude', 'claude-opus-4-7', 'bogus', 0, 1000000, 'full-access', 1)`,
	); err == nil {
		t.Fatal("profile with invalid effort inserted; want CHECK failure")
	}
}

// TestMigrationV29ThreadPinnedAt asserts the v29 migration adds the
// pinned_at column AND the partial index. A regression that drops the
// `WHERE pinned_at IS NOT NULL` predicate (turning the index into a
// dense index over every row) wouldn't fail any other test today.
func TestMigrationV29ThreadPinnedAt(t *testing.T) {
	s := newTestStore(t)

	// Column exists with INTEGER affinity.
	rows, err := s.db.Query(`PRAGMA table_info(threads)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "pinned_at" {
			found = true
			if typ != "INTEGER" {
				t.Errorf("pinned_at type = %q, want INTEGER", typ)
			}
			if notnull != 0 {
				t.Errorf("pinned_at NOT NULL = %d, want 0 (nullable)", notnull)
			}
		}
	}
	if !found {
		t.Fatalf("pinned_at column missing from threads table")
	}

	// Partial index exists with the expected WHERE clause.
	var indexSQL sql.NullString
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_threads_pinned_at'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("look up index: %v", err)
	}
	if !indexSQL.Valid {
		t.Fatalf("idx_threads_pinned_at index missing")
	}
	if !strings.Contains(indexSQL.String, "WHERE pinned_at IS NOT NULL") {
		t.Errorf("expected partial index predicate; got %q", indexSQL.String)
	}
}

func TestMigrationV40ThreadTrackedFiles(t *testing.T) {
	s := newTestStore(t)

	rows, err := s.db.Query(`PRAGMA table_info(thread_checkpoints)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	var (
		found       bool
		columnType  string
		columnDflt  sql.NullString
		columnNotNN int
	)
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		if name == "user_item_id" {
			found = true
			columnType = typ
			columnDflt = dflt
			columnNotNN = notnull
		}
	}
	if !found {
		t.Fatalf("user_item_id column missing from thread_checkpoints")
	}
	if columnType != "TEXT" {
		t.Errorf("user_item_id type = %q, want TEXT", columnType)
	}
	if columnNotNN != 1 {
		t.Errorf("user_item_id NOT NULL = %d, want 1", columnNotNN)
	}
	if columnDflt.Valid && strings.TrimSpace(columnDflt.String) != "" {
		t.Errorf("user_item_id default = %v, want none", columnDflt.String)
	}

	mustCreateThreadForCheckpoint(t, s, "t-mig-v32")
	if err := s.SaveCheckpoint(Checkpoint{
		ID: "chk-v40", ThreadID: "t-mig-v32", UserItemID: "t-mig-v32-user:1", TurnIndex: 1,
		RefName:    "refs/v40-test",
		CapturedAt: time.Now().UnixMilli(), WorkspacePath: "/w",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := s.GetCheckpointByUserItemID("t-mig-v32", "t-mig-v32-user:1")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if got.TurnIndex != 1 {
		t.Errorf("TurnIndex = %d, want 1", got.TurnIndex)
	}
	if err := s.UpsertTrackedFiles("t-mig-v32", 1, []string{"x.go", "y.go"}); err != nil {
		t.Fatalf("upsert tracked: %v", err)
	}
	tracked, err := s.ListTrackedFiles("t-mig-v32")
	if err != nil {
		t.Fatalf("list tracked: %v", err)
	}
	if len(tracked) != 2 || tracked[0] != "x.go" || tracked[1] != "y.go" {
		t.Errorf("tracked files = %v", tracked)
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
	if count != len(migrations) {
		t.Errorf("expected %d version rows after idempotent re-run, got %d", len(migrations), count)
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
	if appliedVersions != len(migrations) {
		t.Fatalf("expected %d applied migrations, got %d", len(migrations), appliedVersions)
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

	if len(versions) != len(migrations) {
		t.Fatalf("expected %d applied migrations, got %d", len(migrations), len(versions))
	}
	for i, want := range expectedMigrationVersions() {
		if versions[i] != want {
			t.Fatalf("unexpected migration versions: %v", versions)
		}
	}

	// v13 wipes the threads table and rebuilds — the legacy seed row
	// should NOT survive. This is the documented behaviour of the
	// breaking reset; the test locks it in so a future migration that
	// accidentally tries to preserve data fails here.
	var threadCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&threadCount); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if threadCount != 0 {
		t.Fatalf("expected v13 reset to clear threads, got %d rows", threadCount)
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

	if len(versions) != len(migrations) {
		t.Fatalf("expected %d migration rows after legacy backfill + new migrations, got %d", len(migrations), len(versions))
	}
	for i, want := range expectedMigrationVersions() {
		if versions[i].version != want {
			t.Fatalf("unexpected migration versions: %+v", versions)
		}
	}

	// v13 is a breaking reset: the legacy parity thread must NOT survive.
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&count); err != nil {
		t.Fatalf("count threads: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected v13 reset to clear threads, got %d rows", count)
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

// TestMigrationV13ThreadColumns verifies the v13 shape of the threads
// table. The legacy test that peeked at interaction_mode / project_path
// is obsolete after the reshape; this covers the new column list instead.
func TestMigrationV13ThreadColumns(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-test', '/project', '/project', 1000, 1000)
	`); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, mode, branch, worktree_path, discussion_id,
			reasoning_effort, fast_mode, context_window, runtime_mode)
		VALUES ('t-test', 'p-test', 'Test', 'claude', '/tmp', '', 1000, 1000,
			'design', 'feature-x', '/tmp/wt', 'disc-1',
			'xhigh', 1, 200000, 'full-access')
	`)
	if err != nil {
		t.Fatalf("insert with v13 columns: %v", err)
	}

	var (
		mode, branch, wtPath, effort, runtimeMode string
		fastMode, contextWindow                   int
		discID                                    sql.NullString
	)
	err = s.db.QueryRow(`
		SELECT mode, COALESCE(branch, ''), COALESCE(worktree_path, ''),
			reasoning_effort, fast_mode, context_window, runtime_mode, discussion_id
		FROM threads WHERE id = 't-test'
	`).Scan(&mode, &branch, &wtPath, &effort, &fastMode, &contextWindow, &runtimeMode, &discID)
	if err != nil {
		t.Fatalf("query new columns: %v", err)
	}
	if mode != "design" {
		t.Errorf("mode = %q, want design", mode)
	}
	if branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", branch)
	}
	if wtPath != "/tmp/wt" {
		t.Errorf("worktree_path = %q, want /tmp/wt", wtPath)
	}
	if effort != "xhigh" {
		t.Errorf("reasoning_effort = %q, want xhigh", effort)
	}
	if fastMode != 1 {
		t.Errorf("fast_mode = %d, want 1", fastMode)
	}
	if contextWindow != 200000 {
		t.Errorf("context_window = %d, want 200000", contextWindow)
	}
	if runtimeMode != "full-access" {
		t.Errorf("runtime_mode = %q, want full-access", runtimeMode)
	}
	if !discID.Valid || discID.String != "disc-1" {
		t.Errorf("discussion_id = %v, want disc-1", discID)
	}
}

func TestMigrationV2NewTables(t *testing.T) {
	s := newTestStore(t)

	// newTestStore already seeded a default project; attach this thread to it.
	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', ?, 'T', 'claude', '/tmp', '', 1000, 1000)
	`, defaultTestProjectID)
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
}

func TestMigrationChannelMessageUniqueness(t *testing.T) {
	s := newTestStore(t)

	// Set up thread + channel.
	s.db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', ?, 'T', 'claude', '/tmp', '', 1000, 1000)`, defaultTestProjectID)
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
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 8 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// Thread FK must exist before we can insert a checkpoint.
	if _, err := db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES
			('chk-old', 't1', 0, 'refs/agent-overflow/x/old', 'oldsha', 1000, '/tmp'),
			('chk-new', 't1', 0, 'refs/agent-overflow/x/new', 'newsha', 2000, '/tmp')`); err != nil {
		t.Fatalf("seed v7 checkpoints: %v", err)
	}

	if err := applyMigration(db, migrations[7]); err != nil {
		t.Fatalf("apply v8: %v", err)
	}

	var id, baseline string
	if err := db.QueryRow(`SELECT id, baseline_sha FROM thread_checkpoints WHERE thread_id = 't1' AND turn_index = 0`).Scan(&id, &baseline); err != nil {
		t.Fatalf("read migrated checkpoint: %v", err)
	}
	if id != "chk-new" || baseline != "newsha" {
		t.Fatalf("v8 should keep latest duplicate checkpoint, got id=%q baseline=%q", id, baseline)
	}

	if _, err := db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-duplicate', 't1', 0, 'refs/agent-overflow/x/duplicate', '', 3000, '/tmp')`); err == nil {
		t.Error("expected unique constraint violation for duplicate (thread_id, turn_index)")
	}

	// A different (thread, turn) pair must still be allowed.
	if _, err := db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-3', 't1', 1, 'refs/agent-overflow/x/1', '', 2000, '/tmp')`); err != nil {
		t.Errorf("second turn insert should succeed: %v", err)
	}
}

func TestMigrationV7ThreadCheckpointsCascadesOnThreadDelete(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version > 7 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO threads (id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t1', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES ('chk-1', 't1', 0, 'refs/agent-overflow/x/0', '', 1000, '/tmp')`); err != nil {
		t.Fatalf("insert checkpoint: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM threads WHERE id = 't1'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM thread_checkpoints`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected cascading delete to drop checkpoints, still have %d", count)
	}
}

// TestConfigureDatabaseEnablesWAL verifies that the WAL pragma was
// actually applied. With a real on-disk database, SQLite should accept
// WAL and PRAGMA journal_mode should report "wal". This catches the
// case where a schema change or library upgrade silently reverts
// journaling to rollback mode (slower write concurrency, checkpointing
// off).
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

// TestConfigureDatabaseWarnsOnFallback covers the documented warning
// path. In-memory databases can't use WAL (SQLite falls back to
// "memory" journaling). We point configureDatabase at an in-memory
// connection and assert the log emits the WAL-fallback warning so
// future maintainers see why their deployment isn't checkpointing.
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
	// Confirm the logged mode is not "wal" — an in-memory DB reports
	// "memory" (older SQLite builds may report "file" or similar).
	if strings.Contains(output, `returned "wal"`) {
		t.Errorf("unexpected: in-memory DB reported WAL journaling: %q", output)
	}
}

// TestModeCheckConstraintRejectsBogusValue confirms the v13 CHECK
// constraint on threads.mode does its job: a raw INSERT or UPDATE with
// an unsupported mode must fail with a CHECK violation. Renamed from
// the v11-era InteractionMode test; the column is now called `mode`
// and the enum dropped "default" in favor of "chat".
func TestModeCheckConstraintRejectsBogusValue(t *testing.T) {
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

	// Sanity: a valid mode does succeed.
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-ok', ?, 'Ok', 'claude', '/tmp', '', 1, 1, 0, 'plan')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("valid INSERT: %v", err)
	}

	// UPDATE to a bogus value must also fail.
	_, err = s.db.Exec(`UPDATE threads SET mode = 'xyz' WHERE id = 't-ok'`)
	if err == nil {
		t.Fatal("UPDATE to bogus mode must fail")
	}
}

// TestProviderCheckConstraintRejectsBogusValue guards the provider
// CHECK constraint in the v13-rebuilt threads table.
func TestProviderCheckConstraintRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-bogus-prov', ?, 'Bogus', 'xyz', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID)
	if err == nil {
		t.Fatal("INSERT with provider='xyz' must violate CHECK constraint")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}

	// Sanity: both allowed values succeed.
	for _, p := range []string{"claude", "codex"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-ok-"+p, defaultTestProjectID, "Ok", p, "/tmp", "", 1, 1, 0, "chat"); err != nil {
			t.Errorf("INSERT with provider=%q: %v", p, err)
		}
	}

	// UPDATE to a bogus value must also fail.
	_, err = s.db.Exec(`UPDATE threads SET provider = 'nope' WHERE id = 't-ok-claude'`)
	if err == nil {
		t.Error("UPDATE to bogus provider must fail")
	}
}

// TestRuntimeModeCheckConstraintRejectsBogusValue guards the CHECK
// constraint on the v13 runtime_mode column.
func TestRuntimeModeCheckConstraintRejectsBogusValue(t *testing.T) {
	s := newTestStore(t)

	_, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode, runtime_mode)
		VALUES ('t-bogus-runtime', ?, 'Bogus', 'claude', '/tmp', '', 1, 1, 0, 'chat', 'yolo')
	`, defaultTestProjectID)
	if err == nil {
		t.Fatal("INSERT with runtime_mode='yolo' must violate CHECK constraint")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
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

// TestRuntimeModeDefaultSeedsFullAccess asserts that rows inserted
// without specifying runtime_mode land on 'full-access'.
func TestRuntimeModeDefaultSeedsFullAccess(t *testing.T) {
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

// TestEffortCheckConstraintRejectsBogusValue guards the CHECK on the
// new v13 reasoning_effort column. Mirrors the mode/provider tests
// above so a future migration widening the enum doesn't sneak past.
func TestEffortCheckConstraintRejectsBogusValue(t *testing.T) {
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
		`, "t-eff-"+eff, defaultTestProjectID, "Ok", "claude", "/tmp", "", 1, 1, 0, "chat", eff); err != nil {
			t.Errorf("INSERT with reasoning_effort=%q: %v", eff, err)
		}
	}
	for _, eff := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		if _, err := s.db.Exec(`
			INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
				created_at, updated_at, archived, mode, reasoning_effort)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, "t-codex-eff-"+eff, defaultTestProjectID, "Ok", "codex", "/tmp", "", 1, 1, 0, "chat", eff); err != nil {
			t.Errorf("INSERT codex with reasoning_effort=%q: %v", eff, err)
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

func TestMigrationV38RewritesCodexMaxReasoningEffort(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version == 38 {
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

	if _, err := db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v38', '/tmp/v38', 'V38', 1, 1)
	`); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, model, workspace_path,
			mode, reasoning_effort, created_at, updated_at, archived)
		VALUES ('t-v38-codex-max', 'p-v38', 'Codex Max', 'codex', 'gpt-5.5', '/tmp/v38',
			'chat', 'max', 1, 1, 0)
	`); err != nil {
		t.Fatalf("insert pre-v38 thread: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO chat_model_profiles (provider, model, reasoning_effort, context_window, updated_at)
		VALUES ('codex', 'gpt-5.5', 'max', 1000000, 1)
	`); err != nil {
		t.Fatalf("insert pre-v38 profile: %v", err)
	}

	if _, err := db.Exec(v38SQL); err != nil {
		t.Fatalf("apply v38: %v", err)
	}

	var threadEffort string
	if err := db.QueryRow(`SELECT reasoning_effort FROM threads WHERE id = 't-v38-codex-max'`).Scan(&threadEffort); err != nil {
		t.Fatalf("select migrated thread effort: %v", err)
	}
	if threadEffort != "xhigh" {
		t.Fatalf("thread reasoning_effort = %q, want xhigh", threadEffort)
	}

	var profileEffort string
	if err := db.QueryRow(`SELECT reasoning_effort FROM chat_model_profiles WHERE provider = 'codex' AND model = 'gpt-5.5'`).Scan(&profileEffort); err != nil {
		t.Fatalf("select migrated profile effort: %v", err)
	}
	if profileEffort != "xhigh" {
		t.Fatalf("profile reasoning_effort = %q, want xhigh", profileEffort)
	}
}

// TestContextWindowCheckConstraintAcceptsPositiveValues confirms dynamic
// provider/model context windows are accepted while nonsensical windows are
// still rejected at the schema boundary.
func TestContextWindowCheckConstraintAcceptsPositiveValues(t *testing.T) {
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

// TestProjectsPathUnique asserts the UNIQUE constraint on projects.path.
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

// TestProjectThreadsCascade confirms deleting a project cascades to its
// threads (threads.project_id is ON DELETE CASCADE).
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

// TestInteractionModeMigrationNormalizesBadRows simulates an upgrade
// from v10 (no CHECK) with a pre-existing row whose interaction_mode
// is invalid. The migration must not abort — instead it normalises
// the offending rows to 'default' before rebuilding the table.
func TestInteractionModeMigrationNormalizesBadRows(t *testing.T) {
	db := openSQLiteDB(t)

	// Apply v1..v10 in order to mimic a pre-v11 database.
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configureDatabase: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensureMigrationTable: %v", err)
	}
	for _, m := range migrations {
		if m.Version > 10 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`
		INSERT INTO threads (id, title, provider, workspace_path, model,
			created_at, updated_at, archived, interaction_mode, project_path)
		VALUES ('t-stale', 'Stale', 'claude', '/tmp', '', 1, 1, 0, 'bogus_value', '/tmp')
	`); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	// Now apply v11 — the migration must rewrite the stale value and
	// the table rebuild must succeed despite the invalid pre-existing
	// data.
	for _, m := range migrations {
		if m.Version != 11 {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v11 with stale row: %v", err)
		}
	}

	var mode string
	if err := db.QueryRow("SELECT interaction_mode FROM threads WHERE id = 't-stale'").Scan(&mode); err != nil {
		t.Fatalf("read stale row after migration: %v", err)
	}
	if mode != "default" {
		t.Errorf("interaction_mode = %q, want %q (normalized)", mode, "default")
	}

	// Follow-up write with the same bogus value must now fail.
	_, err := db.Exec(`UPDATE threads SET interaction_mode = 'bogus_value' WHERE id = 't-stale'`)
	if err == nil {
		t.Error("UPDATE with bogus value must fail after v11")
	}
}

// TestMigrationV15PreservesThreadsAndWipesPayloads pins the v15 reset
// semantics: the chat-rewrite migration REBUILDS items + payloads from
// scratch (both tables are dropped and recreated), but threads are
// left intact — an existing thread survives, while its timeline and
// every stored blob are discarded. This is the contract callers rely
// on to seed their v14-schema chat history forward without losing the
// top-level project / thread structure.
//
// Seeded in parallel schema shape: we run every migration EXCEPT v15
// against an in-memory DB, seed one project + one thread + two items
// + two payloads, then apply v15 and assert the cut pattern
// (threads=1, items=0, payloads=0).
func TestMigrationV15PreservesThreadsAndWipesPayloads(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configureDatabase: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensureMigrationTable: %v", err)
	}
	// Apply v1..v14 — everything before the v15 rebuild. Later migrations
	// (v16+) are skipped here because they assume the v15-shaped tables.
	for _, m := range migrations {
		if m.Version >= 15 {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// Seed the parent rows.
	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-keep', '/tmp/keep', 'Keep Me', 500, 500)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t-keep', 'p-keep', 'Survives v15', 'claude', '/tmp', '', 500, 500)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Two items + two payloads. item.payload_id points at the payload
	// so the round-trip read would be valid before v15.
	if _, err := db.Exec(`INSERT INTO payloads
		(id, kind, meta, data, created_at) VALUES
		('pay-1', 'command_output', '{}', ?, 500),
		('pay-2', 'diff', '{}', ?, 500)`,
		[]byte("out"), []byte("patch")); err != nil {
		t.Fatalf("seed payloads: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, parent_tool_use_id,
		 status, is_background, completion_of_item_id, payload_id, created_at)
		VALUES
		('i-1', 't-keep', 0, 0, 'text', 'assistant', 'old body', '',
		 'completed', 0, '', 'pay-1', 500),
		('i-2', 't-keep', 0, 1, 'text', 'assistant', 'second body', '',
		 'completed', 0, '', 'pay-2', 500)`); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	// Baseline counts — confirm seeding actually landed before the cut.
	assertRowCount(t, db, "threads", 1)
	assertRowCount(t, db, "items", 2)
	assertRowCount(t, db, "payloads", 2)

	// Apply v15 in isolation.
	var v15 *Migration
	for i := range migrations {
		if migrations[i].Version == 15 {
			v15 = &migrations[i]
			break
		}
	}
	if v15 == nil {
		t.Fatal("v15 migration missing from list")
	}
	if err := applyMigration(db, *v15); err != nil {
		t.Fatalf("apply v15: %v", err)
	}

	// Threads survive the rebuild; items + payloads are wiped clean.
	assertRowCount(t, db, "threads", 1)
	assertRowCount(t, db, "items", 0)
	assertRowCount(t, db, "payloads", 0)

	// The surviving thread row still points at its project — project FK
	// cascades are unaffected by the reset, and project itself is
	// untouched.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM threads WHERE id = 't-keep' AND project_id = 'p-keep'`).Scan(&count); err != nil {
		t.Fatalf("query surviving thread: %v", err)
	}
	if count != 1 {
		t.Errorf("surviving thread count = %d, want 1", count)
	}
}

// TestMigrationV16AddsPayloadIDIndex pins the partial index added in v16
// so findItemByPayloadID runs as an index lookup rather than a full
// thread+item scan. The index is partial (payload_id IS NOT NULL) so it
// stays small even on long histories dominated by user/assistant-text
// rows with no payload.
func TestMigrationV16AddsPayloadIDIndex(t *testing.T) {
	s := newTestStore(t)

	var foundIndex bool
	rows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'items'`,
	)
	if err != nil {
		t.Fatalf("list item indexes: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "idx_items_payload_id" {
			foundIndex = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if !foundIndex {
		t.Error("idx_items_payload_id missing after v16")
	}

	// Confirm the index is partial with the expected WHERE clause so a
	// later migration that widens it (or drops the WHERE) surfaces as
	// a regression here.
	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_items_payload_id'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read index sql: %v", err)
	}
	if !strings.Contains(sqlText, "payload_id IS NOT NULL") {
		t.Errorf("expected partial index on payload_id IS NOT NULL, got: %s", sqlText)
	}
}

// TestMigrationV17AddsMetaTaskIDIndex pins the partial expression index
// added in v17 so findToolCallByTaskID can resolve the matching
// tool_call row via an index seek instead of a full thread scan +
// per-row JSON unmarshal. The index expression mirrors the lookup in
// FindToolCallByTaskID (which compares json_extract(meta,
// '$.task_id')), so it stays usable only while both shapes agree.
func TestMigrationV17AddsMetaTaskIDIndex(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_items_meta_task_id'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_items_meta_task_id sql: %v", err)
	}
	if !strings.Contains(sqlText, "json_extract(meta, '$.task_id')") {
		t.Errorf("expected index expression on json_extract(meta, '$.task_id'), got: %s", sqlText)
	}
	if !strings.Contains(sqlText, "WHERE json_extract(meta, '$.task_id') IS NOT NULL") {
		t.Errorf("expected partial predicate on task_id IS NOT NULL, got: %s", sqlText)
	}

	// Confirm the planner actually uses the index for the exact
	// predicate FindToolCallByTaskID emits — without this check a
	// future refactor of the query shape could silently degrade back
	// to a scan while leaving the index orphaned.
	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN
		 SELECT id FROM items
		  WHERE thread_id = ? AND json_extract(meta, '$.task_id') = ?`,
		"t", "task-1",
	)
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
	if !strings.Contains(plan.String(), "idx_items_meta_task_id") {
		t.Errorf("query plan did not use idx_items_meta_task_id: %s", plan.String())
	}
}

// TestMigrationV18CreatesTurnsTableOnFreshDB asserts the v18 migration
// installs the turns table with its CHECK constraint, its index, and
// its cascade FK on a fresh database. The broader migration pipeline
// is covered by TestMigrationFreshDB; this test focuses on shape
// assertions that TestMigrationFreshDB would miss (CHECK, index
// ordering).
func TestMigrationV18CreatesTurnsTableOnFreshDB(t *testing.T) {
	s := newTestStore(t)

	// Index exists and is the composite (thread_id, turn_index DESC).
	var indexSQL string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='turns_thread_index'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read turns_thread_index sql: %v", err)
	}
	if !strings.Contains(indexSQL, "turn_index DESC") {
		t.Errorf("index sql = %q, want turn_index DESC for newest-first scans", indexSQL)
	}
	if !strings.Contains(indexSQL, "thread_id") {
		t.Errorf("index sql = %q, missing thread_id column", indexSQL)
	}

	// CHECK constraint is present and rejects a negative turn_index via
	// raw SQL (bypasses any Go-level validation in InsertTurn so this
	// really tests the constraint, not the wrapper).
	if _, err := s.db.Exec(`
		INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v18', '/tmp/v18', 'v18', 1, 1)
	`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-v18', 'p-v18', 'v18', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	_, err := s.db.Exec(`
		INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('t-bad', 't-v18', -1, 1)
	`)
	if err == nil {
		t.Fatal("expected CHECK(turn_index >= 0) to reject negative index")
	}
	if !strings.Contains(err.Error(), "CHECK") && !strings.Contains(err.Error(), "constraint") {
		t.Errorf("error = %v, want CHECK constraint violation", err)
	}

	// Valid boundary: zero must be accepted.
	if _, err := s.db.Exec(`
		INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('t-zero', 't-v18', 0, 1)
	`); err != nil {
		t.Errorf("INSERT with turn_index=0 should succeed: %v", err)
	}
}

// TestMigrationV18UpgradesFromV17 drives the same migration on a database
// that's been migrated up to v17, seeded with real data, and then stepped
// forward. Mirrors the pattern in TestMigrationV15PreservesThreadsAndWipesPayloads
// and TestMigrationV9SweepsPreExistingOrphanPayloads.
func TestMigrationV18UpgradesFromV17(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	// Apply v1..v17.
	for _, m := range migrations {
		if m.Version >= 18 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// The turns table must NOT exist yet — we're at v17.
	exists, err := tableExists(db, "turns")
	if err != nil {
		t.Fatalf("tableExists(turns): %v", err)
	}
	if exists {
		t.Fatal("turns table exists at v17; migration v18 has not run yet")
	}

	// Seed a thread so that after v18 we can insert a turn row and
	// observe the FK wiring is live.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-upgrade', '/tmp/upgrade', 'upgrade', 1, 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-upgrade', 'p-upgrade', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Apply v18.
	var v18 *Migration
	for i := range migrations {
		if migrations[i].Version == 18 {
			v18 = &migrations[i]
			break
		}
	}
	if v18 == nil {
		t.Fatal("v18 migration missing from list")
	}
	if err := applyMigration(db, *v18); err != nil {
		t.Fatalf("apply v18 on top of v17: %v", err)
	}

	// Turns table now exists and accepts a row.
	exists, err = tableExists(db, "turns")
	if err != nil {
		t.Fatalf("tableExists(turns) post-v18: %v", err)
	}
	if !exists {
		t.Fatal("expected turns table after v18")
	}
	if _, err := db.Exec(`INSERT INTO turns (turn_id, thread_id, turn_index, started_at)
		VALUES ('t-first', 't-upgrade', 0, 1)`); err != nil {
		t.Fatalf("insert turn after v18 upgrade: %v", err)
	}

	// FK CASCADE wiring: delete the thread, turn row should go.
	if _, err := db.Exec(`DELETE FROM threads WHERE id = 't-upgrade'`); err != nil {
		t.Fatalf("delete thread: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM turns WHERE thread_id = 't-upgrade'`).Scan(&remaining); err != nil {
		t.Fatalf("count turns post-cascade: %v", err)
	}
	if remaining != 0 {
		t.Errorf("expected CASCADE to drop turn rows, still have %d", remaining)
	}
}

// TestMigrationV26AddsTurnCompletionIndex pins the partial covering index
// used by thread list reads and MarkThreadReadNow when they need the newest
// completed turn for a thread. In-flight rows have completed_at=NULL and are
// deliberately excluded from both the index and the aggregate query.
func TestMigrationV26AddsTurnCompletionIndex(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_turns_thread_completed'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_turns_thread_completed sql: %v", err)
	}
	if !strings.Contains(sqlText, "thread_id") || !strings.Contains(sqlText, "completed_at DESC") {
		t.Errorf("expected index on (thread_id, completed_at DESC), got: %s", sqlText)
	}
	if !strings.Contains(sqlText, "completed_at IS NOT NULL") {
		t.Errorf("expected partial predicate on completed_at IS NOT NULL, got: %s", sqlText)
	}

	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN
		 SELECT MAX(completed_at)
		   FROM turns
		  WHERE thread_id = ?
		    AND completed_at IS NOT NULL`,
		"thread-1",
	)
	if err != nil {
		t.Fatalf("explain latest completed turn query: %v", err)
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
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	if !strings.Contains(plan.String(), "idx_turns_thread_completed") {
		t.Errorf("query plan did not use idx_turns_thread_completed: %s", plan.String())
	}
}

// TestMigrationV21AddsLiveBackgroundIndex pins the partial covering
// index for the BackgroundTaskTray query. The index is both shape-
// asserted (partial WHERE predicate stays aligned with the SQL's
// launch-branch filter) and usage-asserted via EXPLAIN — a future
// refactor that widens the WHERE, or a query-shape drift that stops
// using it, regresses this test instead of silently degrading to a
// full thread scan.
func TestMigrationV21AddsLiveBackgroundIndex(t *testing.T) {
	s := newTestStore(t)

	var sqlText string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE name = 'idx_items_live_background'`,
	).Scan(&sqlText); err != nil {
		t.Fatalf("read idx_items_live_background sql: %v", err)
	}
	if !strings.Contains(sqlText, "is_background = 1") ||
		!strings.Contains(sqlText, "status = 'running'") ||
		!strings.Contains(sqlText, "live_background_active") {
		t.Errorf("expected partial predicate on active running background rows, got: %s", sqlText)
	}

	// The launch branch of ListLiveBackgroundTasks filters by
	// (thread_id, is_background=1, status='running'). EXPLAIN must
	// show the planner consults idx_items_live_background — a regression
	// here lands the query back on the generic idx_items_thread scan.
	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN
		 SELECT id FROM items
		  WHERE thread_id = ?
		    AND is_background = 1
		    AND status = 'running'
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0`,
		"t",
	)
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
	if !strings.Contains(plan.String(), "idx_items_live_background") {
		t.Errorf("query plan did not use idx_items_live_background: %s", plan.String())
	}
}

// TestMigrationV22AddsKilledStatus pins the Phase 1 contract for
// user-initiated stops: the items.status CHECK enum must include the
// literal 'killed', and an INSERT with status='killed' must succeed on
// a post-v22 DB. This is the store-level half of the feature that
// Claude's stop_task control_request flows through (see the parser,
// triage, and Wails-binding sides in internal/provider/claude,
// internal/triage, and app_claude_stop.go).
func TestMigrationV22AddsKilledStatus(t *testing.T) {
	s := newTestStore(t)

	// The CHECK clause must contain the exact literal 'killed'. A
	// refactor that widened the enum by any other spelling would
	// silently let the frontend render a status the UI doesn't branch
	// on — this check keeps the spelling honest.
	var itemsSQL string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='items'`,
	).Scan(&itemsSQL); err != nil {
		t.Fatalf("read items table sql: %v", err)
	}
	if !strings.Contains(itemsSQL, "'killed'") {
		t.Errorf("expected items.status CHECK to include 'killed', got table sql:\n%s", itemsSQL)
	}

	// Seed the parent rows so the FK holds, then insert a row with
	// status='killed'. An INSERT that violates the CHECK would return
	// a constraint error — a silent success here is the signal we
	// actually widened the enum.
	if _, err := s.db.Exec(`
		INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
			created_at, updated_at, archived, mode)
		VALUES ('t-killed', ?, 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')
	`, defaultTestProjectID); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-killed', 't-killed', 0, 0, 'tool_completion', 'assistant', 'killed',
			'Stopped by user', '', 1, 'i-launch', 'Bash', '', '{}', 1, 1)
	`); err != nil {
		t.Fatalf("INSERT with status='killed' must succeed post-v22: %v", err)
	}

	var got string
	if err := s.db.QueryRow(`SELECT status FROM items WHERE id = 'i-killed'`).Scan(&got); err != nil {
		t.Fatalf("read killed row: %v", err)
	}
	if got != "killed" {
		t.Errorf("status round-trip: got %q, want killed", got)
	}

	// A bogus status must still fail — the widening adds one value
	// without opening the CHECK to everything.
	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-bogus', 't-killed', 1, 0, 'tool_completion', 'assistant', 'stopped',
			'Bogus', '', 0, '', 'Bash', '', '{}', 1, 1)
	`); err == nil {
		t.Error("INSERT with status='stopped' must violate CHECK (widening didn't open the enum)")
	}
}

// TestMigrationV22PreservesExistingItems drives the v22 migration on a
// database that's been stepped through v21 with real row data, then
// confirms every row survives the rebuild with its original column
// values intact. The v22 migration widens a CHECK constraint; any
// drift that lost data here would be a silent regression in the
// rebuild-and-copy pattern.
func TestMigrationV22PreservesExistingItems(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 22 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// Seed: project, thread, and a mix of items covering every kind
	// and status the v15 enum allowed. Status=killed is NOT seeded
	// because it doesn't yet exist — the v22 test above validates
	// inserting it post-migration.
	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-v22', '/tmp/v22', 'v22', 1, 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model,
		 created_at, updated_at, archived, mode)
		VALUES ('t-v22', 'p-v22', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	seedItems := []struct {
		id, kind, role, status, summary string
		isBackground                    int
		completionOf                    string
	}{
		{"i-user", "user_text", "user", "completed", "hi", 0, ""},
		{"i-asst", "assistant_text", "assistant", "completed", "yo", 0, ""},
		{"i-bash", "tool_call", "assistant", "running", "Bash: sleep 30", 1, ""},
		{"i-done", "tool_completion", "assistant", "errored", "Bash: failed", 1, "i-bash"},
		{"i-stream", "thinking", "assistant", "streaming", "thinking...", 0, ""},
		{"i-declined", "tool_call", "assistant", "declined", "user said no", 0, ""},
	}
	for i, it := range seedItems {
		if _, err := db.Exec(`INSERT INTO items
			(id, thread_id, turn_index, item_index, kind, role, status, summary,
			 parent_id, is_background, completion_of, tool_name, decision,
			 meta, created_at, updated_at, highlighted_content)
			VALUES (?, 't-v22', 0, ?, ?, ?, ?, ?, '', ?, ?, '', '', '{}', 1, 1, '')`,
			it.id, i, it.kind, it.role, it.status, it.summary,
			it.isBackground, it.completionOf); err != nil {
			t.Fatalf("seed item %s: %v", it.id, err)
		}
	}

	var preCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE thread_id = 't-v22'`).Scan(&preCount); err != nil {
		t.Fatalf("pre-v22 count: %v", err)
	}
	if preCount != len(seedItems) {
		t.Fatalf("pre-v22 seeded %d items but found %d", len(seedItems), preCount)
	}

	// Apply v22 in isolation.
	var v22 *Migration
	for i := range migrations {
		if migrations[i].Version == 22 {
			v22 = &migrations[i]
			break
		}
	}
	if v22 == nil {
		t.Fatal("v22 migration missing from list")
	}
	if err := applyMigration(db, *v22); err != nil {
		t.Fatalf("apply v22: %v", err)
	}

	// Every seeded row must survive with matching status / kind.
	for _, it := range seedItems {
		var gotStatus, gotKind string
		if err := db.QueryRow(`SELECT status, kind FROM items WHERE id = ?`, it.id).Scan(&gotStatus, &gotKind); err != nil {
			t.Errorf("%s: post-v22 lookup: %v", it.id, err)
			continue
		}
		if gotStatus != it.status {
			t.Errorf("%s: status = %q, want %q (v22 must preserve row values)", it.id, gotStatus, it.status)
		}
		if gotKind != it.kind {
			t.Errorf("%s: kind = %q, want %q", it.id, gotKind, it.kind)
		}
	}
}

// TestMigrationV23AddsTerminalInteractionKind pins the Phase 6 contract
// for the Codex "Waited for background terminal" cell: the items.kind
// CHECK enum must contain the exact literal 'terminal_interaction' and
// an INSERT with that kind must succeed on a post-v23 DB. The widening
// is strict — bogus kinds must still fail.
func TestMigrationV23AddsTerminalInteractionKind(t *testing.T) {
	s := newTestStore(t)

	var itemsSQL string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='items'`,
	).Scan(&itemsSQL); err != nil {
		t.Fatalf("read items table sql: %v", err)
	}
	if !strings.Contains(itemsSQL, "'terminal_interaction'") {
		t.Errorf("expected items.kind CHECK to include 'terminal_interaction', got table sql:\n%s", itemsSQL)
	}

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
		t.Fatalf("INSERT with kind='terminal_interaction' must succeed post-v23: %v", err)
	}

	var got string
	if err := s.db.QueryRow(`SELECT kind FROM items WHERE id = 'i-waited'`).Scan(&got); err != nil {
		t.Fatalf("read terminal_interaction row: %v", err)
	}
	if got != "terminal_interaction" {
		t.Errorf("kind round-trip: got %q, want terminal_interaction", got)
	}

	// A bogus kind must still fail — the widening adds exactly one value
	// without opening the CHECK to everything.
	if _, err := s.db.Exec(`
		INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
			summary, parent_id, is_background, completion_of, tool_name, decision,
			meta, created_at, updated_at)
		VALUES ('i-bogus', 't-ti', 1, 0, 'not_a_kind', 'assistant', 'completed',
			'Bogus', '', 0, '', '', '', '{}', 1, 1)
	`); err == nil {
		t.Error("INSERT with kind='not_a_kind' must violate CHECK (widening didn't open the enum)")
	}
}

// TestMigrationV23PreservesExistingItems drives the v23 migration on a
// database stepped through v22 with real row data, then confirms every
// row survives the rebuild with its original column values intact. The
// v23 migration widens the kind CHECK enum; drift that lost data here
// would be a silent regression in the rebuild-and-copy pattern.
func TestMigrationV23PreservesExistingItems(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 23 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-v23', '/tmp/v23', 'v23', 1, 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model,
		 created_at, updated_at, archived, mode)
		VALUES ('t-v23', 'p-v23', 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}

	// Seed a row for each kind allowed by the v22 enum plus one with
	// status='killed' to prove the v22 status widening survives v23.
	seedItems := []struct {
		id, kind, role, status, summary string
		isBackground                    int
	}{
		{"i-user", "user_text", "user", "completed", "hi", 0},
		{"i-asst", "assistant_text", "assistant", "completed", "yo", 0},
		{"i-think", "thinking", "assistant", "streaming", "thinking...", 0},
		{"i-call", "tool_call", "assistant", "running", "Bash", 0},
		{"i-done", "tool_completion", "assistant", "completed", "Bash done", 0},
		{"i-err", "error", "system", "completed", "err", 0},
		{"i-comp", "compaction", "system", "completed", "compacted", 0},
		{"i-killed", "tool_completion", "assistant", "killed", "stopped", 1},
	}
	for i, it := range seedItems {
		if _, err := db.Exec(`INSERT INTO items
			(id, thread_id, turn_index, item_index, kind, role, status, summary,
			 parent_id, is_background, completion_of, tool_name, decision,
			 meta, created_at, updated_at, highlighted_content)
			VALUES (?, 't-v23', 0, ?, ?, ?, ?, ?, '', ?, '', '', '', '{}', 1, 1, '')`,
			it.id, i, it.kind, it.role, it.status, it.summary,
			it.isBackground); err != nil {
			t.Fatalf("seed item %s: %v", it.id, err)
		}
	}

	var v23 *Migration
	for i := range migrations {
		if migrations[i].Version == 23 {
			v23 = &migrations[i]
			break
		}
	}
	if v23 == nil {
		t.Fatal("v23 migration missing from list")
	}
	if err := applyMigration(db, *v23); err != nil {
		t.Fatalf("apply v23: %v", err)
	}

	for _, it := range seedItems {
		var gotStatus, gotKind string
		if err := db.QueryRow(`SELECT status, kind FROM items WHERE id = ?`, it.id).Scan(&gotStatus, &gotKind); err != nil {
			t.Errorf("%s: post-v23 lookup: %v", it.id, err)
			continue
		}
		if gotStatus != it.status {
			t.Errorf("%s: status = %q, want %q (v23 must preserve row values)", it.id, gotStatus, it.status)
		}
		if gotKind != it.kind {
			t.Errorf("%s: kind = %q, want %q", it.id, gotKind, it.kind)
		}
	}
}

func TestMigrationV27DropsEmptyToolCallResultPayloads(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 27 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-v27', '/tmp/v27', 'v27', 1, 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model,
		 created_at, updated_at, archived, mode)
		VALUES ('t-v27', 'p-v27', 'T', 'codex', '/tmp', '', 1, 1, 0, 'chat')`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES
			('p-empty', 'tool_call_result', '{}', '', 1),
			('p-body', 'tool_call_result', '{}', 'real output', 1),
			('p-command-empty', 'command_output', '{}', '', 1)`); err != nil {
		t.Fatalf("seed payloads: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary,
		 payload_id, parent_id, is_background, completion_of, tool_name, decision,
		 meta, created_at, updated_at)
		VALUES
			('i-empty', 't-v27', 0, 0, 'tool_call', 'assistant', 'completed',
			 'WebSearch', 'p-empty', '', 0, '', 'WebSearch', '', '{}', 1, 1),
			('i-body', 't-v27', 0, 1, 'tool_call', 'assistant', 'completed',
			 'MCP', 'p-body', '', 0, '', 'MCP/lookup', '', '{}', 1, 1),
			('i-command-empty', 't-v27', 0, 2, 'tool_call', 'assistant', 'completed',
			 'Bash', 'p-command-empty', '', 0, '', 'Bash', '', '{}', 1, 1)`); err != nil {
		t.Fatalf("seed items: %v", err)
	}

	var v27 *Migration
	for i := range migrations {
		if migrations[i].Version == 27 {
			v27 = &migrations[i]
			break
		}
	}
	if v27 == nil {
		t.Fatal("v27 migration missing from list")
	}
	if err := applyMigration(db, *v27); err != nil {
		t.Fatalf("apply v27: %v", err)
	}

	var emptyPayloadID string
	if err := db.QueryRow(`SELECT COALESCE(payload_id, '') FROM items WHERE id = 'i-empty'`).Scan(&emptyPayloadID); err != nil {
		t.Fatalf("read empty item: %v", err)
	}
	if emptyPayloadID != "" {
		t.Fatalf("empty tool_call_result payload still linked: %q", emptyPayloadID)
	}

	var emptyPayloadRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payloads WHERE id = 'p-empty'`).Scan(&emptyPayloadRows); err != nil {
		t.Fatalf("count empty payload: %v", err)
	}
	if emptyPayloadRows != 0 {
		t.Fatalf("empty tool_call_result payload was not deleted")
	}

	for _, id := range []string{"i-body", "i-command-empty"} {
		var payloadID string
		if err := db.QueryRow(`SELECT COALESCE(payload_id, '') FROM items WHERE id = ?`, id).Scan(&payloadID); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if payloadID == "" {
			t.Fatalf("%s payload was incorrectly unlinked", id)
		}
	}
}

func TestMigrationV28BackfillsCheckpointTurnCountsByProvider(t *testing.T) {
	db := openSQLiteDB(t)

	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 28 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO projects
		(id, path, name, created_at, updated_at)
		VALUES ('p-v28', '/tmp/v28', 'v28', 1, 1)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads
		(id, project_id, title, provider, workspace_path, model,
		 created_at, updated_at, archived, mode)
		VALUES
			('t-codex', 'p-v28', 'Codex', 'codex', '/tmp', '', 1, 1, 0, 'chat'),
			('t-claude', 'p-v28', 'Claude', 'claude', '/tmp', '', 1, 1, 0, 'chat')`); err != nil {
		t.Fatalf("seed threads: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO thread_checkpoints
		(id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
		VALUES
			('codex-baseline', 't-codex', 1, 'refs/agent-overflow/codex/1', '', 1, '/tmp'),
			('codex-turn-1', 't-codex', 2, 'refs/agent-overflow/codex/2', '', 2, '/tmp'),
			('claude-baseline', 't-claude', 0, 'refs/agent-overflow/claude/0', '', 1, '/tmp'),
			('claude-turn-1', 't-claude', 1, 'refs/agent-overflow/claude/1', '', 2, '/tmp')`); err != nil {
		t.Fatalf("seed checkpoints: %v", err)
	}

	var v28 *Migration
	for i := range migrations {
		if migrations[i].Version == 28 {
			v28 = &migrations[i]
			break
		}
	}
	if v28 == nil {
		t.Fatal("v28 migration missing from list")
	}
	if err := applyMigration(db, *v28); err != nil {
		t.Fatalf("apply v28: %v", err)
	}

	got := map[string]int{}
	rows, err := db.Query(`SELECT id, checkpoint_turn_count FROM thread_checkpoints`)
	if err != nil {
		t.Fatalf("query checkpoints: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var turnCount int
		if err := rows.Scan(&id, &turnCount); err != nil {
			t.Fatalf("scan checkpoint: %v", err)
		}
		got[id] = turnCount
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	want := map[string]int{
		"codex-baseline":  0,
		"codex-turn-1":    1,
		"claude-baseline": 0,
		"claude-turn-1":   1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkpoint turn counts = %#v, want %#v", got, want)
	}
}

// TestMigrationV43DropsDesignSnapshots seeds a database up to v42 (the
// design_snapshots table being live), inserts a row, runs v43, and
// verifies the table is gone. v43 drops the speculative snapshot ladder
// because conversation-level rewind ended up being the right layer.
func TestMigrationV43DropsDesignSnapshots(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 43 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// Sanity: design_snapshots exists at v42.
	var pre string
	if err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='design_snapshots'",
	).Scan(&pre); err != nil {
		t.Fatalf("design_snapshots should exist before v43: %v", err)
	}

	// Seed a project (v13 made project_id NOT NULL on threads), then a
	// thread the snapshot can FK to, then a snapshot row.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-design', '/design', '/design', 1000, 1000)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t-design', 'p-design', 'design', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO design_snapshots (id, thread_id, label, dir_path, parent_snapshot_id, auto, created_at)
		VALUES ('snap-1', 't-design', 'manual', '/tmp/snap-1', NULL, 0, 1500)`); err != nil {
		t.Fatalf("seed design_snapshots row: %v", err)
	}

	// Apply v43.
	v43 := findMigration(t, 43)
	if err := applyMigration(db, v43); err != nil {
		t.Fatalf("apply v43: %v", err)
	}

	// design_snapshots must be gone.
	var post string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name='design_snapshots'",
	).Scan(&post)
	if err == nil {
		t.Errorf("design_snapshots should be dropped by v43, found %q", post)
	}

	// migration_versions records v43.
	var recorded string
	if err := db.QueryRow(
		"SELECT name FROM migration_versions WHERE version=43",
	).Scan(&recorded); err != nil {
		t.Fatalf("migration_versions row for v43 missing: %v", err)
	}
	if recorded != "drop_design_snapshots" {
		t.Errorf("v43 name = %q, want %q", recorded, "drop_design_snapshots")
	}
}

// TestMigrationV44AddsInputPayloadID asserts the v44 migration adds the
// items.input_payload_id column, the partial index that backs reverse
// lookups, and the GC trigger that sweeps a tool_call_input payload
// when its owning item is deleted (and no other item still references
// it via either payload column).
func TestMigrationV44AddsInputPayloadID(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 44 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	// Sanity: column does not exist before v44.
	if columnExists(t, db, "items", "input_payload_id") {
		t.Fatalf("items.input_payload_id should not exist before v44")
	}

	v44 := findMigration(t, 44)
	if err := applyMigration(db, v44); err != nil {
		t.Fatalf("apply v44: %v", err)
	}

	// Column landed.
	if !columnExists(t, db, "items", "input_payload_id") {
		t.Fatalf("items.input_payload_id should exist after v44")
	}

	// Partial index landed with the expected predicate.
	var indexSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_items_input_payload_id'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("read idx_items_input_payload_id sql: %v", err)
	}
	if !strings.Contains(indexSQL, "input_payload_id IS NOT NULL") {
		t.Errorf("expected partial index on input_payload_id IS NOT NULL, got: %s", indexSQL)
	}

	// GC trigger landed.
	var triggerSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='trg_items_gc_input_payload'`,
	).Scan(&triggerSQL); err != nil {
		t.Fatalf("read trg_items_gc_input_payload sql: %v", err)
	}
	if !strings.Contains(triggerSQL, "OLD.input_payload_id") {
		t.Errorf("trigger must reference OLD.input_payload_id, got: %s", triggerSQL)
	}

	// migration_versions records v44.
	var recorded string
	if err := db.QueryRow(
		"SELECT name FROM migration_versions WHERE version=44",
	).Scan(&recorded); err != nil {
		t.Fatalf("migration_versions row for v44 missing: %v", err)
	}
	if recorded != "items_input_payload" {
		t.Errorf("v44 name = %q, want %q", recorded, "items_input_payload")
	}

	// End-to-end GC behaviour: deleting an item with input_payload_id
	// sweeps the matching payload row when nothing else references it.
	// Seed a project + thread + payload + item, then delete the item.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-input', '/p', '/p', 1000, 1000)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES ('t-input', 'p-input', 'T', 'claude', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO payloads (id, kind, meta, data, created_at)
		VALUES ('p-tool-input', 'tool_call_input', '{}', x'00', 1000)`); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, summary, input_payload_id, created_at, updated_at)
		VALUES ('i-tool', 't-input', 0, 0, 'tool_call', 'assistant', 'Edit', 'p-tool-input', 1000, 1000)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM items WHERE id = 'i-tool'`); err != nil {
		t.Fatalf("delete item: %v", err)
	}

	var orphaned int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM payloads WHERE id = 'p-tool-input'`,
	).Scan(&orphaned); err != nil {
		t.Fatalf("count payload: %v", err)
	}
	if orphaned != 0 {
		t.Errorf("trg_items_gc_input_payload should sweep p-tool-input, got %d row(s)", orphaned)
	}
}

func TestMigrationV46SettlesObsoleteInflightTurns(t *testing.T) {
	db := openSQLiteDB(t)
	if err := configureDatabase(db); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if err := ensureMigrationTable(db); err != nil {
		t.Fatalf("ensure migration table: %v", err)
	}
	for _, m := range migrations {
		if m.Version >= 46 {
			break
		}
		if err := applyMigration(db, m); err != nil {
			t.Fatalf("apply v%d: %v", m.Version, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-stale', '/p-stale', 'p-stale', 1000, 1000)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, model, created_at, updated_at)
		VALUES
		('t-repair', 'p-stale', 'repair', 'codex', '/tmp', '', 1000, 1000),
		('t-latest', 'p-stale', 'latest', 'codex', '/tmp', '', 1000, 1000)`); err != nil {
		t.Fatalf("seed threads: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO turns
		(turn_id, thread_id, turn_index, started_at, completed_at, stop_reason, assistant_message_id, token_usage_json, error_message)
		VALUES
		('turn-stale-with-items', 't-repair', 0, 1000, NULL, '', '', '', ''),
		('turn-stale-no-items', 't-repair', 1, 2000, NULL, 'custom', '', '', 'keep me'),
		('turn-done', 't-repair', 2, 3000, 4000, 'end_turn', '', '', ''),
		('turn-latest-open', 't-latest', 0, 5000, NULL, '', '', '', '')`); err != nil {
		t.Fatalf("seed turns: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO items
		(id, thread_id, turn_index, item_index, kind, role, status, summary, created_at, updated_at)
		VALUES ('i-stale', 't-repair', 0, 0, 'error', 'system', 'completed', 'failed', 1100, 2500)`); err != nil {
		t.Fatalf("seed item: %v", err)
	}

	v46 := findMigration(t, 46)
	if err := applyMigration(db, v46); err != nil {
		t.Fatalf("apply v46: %v", err)
	}

	var completedAt int64
	var stopReason, errorMessage string
	if err := db.QueryRow(`SELECT completed_at, stop_reason, error_message FROM turns WHERE turn_id='turn-stale-with-items'`).
		Scan(&completedAt, &stopReason, &errorMessage); err != nil {
		t.Fatalf("read repaired item-backed turn: %v", err)
	}
	if completedAt != 2000 {
		t.Errorf("turn-stale-with-items completed_at = %d, want capped next started_at 2000", completedAt)
	}
	if stopReason != "interrupted" {
		t.Errorf("turn-stale-with-items stop_reason = %q, want interrupted", stopReason)
	}
	if !strings.Contains(errorMessage, "obsolete in-flight turn") {
		t.Errorf("turn-stale-with-items error_message = %q, want migration marker", errorMessage)
	}

	if err := db.QueryRow(`SELECT completed_at, stop_reason, error_message FROM turns WHERE turn_id='turn-stale-no-items'`).
		Scan(&completedAt, &stopReason, &errorMessage); err != nil {
		t.Fatalf("read repaired next-start-backed turn: %v", err)
	}
	if completedAt != 3000 {
		t.Errorf("turn-stale-no-items completed_at = %d, want next newer started_at 3000", completedAt)
	}
	if stopReason != "custom" {
		t.Errorf("turn-stale-no-items stop_reason = %q, want preserved custom", stopReason)
	}
	if errorMessage != "keep me" {
		t.Errorf("turn-stale-no-items error_message = %q, want preserved message", errorMessage)
	}

	var latestCompleted sql.NullInt64
	if err := db.QueryRow(`SELECT completed_at FROM turns WHERE turn_id='turn-latest-open'`).Scan(&latestCompleted); err != nil {
		t.Fatalf("read latest open turn: %v", err)
	}
	if latestCompleted.Valid {
		t.Errorf("latest NULL turn should remain untouched, got completed_at=%d", latestCompleted.Int64)
	}
}

func findMigration(t *testing.T, version int) Migration {
	t.Helper()
	for _, m := range migrations {
		if m.Version == version {
			return m
		}
	}
	t.Fatalf("migration v%d not found", version)
	return Migration{}
}

// assertRowCount is a focused helper for migration tests — the parent
// tests assert precise row counts on table resets; putting this here
// avoids a fragile format-string assertion in the body of each caller.
func assertRowCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	if !validTableName.MatchString(table) {
		t.Fatalf("invalid table name: %q", table)
	}
	var got int
	if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s rowcount = %d, want %d", table, got, want)
	}
}

func columnExists(t *testing.T, db *sql.DB, table string, column string) bool {
	t.Helper()
	if !validTableName.MatchString(table) {
		t.Fatalf("invalid table name: %q", table)
	}
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatalf("table info %s: %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid       int
			name, typ string
			notnull   int
			dflt      sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table info %s: %v", table, err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table info %s: %v", table, err)
	}
	return false
}

func expectedMigrationVersions() []int {
	versions := make([]int, 0, len(migrations))
	for _, migration := range migrations {
		versions = append(versions, migration.Version)
	}
	return versions
}
