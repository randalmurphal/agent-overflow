// Tool: one-shot db rebake for the v0.0.1 migration squash.
//
// The first agent-overflow binaries shipped a 51-migration chain. v0.0.1
// collapses that chain to a single canonical v1 migration. Existing
// databases would otherwise diverge from the canonical schema in three
// places this tool corrects:
//
//  1. orphan tables left behind by removed-but-not-DROPped migrations
//     (`new_thread_drafts`, `draft_attachments`)
//  2. an items.decision CHECK that still admits the unreachable value
//     'timeout' (no production code ever wrote it)
//  3. a migration_versions table holding 51 rows (one per legacy
//     migration) where the new runner expects exactly one
//     (1, 'initial_schema')
//
// Usage:
//
//	go run ./cmd/db-rebake --db ~/.config/agent-overflow/agent-overflow.db
//	go run ./cmd/db-rebake --db <path> --dry-run
//
// Always pass --db explicitly so the tool can't accidentally clobber a
// neighbour DB. Take a backup first; the tool refuses to start if it
// detects the rebake has already been applied (idempotent re-run is a
// no-op anyway, but the early bail keeps log noise down).
//
// Throwaway. Delete after every friend's DB is rebaked.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := flag.String("db", "", "absolute path to agent-overflow sqlite database (required)")
	dryRun := flag.Bool("dry-run", false, "print planned actions without writing")
	skipVacuum := flag.Bool("skip-vacuum", false, "skip VACUUM at the end (faster, leaves free pages)")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "missing required --db path")
		flag.Usage()
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatalf("open %s: %v", *dbPath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA busy_timeout=10000"); err != nil {
		log.Fatalf("set busy_timeout: %v", err)
	}

	plan, err := assessRebake(db)
	if err != nil {
		log.Fatalf("assess: %v", err)
	}
	plan.print()

	if !plan.needsWork() {
		log.Printf("nothing to do — database already matches canonical v1 schema")
		return
	}
	if *dryRun {
		log.Printf("--dry-run: stopping before writes")
		return
	}

	if err := rebake(db, plan, *skipVacuum); err != nil {
		log.Fatalf("rebake: %v", err)
	}
	log.Printf("rebake complete — database now matches canonical v1 schema")
}

type plan struct {
	hasNewThreadDrafts    bool
	hasDraftAttachments   bool
	itemsHasTimeoutInCHK  bool
	migrationVersionsRows int
	itemsTimeoutRowCount  int
}

func (p plan) needsWork() bool {
	return p.hasNewThreadDrafts ||
		p.hasDraftAttachments ||
		p.itemsHasTimeoutInCHK ||
		p.migrationVersionsRows != 1
}

func (p plan) print() {
	log.Printf("assessment:")
	log.Printf("  new_thread_drafts  table present : %v", p.hasNewThreadDrafts)
	log.Printf("  draft_attachments  table present : %v", p.hasDraftAttachments)
	log.Printf("  items.decision     allows timeout: %v", p.itemsHasTimeoutInCHK)
	log.Printf("  items rows where decision='timeout': %d", p.itemsTimeoutRowCount)
	log.Printf("  migration_versions rows           : %d (want 1)", p.migrationVersionsRows)
}

func assessRebake(db *sql.DB) (plan, error) {
	var p plan
	tables, err := tableSet(db)
	if err != nil {
		return p, err
	}
	p.hasNewThreadDrafts = tables["new_thread_drafts"]
	p.hasDraftAttachments = tables["draft_attachments"]

	var itemsSQL sql.NullString
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='items'`,
	).Scan(&itemsSQL); err != nil {
		return p, fmt.Errorf("read items table sql: %w", err)
	}
	p.itemsHasTimeoutInCHK = itemsSQL.Valid && containsTimeoutInDecisionCHECK(itemsSQL.String)

	if err := db.QueryRow(`SELECT COUNT(*) FROM migration_versions`).Scan(&p.migrationVersionsRows); err != nil {
		return p, fmt.Errorf("count migration_versions: %w", err)
	}

	if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE decision='timeout'`).Scan(&p.itemsTimeoutRowCount); err != nil {
		return p, fmt.Errorf("count items with decision='timeout': %w", err)
	}
	return p, nil
}

