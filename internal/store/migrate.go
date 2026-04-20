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
	{
		Version: 7,
		Name:    "thread_checkpoints",
		SQL: `
CREATE TABLE IF NOT EXISTS thread_checkpoints (
	id             TEXT    PRIMARY KEY,
	thread_id      TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	turn_index     INTEGER NOT NULL,
	ref_name       TEXT    NOT NULL UNIQUE,
	baseline_sha   TEXT    NOT NULL DEFAULT '',
	captured_at    INTEGER NOT NULL,
	workspace_path TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_thread_checkpoints_thread_turn
	ON thread_checkpoints(thread_id, turn_index);
`,
	},
	{
		Version: 8,
		Name:    "thread_checkpoints_unique_thread_turn",
		// v7 put UNIQUE on ref_name, which is redundant with the PRIMARY KEY
		// id and wrong for the semantics we actually want: exactly one
		// checkpoint row per (thread_id, turn_index). A recapture for the
		// same turn should replace the row, not coexist with it. SQLite
		// can't ALTER a column's uniqueness in place, so we rebuild the
		// table. Collisions on (thread_id, turn_index) are collapsed by
		// keeping only the most recent capture.
		SQL: `
CREATE TABLE thread_checkpoints_v8 (
	id             TEXT    PRIMARY KEY,
	thread_id      TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	turn_index     INTEGER NOT NULL,
	ref_name       TEXT    NOT NULL,
	baseline_sha   TEXT    NOT NULL DEFAULT '',
	captured_at    INTEGER NOT NULL,
	workspace_path TEXT    NOT NULL,
	UNIQUE(thread_id, turn_index)
);

INSERT INTO thread_checkpoints_v8
    (id, thread_id, turn_index, ref_name, baseline_sha, captured_at, workspace_path)
SELECT c.id, c.thread_id, c.turn_index, c.ref_name, c.baseline_sha,
       c.captured_at, c.workspace_path
FROM thread_checkpoints c
JOIN (
    SELECT thread_id, turn_index, MAX(captured_at) AS captured_at
    FROM thread_checkpoints
    GROUP BY thread_id, turn_index
) latest
  ON latest.thread_id  = c.thread_id
 AND latest.turn_index = c.turn_index
 AND latest.captured_at = c.captured_at;

DROP TABLE thread_checkpoints;
ALTER TABLE thread_checkpoints_v8 RENAME TO thread_checkpoints;

CREATE INDEX IF NOT EXISTS idx_thread_checkpoints_thread_turn
	ON thread_checkpoints(thread_id, turn_index);
`,
	},
	{
		Version: 9,
		Name:    "payload_gc_on_item_delete",
		// items.payload_id points FROM items TO payloads, so a FK ON DELETE
		// CASCADE on that column would only propagate payloads→items
		// (delete the payload, lose the item), which is the wrong
		// direction. What we actually want is: when an item is deleted,
		// garbage-collect the payload it referenced. Deleting a thread
		// cascade-drops its items (via items.thread_id REFERENCES
		// threads(id) ON DELETE CASCADE), but before v9 their heavy
		// payloads stuck around forever.
		//
		// We install an AFTER DELETE trigger on items that removes the
		// payload the item pointed at, provided no other item still
		// references it. The "still referenced?" guard matters because
		// some event flows (e.g. payload-replacement upserts) transiently
		// share a payload id while an old item row is being swapped for
		// a new one; we don't want to delete a payload that a sibling
		// item still owns.
		//
		// We also sweep payloads that were already orphaned under the old
		// schema so the GC covers pre-existing leakage, not just future
		// deletes.
		SQL: `
CREATE TRIGGER IF NOT EXISTS trg_items_gc_payload
AFTER DELETE ON items
WHEN OLD.payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE payload_id = OLD.payload_id
       );
END;

DELETE FROM payloads
 WHERE id NOT IN (SELECT payload_id FROM items WHERE payload_id IS NOT NULL);
`,
	},
	{
		Version: 10,
		Name:    "items_unique_turn_item_index",
		// InsertItem does MAX(item_index)+1 inside a transaction and
		// inserts with the computed value. The store's SetMaxOpenConns(1)
		// serialises those transactions today, but that's a runtime knob
		// — nothing in the schema stops two writers from racing to the
		// same (thread, turn, item_index) if connection pooling is ever
		// loosened. A UNIQUE index makes the invariant schema-enforced so
		// a duplicate insert surfaces as a clean UNIQUE error instead of
		// silently corrupting timeline ordering.
		SQL: `
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_thread_turn_item_unique
    ON items(thread_id, turn_index, item_index);
`,
	},
	{
		Version: 11,
		Name:    "threads_interaction_mode_check",
		// v2 added interaction_mode with no CHECK constraint. That
		// leaves the column open to typos and stale-client writes
		// (a user with sqlite3 and a v1-era dump could plant "plann"
		// or any other value, and the app would carry it forever).
		// Adding a CHECK enforces the contract at the storage layer
		// so every subsequent read is guaranteed to see one of the
		// four canonical values.
		//
		// SQLite does not support ALTER TABLE ADD CONSTRAINT, so we
		// rebuild the threads table into a shadow with the new
		// constraint, copy rows, rename, and recreate indexes. Any
		// pre-existing row with an unexpected interaction_mode is
		// normalized to 'default' first so the copy succeeds instead
		// of aborting the whole migration — keeping the thread with
		// a safe default beats leaving the user with a database the
		// new binary refuses to open.
		SQL: `
UPDATE threads
   SET interaction_mode = 'default'
 WHERE interaction_mode NOT IN ('default', 'plan', 'design', 'discussion');

CREATE TABLE threads_v11 (
    id             TEXT PRIMARY KEY,
    title          TEXT NOT NULL DEFAULT 'New Thread',
    provider       TEXT NOT NULL CHECK(provider IN ('claude', 'codex')),
    session_ref    TEXT,
    workspace_path TEXT NOT NULL,
    model          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    archived       INTEGER NOT NULL DEFAULT 0,
    interaction_mode TEXT NOT NULL DEFAULT 'default'
        CHECK(interaction_mode IN ('default', 'plan', 'design', 'discussion')),
    branch         TEXT,
    worktree_path  TEXT,
    project_path   TEXT NOT NULL DEFAULT '',
    discussion_id  TEXT,
    parent_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL,
    pending_fork_session_ref TEXT,
    forked_from_thread_id TEXT REFERENCES threads(id) ON DELETE SET NULL
);

INSERT INTO threads_v11
    (id, title, provider, session_ref, workspace_path, model, created_at,
     updated_at, archived, interaction_mode, branch, worktree_path,
     project_path, discussion_id, parent_thread_id, pending_fork_session_ref,
     forked_from_thread_id)
SELECT id, title, provider, session_ref, workspace_path, model, created_at,
       updated_at, archived, interaction_mode, branch, worktree_path,
       project_path, discussion_id, parent_thread_id, pending_fork_session_ref,
       forked_from_thread_id
FROM threads;

DROP TABLE threads;
ALTER TABLE threads_v11 RENAME TO threads;

CREATE INDEX IF NOT EXISTS idx_threads_updated ON threads(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_threads_forked_from_thread ON threads(forked_from_thread_id);
`,
	},
	{
		Version: 12,
		Name:    "threads_runtime_mode",
		// Adds the three-tier runtime-mode axis (approval-required /
		// auto-accept-edits / full-access) as a persisted per-thread
		// column. Default is 'full-access' to match the frictionless
		// default that AllRuntimeModes / DefaultRuntimeMode in the
		// provider package reflect.
		//
		// Existing rows pre-v12 had no column; the ALTER seeds every
		// row with 'full-access' so legacy threads inherit the default
		// rather than tripping the CHECK constraint with NULL.
		SQL: `
ALTER TABLE threads
ADD COLUMN runtime_mode TEXT NOT NULL DEFAULT 'full-access'
    CHECK(runtime_mode IN ('approval-required', 'auto-accept-edits', 'full-access'));
`,
	},
	{
		Version: 13,
		Name:    "projects_and_thread_reshape",
		// Breaking reshape. The sidebar + composer rewrite introduces
		// Project as a first-class entity, renames interaction_mode →
		// mode, drops the free-form project_path in favor of a FK to
		// projects(id), and adds three new per-thread controls
		// (reasoning_effort, fast_mode, context_window). Because the
		// column list and FK topology change materially, ALTER TABLE
		// is not enough — we drop every v12 table and recreate from
		// the new shape. The user-facing contract is that this bump
		// wipes history; runMigrations logs a prominent line when v13
		// applies so the reset is visible in the app log.
		//
		// Dependent tables (items, payloads, channels,
		// channel_messages, discussion_definitions, design_artifacts,
		// attachments, thread_drafts, thread_checkpoints) are
		// recreated with the same shape they had at v12 after all
		// previous migrations — just referencing the new threads
		// table. Keeping the shapes identical avoids spreading the
		// reshape across unrelated subsystems.
		SQL: v13SQL,
	},
	{
		Version: 14,
		Name:    "items_tool_call_lifecycle",
		// Tool-call lifecycle columns. Inline tool calls update status in
		// place (running → completed|errored|declined). Backgrounded tool
		// calls keep their launch row as-is (is_background=1) and append a
		// second "completion" row whose completion_of_item_id points at the
		// launch — that pair is how the frontend renders the deferred
		// result without mutating persisted history.
		//
		// Every existing row represents a finished item (we only persisted
		// on completion before this migration), so the 'completed' default
		// is a correct backfill: no read path needs to retroactively
		// classify old rows.
		//
		// completion_of_item_id is a sibling to parent_tool_use_id: a TEXT
		// NOT NULL DEFAULT '' column with no FK (matches house style —
		// the id points at another item row but we don't want a cascade
		// delete path binding the pair together; the frontend treats them
		// as a loose reference).
		//
		// The partial index mirrors idx_items_parent_tool_use: we only
		// index non-empty values so the vast majority of rows (which never
		// complete a backgrounded launch) don't pay storage cost.
		SQL: `
ALTER TABLE items ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'
    CHECK(status IN ('running', 'completed', 'errored', 'declined'));
ALTER TABLE items ADD COLUMN is_background INTEGER NOT NULL DEFAULT 0
    CHECK(is_background IN (0, 1));
ALTER TABLE items ADD COLUMN completion_of_item_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_items_completion_of
    ON items(thread_id, completion_of_item_id) WHERE completion_of_item_id <> '';
`,
	},
	{
		Version: 15,
		Name:    "chat_rewrite_unified_items",
		SQL: `
ALTER TABLE threads ADD COLUMN last_token_usage TEXT NOT NULL DEFAULT '';

DROP TRIGGER IF EXISTS trg_items_gc_payload;
DROP TABLE IF EXISTS items;
DROP TABLE IF EXISTS payloads;

CREATE TABLE payloads (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT '{}',
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE items (
    id            TEXT    NOT NULL,
    thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index    INTEGER NOT NULL,
    item_index    INTEGER NOT NULL,
    kind          TEXT    NOT NULL CHECK(kind IN (
        'user_text',
        'assistant_text',
        'thinking',
        'tool_call',
        'tool_completion',
        'error',
        'compaction'
    )),
    role          TEXT    NOT NULL CHECK(role IN ('assistant', 'user', 'system')),
    status        TEXT    NOT NULL DEFAULT 'completed' CHECK(status IN (
        'streaming',
        'running',
        'completed',
        'errored',
        'declined'
    )),
    summary       TEXT    NOT NULL DEFAULT '',
    payload_id    TEXT    REFERENCES payloads(id),
    parent_id     TEXT    NOT NULL DEFAULT '',
    is_background INTEGER NOT NULL DEFAULT 0 CHECK(is_background IN (0, 1)),
    completion_of TEXT    NOT NULL DEFAULT '',
    tool_name     TEXT    NOT NULL DEFAULT '',
    decision      TEXT    NOT NULL DEFAULT '' CHECK(decision IN (
        '',
        'approved',
        'declined',
        'amended',
        'timeout',
        'lost'
    )),
    meta          TEXT    NOT NULL DEFAULT '{}',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (thread_id, id)
);

CREATE INDEX idx_items_thread            ON items(thread_id, turn_index, item_index);
CREATE INDEX idx_items_id                ON items(id);
CREATE UNIQUE INDEX idx_items_thread_turn_item_unique
    ON items(thread_id, turn_index, item_index);
CREATE INDEX idx_items_parent            ON items(thread_id, parent_id) WHERE parent_id <> '';
CREATE INDEX idx_items_completion_of     ON items(thread_id, completion_of) WHERE completion_of <> '';

CREATE TRIGGER trg_items_gc_payload
AFTER DELETE ON items
WHEN OLD.payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE payload_id = OLD.payload_id
       );
END;
`,
	},
	{
		Version: 16,
		Name:    "items_payload_id_index",
		// Payload reverse lookup (items keyed by payload_id) is used by
		// the "save payload to file" flow, which otherwise does a
		// threads × items nested scan to find the owning item. A partial
		// index skips rows where payload_id is NULL — the overwhelming
		// majority of text/user-message rows — so the index stays small
		// even on large history caches.
		SQL: `
CREATE INDEX IF NOT EXISTS idx_items_payload_id
    ON items(payload_id) WHERE payload_id IS NOT NULL;
`,
	},
	{
		Version: 17,
		Name:    "items_meta_task_id_index",
		// Background tool completion routing used to O(N) scan every
		// item on the thread to find the tool_call row whose
		// items.meta.task_id matches a Claude task_updated event with
		// an unknown tool_use_id. A partial expression index on
		// json_extract(meta, '$.task_id') — restricted to rows that
		// actually carry a task_id — converts that scan into an index
		// lookup. The predicate keeps the index tiny: only the handful
		// of inline background-tool rows (Bash with run_in_background,
		// Task subagents) ever have task_id set.
		SQL: `
CREATE INDEX IF NOT EXISTS idx_items_meta_task_id
    ON items(thread_id, json_extract(meta, '$.task_id'))
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;
`,
	},
}

