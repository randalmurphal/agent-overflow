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
	// Fix is an optional Go-side data fixup that runs inside the same
	// transaction as SQL (after it, when both are set) and the version
	// bump. Use it when the row transformation needs logic SQL can't
	// express (JSON reshaping, per-row Go helpers). A migration must set
	// SQL, Fix, or both. Not supported on Rebuild migrations — those are
	// table-shape changes by definition.
	Fix func(tx *sql.Tx) error
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

// rebuildProvidersV10SQL widens the provider CHECK on every table that pins it
// to ('claude','codex') so the claude-tui provider — the real interactive
// Claude Code TUI, additive to headless claude — becomes a persistable
// provider. Four tables carry the constraint and are each rebuilt (CREATE
// _new / copy / DROP / RENAME, with explicit column lists guarding against
// drift) because SQLite cannot alter a CHECK in place:
//
//   - threads                 (provider CHECK + provider/effort coupling)
//   - chat_model_profiles     (provider CHECK + provider/effort coupling)
//   - chat_bar_favorites      (compound kind/provider CHECK)
//   - new_thread_mcp_defaults (provider CHECK)
//
// claude-tui runs the same claude binary, so it shares claude's reasoning
// effort set (low/medium/high/xhigh/max); the provider/effort coupling CHECK
// on threads and chat_model_profiles gains a matching clause. The threads
// block is rebuildThreadsV5SQL verbatim with only the two CHECK clauses
// widened — keep its column list in lockstep with v5.
//
// Runs with foreign_keys OFF (see applyRebuildMigration) so DROP TABLE threads
// does not cascade-delete the child tables that REFERENCE threads(id) ON
// DELETE CASCADE; the three leaf tables carry no foreign keys but ride the
// same FK-off window. Every table's indexes are dropped with the old table and
// recreated here (threads: 5, chat_bar_favorites: 1, chat_model_profiles: 1,
// new_thread_mcp_defaults: none).
const rebuildProvidersV10SQL = `
CREATE TABLE threads_new (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
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
            OR (provider = 'claude-tui' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
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

CREATE TABLE chat_model_profiles_new (
    provider         TEXT    NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
    model            TEXT    NOT NULL,
    reasoning_effort TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
            OR (provider = 'claude-tui' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
        ),
    fast_mode        INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window   INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode     TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access')),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY(provider, model)
);

INSERT INTO chat_model_profiles_new (
    provider, model, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    updated_at
)
SELECT
    provider, model, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    updated_at
FROM chat_model_profiles;

DROP TABLE chat_model_profiles;

ALTER TABLE chat_model_profiles_new RENAME TO chat_model_profiles;

CREATE INDEX idx_chat_model_profiles_updated ON chat_model_profiles(updated_at DESC);

CREATE TABLE chat_bar_favorites_new (
	kind       TEXT    NOT NULL CHECK(kind IN ('model','discussion')),
	provider   TEXT    NOT NULL DEFAULT '',
	value      TEXT    NOT NULL,
	label      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(kind, provider, value),
	CHECK (
		(kind = 'model' AND provider IN ('claude','codex','claude-tui'))
		OR
		(kind = 'discussion' AND provider = '')
	)
);

INSERT INTO chat_bar_favorites_new (kind, provider, value, label, created_at)
SELECT kind, provider, value, label, created_at FROM chat_bar_favorites;

DROP TABLE chat_bar_favorites;

ALTER TABLE chat_bar_favorites_new RENAME TO chat_bar_favorites;

CREATE INDEX idx_chat_bar_favorites_created ON chat_bar_favorites(created_at DESC);

CREATE TABLE new_thread_mcp_defaults_new (
    provider         TEXT NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
    workspace_path   TEXT NOT NULL DEFAULT '',
    disabled_servers TEXT NOT NULL DEFAULT '[]' CHECK(json_valid(disabled_servers)),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (provider, workspace_path)
);

INSERT INTO new_thread_mcp_defaults_new (provider, workspace_path, disabled_servers, updated_at)
SELECT provider, workspace_path, disabled_servers, updated_at FROM new_thread_mcp_defaults;

DROP TABLE new_thread_mcp_defaults;

ALTER TABLE new_thread_mcp_defaults_new RENAME TO new_thread_mcp_defaults;
`

