package store

// Prepared chunks may divide a turn. Collision checks use independent ID
// and position probes instead of joining every pair of rows in that turn.
const historyPreparationV108SQL = `
CREATE INDEX idx_items_history_preparation ON items(thread_id,turn_index,item_index)
 WHERE status NOT IN ('running','streaming')
 AND kind NOT IN ('user_text','tool_call','tool_completion','workflow_proposal')
 AND is_background=0 AND completion_of='';
CREATE INDEX idx_import_history_items_id ON import_history_items(id,chunk_id);
CREATE INDEX idx_import_history_payloads_id ON import_history_payloads(id,chunk_id);

` + dropSharedChunkAdmissionTriggersSQL + sharedChunkAdmissionTriggersSQL

const dropSharedChunkAdmissionTriggersSQL = `DROP TRIGGER IF EXISTS trg_thread_import_chunks_turn_overlap;
DROP TRIGGER IF EXISTS trg_thread_import_chunks_payload_overlap;
DROP TRIGGER IF EXISTS trg_thread_import_chunks_order;`

const sharedChunkAdmissionTriggersSQL = `
CREATE TRIGGER trg_thread_import_chunks_order BEFORE INSERT ON thread_import_chunks
WHEN NEW.chunk_order <> COALESCE((SELECT MAX(chunk_order)+1 FROM thread_import_chunks WHERE thread_id=NEW.thread_id),0)
BEGIN SELECT RAISE(ABORT,'import history chunks must attach in contiguous order'); END;
CREATE TRIGGER trg_thread_import_chunks_turn_overlap BEFORE INSERT ON thread_import_chunks
WHEN EXISTS (
 SELECT 1 FROM import_history_items incoming
 JOIN import_history_items existing ON existing.id=incoming.id
 JOIN thread_import_chunks refs ON refs.chunk_id=existing.chunk_id AND refs.thread_id=NEW.thread_id
 WHERE incoming.chunk_id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_chunks incoming
 JOIN thread_import_chunks refs ON refs.thread_id=NEW.thread_id
 JOIN import_history_chunks existing ON existing.id=refs.chunk_id
   AND existing.min_turn_index<=incoming.max_turn_index AND existing.max_turn_index>=incoming.min_turn_index
 JOIN import_history_items i ON i.chunk_id=incoming.id
 JOIN import_history_items e ON e.chunk_id=existing.id AND e.turn_index=i.turn_index AND e.item_index=i.item_index
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
 JOIN import_history_payloads existing ON existing.id=incoming.id
 JOIN thread_import_chunks refs ON refs.chunk_id=existing.chunk_id AND refs.thread_id=NEW.thread_id
 WHERE incoming.chunk_id=NEW.chunk_id
) OR EXISTS (
 SELECT 1 FROM import_history_payloads incoming JOIN payloads local ON local.thread_id=NEW.thread_id AND local.id=incoming.id
 WHERE incoming.chunk_id=NEW.chunk_id
)
BEGIN SELECT RAISE(ABORT,'import history chunks overlap an existing payload'); END;
`