func tableSet(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

func containsTimeoutInDecisionCHECK(itemsSQL string) bool {
	return strings.Contains(itemsSQL, "'timeout'")
}

func rebake(db *sql.DB, p plan, skipVacuum bool) error {
	// foreign_keys must be toggled OUTSIDE any transaction — SQLite
	// silently no-ops the pragma when a BEGIN is active. So we toggle
	// off here, run all DDL/DML inside a single BEGIN/COMMIT (so a
	// partial failure rolls back atomically), then toggle on after.
	// We drop and rebuild tables that have CASCADE-referencing children
	// (items has many) — without the off-toggle the rebuild would
	// abort. foreign_key_check is the post-rebuild gate that proves
	// nothing slipped in while enforcement was off.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("disable foreign_keys: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin rebake transaction: %w", err)
	}
	// Rollback is a no-op after a successful Commit, so this defer is
	// safe to leave in place as a belt-and-braces guard against any
	// early return below.
	defer tx.Rollback()

	if p.itemsTimeoutRowCount > 0 {
		log.Printf("step: clear items.decision='timeout' on %d row(s)", p.itemsTimeoutRowCount)
		if _, err := tx.Exec(`UPDATE items SET decision='' WHERE decision='timeout'`); err != nil {
			return fmt.Errorf("clear timeout rows: %w", err)
		}
	}

	if p.hasDraftAttachments {
		log.Printf("step: drop orphan table draft_attachments")
		if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_draft_attachments_slot`); err != nil {
			return fmt.Errorf("drop idx_draft_attachments_slot: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE IF EXISTS draft_attachments`); err != nil {
			return fmt.Errorf("drop draft_attachments: %w", err)
		}
	}
	if p.hasNewThreadDrafts {
		log.Printf("step: drop orphan table new_thread_drafts")
		if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_new_thread_drafts_updated`); err != nil {
			return fmt.Errorf("drop idx_new_thread_drafts_updated: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE IF EXISTS new_thread_drafts`); err != nil {
			return fmt.Errorf("drop new_thread_drafts: %w", err)
		}
	}

	if p.itemsHasTimeoutInCHK {
		log.Printf("step: rebuild items table without 'timeout' in decision CHECK")
		if err := rebuildItemsTable(tx); err != nil {
			return fmt.Errorf("rebuild items: %w", err)
		}
	}

	if p.migrationVersionsRows != 1 {
		log.Printf("step: rewrite migration_versions to single (1, 'initial_schema') row")
		now := time.Now().UnixMilli()
		if _, err := tx.Exec(`DELETE FROM migration_versions`); err != nil {
			return fmt.Errorf("clear migration_versions: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO migration_versions (version, name, applied) VALUES (1, 'initial_schema', ?)`,
			now,
		); err != nil {
			return fmt.Errorf("insert migration_versions row: %w", err)
		}
	}

	// foreign_key_check inside the transaction — a violation triggers
	// the deferred Rollback and the whole rebake unwinds atomically.
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	type fkRow struct {
		Table, Parent string
		RowID         sql.NullInt64
		FKID          int
	}
	var violations []fkRow
	for rows.Next() {
		var fk fkRow
		if err := rows.Scan(&fk.Table, &fk.RowID, &fk.Parent, &fk.FKID); err != nil {
			rows.Close()
			return fmt.Errorf("scan fk_check: %w", err)
		}
		violations = append(violations, fk)
	}
	rows.Close()
	if len(violations) > 0 {
		for _, v := range violations {
			log.Printf("FK VIOLATION: table=%s rowid=%v parent=%s fkid=%d",
				v.Table, v.RowID, v.Parent, v.FKID)
		}
		return fmt.Errorf("foreign_key_check reported %d violations; aborting (rolling back)", len(violations))
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rebake: %w", err)
	}

	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		return fmt.Errorf("re-enable foreign_keys: %w", err)
	}

	if !skipVacuum {
		// VACUUM must run outside any transaction — SQLite rejects it
		// while a BEGIN is active. Run after Commit.
		log.Printf("step: VACUUM (may take a while on large DBs)")
		if _, err := db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
	}
	return nil
}

