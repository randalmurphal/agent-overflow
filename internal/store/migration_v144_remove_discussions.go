package store

// Migration v144 removes the discussion feature's schema
// (docs/decisions.md, discussions removed).
//
//   - channel_messages, channels and discussion_definitions are dropped.
//   - threads loses discussion_id, parent_thread_id, idx_threads_parent
//     and the 'discussion' mode. A former discussion thread becomes a
//     chat thread and its participants stay as independent chat threads.
//     discussion_id and parent_thread_id carry REFERENCES clauses and the
//     mode CHECK changes, so the table is rebuilt with every other current
//     column, index and trigger exactly as v139 left them. owned_threads
//     and trg_threads_fork_source_delete are restated around the rebuild
//     as v139 does.
//   - scratch_threads.return_mode loses 'discussion' (mapped to 'chat').
//   - chat_bar_favorites keeps only model favorites.
const removeDiscussionsMigrationVersion = 144

const removeDiscussionsV144SQL = `DROP TABLE channel_messages;

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
    session_ref              TEXT,
    pending_fork_session_ref TEXT,
    mode                     TEXT    NOT NULL DEFAULT 'chat'
        CHECK(mode IN ('chat','plan','terminal','workflow','workflow-studio','workflow-triage','scratch','holder')),
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
    auto_compact_extended_percent, runtime_mode, forked_from_thread_id,
    last_token_usage, last_read_at, pinned_at, created_at, updated_at,
    archived, worktree_setup_state, import_source, history_rev,
    history_epoch, history_bulk_load, live_todo, pending_fork_resume_at,
    pin_group, group_id, created_by_device, created_branch,
    created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting
)
SELECT
    id, project_id, title, provider, model, workspace_path, worktree_path,
    branch, session_ref, pending_fork_session_ref,
    CASE mode WHEN 'discussion' THEN 'chat' ELSE mode END, reasoning_effort,
    fast_mode, context_window, auto_compact_standard_percent,
    auto_compact_extended_percent, runtime_mode, forked_from_thread_id,
    last_token_usage, last_read_at, pinned_at, created_at, updated_at,
    archived, worktree_setup_state, import_source, history_rev,
    history_epoch, history_bulk_load, live_todo, pending_fork_resume_at,
    pin_group, group_id, created_by_device, created_branch,
    created_remote_url, created_head_commit, fork_preparing,
    fork_source_thread_id, fork_cut_turn_index, fork_cut_item_index,
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting
FROM threads;

DROP TABLE threads;

ALTER TABLE threads_new RENAME TO threads;

CREATE TABLE scratch_threads_new (
    thread_id        TEXT PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    source_thread_id TEXT NOT NULL,
    return_mode      TEXT NOT NULL
        CHECK(return_mode IN ('chat','plan','terminal','workflow','workflow-studio','workflow-triage')),
    created_at       INTEGER NOT NULL,
    request_token    TEXT
);

INSERT INTO scratch_threads_new (thread_id, source_thread_id, return_mode, created_at, request_token)
SELECT thread_id, source_thread_id,
       CASE return_mode WHEN 'discussion' THEN 'chat' ELSE return_mode END,
       created_at, request_token
FROM scratch_threads;

DROP TABLE scratch_threads;

ALTER TABLE scratch_threads_new RENAME TO scratch_threads;

PRAGMA legacy_alter_table=OFF;

CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);
CREATE INDEX idx_threads_group       ON threads(group_id) WHERE group_id IS NOT NULL;
CREATE INDEX idx_threads_pinned_at   ON threads(pinned_at) WHERE pinned_at IS NOT NULL;
CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);
CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);
CREATE INDEX idx_threads_deleting    ON threads(id) WHERE deleting = 1;

CREATE INDEX idx_scratch_threads_source ON scratch_threads(source_thread_id);

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

DROP TABLE channels;

DROP TABLE discussion_definitions;

CREATE TABLE chat_bar_favorites_new (
	kind       TEXT    NOT NULL CHECK(kind IN ('model')),
	provider   TEXT    NOT NULL CHECK(provider IN ('claude','codex','claude-tui')),
	value      TEXT    NOT NULL,
	label      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(kind, provider, value)
);

INSERT INTO chat_bar_favorites_new (kind, provider, value, label, created_at)
SELECT kind, provider, value, label, created_at FROM chat_bar_favorites WHERE kind = 'model';

DROP TABLE chat_bar_favorites;

ALTER TABLE chat_bar_favorites_new RENAME TO chat_bar_favorites;

CREATE INDEX idx_chat_bar_favorites_created ON chat_bar_favorites(created_at DESC);
`
