package store

// sharedImportHistoryMigrationSQL adds the immutable half of the timeline
// store. Provider imports can materialize the same Claude prefix in many
// logical threads; storing those rows once in content-addressed chunks keeps
// the observable thread history unchanged without making live history
// mutable through aliases.
//
// The existing items/payloads tables remain the mutable, thread-owned overlay.
// timeline_items and timeline_payloads define the logical read semantics: a
// local override explicitly hides its imported item, and a local payload
// shadows an imported payload of the same logical id. Hot item hydration may
// implement those semantics with indexed physical-branch probes rather than
// joining the compound views. The overlap triggers make a thread with two
// conflicting imported chunks unconstructible even for a future raw SQL
// caller.
const sharedImportHistoryMigrationSQL = `
CREATE TABLE import_history_chunks (
    id             TEXT    PRIMARY KEY,
    item_count     INTEGER NOT NULL CHECK(item_count > 0),
    min_turn_index INTEGER NOT NULL,
    max_turn_index INTEGER NOT NULL,
    CHECK(min_turn_index <= max_turn_index)
);

CREATE TABLE import_history_payloads (
    chunk_id      TEXT    NOT NULL REFERENCES import_history_chunks(id) ON DELETE CASCADE,
    id            TEXT    NOT NULL,
    kind          TEXT    NOT NULL,
    meta          TEXT    NOT NULL DEFAULT '{}',
    data          BLOB    NOT NULL,
    created_at    INTEGER NOT NULL,
    preview_spans TEXT    NOT NULL DEFAULT '',
    spans         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (chunk_id, id)
);

CREATE TABLE import_history_items (
    chunk_id            TEXT    NOT NULL REFERENCES import_history_chunks(id) ON DELETE CASCADE,
    id                  TEXT    NOT NULL,
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
    PRIMARY KEY (chunk_id, id),
    UNIQUE (chunk_id, turn_index, item_index),
    FOREIGN KEY (chunk_id, payload_id)
        REFERENCES import_history_payloads(chunk_id, id),
    FOREIGN KEY (chunk_id, input_payload_id)
        REFERENCES import_history_payloads(chunk_id, id)
);

CREATE TABLE thread_import_chunks (
    thread_id   TEXT    NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    chunk_order INTEGER NOT NULL CHECK(chunk_order >= 0),
    chunk_id    TEXT    NOT NULL REFERENCES import_history_chunks(id) ON DELETE RESTRICT,
    PRIMARY KEY (thread_id, chunk_order),
    UNIQUE (thread_id, chunk_id)
);

CREATE INDEX idx_thread_import_chunks_chunk
    ON thread_import_chunks(chunk_id, thread_id);

CREATE INDEX idx_import_history_items_timeline
    ON import_history_items(chunk_id, turn_index, item_index);

CREATE INDEX idx_import_history_items_parent
    ON import_history_items(chunk_id, parent_id) WHERE parent_id <> '';

CREATE INDEX idx_import_history_items_completion
    ON import_history_items(chunk_id, completion_of) WHERE completion_of <> '';

CREATE INDEX idx_import_history_items_task
    ON import_history_items(chunk_id, json_extract(meta, '$.task_id'))
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;

CREATE TABLE thread_import_item_overrides (
    thread_id TEXT NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    item_id   TEXT NOT NULL,
    PRIMARY KEY (thread_id, item_id)
);

CREATE TRIGGER trg_thread_import_chunks_order
BEFORE INSERT ON thread_import_chunks
WHEN NEW.chunk_order <> COALESCE((
    SELECT MAX(chunk_order) + 1
      FROM thread_import_chunks
     WHERE thread_id = NEW.thread_id
), 0)
BEGIN
    SELECT RAISE(ABORT, 'import history chunks must attach in contiguous order');
END;

CREATE TRIGGER trg_thread_import_chunks_turn_overlap
BEFORE INSERT ON thread_import_chunks
WHEN (
    SELECT min_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id
) <= MAX(
    COALESCE((SELECT MAX(existing.max_turn_index)
       FROM thread_import_chunks refs
       JOIN import_history_chunks existing ON existing.id = refs.chunk_id
      WHERE refs.thread_id = NEW.thread_id), -1),
    COALESCE((SELECT MAX(turn_index) FROM items WHERE thread_id = NEW.thread_id), -1)
)
AND (
    EXISTS (
        SELECT 1
          FROM import_history_items incoming
          JOIN thread_import_chunks existing_ref
            ON existing_ref.thread_id = NEW.thread_id
          JOIN import_history_items existing
            ON existing.chunk_id = existing_ref.chunk_id
         WHERE incoming.chunk_id = NEW.chunk_id
           AND (incoming.id = existing.id OR
                (incoming.turn_index = existing.turn_index AND incoming.item_index = existing.item_index))
    )
    OR EXISTS (
        SELECT 1
          FROM import_history_items incoming
          JOIN items local ON local.thread_id = NEW.thread_id
         WHERE incoming.chunk_id = NEW.chunk_id
           AND (incoming.id = local.id OR
                (incoming.turn_index = local.turn_index AND incoming.item_index = local.item_index))
    )
)
BEGIN
    SELECT RAISE(ABORT, 'import history chunks overlap an existing item');
END;

CREATE TRIGGER trg_thread_import_chunks_payload_overlap
BEFORE INSERT ON thread_import_chunks
WHEN (
    SELECT min_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id
) <= MAX(
    COALESCE((SELECT MAX(existing.max_turn_index)
       FROM thread_import_chunks refs
       JOIN import_history_chunks existing ON existing.id = refs.chunk_id
      WHERE refs.thread_id = NEW.thread_id), -1),
    COALESCE((SELECT MAX(turn_index) FROM items WHERE thread_id = NEW.thread_id), -1)
)
AND (EXISTS (
    SELECT 1
      FROM import_history_payloads incoming
      JOIN thread_import_chunks existing_ref
        ON existing_ref.thread_id = NEW.thread_id
      JOIN import_history_payloads existing
        ON existing.chunk_id = existing_ref.chunk_id AND existing.id = incoming.id
     WHERE incoming.chunk_id = NEW.chunk_id
)
OR EXISTS (
    SELECT 1
      FROM import_history_payloads incoming
      JOIN payloads local ON local.thread_id = NEW.thread_id AND local.id = incoming.id
     WHERE incoming.chunk_id = NEW.chunk_id
))
BEGIN
    SELECT RAISE(ABORT, 'import history chunks overlap an existing payload');
END;

CREATE TRIGGER trg_thread_import_chunks_gc
AFTER DELETE ON thread_import_chunks
BEGIN
    DELETE FROM import_history_chunks
     WHERE id = OLD.chunk_id
       AND NOT EXISTS (
           SELECT 1 FROM thread_import_chunks WHERE chunk_id = OLD.chunk_id
       );
END;

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

CREATE TRIGGER trg_items_reject_import_position_collision
BEFORE INSERT ON items
WHEN EXISTS (
    SELECT 1
      FROM thread_import_chunks refs
      JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
      LEFT JOIN thread_import_item_overrides overrides
        ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
     WHERE refs.thread_id = NEW.thread_id
       AND imported.turn_index = NEW.turn_index
       AND imported.item_index = NEW.item_index
       AND imported.id <> NEW.id
       AND overrides.item_id IS NULL
)
BEGIN
    SELECT RAISE(ABORT, 'local item collides with an imported timeline position');
END;

CREATE TRIGGER trg_items_reject_import_position_update
BEFORE UPDATE OF thread_id, turn_index, item_index ON items
WHEN EXISTS (
    SELECT 1
      FROM thread_import_chunks refs
      JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
      LEFT JOIN thread_import_item_overrides overrides
        ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
     WHERE refs.thread_id = NEW.thread_id
       AND imported.turn_index = NEW.turn_index
       AND imported.item_index = NEW.item_index
       AND imported.id <> NEW.id
       AND overrides.item_id IS NULL
)
BEGIN
    SELECT RAISE(ABORT, 'local item update collides with an imported timeline position');
END;

CREATE VIEW timeline_payloads AS
SELECT
    payloads.thread_id,
    payloads.id,
    payloads.kind,
    payloads.meta,
    payloads.data,
    payloads.created_at,
    payloads.preview_spans,
    payloads.spans
  FROM payloads
UNION ALL
SELECT
    refs.thread_id,
    imported.id,
    imported.kind,
    imported.meta,
    imported.data,
    imported.created_at,
    imported.preview_spans,
    imported.spans
  FROM thread_import_chunks refs
  JOIN import_history_payloads imported ON imported.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (
     SELECT 1 FROM payloads local
      WHERE local.thread_id = refs.thread_id AND local.id = imported.id
 );

CREATE VIEW timeline_items AS
SELECT
    items.id,
    items.thread_id,
    items.turn_index,
    items.item_index,
    items.kind,
    items.role,
    items.status,
    items.summary,
    items.payload_id,
    items.parent_id,
    items.is_background,
    items.completion_of,
    items.tool_name,
    items.decision,
    items.meta,
    items.created_at,
    items.updated_at,
    items.input_payload_id
  FROM items
UNION ALL
SELECT
    imported.id,
    refs.thread_id,
    imported.turn_index,
    imported.item_index,
    imported.kind,
    imported.role,
    imported.status,
    imported.summary,
    imported.payload_id,
    imported.parent_id,
    imported.is_background,
    imported.completion_of,
    imported.tool_name,
    imported.decision,
    imported.meta,
    imported.created_at,
    imported.updated_at,
    imported.input_payload_id
  FROM thread_import_chunks refs
  JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (
     SELECT 1 FROM thread_import_item_overrides overrides
      WHERE overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
 );
`
