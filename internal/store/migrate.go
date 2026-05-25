package store

import (
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
}

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
		if err := applyMigration(db, m); err != nil {
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
