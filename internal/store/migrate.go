package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
)

const createMigrationVersionsTableSQL = `CREATE TABLE IF NOT EXISTS migration_versions (
	version  INTEGER PRIMARY KEY,
	name     TEXT    NOT NULL,
	applied  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
)`

// Migration represents a versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Rebuild marks a migration whose SQL performs a full table rebuild
	// (CREATE new / copy / DROP old / RENAME) to change a CHECK or drop a
	// NOT NULL that SQLite can't alter in place. Such a migration MUST run
	// with foreign_keys disabled so DROP TABLE doesn't fire ON DELETE
	// CASCADE against child tables — and foreign_keys can only be toggled
	// outside a transaction, so these run through applyRebuildMigration.
	Rebuild bool
}

// rebuildThreadsV5SQL rebuilds the threads table to (a) extend the mode
// CHECK with 'terminal' (so a terminal-mode thread can be persisted) and
// (b) drop NOT NULL from project_id (so a standalone "home" terminal can
// exist with no project). SQLite cannot alter a CHECK or drop a NOT NULL in
// place, so the whole table is rebuilt. Every column is preserved verbatim
// except those two changes; the explicit INSERT/SELECT column lists guard
// against schema drift. Runs with foreign_keys OFF (see
// applyRebuildMigration) so DROP TABLE threads does not cascade-delete the
// many child tables that REFERENCE threads(id) ON DELETE CASCADE. The five
// threads indexes are dropped with the old table and recreated here; the
// only triggers in the schema are on items, so none need recreating.
const rebuildThreadsV5SQL = `
CREATE TABLE threads_new (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(provider IN ('claude','codex')),
    model                    TEXT    NOT NULL DEFAULT '',
    workspace_path           TEXT    NOT NULL,
    worktree_path            TEXT,
    branch                   TEXT,
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','design','discussion','terminal')),
    reasoning_effort         TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
        ),
    fast_mode                INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window           INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode             TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access')),
    discussion_id            TEXT    REFERENCES channels(id) ON DELETE SET NULL,
    parent_thread_id         TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    forked_from_thread_id    TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    last_token_usage         TEXT    NOT NULL DEFAULT ''
        CHECK(last_token_usage = '' OR json_valid(last_token_usage)),
    last_read_at             INTEGER,
    pinned_at                INTEGER,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    archived                 INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)),
    disabled_mcp_servers     TEXT NULL CHECK(disabled_mcp_servers IS NULL OR json_valid(disabled_mcp_servers))
);

INSERT INTO threads_new (
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, disabled_mcp_servers
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, disabled_mcp_servers
FROM threads;

DROP TABLE threads;

ALTER TABLE threads_new RENAME TO threads;

CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);
CREATE INDEX idx_threads_parent      ON threads(parent_thread_id);
CREATE INDEX idx_threads_pinned_at   ON threads(pinned_at) WHERE pinned_at IS NOT NULL;
CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);
`

// migrations is the ordered list of all schema migrations. Squashed
// for v0.0.1: the prior 51-migration chain produced this schema; old
// databases were rebaked into a single (1, 'initial_schema') row by
// cmd/db-rebake. New columns / indexes / CHECKs from this point on
// append a new Migration entry — never edit v1.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		SQL:     initialSchemaSQL,
	},
	{
		Version: 2,
		Name:    "channel_messages_meta",
		SQL:     `ALTER TABLE channel_messages ADD COLUMN meta TEXT NULL;`,
	},
	{
		Version: 3,
		Name:    "thread_drafts_has_content",
		SQL: `ALTER TABLE thread_drafts ADD COLUMN has_content INTEGER NOT NULL DEFAULT 0 CHECK(has_content IN (0,1));

UPDATE thread_drafts SET has_content = 1
WHERE TRIM(content) <> ''
   OR COALESCE(attachments, '[]') NOT IN ('', '[]', 'null')
   OR COALESCE(terminal_chips, '[]') NOT IN ('', '[]', 'null')
   OR pending_plan_implementation IS NOT NULL;

CREATE INDEX idx_thread_drafts_has_content
  ON thread_drafts(thread_id) WHERE has_content = 1;`,
	},
	{
		Version: 4,
		Name:    "thread_disabled_mcp_servers",
		SQL:     `ALTER TABLE threads ADD COLUMN disabled_mcp_servers TEXT NULL CHECK(disabled_mcp_servers IS NULL OR json_valid(disabled_mcp_servers));`,
	},
	{
		Version: 5,
		Name:    "thread_terminal_mode",
		SQL:     rebuildThreadsV5SQL,
		Rebuild: true,
	},
	{
		Version: 6,
		Name:    "new_thread_mcp_defaults",
		SQL: `CREATE TABLE new_thread_mcp_defaults (
    provider         TEXT NOT NULL CHECK(provider IN ('claude','codex')),
    workspace_path   TEXT NOT NULL DEFAULT '',
    disabled_servers TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(disabled_servers)),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (provider, workspace_path)
);`,
	},
}

// runMigrations sets PRAGMAs, creates the version tracking table, and applies
// any unapplied migrations in order.
func runMigrations(db *sql.DB) error {
	if err := configureDatabase(db); err != nil {
		return err
	}
	if err := ensureMigrationTable(db); err != nil {
		return err
	}

	applied, err := currentMigrationVersion(db)
	if err != nil {
		return err
	}

	return applyPendingMigrations(db, applied)
}

