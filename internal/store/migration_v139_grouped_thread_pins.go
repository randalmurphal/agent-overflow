package store

// Migration v139 lets a grouped thread hold its own pin and takes the pin
// off thread groups (docs/specs/sidebar-thread-groups.md).
//
//   - threads loses CHECK(group_id IS NULL OR pinned_at IS NULL). SQLite
//     cannot drop a CHECK, or a column with a REFERENCES clause, in place,
//     so the table is rebuilt with every current column, index and
//     trigger, exactly as v125 left them less v138's pr_ref.
//     legacy_alter_table keeps the rename from re-resolving the triggers
//     and views that name threads; the rebuild runner resets the pragma on
//     its connection whatever happens. owned_threads is dropped and
//     restated around the rebuild, and trg_threads_fork_source_delete,
//     which DROP TABLE removes, is restated after it.
//   - thread_groups drops pin_group, then pinned_at: pin_group's CHECK
//     names pinned_at, so pinned_at can only go second.
const groupedThreadPinsMigrationVersion = 139

const groupedThreadPinsV139SQL = `DROP VIEW owned_threads;

PRAGMA legacy_alter_table=ON;

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
        CHECK(mode IN ('chat','plan','discussion','terminal','workflow','workflow-studio','workflow-triage','scratch','holder')),
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
    deleting                 INTEGER NOT NULL DEFAULT 0 CHECK(deleting IN (0,1))
);

INSERT INTO threads_new (
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, worktree_setup_state,
    import_source, history_rev, history_epoch, history_bulk_load, live_todo,
    pending_fork_resume_at, pin_group, group_id, created_by_device,
    created_branch, created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref, mode, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, discussion_id,
    parent_thread_id, forked_from_thread_id, last_token_usage, last_read_at,
    pinned_at, created_at, updated_at, archived, worktree_setup_state,
    import_source, history_rev, history_epoch, history_bulk_load, live_todo,
    pending_fork_resume_at, pin_group, group_id, created_by_device,
    created_branch, created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting
FROM threads;

DROP TABLE threads;

ALTER TABLE threads_new RENAME TO threads;

PRAGMA legacy_alter_table=OFF;

CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);
CREATE INDEX idx_threads_group       ON threads(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX idx_threads_parent      ON threads(parent_thread_id);
CREATE INDEX idx_threads_pinned_at   ON threads(pinned_at) WHERE pinned_at IS NOT NULL;
CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);
CREATE INDEX idx_threads_deleting    ON threads(id) WHERE deleting = 1;

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

ALTER TABLE thread_groups DROP COLUMN pin_group;

ALTER TABLE thread_groups DROP COLUMN pinned_at;
`
