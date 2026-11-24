package store

import (
	"database/sql"
	"slices"
	"testing"
)

func TestMigrationV159WidensProviderChecksAndPreservesSchemaObjects(t *testing.T) {
	t.Parallel()
	db := migrateThrough(t, widenProviderChecksMigrationVersion-1)

	// Seed rows across all five rebuilt tables before migrating.
	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v159', '/tmp/p-v159', 'P159', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, model, workspace_path, created_at, updated_at, delete_claim, spawn_instructions, spawn_level)
		VALUES ('t-v159', 'p-v159', 'Existing', 'claude', 'claude-sonnet-4-6', '/tmp/p-v159', 10, 20, 'claim-1', 'follow rules', 2)`)
	mustExec(t, db, `INSERT INTO chat_model_profiles (provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at)
		VALUES ('codex', 'gpt-5', 'high', 0, 272000, 'auto', 30)`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		VALUES ('model', 'claude', 'claude-sonnet-4-6', 'Sonnet', 40)`)
	mustExec(t, db, `INSERT INTO thread_import_state (thread_id, provider, source_path, source_session_id, imported_at, source_parent_session_id, source_meta_hash, source_history_mode)
		VALUES ('t-v159', 'claude', '/tmp/src.jsonl', 'sess-1', 60, 'parent-1', 'hash-1', 'full')`)
	mustExec(t, db, `INSERT INTO thread_transfers (id, thread_id, target_thread_id, peer_backend_id, project_id, kind, direction, phase, activation_hash, private_state, created_at, updated_at)
		VALUES ('xf-v159', 't-v159', 't-v159', 'peer-1', 'p-v159', 'move', 'outgoing', 'preparing', 'hash', X'7B7D', 70, 70)`)
	mustExec(t, db, `INSERT INTO thread_transfer_sessions (transfer_id, provider, session_ref)
		VALUES ('xf-v159', 'claude', 'sess-1')`)

	beforeObjects := snapshotSchemaObjectNames(t, db)
	beforeCols := make(map[string][]string, 5)
	rebuiltTables := []string{
		"threads", "chat_model_profiles", "chat_bar_favorites",
		"thread_import_state", "thread_transfer_sessions",
	}
	for _, table := range rebuiltTables {
		beforeCols[table] = tableColumnNames(t, db, table)
	}

	if err := applyRebuildMigration(db, migrationByVersion(t, widenProviderChecksMigrationVersion)); err != nil {
		t.Fatalf("apply v159: %v", err)
	}

	// Zero tables, indexes, views, or triggers may be lost or renamed.
	afterObjects := snapshotSchemaObjectNames(t, db)
	if !slices.Equal(beforeObjects, afterObjects) {
		t.Fatalf("schema object inventory changed across v159:\nbefore=%v\nafter =%v", beforeObjects, afterObjects)
	}
	for _, table := range rebuiltTables {
		afterCols := tableColumnNames(t, db, table)
		if !slices.Equal(beforeCols[table], afterCols) {
			t.Fatalf("%s columns changed across v159:\nbefore=%v\nafter =%v", table, beforeCols[table], afterCols)
		}
	}

	// Existing data survived verbatim, including v155 columns on threads and v63/v80 columns on thread_import_state.
	var deleteClaim, spawnInstr string
	var spawnLevel int
	if err := db.QueryRow(`SELECT delete_claim, spawn_instructions, spawn_level FROM threads WHERE id = 't-v159'`).Scan(&deleteClaim, &spawnInstr, &spawnLevel); err != nil {
		t.Fatalf("read preserved thread: %v", err)
	}
	if deleteClaim != "claim-1" || spawnInstr != "follow rules" || spawnLevel != 2 {
		t.Fatalf("preserved v155 thread columns = (%q, %q, %d)", deleteClaim, spawnInstr, spawnLevel)
	}
	var metaHash, histMode string
	if err := db.QueryRow(`SELECT source_meta_hash, source_history_mode FROM thread_import_state WHERE thread_id = 't-v159'`).Scan(&metaHash, &histMode); err != nil {
		t.Fatalf("read preserved import state: %v", err)
	}
	if metaHash != "hash-1" || histMode != "full" {
		t.Fatalf("preserved import state = (%q, %q)", metaHash, histMode)
	}

	var xfProvider, xfRef string
	if err := db.QueryRow(`SELECT provider, session_ref FROM thread_transfer_sessions WHERE transfer_id = 'xf-v159'`).Scan(&xfProvider, &xfRef); err != nil {
		t.Fatalf("read preserved transfer session: %v", err)
	}
	if xfProvider != "claude" || xfRef != "sess-1" {
		t.Fatalf("preserved transfer session = (%q, %q)", xfProvider, xfRef)
	}

	// New provider names now pass SQLite CHECKs on all five tables, while blank provider names fail.
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, model, workspace_path, reasoning_effort, import_source, created_at, updated_at)
		VALUES ('t-new-prov', 'p-v159', 'NewProv', 'agy', 'gemini-3', '/tmp/p-v159', 'high', 'agy', 100, 100)`)
	mustExec(t, db, `INSERT INTO chat_model_profiles (provider, model, reasoning_effort, fast_mode, context_window, runtime_mode, updated_at)
		VALUES ('agy', 'gemini-3', 'xhigh', 0, 1000000, 'auto', 100)`)
	mustExec(t, db, `INSERT INTO chat_bar_favorites (kind, provider, value, label, created_at)
		VALUES ('model', 'agy', 'gemini-3', 'Gemini 3', 100)`)
	mustExec(t, db, `INSERT INTO thread_import_state (thread_id, provider, source_path, source_session_id, imported_at)
		VALUES ('t-new-prov', 'agy', '/tmp/conv.db', 'conv-1', 100)`)
	mustExec(t, db, `INSERT INTO thread_transfer_sessions (transfer_id, provider, session_ref)
		VALUES ('xf-v159', 'agy', 'conv-1')`)

	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, created_at, updated_at)
		VALUES ('t-blank', 'p-v159', 'Blank', '   ', '/tmp/p-v159', 1, 1)`); err == nil {
		t.Fatal("threads CHECK accepted a blank provider")
	}
	if _, err := db.Exec(`INSERT INTO threads (id, project_id, title, provider, workspace_path, reasoning_effort, created_at, updated_at)
		VALUES ('t-bad-eff', 'p-v159', 'BadEff', 'agy', '/tmp/p-v159', 'bogus', 1, 1)`); err == nil {
		t.Fatal("threads CHECK accepted an invalid reasoning_effort")
	}
}