// rebuildItemsTable drops 'timeout' from the items.decision CHECK by
// CREATE-ing items_rebake, copying every row, dropping items, renaming,
// and recreating every items index/trigger. Runs inside the caller's
// transaction so a failure mid-rebuild rolls back atomically with the
// rest of the rebake. The canonical CREATE text here is the same shape
// as initialSchemaSQL — it is the rebuild copy of that template for the
// one table whose CHECK we tighten.
func rebuildItemsTable(tx *sql.Tx) error {
	const createItems = `
CREATE TABLE items_rebake (
    id                  TEXT    NOT NULL,
    thread_id           TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index          INTEGER NOT NULL,
    item_index          INTEGER NOT NULL,
    kind                TEXT    NOT NULL CHECK(kind IN (
        'user_text',
        'assistant_text',
        'thinking',
        'tool_call',
        'tool_completion',
        'error',
        'compaction',
        'terminal_interaction',
        'notification',
        'api_retry',
        'api_error'
    )),
    role                TEXT    NOT NULL CHECK(role IN ('assistant', 'user', 'system')),
    status              TEXT    NOT NULL DEFAULT 'completed' CHECK(status IN (
        'streaming',
        'running',
        'completed',
        'errored',
        'declined',
        'killed'
    )),
    summary             TEXT    NOT NULL DEFAULT '',
    payload_id          TEXT    REFERENCES payloads(id),
    parent_id           TEXT    NOT NULL DEFAULT '',
    is_background       INTEGER NOT NULL DEFAULT 0 CHECK(is_background IN (0, 1)),
    completion_of       TEXT    NOT NULL DEFAULT '',
    tool_name           TEXT    NOT NULL DEFAULT '',
    decision            TEXT    NOT NULL DEFAULT '' CHECK(decision IN (
        '',
        'approved',
        'declined',
        'amended',
        'lost'
    )),
    meta                TEXT    NOT NULL DEFAULT '{}',
    created_at          INTEGER NOT NULL,
    updated_at          INTEGER NOT NULL,
    input_payload_id    TEXT    REFERENCES payloads(id),
    PRIMARY KEY (thread_id, id)
)`

	if _, err := tx.Exec(createItems); err != nil {
		return fmt.Errorf("create items_rebake: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO items_rebake
		SELECT id, thread_id, turn_index, item_index, kind, role, status,
		       summary, payload_id, parent_id, is_background, completion_of,
		       tool_name, decision, meta, created_at, updated_at, input_payload_id
		FROM items`); err != nil {
		return fmt.Errorf("copy items rows: %w", err)
	}

	// Drop the old table — this also drops every dependent index and
	// trigger. We rebuild them below.
	if _, err := tx.Exec(`DROP TABLE items`); err != nil {
		return fmt.Errorf("drop old items: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE items_rebake RENAME TO items`); err != nil {
		return fmt.Errorf("rename items_rebake: %w", err)
	}

	// Recreate indexes in canonical order (matches initialSchemaSQL).
	indexStatements := []string{
		`CREATE INDEX idx_items_thread
		    ON items(thread_id, turn_index, item_index)`,
		`CREATE INDEX idx_items_id
		    ON items(id)`,
		`CREATE UNIQUE INDEX idx_items_thread_turn_item_unique
		    ON items(thread_id, turn_index, item_index)`,
		`CREATE INDEX idx_items_parent
		    ON items(thread_id, parent_id) WHERE parent_id <> ''`,
		`CREATE INDEX idx_items_completion_of
		    ON items(thread_id, completion_of) WHERE completion_of <> ''`,
		`CREATE INDEX idx_items_payload_id
		    ON items(payload_id) WHERE payload_id IS NOT NULL`,
		`CREATE INDEX idx_items_meta_task_id
		    ON items(thread_id, json_extract(meta, '$.task_id'))
		 WHERE json_extract(meta, '$.task_id') IS NOT NULL`,
		`CREATE INDEX idx_items_input_payload_id
		    ON items(input_payload_id) WHERE input_payload_id IS NOT NULL`,
		`CREATE INDEX idx_items_live_background
		    ON items(thread_id, id)
		 WHERE is_background = 1
		   AND status = 'running'
		   AND parent_id = ''
		   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0`,
		`CREATE INDEX idx_items_live_codex_subagent
		  ON items(thread_id, turn_index, item_index)
		  WHERE kind = 'tool_call'
		    AND status = 'completed'
		    AND tool_name = 'collab_agent'
		    AND is_background = 1
		    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
		    AND json_extract(meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent')`,
	}
	for _, sqlStmt := range indexStatements {
		if _, err := tx.Exec(sqlStmt); err != nil {
			return fmt.Errorf("create index: %w (sql=%s)", err, sqlStmt)
		}
	}

	// Recreate triggers.
	triggerStatements := []string{
		`CREATE TRIGGER trg_items_gc_input_payload
		AFTER DELETE ON items
		WHEN OLD.input_payload_id IS NOT NULL
		BEGIN
		    DELETE FROM payloads
		     WHERE id = OLD.input_payload_id
		       AND NOT EXISTS (
		           SELECT 1 FROM items WHERE payload_id = OLD.input_payload_id
		       )
		       AND NOT EXISTS (
		           SELECT 1 FROM items WHERE input_payload_id = OLD.input_payload_id
		       );
		END`,
		`CREATE TRIGGER trg_items_gc_payload
		AFTER DELETE ON items
		WHEN OLD.payload_id IS NOT NULL
		BEGIN
		    DELETE FROM payloads
		     WHERE id = OLD.payload_id
		       AND NOT EXISTS (
		           SELECT 1 FROM items WHERE payload_id = OLD.payload_id
		       );
		END`,
	}
	for _, sqlStmt := range triggerStatements {
		if _, err := tx.Exec(sqlStmt); err != nil {
			return fmt.Errorf("create trigger: %w", err)
		}
	}
	return nil
}
