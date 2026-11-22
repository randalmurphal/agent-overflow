package store

// Migration v159 replaces hardcoded provider-name enum CHECK constraints
// (`IN ('claude','codex','claude-tui')` and `IN ('claude','codex')`) and
// provider-coupled reasoning-effort CHECKs with structural non-empty provider
// checks and the global reasoning-effort enum across the five tables that carry
// them: threads, chat_model_profiles, chat_bar_favorites, thread_import_state,
// and thread_transfer_sessions.
//
// Provider allowlists and per-provider effort legality are enforced in Go
// (`internal/store/threads.go` + `internal/provider`) and cross-checked by
// `TestReasoningEffortSetsMatchProvider`, so adding a new provider never
// requires a six-table SQLite rebuild.
const widenProviderChecksMigrationVersion = 159

const widenProviderChecksV159SQL = `DROP VIEW owned_threads;

PRAGMA legacy_alter_table=ON;

CREATE TABLE threads_new (
    id                       TEXT    PRIMARY KEY,
    project_id               TEXT    REFERENCES projects(id) ON DELETE CASCADE,
    title                    TEXT    NOT NULL DEFAULT 'New Thread',
    provider                 TEXT    NOT NULL CHECK(length(trim(provider)) > 0),
    model                    TEXT    NOT NULL DEFAULT '',
    workspace_path           TEXT    NOT NULL,
    worktree_path            TEXT,
    branch                   TEXT,
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','terminal','workflow','workflow-studio','workflow-triage','scratch','holder')),
    reasoning_effort         TEXT    NOT NULL DEFAULT 'high'
        CHECK(reasoning_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra')),
    fast_mode                INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window           INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode             TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('read-only','approval-required','auto-accept-edits','auto','full-access')),
    forked_from_thread_id    TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    last_token_usage         TEXT    NOT NULL DEFAULT ''
        CHECK(last_token_usage = '' OR json_valid(last_token_usage)),
    last_read_at             INTEGER,
    pinned_at                INTEGER,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    archived                 INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1)),
    worktree_setup_state     TEXT    NOT NULL DEFAULT ''
        CHECK(worktree_setup_state IN ('', 'running', 'failed')),
    import_source            TEXT    NOT NULL DEFAULT ''
        CHECK(import_source = '' OR length(trim(import_source)) > 0),
    history_rev              INTEGER NOT NULL DEFAULT 0,
    history_epoch            INTEGER NOT NULL DEFAULT 0,
    history_bulk_load        INTEGER NOT NULL DEFAULT 0 CHECK(history_bulk_load IN (0,1)),
    live_todo                TEXT    NOT NULL DEFAULT ''
        CHECK(live_todo = '' OR json_valid(live_todo)),
    pending_fork_resume_at   TEXT    NOT NULL DEFAULT '',
    pin_group                INTEGER
        CHECK(pin_group IS NULL OR (pinned_at IS NOT NULL AND pin_group IN (0,1))),
    group_id                 TEXT
        REFERENCES thread_groups(id) ON DELETE SET NULL,
    created_by_device        TEXT    NOT NULL DEFAULT '',
    created_branch           TEXT    NOT NULL DEFAULT '',
    created_remote_url       TEXT    NOT NULL DEFAULT '',
    created_head_commit      TEXT    NOT NULL DEFAULT '',
    fork_preparing           INTEGER NOT NULL DEFAULT 0 CHECK(fork_preparing IN (0,1)),
    fork_source_thread_id    TEXT    NOT NULL DEFAULT '',
    fork_cut_turn_index      INTEGER NOT NULL DEFAULT 0,
    fork_cut_item_index      INTEGER NOT NULL DEFAULT 0,
    fork_source_title        TEXT    NOT NULL DEFAULT '',
    newest_turn_error_at     INTEGER,
    newest_turn_error_turn   INTEGER,
    deleting                 INTEGER NOT NULL DEFAULT 0 CHECK(deleting IN (0,1)),
    placement                TEXT    NOT NULL DEFAULT 'standalone'
        CHECK(placement IN ('owned','grouped','standalone')
          AND (placement = 'grouped') = (group_id IS NOT NULL)),
    attention_kind           TEXT    NOT NULL DEFAULT ''
        CHECK(attention_kind IN ('', 'decision', 'review', 'problem')),
    attention_reason         TEXT    NOT NULL DEFAULT '',
    attention_at             INTEGER,
    delete_claim             TEXT    NOT NULL DEFAULT '',
    spawn_instructions       TEXT    NOT NULL DEFAULT '',
    spawn_level              INTEGER NOT NULL DEFAULT 0 CHECK(spawn_level >= 0)
);

INSERT INTO threads_new (
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, forked_from_thread_id,
    last_token_usage, last_read_at, pinned_at, created_at, updated_at,
    archived, worktree_setup_state, import_source, history_rev,
    history_epoch, history_bulk_load, live_todo, pending_fork_resume_at,
    pin_group, group_id, created_by_device, created_branch,
    created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting,
    placement, attention_kind, attention_reason, attention_at,
    delete_claim, spawn_instructions, spawn_level
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, forked_from_thread_id,
    last_token_usage, last_read_at, pinned_at, created_at, updated_at,
    archived, worktree_setup_state, import_source, history_rev,
    history_epoch, history_bulk_load, live_todo, pending_fork_resume_at,
    pin_group, group_id, created_by_device, created_branch,
    created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting,
    placement, attention_kind, attention_reason, attention_at,
    delete_claim, spawn_instructions, spawn_level
FROM threads;

DROP TABLE threads;

ALTER TABLE threads_new RENAME TO threads;

CREATE TABLE chat_model_profiles_new (
    provider         TEXT    NOT NULL CHECK(length(trim(provider)) > 0),
    model            TEXT    NOT NULL,
    reasoning_effort TEXT    NOT NULL DEFAULT 'high'
        CHECK(reasoning_effort IN ('none','minimal','low','medium','high','xhigh','max','ultra')),
    fast_mode        INTEGER NOT NULL DEFAULT 0 CHECK(fast_mode IN (0,1)),
    context_window   INTEGER NOT NULL DEFAULT 1000000 CHECK(context_window > 0),
    auto_compact_standard_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_standard_percent BETWEEN 0 AND 90),
    auto_compact_extended_percent INTEGER NOT NULL DEFAULT 0
        CHECK(auto_compact_extended_percent BETWEEN 0 AND 90),
    runtime_mode     TEXT    NOT NULL DEFAULT 'full-access'
        CHECK(runtime_mode IN ('read-only','approval-required','auto-accept-edits','auto','full-access')),
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

CREATE TABLE chat_bar_favorites_new (
    kind       TEXT    NOT NULL CHECK(kind IN ('model')),
    provider   TEXT    NOT NULL CHECK(length(trim(provider)) > 0),
    value      TEXT    NOT NULL,
    label      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(kind, provider, value)
);

INSERT INTO chat_bar_favorites_new (kind, provider, value, label, created_at)
SELECT kind, provider, value, label, created_at FROM chat_bar_favorites;

DROP TABLE chat_bar_favorites;

ALTER TABLE chat_bar_favorites_new RENAME TO chat_bar_favorites;

CREATE TABLE thread_import_state_new (
    thread_id                TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    provider                 TEXT    NOT NULL CHECK(length(trim(provider)) > 0),
    source_path              TEXT    NOT NULL,
    source_session_id        TEXT    NOT NULL,
    leaf_uuid                TEXT    NOT NULL DEFAULT '',
    last_source_uuid         TEXT    NOT NULL DEFAULT '',
    last_source_offset       INTEGER NOT NULL DEFAULT 0,
    last_turn_index          INTEGER NOT NULL DEFAULT -1,
    last_item_index          INTEGER NOT NULL DEFAULT -1,
    imported_at              INTEGER NOT NULL,
    refreshed_at             INTEGER NOT NULL DEFAULT 0,
    source_parent_session_id TEXT    NOT NULL DEFAULT '',
    source_meta_hash         TEXT    NOT NULL DEFAULT '',
    source_history_mode      TEXT    NOT NULL DEFAULT ''
);

INSERT INTO thread_import_state_new (
    thread_id, provider, source_path, source_session_id, leaf_uuid,
    last_source_uuid, last_source_offset, last_turn_index, last_item_index,
    imported_at, refreshed_at, source_parent_session_id, source_meta_hash,
    source_history_mode
)
SELECT
    thread_id, provider, source_path, source_session_id, leaf_uuid,
    last_source_uuid, last_source_offset, last_turn_index, last_item_index,
    imported_at, refreshed_at, source_parent_session_id, source_meta_hash,
    source_history_mode
FROM thread_import_state;

DROP TABLE thread_import_state;

ALTER TABLE thread_import_state_new RENAME TO thread_import_state;

CREATE TABLE thread_transfer_sessions_new (
    transfer_id TEXT NOT NULL REFERENCES thread_transfers(id),
    provider    TEXT NOT NULL CHECK(length(trim(provider)) > 0),
    session_ref TEXT NOT NULL,
    PRIMARY KEY(transfer_id, provider, session_ref)
);

INSERT INTO thread_transfer_sessions_new (transfer_id, provider, session_ref)
SELECT transfer_id, provider, session_ref FROM thread_transfer_sessions;

DROP TABLE thread_transfer_sessions;

ALTER TABLE thread_transfer_sessions_new RENAME TO thread_transfer_sessions;

PRAGMA legacy_alter_table=OFF;

CREATE INDEX idx_threads_forked_from  ON threads(forked_from_thread_id);
CREATE INDEX idx_threads_group        ON threads(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX idx_threads_pinned_at    ON threads(pinned_at) WHERE pinned_at IS NOT NULL;
CREATE INDEX idx_threads_project      ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated      ON threads(updated_at DESC);
CREATE INDEX idx_threads_deleting     ON threads(id) WHERE deleting = 1;
CREATE INDEX idx_threads_delete_claim ON threads(delete_claim) WHERE delete_claim <> '';

CREATE INDEX idx_chat_model_profiles_updated ON chat_model_profiles(updated_at DESC);
CREATE INDEX idx_chat_bar_favorites_created  ON chat_bar_favorites(created_at DESC);

CREATE INDEX idx_thread_import_state_parent
    ON thread_import_state(provider, source_parent_session_id)
    WHERE source_parent_session_id <> '';
CREATE INDEX idx_thread_import_state_source
    ON thread_import_state(provider, source_session_id);

CREATE INDEX idx_thread_transfer_sessions_ref
    ON thread_transfer_sessions(provider, session_ref);

CREATE VIEW owned_threads AS
SELECT threads.*, COALESCE((SELECT MAX(ownership_epoch) FROM thread_transfers
    WHERE thread_id = threads.id AND direction = 'incoming' AND phase = 'complete'),0) AS ownership_epoch
FROM threads
WHERE threads.deleting = 0 AND threads.mode <> 'holder' AND COALESCE((
    SELECT CASE
        WHEN direction = 'incoming' THEN phase = 'complete'
        WHEN kind = 'copy' THEN 1
        WHEN phase IN ('committed', 'complete') THEN 0
        ELSE 1 END
    FROM thread_transfers WHERE thread_id = threads.id AND phase <> 'canceled'
    ORDER BY rowid DESC LIMIT 1
), 1) = 1;

CREATE TRIGGER trg_threads_fork_source_delete BEFORE DELETE ON threads
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.id)
BEGIN
  SELECT RAISE(ABORT, 'pointer forks read this thread; it is kept as a holder, not deleted');
END;

CREATE TRIGGER trg_spawn_revision_thread
AFTER UPDATE OF title, placement, archived, deleting ON threads
WHEN OLD.title IS NOT NEW.title OR OLD.placement IS NOT NEW.placement
  OR OLD.archived IS NOT NEW.archived OR OLD.deleting IS NOT NEW.deleting
BEGIN
  INSERT OR REPLACE INTO spawn_revisions(owner_device_id, token, target, end)
  SELECT owner_device_id, token, NEW.id, CASE WHEN NEW.deleting THEN 'gone' WHEN spawn_link THEN '' ELSE 'released' END
    FROM thread_request_receipts WHERE target_thread_id = NEW.id AND track_spawn = 1;
END;

CREATE TRIGGER thread_import_state_unique_source_insert
BEFORE INSERT ON thread_import_state
WHEN NOT EXISTS (
    SELECT 1 FROM thread_import_state AS own
     WHERE own.thread_id = NEW.thread_id
       AND own.provider = NEW.provider
       AND own.source_session_id = NEW.source_session_id
)
 AND (EXISTS (
    SELECT 1 FROM thread_import_state AS existing
     WHERE existing.provider = NEW.provider
       AND existing.source_session_id = NEW.source_session_id
       AND existing.thread_id <> NEW.thread_id
) OR EXISTS (
    SELECT 1 FROM threads AS existing
     WHERE existing.id <> NEW.thread_id
       AND CASE existing.provider
             WHEN 'claude-tui' THEN 'claude'
             ELSE existing.provider
           END = NEW.provider
       AND (existing.session_ref = NEW.source_session_id
            OR existing.pending_fork_session_ref = NEW.source_session_id)
))
BEGIN
    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
END;

CREATE TRIGGER thread_import_state_unique_source_update
BEFORE UPDATE OF provider, source_session_id ON thread_import_state
WHEN (OLD.provider <> NEW.provider OR OLD.source_session_id <> NEW.source_session_id)
 AND (EXISTS (
    SELECT 1 FROM thread_import_state AS existing
     WHERE existing.provider = NEW.provider
       AND existing.source_session_id = NEW.source_session_id
       AND existing.thread_id <> NEW.thread_id
) OR EXISTS (
    SELECT 1 FROM threads AS existing
     WHERE existing.id <> NEW.thread_id
       AND CASE existing.provider
             WHEN 'claude-tui' THEN 'claude'
             ELSE existing.provider
           END = NEW.provider
       AND (existing.session_ref = NEW.source_session_id
            OR existing.pending_fork_session_ref = NEW.source_session_id)
))
BEGIN
    SELECT RAISE(ABORT, 'provider session is already claimed by another thread');
END;
`
