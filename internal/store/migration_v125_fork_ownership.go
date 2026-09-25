package store

// forkOwnershipV125SQL moves pointer forks from copies to ownership
// transfer (docs/architecture/sqlite-store.md#pointer-forks).
//
//   - The `holder` thread mode: a deleted thread its forks still read, or
//     the rows a revert moved out of a thread its forks read through. The
//     mode lives in the threads CHECK, so the table is rebuilt with every
//     current column, index and trigger. legacy_alter_table keeps the
//     rename from re-resolving the triggers and views that name threads,
//     which reach the new table by name once it is renamed; the rebuild
//     runner resets the pragma on its connection whatever happens.
//   - owned_threads leaves holders out, as it leaves out a thread whose
//     delete has begun, so no listing, search, catalog, transfer or fork
//     admission sees one.
//   - The fork divider rows go: a fork's origin is its thread row
//     (fork_source_title). A divider sits in its fork or, handed off or
//     transferred, in another thread under the same id prefix, so each
//     thread's prefix range of the primary key is read, never the table.
//     A fork that reads a thread holding one may show it, so its stamps
//     move with the delete, as the thread's own do through the item
//     delete trigger.
//   - thread_fork_copied goes with the materialization it recorded.
//   - The fork triggers are replaced: trg_items_fork_reader_stamp goes,
//     since a row a fork shows no longer changes, and the guards that
//     refuse such a change, the lineage release trigger and the revive of
//     a launch whose completion a holder takes arrive.
//   - idx_threads_fork_source goes: a deleted source becomes a holder, so
//     nothing looks up the forks made from one.
var forkOwnershipV125SQL = dropForkTriggersV120SQL + `
UPDATE threads SET history_rev = history_rev + 1, history_epoch = history_epoch + 1
 WHERE id IN (SELECT l.thread_id FROM thread_fork_lineage l
               WHERE EXISTS (SELECT 1 FROM items i
                              WHERE i.thread_id = l.ancestor_id AND i.id >= 'fork-origin-' AND i.id < 'fork-origin.'
                                AND i.tool_name = 'fork_origin'));

DELETE FROM items WHERE rowid IN (
  SELECT i.rowid FROM threads t
   CROSS JOIN items i ON i.thread_id = t.id AND i.id >= 'fork-origin-' AND i.id < 'fork-origin.'
   WHERE i.tool_name = 'fork_origin');

DROP TABLE thread_fork_copied;

DROP VIEW owned_threads;

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
    pr_ref                   TEXT    NOT NULL DEFAULT '',
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
        REFERENCES thread_groups(id) ON DELETE SET NULL
        CHECK(group_id IS NULL OR pinned_at IS NULL),
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
    branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
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
    branch, pr_ref, session_ref, pending_fork_session_ref, mode, reasoning_effort,
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
` + forkTriggersSQL

// dropForkTriggersV120SQL is the trigger list v120 installed.
const dropForkTriggersV120SQL = `DROP TRIGGER trg_threads_fork_source_delete;
DROP TRIGGER trg_items_fork_position;
DROP TRIGGER trg_items_fork_position_update;
DROP TRIGGER trg_items_fork_snapshot;
DROP TRIGGER trg_items_fork_snapshot_move;
DROP TRIGGER trg_items_fork_reader_stamp;
`
