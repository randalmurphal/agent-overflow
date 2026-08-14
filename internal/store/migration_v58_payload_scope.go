package store

// payloadThreadScopeMigrationSQL makes payload ownership match the items key
// scope. Claude transcript branches deliberately reuse provider item IDs in
// separate threads; a globally keyed payload table turned that valid reuse
// into either an import failure (INSERT) or cross-thread corruption
// (INSERT OR REPLACE).
//
// Existing payload bytes are copied once for every distinct thread that
// references them. That preserves all readable history and makes branches
// independent from this migration onward. A historical live overwrite cannot
// be reconstructed here: the old schema retained only the last writer's bytes.
// Unreferenced payloads are intentionally discarded; payloads are item-owned
// cache rows and the item GC triggers already define that lifecycle.
const payloadThreadScopeMigrationSQL = `
CREATE TABLE payloads_new (
    thread_id     TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    id            TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    meta          TEXT    NOT NULL DEFAULT '{}',
    data          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    preview_spans TEXT    NOT NULL DEFAULT '',
    spans         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (thread_id, id)
);

INSERT INTO payloads_new (
    thread_id, id, kind, meta, data, created_at, preview_spans, spans
)
SELECT owners.thread_id, p.id, p.kind, p.meta, p.data, p.created_at,
       p.preview_spans, p.spans
  FROM payloads p
  JOIN (
        SELECT thread_id, payload_id AS id
          FROM items
         WHERE payload_id IS NOT NULL
        UNION
        SELECT thread_id, input_payload_id AS id
          FROM items
         WHERE input_payload_id IS NOT NULL
  ) owners ON owners.id = p.id;

CREATE TABLE payload_chunks_new (
    thread_id    TEXT    NOT NULL,
    payload_id   TEXT    NOT NULL,
    chunk_index  INTEGER NOT NULL,
    start_offset INTEGER NOT NULL CHECK(start_offset >= 0),
    data         BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (thread_id, payload_id, chunk_index),
    FOREIGN KEY (thread_id, payload_id)
        REFERENCES payloads_new(thread_id, id) ON DELETE CASCADE
);

INSERT INTO payload_chunks_new (
    thread_id, payload_id, chunk_index, start_offset, data, created_at
)
SELECT owners.thread_id, c.payload_id, c.chunk_index, c.start_offset,
       c.data, c.created_at
  FROM payload_chunks c
  JOIN (
        SELECT thread_id, payload_id AS id
          FROM items
         WHERE payload_id IS NOT NULL
        UNION
        SELECT thread_id, input_payload_id AS id
          FROM items
         WHERE input_payload_id IS NOT NULL
  ) owners ON owners.id = c.payload_id;

CREATE TABLE edit_file_snapshots_new (
    thread_id  TEXT    NOT NULL,
    payload_id TEXT    NOT NULL,
    path       TEXT    NOT NULL,
    content    BLOB    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (thread_id, payload_id, path),
    FOREIGN KEY (thread_id, payload_id)
        REFERENCES payloads_new(thread_id, id) ON DELETE CASCADE
);

INSERT INTO edit_file_snapshots_new (
    thread_id, payload_id, path, content, created_at
)
SELECT owners.thread_id, s.payload_id, s.path, s.content, s.created_at
  FROM edit_file_snapshots s
  JOIN (
        SELECT thread_id, payload_id AS id
          FROM items
         WHERE payload_id IS NOT NULL
        UNION
        SELECT thread_id, input_payload_id AS id
          FROM items
         WHERE input_payload_id IS NOT NULL
  ) owners ON owners.id = s.payload_id;

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
        'workflow_proposal',
        'command_result',
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
    payload_id          TEXT,
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
    input_payload_id    TEXT,
    PRIMARY KEY (thread_id, id),
    FOREIGN KEY (thread_id, payload_id)
        REFERENCES payloads_new(thread_id, id),
    FOREIGN KEY (thread_id, input_payload_id)
        REFERENCES payloads_new(thread_id, id)
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
DROP TABLE payload_chunks;
DROP TABLE edit_file_snapshots;
DROP TABLE payloads;

ALTER TABLE payloads_new RENAME TO payloads;
ALTER TABLE payload_chunks_new RENAME TO payload_chunks;
ALTER TABLE edit_file_snapshots_new RENAME TO edit_file_snapshots;
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
    ON items(thread_id, payload_id) WHERE payload_id IS NOT NULL;

CREATE INDEX idx_items_meta_task_id
    ON items(thread_id, json_extract(meta, '$.task_id'))
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;

CREATE INDEX idx_items_input_payload_id
    ON items(thread_id, input_payload_id) WHERE input_payload_id IS NOT NULL;

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

CREATE INDEX idx_items_running_bg_tool_calls
    ON items(thread_id, id)
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 1
   AND COALESCE(json_extract(meta, '$.live_background_active'), 1) != 0;

CREATE INDEX idx_items_running_fg_tool_calls
    ON items(thread_id)
 WHERE kind = 'tool_call'
   AND status = 'running'
   AND is_background = 0
   AND parent_id = '';

CREATE INDEX idx_payload_chunks_payload_start
  ON payload_chunks(thread_id, payload_id, start_offset);

CREATE TRIGGER trg_items_gc_input_payload
AFTER DELETE ON items
WHEN OLD.input_payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE thread_id = OLD.thread_id
       AND id = OLD.input_payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND payload_id = OLD.input_payload_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND input_payload_id = OLD.input_payload_id
       );
END;

CREATE TRIGGER trg_items_gc_payload
AFTER DELETE ON items
WHEN OLD.payload_id IS NOT NULL
BEGIN
    DELETE FROM payloads
     WHERE thread_id = OLD.thread_id
       AND id = OLD.payload_id
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND payload_id = OLD.payload_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM items
            WHERE thread_id = OLD.thread_id
              AND input_payload_id = OLD.payload_id
       );
END;

` + historyRevTriggersLegacySQL
