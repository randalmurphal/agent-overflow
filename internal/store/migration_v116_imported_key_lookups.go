package store

// A thread reaches shared import history through its chunk references, so
// a lookup that starts from the thread probes every chunk it references.
// v116 lets lookups start from their key instead: key-first indexes for the
// imported keys a point lookup pins, and each reference carries its chunk's
// turn range so a turn lookup ranges over the references that can hold the
// turn. trg_thread_import_chunks_turn_range fills the range on attach;
// restore copies it verbatim.
//
// Turn reads skip a chunk whose range excludes the turn, so the range must
// bound the chunk's rows. The migration recomputes every chunk's range from
// its rows before copying it, and trg_import_history_items_turn_range
// rejects a row outside its chunk's range.
const importedKeyLookupsV116SQL = `
UPDATE import_history_chunks
   SET min_turn_index = (SELECT MIN(turn_index) FROM import_history_items WHERE chunk_id = import_history_chunks.id),
       max_turn_index = (SELECT MAX(turn_index) FROM import_history_items WHERE chunk_id = import_history_chunks.id)
 WHERE min_turn_index <> (SELECT MIN(turn_index) FROM import_history_items WHERE chunk_id = import_history_chunks.id)
    OR max_turn_index <> (SELECT MAX(turn_index) FROM import_history_items WHERE chunk_id = import_history_chunks.id);
CREATE TRIGGER trg_import_history_items_turn_range BEFORE INSERT ON import_history_items
WHEN NEW.turn_index < (SELECT min_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id)
  OR NEW.turn_index > (SELECT max_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id)
BEGIN
 SELECT RAISE(ABORT, 'imported item lies outside its chunk turn range');
END;
ALTER TABLE thread_import_chunks ADD COLUMN min_turn_index INTEGER;
ALTER TABLE thread_import_chunks ADD COLUMN max_turn_index INTEGER;
UPDATE thread_import_chunks
   SET min_turn_index = (SELECT min_turn_index FROM import_history_chunks WHERE id = thread_import_chunks.chunk_id),
       max_turn_index = (SELECT max_turn_index FROM import_history_chunks WHERE id = thread_import_chunks.chunk_id);
CREATE INDEX idx_thread_import_chunks_turns
 ON thread_import_chunks(thread_id, max_turn_index, min_turn_index, chunk_id);
CREATE TRIGGER trg_thread_import_chunks_turn_range AFTER INSERT ON thread_import_chunks
WHEN NEW.min_turn_index IS NULL OR NEW.max_turn_index IS NULL
BEGIN
 UPDATE thread_import_chunks
    SET min_turn_index = (SELECT min_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id),
        max_turn_index = (SELECT max_turn_index FROM import_history_chunks WHERE id = NEW.chunk_id)
  WHERE thread_id = NEW.thread_id AND chunk_order = NEW.chunk_order;
END;

CREATE INDEX idx_import_history_items_completion_lookup
 ON import_history_items(completion_of, chunk_id) WHERE completion_of <> '';
CREATE INDEX idx_import_history_items_task_lookup
 ON import_history_items(json_extract(meta, '$.task_id'), chunk_id)
 WHERE json_extract(meta, '$.task_id') IS NOT NULL;
CREATE INDEX idx_import_history_items_scope_root_lookup
 ON import_history_items(CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END, chunk_id)
 WHERE kind = 'tool_call' AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END IS NOT NULL;

DROP TRIGGER trg_items_require_import_override;
CREATE TRIGGER trg_items_require_import_override
BEFORE INSERT ON items
WHEN EXISTS (
    SELECT 1
      FROM import_history_items imported
      CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id
     WHERE imported.id = NEW.id
       AND refs.thread_id = NEW.thread_id
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

DROP TRIGGER trg_items_reject_import_position_collision;
CREATE TRIGGER trg_items_reject_import_position_collision
BEFORE INSERT ON items
WHEN EXISTS (
    SELECT 1
      FROM thread_import_chunks refs
      CROSS JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
      LEFT JOIN thread_import_item_overrides overrides
        ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
     WHERE refs.thread_id = NEW.thread_id
       AND refs.max_turn_index >= NEW.turn_index
       AND refs.min_turn_index <= NEW.turn_index
       AND imported.turn_index = NEW.turn_index
       AND imported.item_index = NEW.item_index
       AND imported.id <> NEW.id
       AND overrides.item_id IS NULL
)
BEGIN
    SELECT RAISE(ABORT, 'local item collides with an imported timeline position');
END;

DROP TRIGGER trg_items_reject_import_position_update;
CREATE TRIGGER trg_items_reject_import_position_update
BEFORE UPDATE OF thread_id, turn_index, item_index ON items
WHEN EXISTS (
    SELECT 1
      FROM thread_import_chunks refs
      CROSS JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
      LEFT JOIN thread_import_item_overrides overrides
        ON overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
     WHERE refs.thread_id = NEW.thread_id
       AND refs.max_turn_index >= NEW.turn_index
       AND refs.min_turn_index <= NEW.turn_index
       AND imported.turn_index = NEW.turn_index
       AND imported.item_index = NEW.item_index
       AND imported.id <> NEW.id
       AND overrides.item_id IS NULL
)
BEGIN
    SELECT RAISE(ABORT, 'local item update collides with an imported timeline position');
END;

` + dropSharedChunkAdmissionTriggersSQL + sharedChunkAdmissionTriggersV116SQL

