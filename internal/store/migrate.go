package store

import (
	"database/sql"
	"fmt"
	"log"
)

// Migration represents a versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// migrations is the ordered list of all schema migrations.
var migrations = []Migration{
	{
		Version: 1,
		Name:    "initial_schema",
		SQL: `
CREATE TABLE IF NOT EXISTS threads (
	id             TEXT PRIMARY KEY,
	title          TEXT NOT NULL DEFAULT 'New Thread',
	provider       TEXT NOT NULL CHECK(provider IN ('claude', 'codex')),
	session_ref    TEXT,
	workspace_path TEXT NOT NULL,
	model          TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL,
	updated_at     INTEGER NOT NULL,
	archived       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC);

CREATE TABLE IF NOT EXISTS payloads (
	id         TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	meta       TEXT NOT NULL DEFAULT '{}',
	data       BLOB NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS items (
	id          TEXT PRIMARY KEY,
	thread_id   TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	turn_index  INTEGER NOT NULL,
	item_index  INTEGER NOT NULL,
	kind        TEXT NOT NULL,
	role        TEXT NOT NULL DEFAULT 'assistant',
	summary     TEXT NOT NULL DEFAULT '',
	payload_id  TEXT REFERENCES payloads(id),
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_thread ON items(thread_id, turn_index, item_index);
`,
	},
	{
		Version: 2,
		Name:    "parity_tables",
		SQL: `
ALTER TABLE threads ADD COLUMN interaction_mode TEXT NOT NULL DEFAULT 'default';
ALTER TABLE threads ADD COLUMN branch TEXT;
ALTER TABLE threads ADD COLUMN worktree_path TEXT;
ALTER TABLE threads ADD COLUMN project_path TEXT NOT NULL DEFAULT '';
ALTER TABLE threads ADD COLUMN discussion_id TEXT;
ALTER TABLE threads ADD COLUMN parent_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL;

CREATE TABLE IF NOT EXISTS channels (
	id          TEXT    PRIMARY KEY,
	thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	type        TEXT    NOT NULL DEFAULT 'deliberation',
	status      TEXT    NOT NULL DEFAULT 'open',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_channels_thread ON channels(thread_id);

CREATE TABLE IF NOT EXISTS channel_messages (
	id          TEXT    PRIMARY KEY,
	channel_id  TEXT    NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
	sequence    INTEGER NOT NULL,
	from_type   TEXT    NOT NULL,
	from_id     TEXT    NOT NULL,
	from_role   TEXT,
	content     TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	UNIQUE(channel_id, sequence)
);

CREATE TABLE IF NOT EXISTS discussion_definitions (
	id          TEXT    PRIMARY KEY,
	name        TEXT    NOT NULL,
	description TEXT    NOT NULL DEFAULT '',
	scope       TEXT    NOT NULL DEFAULT 'global',
	project_id  TEXT,
	definition  TEXT    NOT NULL,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL,
	UNIQUE(name, scope, project_id)
);

CREATE TABLE IF NOT EXISTS design_artifacts (
	id          TEXT    PRIMARY KEY,
	thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	title       TEXT    NOT NULL,
	description TEXT    NOT NULL DEFAULT '',
	kind        TEXT    NOT NULL DEFAULT 'render',
	html_path   TEXT    NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_design_artifacts_thread ON design_artifacts(thread_id);
`,
	},
}

// runMigrations sets PRAGMAs, creates the version tracking table, and applies
// any unapplied migrations in order.
func runMigrations(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	// Create the tracking table (outside any migration transaction).
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS migration_versions (
		version  INTEGER PRIMARY KEY,
		name     TEXT    NOT NULL,
		applied  INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
	)`)
	if err != nil {
		return fmt.Errorf("create migration_versions table: %w", err)
	}

	// Find the highest applied version.
	var maxVersion sql.NullInt64
	err = db.QueryRow("SELECT MAX(version) FROM migration_versions").Scan(&maxVersion)
	if err != nil {
		return fmt.Errorf("query max migration version: %w", err)
	}
	applied := 0
	if maxVersion.Valid {
		applied = int(maxVersion.Int64)
	}

	// Apply each unapplied migration inside its own transaction.
	for _, m := range migrations {
		if m.Version <= applied {
			continue
		}
		log.Printf("store: applying migration v%d: %s", m.Version, m.Name)

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", m.Version, err)
		}

		if _, err := tx.Exec(m.SQL); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Name, err)
		}

		if _, err := tx.Exec(
			"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
			m.Version, m.Name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration v%d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", m.Version, err)
		}
		log.Printf("store: migration v%d applied", m.Version)
	}

	return nil
}