// rebuildItemsV11SQL widens the items.kind CHECK to admit
// 'compaction_reasoning' — the live "compact" tail row that streams the
// claudetui compaction summarizer's reasoning above the 'compaction' divider.
// SQLite cannot alter a CHECK in place, so the items table is rebuilt (CREATE
// items_new / copy / DROP / RENAME) with an explicit column list guarding
// against drift; the body is the v1 items definition verbatim with only the
// kind CHECK extended and every index recreated.
//
// Runs with foreign_keys OFF (see applyRebuildMigration) so DROP TABLE items
// does not cascade-delete thread_checkpoints, whose
// (thread_id, user_item_id) FK REFERENCES items(thread_id, id) ON DELETE
// CASCADE. The rename re-links that FK by name and assertForeignKeysIntact
// (foreign_key_check) verifies no row was stranded — always clean here because
// ids are copied verbatim. Nine indexes AND the two payload-GC triggers
// (trg_items_gc_payload / trg_items_gc_input_payload, dropped with the old
// table) ride the same window and are recreated below.
const rebuildItemsV11SQL = `
CREATE TABLE items_new (
    id                  TEXT    NOT NULL,
    thread_id           TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index          INTEGER NOT NULL,
    item_index          INTEGER NOT NULL,
    kind                TEXT    NOT NULL CHECK(kind IN (
        'user_text',
        'assistant_text',
        'thinking',
        'compaction_reasoning',
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
    input_payload_id    TEXT REFERENCES payloads(id),
    PRIMARY KEY (thread_id, id)
);

INSERT INTO items_new (
    id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, parent_id, is_background, completion_of, tool_name, decision,
    meta, created_at, updated_at, input_payload_id
)
SELECT
    id, thread_id, turn_index, item_index, kind, role, status, summary,
    payload_id, parent_id, is_background, completion_of, tool_name, decision,
    meta, created_at, updated_at, input_payload_id
FROM items;

DROP TABLE items;

ALTER TABLE items_new RENAME TO items;

CREATE INDEX idx_items_thread
    ON items(thread_id, turn_index, item_index);

CREATE UNIQUE INDEX idx_items_thread_turn_item_unique
    ON items(thread_id, turn_index, item_index);

CREATE INDEX idx_items_parent
    ON items(thread_id, parent_id) WHERE parent_id <> '';

CREATE INDEX idx_items_completion_of
    ON items(thread_id, completion_of) WHERE completion_of <> '';

CREATE INDEX idx_items_payload_id
    ON items(payload_id) WHERE payload_id IS NOT NULL;

CREATE INDEX idx_items_meta_task_id
    ON items(thread_id, json_extract(meta, '$.task_id'))
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;

CREATE INDEX idx_items_input_payload_id
    ON items(input_payload_id) WHERE input_payload_id IS NOT NULL;

CREATE INDEX idx_items_live_background
    ON items(thread_id, id)
 WHERE is_background = 1
   AND status = 'running'
   AND parent_id = ''
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0;

CREATE INDEX idx_items_live_codex_subagent
  ON items(thread_id, turn_index, item_index)
  WHERE kind = 'tool_call'
    AND status = 'completed'
    AND tool_name = 'collab_agent'
    AND is_background = 1
    AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0
    AND json_extract(meta, '$.input.tool') IN ('spawn_agent', 'spawnAgent');

CREATE TRIGGER trg_items_gc_input_payload
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
END;

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
`

const rebuildDiffReviewCommentsV16SQL = `
CREATE TABLE diff_review_comments_new (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	scope         TEXT    NOT NULL CHECK(scope IN ('turn', 'session', 'workspace', 'branch')),
	source_key    TEXT    NOT NULL,
	file_path     TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'sent', 'resolved')),
	old_line      INTEGER NOT NULL DEFAULT 0 CHECK(old_line >= 0),
	new_line      INTEGER NOT NULL DEFAULT 0 CHECK(new_line >= 0),
	side          TEXT    NOT NULL CHECK(side IN ('file', 'old', 'new', 'context')),
	selected_text TEXT    NOT NULL DEFAULT '',
	body          TEXT    NOT NULL,
	sent_at       INTEGER NOT NULL DEFAULT 0,
	sent_turn_id  TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
)
SELECT
	id, thread_id, scope, source_key, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);
`