// sharedChunkAdmissionTriggersV116SQL keeps v108's admission contract (an
// attached chunk may not repeat an id or a timeline position the thread
// already has) and probes from the incoming chunk: ids through
// idx_import_history_items_id, positions only in the references whose turn
// range overlaps the incoming chunk. Attaching a chunk past the thread's
// history probes no existing chunk. RestoreFrom reinstalls it.
const sharedChunkAdmissionTriggersV116SQL = `
CREATE TRIGGER trg_thread_import_chunks_order BEFORE INSERT ON thread_import_chunks
WHEN NEW.chunk_order <> COALESCE((SELECT MAX(chunk_order)+1 FROM thread_import_chunks WHERE thread_id=NEW.thread_id),0)
BEGIN SELECT RAISE(ABORT,'import history chunks must attach in contiguous order'); END;
CREATE TRIGGER trg_thread_import_chunks_turn_overlap BEFORE INSERT ON thread_import_chunks
WHEN EXISTS (
 SELECT 1 FROM import_history_items incoming
 CROSS JOIN import_history_items existing ON existing.id=incoming.id
 CROSS JOIN thread_import_chunks refs ON refs.chunk_id=existing.chunk_id AND refs.thread_id=NEW.thread_id
 WHERE incoming.chunk_id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_chunks incoming
 CROSS JOIN thread_import_chunks refs ON refs.thread_id=NEW.thread_id
   AND refs.max_turn_index>=incoming.min_turn_index AND refs.min_turn_index<=incoming.max_turn_index
 CROSS JOIN import_history_items i ON i.chunk_id=incoming.id
 CROSS JOIN import_history_items e ON e.chunk_id=refs.chunk_id AND e.turn_index=i.turn_index AND e.item_index=i.item_index
 WHERE incoming.id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_items incoming JOIN items local ON local.thread_id=NEW.thread_id AND local.id=incoming.id
 WHERE incoming.chunk_id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_items incoming JOIN items local ON local.thread_id=NEW.thread_id
   AND local.turn_index=incoming.turn_index AND local.item_index=incoming.item_index
 WHERE incoming.chunk_id=NEW.chunk_id
)
BEGIN SELECT RAISE(ABORT,'import history chunks overlap an existing item'); END;

CREATE TRIGGER trg_thread_import_chunks_payload_overlap BEFORE INSERT ON thread_import_chunks
WHEN EXISTS (
 SELECT 1 FROM import_history_payloads incoming
 CROSS JOIN import_history_payloads existing ON existing.id=incoming.id
 CROSS JOIN thread_import_chunks refs ON refs.chunk_id=existing.chunk_id AND refs.thread_id=NEW.thread_id
 WHERE incoming.chunk_id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_payloads incoming JOIN payloads local ON local.thread_id=NEW.thread_id AND local.id=incoming.id
 WHERE incoming.chunk_id=NEW.chunk_id
)
BEGIN SELECT RAISE(ABORT,'import history chunks overlap an existing payload'); END;
`