func TestClaudeThreadRefusesCodexOnlyReasoningEffort(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	if _, err := st.CreateProject(Project{ID: "p-eff", Path: "/tmp/p-eff", Name: "PEff", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateThread(Thread{
		ID:              "t-ultra",
		ProjectID:       "p-eff",
		Title:           "Claude High",
		Provider:        "claude",
		Model:           "claude-opus-4-7",
		ReasoningEffort: "high",
		WorkspacePath:   "/tmp/p-eff",
		CreatedAt:       1,
		UpdatedAt:       1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetThread("t-ultra")
	if err != nil {
		t.Fatal(err)
	}
	// UpdateReasoningEffort("ultra") on the Claude thread must be rejected.
	if _, _, err := st.UpdateReasoningEffort("t-ultra", "ultra"); err == nil {
		t.Fatal("UpdateReasoningEffort(ultra) succeeded on claude thread")
	}
	// CompareAndSwapModelProfile must refuse a stale cross-provider CAS so a
	// Codex-validated effort cannot be written onto a Claude thread.
	staleBefore := got
	staleBefore.Provider = "codex"
	staleAfter := staleBefore
	staleAfter.ReasoningEffort = "ultra"
	if swapped, err := st.CompareAndSwapModelProfile(staleBefore, staleAfter); err == nil && swapped {
		t.Fatal("CompareAndSwapModelProfile unexpectedly wrote codex/ultra onto claude thread")
	}
}

func snapshotSchemaObjectNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT type || ':' || name FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows sqlite_master: %v", err)
	}
	return out
}

func tableColumnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var cols []string
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows table_info(%s): %v", table, err)
	}
	return cols
}
