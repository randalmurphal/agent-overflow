package store

import (
	"database/sql"
	"strings"
	"testing"
)

// normalizeSQLText collapses whitespace so a DDL string can be compared
// against the text SQLite stored for it. SQLite keeps a trigger's source
// verbatim apart from the statement terminator, so the only difference a
// comparison has to forgive is how the Go source happened to indent it.
func normalizeSQLText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// historyRevTriggerStatements splits historyRevTriggersSQL into its three
// CREATE TRIGGER statements, terminator removed, in install order.
func historyRevTriggerStatements(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, part := range strings.Split(historyRevTriggersSQL, "END;") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		out = append(out, part+"END")
	}
	if len(out) != 3 {
		t.Fatalf("historyRevTriggersSQL split into %d statements, want 3", len(out))
	}
	return out
}

// TestMigrationV100StampsExistingItemRows drives the migration over a
// database that already has history. Every claim the migration makes is
// checked against the upgraded database rather than against a freshly
// created one: a new store gets the column from the baseline schema and
// would pass whatever v100 did.
func TestMigrationV100StampsExistingItemRows(t *testing.T) {
	db := migrateThrough(t, 99)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v100', '/v100', 'v100', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v100', 'p-v100', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('i-old', 't-v100', 0, 0, 'assistant_text', 'assistant', 'completed',
		'before the column existed', '', 0, '', '', '', '{}', 1, 1)`)

	migrateFrom(t, db, 99)

	// An existing row is NOT backfilled: 0 is "never stamped since the
	// column existed", which no thread stamp can equal.
	var rev int64
	if err := db.QueryRow(
		`SELECT rev FROM items WHERE thread_id = 't-v100' AND id = 'i-old'`,
	).Scan(&rev); err != nil {
		t.Fatalf("read migrated row rev: %v", err)
	}
	if rev != 0 {
		t.Fatalf("pre-existing row rev = %d, want 0", rev)
	}

	// The view is the logical row set, so `rev` has to be part of it.
	var viewRev int64
	if err := db.QueryRow(
		`SELECT rev FROM timeline_items WHERE thread_id = 't-v100' AND id = 'i-old'`,
	).Scan(&viewRev); err != nil {
		t.Fatalf("read migrated row rev through the view: %v", err)
	}
	if viewRev != 0 {
		t.Fatalf("view rev = %d, want 0", viewRev)
	}

	// The triggers the upgraded database runs must be the same text
	// RestoreFrom reinstalls, or a restored database and a migrated one
	// would maintain different contracts.
	want := historyRevTriggerStatements(t)
	names := []string{"trg_items_rev_insert", "trg_items_rev_update", "trg_items_rev_delete"}
	for i, name := range names {
		var installed string
		if err := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name,
		).Scan(&installed); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if normalizeSQLText(installed) != normalizeSQLText(want[i]) {
			t.Errorf("%s is not the text historyRevTriggersSQL installs:\n got %s\nwant %s",
				name, normalizeSQLText(installed), normalizeSQLText(want[i]))
		}
	}
	insertBody := normalizeSQLText(mustTriggerSQL(t, db, "trg_items_rev_insert"))
	if !strings.Contains(insertBody, normalizeSQLText(stampedRowIDsSQL("NEW"))) {
		t.Error("the insert trigger does not stamp the rows stampedRowIDsSQL names")
	}
	deleteBody := normalizeSQLText(mustTriggerSQL(t, db, "trg_items_rev_delete"))
	if !strings.Contains(deleteBody, normalizeSQLText(stampedRowIDsSQL("OLD"))) {
		t.Error("the delete trigger does not stamp the rows stampedRowIDsSQL names")
	}

	// The carrier leg of the stamp probes a partial expression index. The
	// predicate has to survive into the index or every stamping UPDATE
	// scans the thread.
	indexSQL := readIndexSQL(t, db, "idx_items_transcript_root")
	if !strings.Contains(indexSQL, transcriptRootExpr) {
		t.Errorf("idx_items_transcript_root does not key %s: %s", transcriptRootExpr, indexSQL)
	}
	if !strings.Contains(normalizeSQLText(indexSQL),
		normalizeSQLText("WHERE "+transcriptRootExpr+" IS NOT NULL")) {
		t.Errorf("idx_items_transcript_root lost its partial predicate: %s", indexSQL)
	}
	assertPlanUses(t, db, "idx_items_transcript_root",
		`EXPLAIN QUERY PLAN SELECT id FROM items
		  WHERE thread_id = ? AND `+transcriptRootExpr+` = ?`,
		"t-v100", "i-old")

	// The point of the migration: a write to a row that predates the
	// column stamps it with the thread's new history_rev.
	mustExec(t, db, `UPDATE items SET summary = 'written after the upgrade'
		 WHERE thread_id = 't-v100' AND id = 'i-old'`)
	var threadRev int64
	if err := db.QueryRow(
		`SELECT history_rev FROM threads WHERE id = 't-v100'`,
	).Scan(&threadRev); err != nil {
		t.Fatalf("read thread history_rev: %v", err)
	}
	if err := db.QueryRow(
		`SELECT rev FROM items WHERE thread_id = 't-v100' AND id = 'i-old'`,
	).Scan(&rev); err != nil {
		t.Fatalf("read stamped rev: %v", err)
	}
	if rev != threadRev || rev <= 0 {
		t.Fatalf("rev after the upgrade's first write = %d, want the thread's history_rev %d and > 0",
			rev, threadRev)
	}
}

func mustTriggerSQL(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var text string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, name,
	).Scan(&text); err != nil {
		t.Fatalf("read trigger %s: %v", name, err)
	}
	return text
}

// TestMigrationV100ViewProjectsImportedRowsUnstamped is the other arm of
// the recreated view. Imported history lives in shared immutable chunks
// with nowhere thread-scoped to stamp, so the view must answer -1 for it
// (importedItemRevExpr) rather than a number a digest could match.
func TestMigrationV100ViewProjectsImportedRowsUnstamped(t *testing.T) {
	db := migrateThrough(t, 99)

	mustExec(t, db, `INSERT INTO projects (id, path, name, created_at, updated_at)
		VALUES ('p-v100i', '/v100i', 'v100i', 1, 1)`)
	mustExec(t, db, `INSERT INTO threads (id, project_id, title, provider, workspace_path, model,
		created_at, updated_at, archived, mode)
		VALUES ('t-v100i', 'p-v100i', 'T', 'claude', '/tmp', '', 1, 1, 0, 'chat')`)
	mustExec(t, db, `INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status,
		summary, parent_id, is_background, completion_of, tool_name, decision, meta, created_at, updated_at)
		VALUES ('i-local', 't-v100i', 0, 0, 'assistant_text', 'assistant', 'completed',
		'local row', '', 0, '', '', '', '{}', 1, 1)`)
	// The imported chunk sits ABOVE the local row's turn so the v61
	// overlap triggers have nothing to object to.
	mustExec(t, db, `INSERT INTO import_history_chunks (id, item_count, min_turn_index, max_turn_index)
		VALUES ('chunk-v100', 1, 1, 1)`)
	mustExec(t, db, `INSERT INTO import_history_items (chunk_id, id, turn_index, item_index, kind, role,
		status, summary, parent_id, is_background, completion_of, tool_name, decision, meta,
		created_at, updated_at)
		VALUES ('chunk-v100', 'i-imported', 1, 0, 'assistant_text', 'assistant', 'completed',
		'imported row', '', 0, '', '', '', '{}', 1, 1)`)
	mustExec(t, db, `INSERT INTO thread_import_chunks (thread_id, chunk_order, chunk_id)
		VALUES ('t-v100i', 0, 'chunk-v100')`)

	migrateFrom(t, db, 99)

	revs := map[string]int64{}
	rows, err := db.Query(
		`SELECT id, rev FROM timeline_items WHERE thread_id = 't-v100i'`)
	if err != nil {
		t.Fatalf("read the view: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			t.Fatalf("scan view row: %v", err)
		}
		revs[id] = rev
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate view rows: %v", err)
	}
	if len(revs) != 2 {
		t.Fatalf("view returned %d rows, want the local and the imported one: %v", len(revs), revs)
	}
	if revs["i-local"] != 0 {
		t.Errorf("local row rev = %d, want the unbackfilled 0", revs["i-local"])
	}
	if revs["i-imported"] != UnstampedItemRev {
		t.Errorf("imported row rev = %d, want %d", revs["i-imported"], UnstampedItemRev)
	}
}
