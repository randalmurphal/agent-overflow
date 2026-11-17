package store

// Migration v154 removes the discussion feature and the workflow scheduler
// (docs/decisions.md, discussions removed; docs/specs/standing-agents.md,
// ruling 13: the wake supervisor is the app's only clock) and rebuilds the
// tables whose declarations that and the standing agents change.
//
//   - channel_messages, channels, discussion_definitions, automation_cursors
//     and automations are dropped.
//   - threads loses discussion_id, parent_thread_id, idx_threads_parent and
//     the 'discussion' mode: a former discussion thread becomes a chat
//     thread and its participants stay as independent chat threads. It
//     gains `placement`, where the sidebar shows the row, and the attention
//     flag an agent raises (attention_kind, attention_reason, attention_at).
//     A thread in a group is grouped and every other thread is standalone,
//     so no thread that existed before loses its sidebar row; only a later
//     spawn makes a thread owned (thread_placement.go).
//     Every other column, index and trigger is restated as v139 left it, and
//     owned_threads and trg_threads_fork_source_delete are restated around
//     the rebuild as v139 does.
//   - scratch_threads.return_mode loses 'discussion' (mapped to 'chat').
//   - chat_bar_favorites keeps only model favorites.
//   - work_items.source loses 'automation': a run an automation started
//     becomes a manual run, and its source_ref, which named the dropped
//     automation, is cleared. idx_work_items_automation_source_ref, which
//     only the scheduler's overlap check read, goes with it.
//   - remote_watches cascades from the thread that receives its completion,
//     so a deleted thread's watch leaves the watcher's due poll; a watch
//     whose thread is already gone is deleted. It gains `confirmed_at`, the
//     provider's echo of a queued completion, recorded as the creation time
//     for a watch already queued whose message the previous version read as
//     delivered: no row of its thread marked pendingFlush that the residue
//     rule calls unconfirmed (store.ListUnconfirmedUserRows) carries it, and
//     its queue row is gone or a row carrying the provider's id answers for
//     it (app.queuedSendDelivered). Any other queued watch is left
//     unconfirmed, and the boot gives its completion back to it;
//     `wake_attempts` and `wake_issue`, which
//     pace and bound the redelivery of a completion a session end gave back;
//     and `launched_in_thread_id`, the thread that launched the job, which
//     is the watch's own thread for every existing watch.
//     idx_remote_watches_done serves the sweep that deletes finished watches
//     past the retention floor.
const retireFeaturesMigrationVersion = 154