func configureDatabase(db *sql.DB) error {
	// PRAGMA journal_mode=WAL returns the resulting mode even on
	// success; SQLite silently falls back to the previous journal mode
	// when WAL can't be enabled (NFS filesystems, read-only mounts,
	// shared-cache databases). We don't treat this as fatal — rollback
	// journaling keeps the app correct — but we log a warning so the
	// user can see why checkpointing is not happening.
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		log.Printf("store: journal_mode=WAL returned %q; SQLite fell back to rollback journaling (often caused by NFS or read-only mount)", journalMode)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}
	// busy_timeout=5000 lets SQLite poll the lock for up to 5s before
	// surfacing a SQLITE_BUSY to the caller. WAL allows concurrent
	// readers + one writer, but UI threads, the checkpoint capture, the
	// replay writer, and the triage flusher all write — without this
	// timeout the rare contention window surfaces as "database is
	// locked" toasts. Five seconds is the canonical SQLite recommendation
	// for a UI-attached database; a turn rarely needs longer than that
	// to land its writes, and longer windows would just mask a real
	// deadlock.
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	// synchronous=NORMAL is the WAL-recommended desktop config. With WAL
	// the journal file is always fsync'd before commit; synchronous=NORMAL
	// drops the redundant fsync of the main database file at checkpoint
	// time. Power-loss can lose the last few committed transactions, but
	// the database cannot corrupt — and per root/CLAUDE.md principle 2
	// the provider session files are the authoritative history, so a
	// re-stream covers any lost SQLite-side writes. synchronous=FULL (the
	// SQLite default) is needed only when the database is the sole record
	// of truth; that's not us. NORMAL meaningfully shortens fsync stalls
	// during stream-bursts, which is the per-block-stop freeze hot path.
	if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
		return fmt.Errorf("set synchronous=NORMAL: %w", err)
	}
	return nil
}

func ensureMigrationTable(db *sql.DB) error {
	if _, err := db.Exec(createMigrationVersionsTableSQL); err != nil {
		return fmt.Errorf("create migration_versions table: %w", err)
	}
	return nil
}

func currentMigrationVersion(db *sql.DB) (int, error) {
	var maxVersion sql.NullInt64
	if err := db.QueryRow("SELECT MAX(version) FROM migration_versions").Scan(&maxVersion); err != nil {
		return 0, fmt.Errorf("query max migration version: %w", err)
	}
	if !maxVersion.Valid {
		return 0, nil
	}
	return int(maxVersion.Int64), nil
}

var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// tableColumns returns the set of column names defined on the given
// table. Kept exported-within-package because items_lifecycle_test.go
// and items_parent_test.go use it as a schema-existence probe.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	if !validTableName.MatchString(table) {
		return nil, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return nil, fmt.Errorf("scan table_info(%s): %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate table_info(%s): %w", table, err)
	}
	return columns, nil
}

func applyPendingMigrations(db *sql.DB, applied int) error {
	for _, m := range migrations {
		if m.Version <= applied {
			continue
		}
		apply := applyMigration
		if m.Rebuild {
			apply = applyRebuildMigration
		}
		if err := apply(db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m Migration) error {
	log.Printf("store: applying migration v%d: %s", m.Version, m.Name)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration v%d: %w", m.Version, err)
	}

	if _, err := tx.Exec(m.SQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(
		"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
		m.Version,
		m.Name,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration v%d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v%d: %w", m.Version, err)
	}

	log.Printf("store: migration v%d applied", m.Version)
	return nil
}

// applyRebuildMigration runs a table-rebuild migration with foreign keys
// disabled. PRAGMA foreign_keys is a no-op inside a transaction, so the
// toggle must happen on a connection with no transaction open — we pin a
// single *sql.Conn for the whole operation rather than leaning on
// SetMaxOpenConns(1), keeping the FK-off window scoped to exactly this
// connection. Sequence: disable FK, run the rebuild + version bump in one
// transaction, verify integrity with foreign_key_check, commit, re-enable FK.
func applyRebuildMigration(db *sql.DB, m Migration) error {
	log.Printf("store: applying rebuild migration v%d: %s", m.Version, m.Name)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pin connection for rebuild v%d: %w", m.Version, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign keys for rebuild v%d: %w", m.Version, err)
	}
	// Always restore enforcement before the connection returns to the pool,
	// even on failure — a leaked foreign_keys=OFF would silently disable
	// cascade integrity for the rest of the process. Deferred after
	// conn.Close so it runs first (LIFO).
	defer func() {
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
			log.Printf("store: WARNING failed to re-enable foreign_keys after rebuild v%d: %v", m.Version, err)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rebuild v%d: %w", m.Version, err)
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rebuild v%d (%s) failed: %w", m.Version, m.Name, err)
	}
	if err := assertForeignKeysIntact(ctx, tx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rebuild v%d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
		m.Version, m.Name,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record rebuild v%d: %w", m.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rebuild v%d: %w", m.Version, err)
	}

	log.Printf("store: rebuild migration v%d applied", m.Version)
	return nil
}

// assertForeignKeysIntact runs PRAGMA foreign_key_check and returns an error
// if any row references a missing parent. After a rebuild that copies ids
// verbatim this is always clean; the check is cheap insurance that turns a
// botched rebuild (which would otherwise silently strand child rows) into a
// loud migration failure + rollback. foreign_key_check works regardless of
// whether enforcement is currently enabled.
func assertForeignKeysIntact(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign_key_check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		// Columns: child table, child rowid, referenced table, fk index.
		var table, referred sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &referred, &fkid); err != nil {
			return fmt.Errorf("foreign_key_check scan: %w", err)
		}
		return fmt.Errorf("foreign key violation after rebuild: table %q row %d references missing %q",
			table.String, rowid.Int64, referred.String)
	}
	return rows.Err()
}