// v13SQL is the DROP-and-rebuild payload for migration v13. Extracted so
// the lint-y size of the SQL doesn't bloat the migrations slice.
const v13SQL = `
-- Drop every v12 table. IF EXISTS keeps the migration idempotent on
-- fresh installs where these have never been created.
DROP TABLE IF EXISTS thread_checkpoints;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS thread_drafts;
DROP TABLE IF EXISTS design_artifacts;
DROP TABLE IF EXISTS channel_messages;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS discussion_definitions;
DROP TABLE IF EXISTS items;
DROP TABLE IF EXISTS payloads;
DROP TABLE IF EXISTS threads;

-- Projects: the new grouping entity. path is UNIQUE so two threads rooted
-- at the same directory share one project row.
CREATE TABLE projects (
    id            TEXT    PRIMARY KEY,
    path          TEXT    NOT NULL UNIQUE,
    name          TEXT    NOT NULL,
    color         TEXT    NOT NULL DEFAULT '',
    sort_position INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    archived      INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1))
);
CREATE INDEX idx_projects_updated  ON projects(updated_at DESC);
CREATE INDEX idx_projects_archived ON projects(archived, updated_at DESC);

-- Threads: replaces the legacy shape. project_id is required, mode
-- replaces interaction_mode (with the new 'chat' default), and the three
-- new composer controls (reasoning_effort, fast_mode, context_window)
-- persist per-thread so two threads in the same project can diverge.
CREATE TABLE threads (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(provider IN ('claude','codex')),
    model                    TEXT    NOT NULL DEFAULT '',
    workspace_path           TEXT    NOT NULL,
    worktree_path            TEXT,
    branch                   TEXT,
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','design','discussion')),
    reasoning_effort         TEXT    NOT NULL DEFAULT 'high'
        CHECK(reasoning_effort IN ('low','medium','high','xhigh','max')),
    fast_mode                INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window           INTEGER NOT NULL DEFAULT 1000000
        CHECK(context_window IN (200000,1000000)),
    runtime_mode             TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access')),
    discussion_id            TEXT,
    parent_thread_id         TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    forked_from_thread_id    TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    archived                 INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1))
);
CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);
CREATE INDEX idx_threads_parent      ON threads(parent_thread_id);
CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);

-- Heavy-payload blob store. Unchanged shape; recreated because v12 threads
-- reference it via items.payload_id and we dropped the whole table graph.
CREATE TABLE payloads (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT '{}',
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

-- Timeline items. parent_tool_use_id carries forward the v4 subagent
-- correlation column; the composite UNIQUE on (thread_id, turn_index,
-- item_index) carries forward v10.
CREATE TABLE items (
    id                 TEXT    PRIMARY KEY,
    thread_id          TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index         INTEGER NOT NULL,
    item_index         INTEGER NOT NULL,
    kind               TEXT    NOT NULL,
    role               TEXT    NOT NULL DEFAULT 'assistant',
    summary            TEXT    NOT NULL DEFAULT '',
    payload_id         TEXT    REFERENCES payloads(id),
    parent_tool_use_id TEXT    NOT NULL DEFAULT '',
    created_at         INTEGER NOT NULL
);
CREATE INDEX idx_items_thread            ON items(thread_id, turn_index, item_index);
CREATE INDEX idx_items_parent_tool_use   ON items(thread_id, parent_tool_use_id) WHERE parent_tool_use_id <> '';
CREATE UNIQUE INDEX idx_items_thread_turn_item_unique
    ON items(thread_id, turn_index, item_index);

-- Payload GC trigger (v9). Without it, deleting an item strands its
-- heavy payload. No legacy cleanup is needed because v13 just dropped
-- every payload row.
CREATE TRIGGER trg_items_gc_payload
AFTER DELETE ON items
WHEN OLD.payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items WHERE payload_id = OLD.payload_id
       );
END;

-- Deliberation channels + messages (v2 parity tables).
CREATE TABLE channels (
    id          TEXT    PRIMARY KEY,
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    type        TEXT    NOT NULL DEFAULT 'deliberation',
    status      TEXT    NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX idx_channels_thread ON channels(thread_id);

CREATE TABLE channel_messages (
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

CREATE TABLE discussion_definitions (
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

-- Design artifacts (v2).
CREATE TABLE design_artifacts (
    id          TEXT    PRIMARY KEY,
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    title       TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT 'render',
    html_path   TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_design_artifacts_thread ON design_artifacts(thread_id);

-- Attachments (v5).
CREATE TABLE attachments (
    id            TEXT    PRIMARY KEY,
    thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    filename      TEXT    NOT NULL,
    mime_type     TEXT    NOT NULL,
    size          INTEGER NOT NULL,
    relative_path TEXT    NOT NULL,
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_attachments_thread ON attachments(thread_id);

-- Thread drafts (v6).
CREATE TABLE thread_drafts (
    thread_id      TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    content        TEXT    NOT NULL DEFAULT '',
    attachments    TEXT    NOT NULL DEFAULT '[]',
    terminal_chips TEXT    NOT NULL DEFAULT '[]',
    updated_at     INTEGER NOT NULL
);

-- Thread checkpoints (v8: composite UNIQUE on thread+turn).
CREATE TABLE thread_checkpoints (
    id             TEXT    PRIMARY KEY,
    thread_id      TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index     INTEGER NOT NULL,
    ref_name       TEXT    NOT NULL,
    baseline_sha   TEXT    NOT NULL DEFAULT '',
    captured_at    INTEGER NOT NULL,
    workspace_path TEXT    NOT NULL,
    UNIQUE(thread_id, turn_index)
);
CREATE INDEX idx_thread_checkpoints_thread_turn
    ON thread_checkpoints(thread_id, turn_index);
`

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
	if m.Version == 13 {
		// v13 drops every user-visible table and rebuilds. Make the
		// data-loss side effect loud enough that a user skimming the
		// log sees it before their first launch on the new binary.
		log.Printf("store: applying breaking migration v13 (data reset)")
	}

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