const retireFeaturesV154SQL = `DROP TABLE channel_messages;

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
    deleting                 INTEGER NOT NULL DEFAULT 0 CHECK(deleting IN (0,1)),
    placement                TEXT    NOT NULL DEFAULT 'standalone'
        CHECK(placement IN ('owned','grouped','standalone')
          AND (placement = 'grouped') = (group_id IS NOT NULL)),
    attention_kind           TEXT    NOT NULL DEFAULT ''
        CHECK(attention_kind IN ('', 'decision', 'review', 'problem')),
    attention_reason         TEXT    NOT NULL DEFAULT '',
    attention_at             INTEGER
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
    placement
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
    fork_source_title, newest_turn_error_at, newest_turn_error_turn, deleting,
    CASE WHEN group_id IS NOT NULL THEN 'grouped' ELSE 'standalone' END
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

DROP TABLE automation_cursors;

DROP TABLE automations;

CREATE TABLE work_items_new (
    id               TEXT    PRIMARY KEY,
    project_id       TEXT    NOT NULL,
    goal             TEXT    NOT NULL,
    workflow_id      TEXT    NOT NULL,
    workflow_scope   TEXT    NOT NULL CHECK(workflow_scope IN ('project','shared')),
    snapshot         TEXT    NOT NULL DEFAULT '' CHECK(snapshot = '' OR json_valid(snapshot)),
    state            TEXT    NOT NULL CHECK(state IN ('running','needs-human','done','failed','cancelled')),
    reason           TEXT    NOT NULL DEFAULT '' CHECK(reason IN ('','gate','question','stuck','stalled','budget-exhausted','retries-exhausted','provider-retries-exhausted','provider-usage-limited','loop-limit-exhausted','check-failed-genuine','agent-error','wiring-error','disposition','setup-failed','interrupted','taken-over','unit-failed','child-failed','paused','checkpoint')),
    seeds            TEXT    NOT NULL DEFAULT '' CHECK(seeds = '' OR json_valid(seeds)),
    step_mode        INTEGER NOT NULL DEFAULT 0 CHECK(step_mode IN (0,1)),
    soft_stop        INTEGER NOT NULL DEFAULT 0 CHECK(soft_stop IN (0,1)),
    wake_signature   TEXT    NOT NULL DEFAULT '',
    pending_guidance TEXT    NOT NULL DEFAULT '',
    auto_resume_at   INTEGER NOT NULL DEFAULT 0,
    worktree_path    TEXT    NOT NULL DEFAULT '',
    branch           TEXT    NOT NULL DEFAULT '',
    base_branch      TEXT    NOT NULL DEFAULT '',
    budget           TEXT    NOT NULL DEFAULT '' CHECK(budget = '' OR json_valid(budget)),
    source           TEXT    NOT NULL CHECK(source IN ('manual','agent','call')),
    source_ref       TEXT    NOT NULL DEFAULT '',
    triage_thread_id TEXT    NOT NULL DEFAULT '',
    origin_thread_id TEXT    NOT NULL DEFAULT '',
    disposition      TEXT    NOT NULL DEFAULT '' CHECK(disposition = '' OR json_valid(disposition)),
    digest           TEXT    NOT NULL DEFAULT '' CHECK(digest = '' OR json_valid(digest)),
    parent_item_id   TEXT    NOT NULL DEFAULT '',
    parent_phase_id  TEXT    NOT NULL DEFAULT '',
    parent_unit_id   TEXT    NOT NULL DEFAULT '',
    parent_attempt   INTEGER NOT NULL DEFAULT 0 CHECK(parent_attempt >= 0),
    call_depth       INTEGER NOT NULL DEFAULT 0 CHECK(call_depth >= 0),
    created_at       INTEGER NOT NULL,
    started_at       INTEGER NOT NULL DEFAULT 0,
    ended_at         INTEGER NOT NULL DEFAULT 0,
    CHECK((parent_item_id = '') = (parent_phase_id = '')),
    CHECK((parent_item_id = '') = (parent_attempt = 0)),
    CHECK((parent_item_id = '') = (call_depth = 0)),
    CHECK(parent_item_id = '' OR origin_thread_id = ''),
    CHECK(parent_unit_id = '' OR parent_item_id <> '')
);

INSERT INTO work_items_new (
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, soft_stop, wake_signature, pending_guidance, auto_resume_at,
    worktree_path, branch, base_branch, budget, source, source_ref,
    triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
    created_at, started_at, ended_at
)
SELECT
    id, project_id, goal, workflow_id, workflow_scope, snapshot, state, reason,
    seeds, step_mode, soft_stop, wake_signature, pending_guidance, auto_resume_at,
    worktree_path, branch, base_branch, budget,
    CASE source WHEN 'automation' THEN 'manual' ELSE source END,
    CASE source WHEN 'automation' THEN '' ELSE source_ref END,
    triage_thread_id, origin_thread_id, disposition, digest,
    parent_item_id, parent_phase_id, parent_unit_id, parent_attempt, call_depth,
    created_at, started_at, ended_at
FROM work_items;

DROP TABLE work_items;

ALTER TABLE work_items_new RENAME TO work_items;

CREATE INDEX idx_work_items_project_state_created
  ON work_items(project_id, state, created_at);

CREATE INDEX idx_work_items_project_created
  ON work_items(project_id, created_at);

CREATE INDEX idx_work_items_state_created
  ON work_items(state, created_at, id);

CREATE INDEX idx_work_items_triage_thread
  ON work_items(triage_thread_id)
  WHERE triage_thread_id <> '';

CREATE UNIQUE INDEX idx_work_items_agent_source_ref
  ON work_items(source_ref)
  WHERE source = 'agent' AND source_ref <> '';

CREATE INDEX idx_work_items_parent
  ON work_items(parent_item_id, parent_phase_id, parent_attempt)
  WHERE parent_item_id <> '';

CREATE INDEX idx_work_items_origin_thread
  ON work_items(origin_thread_id)
  WHERE origin_thread_id <> '';

CREATE TABLE remote_watches_new (
 computer_id TEXT NOT NULL, request_id TEXT NOT NULL,
 thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
 fingerprint TEXT NOT NULL, label TEXT NOT NULL, receipt TEXT NOT NULL DEFAULT '{}',
 error TEXT NOT NULL DEFAULT '', notification TEXT NOT NULL DEFAULT 'pending'
 CHECK(notification IN ('pending','queued','dismissed')),
 next_check INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, command TEXT NOT NULL DEFAULT '',
 confirmed_at INTEGER, wake_attempts INTEGER NOT NULL DEFAULT 0, wake_issue TEXT NOT NULL DEFAULT '',
 launched_in_thread_id TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(computer_id,request_id)
);
INSERT INTO remote_watches_new (computer_id, request_id, thread_id, fingerprint, label, receipt, error,
    notification, next_check, created_at, command, confirmed_at, launched_in_thread_id)
SELECT w.computer_id, w.request_id, w.thread_id, w.fingerprint, w.label, w.receipt, w.error,
    w.notification, w.next_check, w.created_at, w.command,
    CASE WHEN w.notification = 'queued'
      AND NOT EXISTS (SELECT 1 FROM items u
            WHERE u.thread_id = w.thread_id AND u.kind = 'user_text' AND u.parent_id = ''
              AND (CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.pendingFlush') END) = 1
              AND u.created_at >= (SELECT t.created_at FROM threads t WHERE t.id = u.thread_id)
              AND NOT EXISTS (SELECT 1 FROM turns later WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index)
              AND NOT EXISTS (SELECT 1 FROM items later
                               WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index AND later.parent_id = '')
              AND ((CASE WHEN json_valid(u.meta) THEN json_extract(u.meta, '$.sendId') END) = 'remote-completion:' || w.computer_id || ':' || w.request_id
                OR 'remote-completion:' || w.computer_id || ':' || w.request_id IN (SELECT j.value
                     FROM json_each(CASE WHEN json_valid(u.meta) THEN u.meta ELSE '{}' END, '$.joinedSendIds') j)))
      AND NOT (EXISTS (SELECT 1 FROM flush_queue_items q
                WHERE q.thread_id = w.thread_id AND q.send_id = 'remote-completion:' || w.computer_id || ':' || w.request_id)
           AND NOT EXISTS (SELECT 1 FROM items d
                WHERE d.thread_id = w.thread_id AND d.kind = 'user_text' AND d.parent_id = ''
                  AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.provider_item_id'), '') END) <> ''
                  AND (CASE WHEN json_valid(d.meta) THEN COALESCE(json_extract(d.meta, '$.wire_only'), 0) END) != 1
                  AND ((CASE WHEN json_valid(d.meta) THEN json_extract(d.meta, '$.sendId') END) = 'remote-completion:' || w.computer_id || ':' || w.request_id
                    OR 'remote-completion:' || w.computer_id || ':' || w.request_id IN (SELECT j.value
                         FROM json_each(CASE WHEN json_valid(d.meta) THEN d.meta ELSE '{}' END, '$.joinedSendIds') j))))
    THEN w.created_at END, w.thread_id
  FROM remote_watches w WHERE w.thread_id IN (SELECT id FROM threads);
DROP TABLE remote_watches;
ALTER TABLE remote_watches_new RENAME TO remote_watches;
CREATE INDEX idx_remote_watches_pending ON remote_watches(next_check) WHERE notification='pending';
CREATE INDEX idx_remote_watches_thread ON remote_watches(thread_id,created_at DESC);
CREATE INDEX idx_remote_watches_done ON remote_watches(created_at)
 WHERE notification = 'dismissed' OR (notification = 'queued' AND confirmed_at IS NOT NULL);
`
