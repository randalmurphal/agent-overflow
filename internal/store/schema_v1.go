package store

const initialSchemaSQL = `
CREATE TABLE attachments (
    id            TEXT    PRIMARY KEY,
    thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    filename      TEXT    NOT NULL,
    mime_type     TEXT    NOT NULL,
    size          INTEGER NOT NULL,
    relative_path TEXT    NOT NULL,
    created_at    INTEGER NOT NULL
, thumbnail_data BLOB, thumbnail_mime TEXT);

CREATE TABLE "channel_messages" (
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

CREATE TABLE channels (
    id          TEXT    PRIMARY KEY,
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    type        TEXT    NOT NULL DEFAULT 'deliberation',
    status      TEXT    NOT NULL DEFAULT 'open',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE chat_bar_favorites (
	kind       TEXT    NOT NULL CHECK(kind IN ('model','discussion')),
	provider   TEXT    NOT NULL DEFAULT '',
	value      TEXT    NOT NULL,
	label      TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY(kind, provider, value),
	CHECK (
		(kind = 'model' AND provider IN ('claude','codex'))
		OR
		(kind = 'discussion' AND provider = '')
	)
);

CREATE TABLE "chat_model_profiles" (
    provider         TEXT    NOT NULL CHECK(provider IN ('claude','codex')),
    model            TEXT    NOT NULL,
    reasoning_effort TEXT    NOT NULL DEFAULT 'high'
        CHECK(
            (provider = 'codex' AND reasoning_effort IN ('none','minimal','low','medium','high','xhigh'))
            OR (provider = 'claude' AND reasoning_effort IN ('low','medium','high','xhigh','max'))
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

CREATE TABLE diff_review_comments (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	scope         TEXT    NOT NULL CHECK(scope IN ('session', 'workspace')),
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

CREATE TABLE "items" (
    id                  TEXT    NOT NULL,
    thread_id           TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index          INTEGER NOT NULL,
    item_index          INTEGER NOT NULL,
    kind                TEXT    NOT NULL CHECK(kind IN (
        'user_text',
        'assistant_text',
        'thinking',
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
    updated_at          INTEGER NOT NULL, input_payload_id TEXT REFERENCES payloads(id),
    PRIMARY KEY (thread_id, id)
);

CREATE TABLE payloads (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT '{}',
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE pending_background_task_terminals (
    thread_id    TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    task_id      TEXT    NOT NULL,
    tool_use_id  TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL,
    exit_code    INTEGER,
    output_file  TEXT    NOT NULL DEFAULT '',
    end_time     INTEGER,
    source       TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (thread_id, task_id)
);

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

CREATE TABLE proposed_plan_comments (
	id            TEXT    PRIMARY KEY,
	thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	plan_item_id  TEXT    NOT NULL,
	status        TEXT    NOT NULL DEFAULT 'draft' CHECK(status IN ('draft', 'sent', 'resolved')),
	start_line    INTEGER NOT NULL CHECK(start_line > 0),
	end_line      INTEGER NOT NULL CHECK(end_line > 0),
	selected_text TEXT    NOT NULL DEFAULT '',
	body          TEXT    NOT NULL,
	sent_at       INTEGER NOT NULL DEFAULT 0,
	sent_turn_id  TEXT    NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	CHECK(end_line >= start_line),
	FOREIGN KEY(thread_id, plan_item_id) REFERENCES proposed_plans(thread_id, item_id) ON DELETE CASCADE
);

CREATE TABLE proposed_plans (
	item_id                  TEXT    NOT NULL,
	thread_id                TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
	revision_parent_item_id  TEXT    NOT NULL DEFAULT '',
	version                  INTEGER NOT NULL CHECK(version > 0),
	implemented_at           INTEGER NOT NULL DEFAULT 0,
	implemented_by_thread_id TEXT    NOT NULL DEFAULT '',
	implemented_by_item_id   TEXT    NOT NULL DEFAULT '',
	created_at               INTEGER NOT NULL,
	updated_at               INTEGER NOT NULL,
	PRIMARY KEY(thread_id, item_id),
	UNIQUE(thread_id, version)
);

CREATE TABLE thread_checkpoints (
    id                       TEXT    PRIMARY KEY,
    thread_id                TEXT    NOT NULL,
    user_item_id             TEXT    NOT NULL,
    turn_index               INTEGER NOT NULL,
    provider_user_message_id TEXT    NOT NULL DEFAULT '',
    provider_parent_uuid     TEXT    NOT NULL DEFAULT '',
    ref_name                 TEXT    NOT NULL,
    baseline_sha             TEXT    NOT NULL DEFAULT '',
    status                   TEXT    NOT NULL DEFAULT 'ready',
    files                    TEXT    NOT NULL DEFAULT '[]',
    captured_at              INTEGER NOT NULL,
    workspace_path           TEXT    NOT NULL,
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE,
    FOREIGN KEY (thread_id, user_item_id) REFERENCES items(thread_id, id) ON DELETE CASCADE,
    UNIQUE(thread_id, user_item_id)
);

CREATE TABLE thread_drafts (
    thread_id      TEXT    PRIMARY KEY REFERENCES threads(id) ON DELETE CASCADE,
    content        TEXT    NOT NULL DEFAULT '',
    attachments    TEXT    NOT NULL DEFAULT '[]',
    terminal_chips TEXT    NOT NULL DEFAULT '[]',
    updated_at     INTEGER NOT NULL
, pending_plan_implementation TEXT);

CREATE TABLE thread_tracked_files (
    thread_id  TEXT    NOT NULL,
    turn_index INTEGER NOT NULL,
    path       TEXT    NOT NULL,
    PRIMARY KEY (thread_id, turn_index, path),
    FOREIGN KEY (thread_id) REFERENCES threads(id) ON DELETE CASCADE
);

CREATE TABLE "threads" (
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
    discussion_id            TEXT,
    parent_thread_id         TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    forked_from_thread_id    TEXT    REFERENCES threads(id) ON DELETE SET NULL,
    last_token_usage         TEXT    NOT NULL DEFAULT ''
        CHECK(last_token_usage = '' OR json_valid(last_token_usage)),
    last_read_at             INTEGER,
    pinned_at                INTEGER,
    created_at               INTEGER NOT NULL,
    updated_at               INTEGER NOT NULL,
    archived                 INTEGER NOT NULL DEFAULT 0 CHECK(archived IN (0,1))
);

CREATE TABLE turns (
    turn_id              TEXT    PRIMARY KEY,
    thread_id            TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    turn_index           INTEGER NOT NULL CHECK(turn_index >= 0),
    started_at           INTEGER NOT NULL,
    completed_at         INTEGER,
    stop_reason          TEXT    NOT NULL DEFAULT '',
    assistant_message_id TEXT    NOT NULL DEFAULT '',
    token_usage_json     TEXT    NOT NULL DEFAULT '',
    error_message        TEXT    NOT NULL DEFAULT '',
    UNIQUE(thread_id, turn_index)
);

CREATE INDEX idx_attachments_thread ON attachments(thread_id);

CREATE INDEX idx_channels_thread ON channels(thread_id);

CREATE INDEX idx_chat_bar_favorites_created
	ON chat_bar_favorites(created_at DESC);

CREATE INDEX idx_chat_model_profiles_updated
    ON chat_model_profiles(updated_at DESC);

CREATE INDEX idx_diff_review_comments_scope
	ON diff_review_comments(thread_id, scope, source_key, status, file_path, old_line, new_line, created_at);

-- items indexes: non-partial indexes first, then specialised partial
-- indexes. The order matters for the SQLite query planner on empty
-- tables (no row stats): when two indexes are equally selective for
-- a WHERE clause, the planner prefers the one whose sqlite_master
-- rowid is larger. Putting the partial filtered indexes last keeps
-- ListLiveBackgroundTasks etc. on their intended index instead of
-- falling back to idx_items_thread_turn_item_unique.
CREATE INDEX idx_items_thread
    ON items(thread_id, turn_index, item_index);

CREATE INDEX idx_items_id
    ON items(id);

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

CREATE INDEX idx_pending_terminals_tool_use
    ON pending_background_task_terminals(thread_id, tool_use_id)
    WHERE tool_use_id <> '';

CREATE INDEX idx_projects_archived ON projects(archived, updated_at DESC);

CREATE INDEX idx_projects_updated  ON projects(updated_at DESC);

CREATE INDEX idx_proposed_plan_comments_plan
	ON proposed_plan_comments(thread_id, plan_item_id, status, start_line, created_at);

CREATE INDEX idx_proposed_plans_thread_version
	ON proposed_plans(thread_id, version DESC);

CREATE INDEX idx_thread_checkpoints_provider_user
    ON thread_checkpoints(thread_id, provider_user_message_id)
    WHERE provider_user_message_id <> '';

CREATE INDEX idx_thread_checkpoints_thread_turn
    ON thread_checkpoints(thread_id, turn_index);

CREATE INDEX idx_thread_drafts_pending_plan_impl
  ON thread_drafts(thread_id)
  WHERE pending_plan_implementation IS NOT NULL;

CREATE INDEX idx_thread_tracked_files_thread_turn
    ON thread_tracked_files(thread_id, turn_index);

CREATE INDEX idx_threads_forked_from ON threads(forked_from_thread_id);

CREATE INDEX idx_threads_parent      ON threads(parent_thread_id);

CREATE INDEX idx_threads_pinned_at
  ON threads(pinned_at)
  WHERE pinned_at IS NOT NULL;

CREATE INDEX idx_threads_project     ON threads(project_id, updated_at DESC);

CREATE INDEX idx_threads_updated     ON threads(updated_at DESC);

CREATE INDEX idx_turns_thread_completed
  ON turns(thread_id, completed_at DESC)
  WHERE completed_at IS NOT NULL;

CREATE INDEX turns_thread_index ON turns(thread_id, turn_index DESC);

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