const rebuildDiffReviewCommentsV18SQL = `
CREATE TABLE diff_review_comments_new (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	scope         TEXT    NOT NULL CHECK(scope IN ('turn', 'session', 'workspace', 'branch', 'pr')),
	source_key    TEXT    NOT NULL,
	commit_sha    TEXT    NOT NULL DEFAULT '',
	file_path     TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'sent', 'resolved')),
	old_line      INTEGER NOT NULL DEFAULT 0 CHECK(old_line >= 0),
	new_line      INTEGER NOT NULL DEFAULT 0 CHECK(new_line >= 0),
	side          TEXT    NOT NULL CHECK(side IN ('file', 'old', 'new', 'context')),
	selected_text TEXT    NOT NULL DEFAULT '',
	body          TEXT    NOT NULL,
	sent_at       INTEGER NOT NULL DEFAULT 0,
	sent_turn_id  TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	CHECK((side = 'file' AND old_line = 0 AND new_line = 0)
	   OR (side = 'old' AND old_line > 0)
	   OR (side = 'new' AND new_line > 0)
	   OR (side = 'context' AND old_line > 0 AND new_line > 0))
);

INSERT INTO diff_review_comments_new (
	id, thread_id, scope, source_key, commit_sha, file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
)
SELECT
	id, thread_id, scope, source_key, '', file_path, status, old_line, new_line, side,
	selected_text, body, sent_at, sent_turn_id, created_at, updated_at
FROM diff_review_comments;

DROP TABLE diff_review_comments;

ALTER TABLE diff_review_comments_new RENAME TO diff_review_comments;

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);
`

// rebuildCodexReasoningEffortsV19SQL widens the Codex reasoning-effort
// CHECKs on threads and chat_model_profiles to admit the GPT-5.6 catalog's
// max and ultra tiers. SQLite cannot alter a CHECK in place, so both tables
// are rebuilt with explicit column lists. Runs with foreign_keys OFF (see
// applyRebuildMigration) so DROP TABLE threads does not cascade-delete child
// rows; ids are copied verbatim and foreign_key_check verifies integrity.
const rebuildCodexReasoningEffortsV19SQL = `
CREATE TABLE threads_new (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
    model                    TEXT    NOT NULL DEFAULT '',
    workspace_path           TEXT    NOT NULL,
    worktree_path            TEXT,
    branch                   TEXT,
    pr_ref                   TEXT    NOT NULL DEFAULT '',
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','design','discussion','terminal')),
    reasoning_effort         TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
            OR (provider = 'claude-tui' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
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
    branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, disabled_mcp_servers
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
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

CREATE TABLE chat_model_profiles_new (
    provider         TEXT    NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
    model            TEXT    NOT NULL,
    reasoning_effort TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
            OR (provider = 'claude-tui' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
        ),
    fast_mode        INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window   INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode     TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access')),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY(provider, model)
);

INSERT INTO chat_model_profiles_new (
    provider, model, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    updated_at
)
SELECT
    provider, model, reasoning_effort, fast_mode, context_window,
    auto_compact_standard_percent, auto_compact_extended_percent, runtime_mode,
    updated_at
FROM chat_model_profiles;

DROP TABLE chat_model_profiles;

ALTER TABLE chat_model_profiles_new RENAME TO chat_model_profiles;

CREATE INDEX idx_chat_model_profiles_updated ON chat_model_profiles(updated_at DESC);
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
	{
		Version: 7,
		Name:    "payload_append_chunks",
		SQL: `CREATE TABLE payload_chunks (
    payload_id   TEXT    NOT NULL REFERENCES payloads(id) ON DELETE CASCADE,
    chunk_index  INTEGER NOT NULL,
    start_offset INTEGER NOT NULL CHECK(start_offset >= 0),
    data         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (payload_id, chunk_index)
);

