package store

import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
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
	{
		Version: 3,
		Name:    "thread_fork_state",
		SQL: `
ALTER TABLE threads ADD COLUMN pending_fork_session_ref TEXT;
ALTER TABLE threads ADD COLUMN forked_from_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_threads_forked_from_thread ON threads(forked_from_thread_id);
`,
	},
	{
		Version: 4,
		Name:    "subagent_correlation",
		SQL: `
ALTER TABLE items ADD COLUMN parent_tool_use_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_items_parent_tool_use ON items(thread_id, parent_tool_use_id) WHERE parent_tool_use_id <> '';
`,
	},
	{
		Version: 5,
		Name:    "attachments",
		SQL: `
CREATE TABLE IF NOT EXISTS attachments (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	filename      TEXT    NOT NULL,
	mime_type     TEXT    NOT NULL,
	size          INTEGER NOT NULL,
	relative_path TEXT    NOT NULL,
	created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_thread ON attachments(thread_id);
`,
	},
	{
		Version: 6,
		Name:    "thread_drafts",
		SQL: `
CREATE TABLE IF NOT EXISTS thread_drafts (
	thread_id      TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
	content        TEXT    NOT NULL DEFAULT '',
	attachments    TEXT    NOT NULL DEFAULT '[]',
	terminal_chips TEXT    NOT NULL DEFAULT '[]',
	updated_at     INTEGER NOT NULL
);
`,
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
	if err := backfillLegacyMigrationVersions(db); err != nil {
		return err
	}

	applied, err := currentMigrationVersion(db)
	if err != nil {
		return err
	}

	return applyPendingMigrations(db, applied)
}

func configureDatabase(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}
	return nil
}

func ensureMigrationTable(db *sql.DB) error {
	if _, err := db.Exec(createMigrationVersionsTableSQL); err != nil {
		return fmt.Errorf("create migration_versions table: %w", err)
	}
	return nil
}

func backfillLegacyMigrationVersions(db *sql.DB) error {
	applied, err := currentMigrationVersion(db)
	if err != nil {
		return err
	}
	if applied != 0 {
		return nil
	}

	legacyVersion, err := detectLegacyMigrationVersion(db)
	if err != nil {
		return fmt.Errorf("detect legacy schema version: %w", err)
	}
	if legacyVersion == 0 {
		return nil
	}

	log.Printf("store: detected legacy schema at v%d; backfilling migration history", legacyVersion)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy migration backfill: %w", err)
	}
	for _, migration := range migrations {
		if migration.Version > legacyVersion {
			break
		}
		if _, err := tx.Exec(
			"INSERT INTO migration_versions (version, name) VALUES (?, ?)",
			migration.Version,
			migration.Name,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record legacy migration v%d: %w", migration.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy migration backfill: %w", err)
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

func hasLegacyV1Schema(db *sql.DB) (bool, error) {
	for _, table := range []string{"threads", "payloads", "items"} {
		exists, err := tableExists(db, table)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}
	return true, nil
}

func detectLegacyMigrationVersion(db *sql.DB) (int, error) {
	legacyV2, err := hasLegacyV2Schema(db)
	if err != nil {
		return 0, err
	}
	if legacyV2 {
		return 2, nil
	}

	legacyV1, err := hasLegacyV1Schema(db)
	if err != nil {
		return 0, err
	}
	if legacyV1 {
		return 1, nil
	}
	return 0, nil
}

func hasLegacyV2Schema(db *sql.DB) (bool, error) {
	for _, table := range []string{"channels", "channel_messages", "discussion_definitions", "design_artifacts"} {
		exists, err := tableExists(db, table)
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
	}

	threadColumns, err := tableColumns(db, "threads")
	if err != nil {
		return false, err
	}
	for _, column := range []string{
		"interaction_mode",
		"branch",
		"worktree_path",
		"project_path",
		"discussion_id",
		"parent_thread_id",
	} {
		if !threadColumns[column] {
			return false, nil
		}
	}
	return true, nil
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type='table' AND name=?",
		table,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup table %s: %w", table, err)
	}
	return true, nil
}

var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

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
