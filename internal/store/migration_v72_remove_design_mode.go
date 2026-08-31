package store

// removeDesignModeV72SQL converts the retired design thread type to ordinary
// chat and rebuilds threads without admitting new design rows. It restates the
// complete v71 table shape: new rebuild migrations must not derive from
// shipped SQL because doing so would make old migration text mutable source.
const removeDesignModeV72SQL = dropHistoryRevTriggersSQL + `
DROP TRIGGER IF EXISTS trg_items_require_import_override;
DROP TRIGGER IF EXISTS thread_import_state_unique_source_insert;
DROP TRIGGER IF EXISTS thread_import_state_unique_source_update;

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
        CHECK(mode IN ('chat','plan','discussion','terminal','workflow','workflow-studio','workflow-triage')),
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
        CHECK(runtime_mode IN ('read-only','approval-required','auto-accept-edits','auto','full-access')),
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
    worktree_setup_state     TEXT    NOT NULL DEFAULT ''
        CHECK(worktree_setup_state IN ('', 'running', 'failed')),
    import_source            TEXT    NOT NULL DEFAULT ''
        CHECK(import_source IN ('', 'claude', 'codex')),
    history_rev              INTEGER NOT NULL DEFAULT 0,
    history_epoch            INTEGER NOT NULL DEFAULT 0,
    history_bulk_load        INTEGER NOT NULL DEFAULT 0 CHECK(history_bulk_load IN (0,1)),
    live_todo                TEXT    NOT NULL DEFAULT ''
        CHECK(live_todo = '' OR json_valid(live_todo)),
    pending_fork_resume_at   TEXT    NOT NULL DEFAULT '',
    pin_group                INTEGER
        CHECK(pin_group IS NULL OR (pinned_at IS NOT NULL AND pin_group IN (0,1)))
);

INSERT INTO threads_new (
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, worktree_setup_state,
    import_source, history_rev, history_epoch, history_bulk_load, live_todo,
    pending_fork_resume_at, pin_group
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, pr_ref, session_ref, pending_fork_session_ref,
    CASE WHEN mode = 'design' THEN 'chat' ELSE mode END,
    reasoning_effort, fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, worktree_setup_state,
    import_source, history_rev, history_epoch, history_bulk_load, live_todo,
    pending_fork_resume_at, pin_group
FROM threads;

DROP TABLE threads;

ALTER TABLE threads_new RENAME TO threads;

CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);
CREATE INDEX idx_threads_parent      ON threads(parent_thread_id);
CREATE INDEX idx_threads_pinned_at   ON threads(pinned_at) WHERE pinned_at IS NOT NULL;
CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);
` + historyRevTriggersSQL + `

CREATE TRIGGER trg_items_require_import_override
BEFORE INSERT ON items
WHEN EXISTS (
    SELECT 1
      FROM thread_import_chunks refs
      JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
     WHERE refs.thread_id = NEW.thread_id
       AND imported.id = NEW.id
)
AND COALESCE((
    SELECT history_bulk_load FROM threads WHERE id = NEW.thread_id
), 0) = 0
AND NOT EXISTS (
    SELECT 1 FROM thread_import_item_overrides
     WHERE thread_id = NEW.thread_id AND item_id = NEW.id
)
BEGIN
    SELECT RAISE(ABORT, 'local item shadows imported history without an override');
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