CREATE INDEX idx_payload_chunks_payload_start
  ON payload_chunks(payload_id, start_offset);`,
	},
	{
		Version: 8,
		Name:    "trim_tool_result_echo_meta",
		Fix:     trimToolResultEchoMetaFixup,
	},
	{
		Version: 9,
		Name:    "trim_collab_agent_state_messages_meta",
		Fix:     trimCollabAgentStateMessagesMetaFixup,
	},
	{
		Version: 10,
		Name:    "claude_tui_provider",
		SQL:     rebuildProvidersV10SQL,
		Rebuild: true,
	},
	{
		Version: 11,
		Name:    "compaction_reasoning_item_kind",
		SQL:     rebuildItemsV11SQL,
		Rebuild: true,
	},
	{
		Version: 12,
		Name:    "channel_max_turns",
		// The DEFAULT 8 literal mirrors discussion.DefaultMaxTurns (and
		// the frontend's DEFAULT_MAX_TURNS in
		// frontend/src/lib/types/discussion.ts), but is frozen by
		// migration semantics — shipped migrations never change. If the
		// shared default ever moves, this SQL stays 8 and a new
		// migration carries the new value.
		SQL: `ALTER TABLE channels ADD COLUMN max_turns INTEGER NOT NULL DEFAULT 8;`,
	},
	{
		Version: 13,
		Name:    "turns_inflight_partial_index",
		// Backs the boot-time crashed-turn sweep (RecoverCrashedTurns).
		// Partial on completed_at IS NULL so it only ever holds the
		// handful of in-flight rows — the sweep's SELECT is O(crashed)
		// instead of a full turns scan, and steady-state maintenance
		// cost is one insert + one delete per turn.
		SQL: `CREATE INDEX idx_turns_inflight
  ON turns(thread_id, turn_index)
  WHERE completed_at IS NULL;`,
	},
	{
		Version: 14,
		Name:    "usage_ledger",
		// Append-only per-turn token/cost accounting (one row per model
		// per settled turn). DELIBERATELY no foreign keys: lifetime
		// usage totals must survive thread and project deletion, so
		// thread_id / project_id / turn_id are plain attribution columns
		// and provider/model are denormalized at write time. cost_usd is
		// wire-reported only (Claude); 0 with no wire cost means
		// "unpriced", which is what cost_source records.
		SQL: `CREATE TABLE usage_ledger (
    id                          INTEGER PRIMARY KEY,
    created_at                  INTEGER NOT NULL,
    thread_id                   TEXT    NOT NULL,
    project_id                  TEXT    NOT NULL DEFAULT '',
    turn_id                     TEXT    NOT NULL DEFAULT '',
    provider                    TEXT    NOT NULL DEFAULT '',
    model                       TEXT    NOT NULL DEFAULT '',
    input_tokens                INTEGER NOT NULL DEFAULT 0,
    output_tokens               INTEGER NOT NULL DEFAULT 0,
    cache_read_input_tokens     INTEGER NOT NULL DEFAULT 0,
    cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_output_tokens     INTEGER NOT NULL DEFAULT 0,
    cost_usd                    REAL    NOT NULL DEFAULT 0,
    cost_source                 TEXT    NOT NULL DEFAULT 'none'
        CHECK(cost_source IN ('wire','none'))
);

CREATE INDEX idx_usage_ledger_created ON usage_ledger(created_at);

CREATE INDEX idx_usage_ledger_thread ON usage_ledger(thread_id, created_at);`,
	},
	{
		Version: 15,
		Name:    "ui_state",
		// Persisted per-client UI view state (sidebar width, pane
		// layout, collapsed sections). scope is an opaque namespace
		// string — "client:<uuid>" today, "user:<id>" reserved for
		// when identities exist. This table exists because webview
		// localStorage is not durable here: the transport binds an
		// ephemeral port, so the webview origin (and its per-origin
		// storage) changes every launch. Frontend $state still owns
		// in-session reactivity; these rows are the restart-surviving
		// copy, hydrated once at boot.
		SQL: `CREATE TABLE ui_state (
    scope      TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (scope, key)
);`,
	},
	{
		Version: 16,
		Name:    "diff_review_comment_local_scopes",
		SQL:     rebuildDiffReviewCommentsV16SQL,
		Rebuild: true,
	},
	{
		Version: 17,
		Name:    "thread_pr_ref",
		Fix:     addThreadPRRefColumn,
	},
	{
		Version: 18,
		Name:    "diff_review_comment_pr_scope",
		SQL:     rebuildDiffReviewCommentsV18SQL,
		Rebuild: true,
	},
	{
		Version: 19,
		Name:    "codex_max_ultra_reasoning",
		SQL:     rebuildCodexReasoningEffortsV19SQL,
		Rebuild: true,
	},
	{
		Version: 20,
		Name:    "turns_provider_turn_id",
		// Provider-assigned turn id, decoupled from the PRIMARY KEY so
		// forked threads can carry copies of their source's turns (a
		// Codex fork keeps the source's turn ids, so the copy is what
		// makes a cloned turn usable as a `thread/fork` lastTurnId
		// anchor — see Store.CloneThreadTurns). Backfill: Codex rows
		// store the wire id verbatim in turn_id; Claude rows use the
		// synthesized `<threadID>:<turnIndex>` form, recognizable by
		// the ':' that never appears in a Codex turn id.
		SQL: `ALTER TABLE turns ADD COLUMN provider_turn_id TEXT NOT NULL DEFAULT '';

