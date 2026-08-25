package store

// Rebuild DDL for the threads table and the three tables that ride the same
// migrations with it — chat_model_profiles, chat_bar_favorites, and
// new_thread_mcp_defaults. The runtime_mode CHECK constants and the v34/v45
// derivations that widen them live here too: a derivation and the text it
// derives from must stay readable together.
//
// The chain driver, the derivation helpers, and the migrations slice are in
// migrate.go.

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

// rebuildThreadsWorkflowModeV27SQL extends threads.mode with the workflow
// value used by phase threads. It preserves the complete v22 table shape and
// recreates every threads index dropped with the old table.
const rebuildThreadsWorkflowModeV27SQL = `
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
        CHECK(mode IN ('chat','plan','design','discussion','terminal','workflow')),
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
`

// rebuildThreadsWorkflowModesV28SQL preserves the complete v27 rebuild text
// and changes only the enumerated mode CHECK. Deriving it from the shipped v27
// SQL makes accidental column/index drift impossible.
var rebuildThreadsWorkflowModesV28SQL = mustReplaceOnce(
	rebuildThreadsWorkflowModeV27SQL,
	"CHECK(mode IN ('chat','plan','design','discussion','terminal','workflow'))",
	"CHECK(mode IN ('chat','plan','design','discussion','terminal','workflow','workflow-studio','workflow-triage'))",
)

// runtimeModeCheckPreV34 is the runtime_mode CHECK shipped on both threads and
// chat_model_profiles up to v33; runtimeModeCheckV34 widened it with the
// read-only tier, and runtimeModeCheckV45 widens it again with the auto tier.
// Written out in full (rather than assembled from provider.AllRuntimeModes)
// because internal/store stays provider-free — TestRuntimeModeCheckMatchesProvider
// asserts the current constant and provider.AllRuntimeModes agree.
const (
	runtimeModeCheckPreV34 = "CHECK(runtime_mode IN ('approval-required','auto-accept-edits','full-access'))"
	runtimeModeCheckV34    = "CHECK(runtime_mode IN ('read-only','approval-required','auto-accept-edits','full-access'))"
	runtimeModeCheckV45    = "CHECK(runtime_mode IN ('read-only','approval-required','auto-accept-edits','auto','full-access'))"
)

// chatModelProfilesRebuildV19SQL is the chat_model_profiles half of the v19
// rebuild — the statement group that established that table's current shape.
// Sliced out of the shipped v19 text rather than retyped so a later rebuild
// cannot drift from the columns and index the table actually has. v19 rebuilds
// threads first and chat_model_profiles second, so cutting at the profiles
// CREATE takes the profiles group and nothing before it.
var chatModelProfilesRebuildV19SQL = mustCutFrom(
	rebuildCodexReasoningEffortsV19SQL,
	"CREATE TABLE chat_model_profiles_new (",
)

// rebuildRuntimeModeReadOnlyV34SQL widens the runtime_mode CHECK on both
// tables that carry one so a workflow phase declaring `access: read-only` can
// persist the restricted runtime mode on its thread row (spec §9, D22).
//
// Both tables must move together. A thread's runtime mode is written back into
// the chat_model_profiles row for its provider/model by rememberChatModelProfile
// (app_thread_model.go), so widening only threads would make opening a
// read-only workflow thread fail on the profile write instead of the thread
// write — a CHECK violation one hop away from the change that caused it.
//
// SQLite cannot alter a CHECK in place, so each table is rebuilt. Both halves
// are derived from the migrations that last established their shapes (threads:
// v28, chat_model_profiles: v19), which preserves every column, index, and FK
// property of those proven rebuilds.
var rebuildRuntimeModeReadOnlyV34SQL = mustReplaceOnce(
	rebuildThreadsWorkflowModesV28SQL, runtimeModeCheckPreV34, runtimeModeCheckV34,
) + mustReplaceOnce(
	chatModelProfilesRebuildV19SQL, runtimeModeCheckPreV34, runtimeModeCheckV34,
)

// rebuildRuntimeModeAutoV45SQL widens the same CHECK on the same two tables
// with the `auto` tier — the AI-reviewed approval mode (Claude
// `--permission-mode auto`, Codex `approvalsReviewer: "auto_review"`).
//
// Derived from v34's text rather than from v28/v19 directly: v34 is now the
// migration that last established these tables' shapes, so re-deriving from
// its output is what keeps this rebuild carrying every column, index, and FK
// those tables actually have. The constraint appears once per table, hence the
// count of 2 — a third table gaining a runtime_mode CHECK would fail this
// derivation loudly instead of quietly leaving that table on the old set.
var rebuildRuntimeModeAutoV45SQL = mustReplaceEvery(
	rebuildRuntimeModeReadOnlyV34SQL, runtimeModeCheckV34, runtimeModeCheckV45, 2,
)