UPDATE turns SET provider_turn_id = turn_id WHERE turn_id NOT LIKE '%:%';`,
	},
	{
		Version: 21,
		Name:    "trim_codex_v2_encrypted_collab_prompts",
		Fix:     trimCodexV2EncryptedCollabPromptsFixup,
	},
	{
		Version: 22,
		Name:    "payload_highlight_spans",
		// Version-stamped, content-addressed highlight span blobs
		// persisted beside the payload they describe (JSON, shape owned
		// by the app layer — PersistedPatchSpans). preview_spans covers
		// the per-file inline-diff preview patches and rides item list
		// reads via the itemColumns join; spans covers the full payload
		// data and is read only by the on-demand payload loads. Empty
		// string means "not computed" — readers fall back to the
		// highlight RPC path.
		SQL: `ALTER TABLE payloads ADD COLUMN preview_spans TEXT NOT NULL DEFAULT '';
ALTER TABLE payloads ADD COLUMN spans TEXT NOT NULL DEFAULT '';`,
	},
	{
		Version: 23,
		Name:    "message_anchors_replace_checkpoints",
		// The per-message git-checkpoint machinery is gone. What fork-
		// from-message and revert-on-interrupt still need from the old
		// thread_checkpoints row is pure provider correlation — turn
		// index + Claude wire uuids — so the table slims into
		// message_anchors and the git-side columns (ref_name,
		// baseline_sha, status, files, workspace_path) drop with the
		// snapshots themselves. thread_tracked_files backed only the
		// removed session-diff/files-revert paths. Review comments
		// scoped to the removed 'turn'/'session' diff sources are
		// orphaned (their diffs can never load again) and go too; the
		// scope CHECK constraint keeps the legacy values because
		// rebuilding the table to tighten it buys nothing. Leftover
		// refs/agent-overflow/* refs in workspaces are cleaned up
		// manually if at all (see docs/architecture/schema.md) —
		// migrations don't run subprocesses.
		SQL: `CREATE TABLE message_anchors (
    thread_id                TEXT    NOT NULL,
    user_item_id             TEXT    NOT NULL,
    turn_index               INTEGER NOT NULL,
    provider_user_message_id TEXT    NOT NULL DEFAULT '',
    provider_parent_uuid     TEXT    NOT NULL DEFAULT '',
    created_at               INTEGER NOT NULL,
    PRIMARY KEY (thread_id, user_item_id),
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id, user_item_id) REFERENCES items(thread_id, id) ON DELETE CASCADE
);

CREATE INDEX idx_message_anchors_thread_turn
    ON message_anchors(thread_id, turn_index);

INSERT INTO message_anchors (
	thread_id, user_item_id, turn_index,
	provider_user_message_id, provider_parent_uuid, created_at
)
SELECT thread_id, user_item_id, turn_index,
       provider_user_message_id, provider_parent_uuid, captured_at
  FROM thread_checkpoints;

DROP TABLE thread_checkpoints;

DROP TABLE thread_tracked_files;

DELETE FROM diff_review_comments WHERE scope IN ('turn', 'session');`,
	},
	{
		Version: 24,
		Name:    "edit_file_snapshots",
		// Per-edit new-side file snapshots backing the review pane's
		// Edits scope: gzip-compressed full file content captured at
		// diff persist time, the one moment the just-edited workspace
		// file provably matches the patch. Hunk-gap expansion and span
		// priming resolve against the snapshot first, so a historical
		// edit stays expandable after later turns drift the workspace
		// file. Rows ride the payload lifecycle (the payload GC
		// triggers cascade here); absent rows — pre-feature history or
		// size-capped writes — degrade to the workspace-verify
		// fallback. No backfill: old edits simply have no snapshot.
		SQL: `CREATE TABLE edit_file_snapshots (
    payload_id TEXT    NOT NULL REFERENCES payloads(id) ON DELETE CASCADE,
    path       TEXT    NOT NULL,
    content    BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (payload_id, path)
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
	if strings.TrimSpace(m.SQL) == "" && m.Fix == nil {
		return fmt.Errorf("migration v%d (%s) has neither SQL nor Fix", m.Version, m.Name)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration v%d: %w", m.Version, err)
	}

	if strings.TrimSpace(m.SQL) != "" {
		if _, err := tx.Exec(m.SQL); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d (%s) failed: %w", m.Version, m.Name, err)
		}
	}
	if m.Fix != nil {
		if err := m.Fix(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d (%s) fixup failed: %w", m.Version, m.Name, err)
		}
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

func addThreadPRRefColumn(tx *sql.Tx) error {
	columns, err := tableColumnsTx(tx, "threads")
	if err != nil {
		return err
	}
	if columns["pr_ref"] {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE threads ADD COLUMN pr_ref TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add threads.pr_ref: %w", err)
	}
	return nil
}

func tableColumnsTx(tx *sql.Tx, table string) (map[string]bool, error) {
	if !validTableName.MatchString(table) {
		return nil, fmt.Errorf("invalid table name: %q", table)
	}
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
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
