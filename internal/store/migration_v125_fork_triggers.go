package store

// forkGuardTriggersV125SQL is the pointer-fork guard trigger DDL v125 and
// v127 installed, frozen with them. forkGuardTriggersSQL (fork_triggers.go)
// is the latest.
const forkGuardTriggersV125SQL = `
CREATE TRIGGER trg_threads_fork_source_delete BEFORE DELETE ON threads
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.id)
BEGIN
  SELECT RAISE(ABORT, 'pointer forks read this thread; it is kept as a holder, not deleted');
END;

CREATE TRIGGER trg_items_fork_position BEFORE INSERT ON items
WHEN EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.thread_id = NEW.thread_id AND l.depth = 1
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
) AND NOT EXISTS (
  SELECT 1 FROM thread_fork_hidden WHERE thread_id = NEW.thread_id AND item_id = NEW.id
)
BEGIN
  SELECT RAISE(ABORT, 'item position precedes the fork cut');
END;

CREATE TRIGGER trg_items_fork_position_update BEFORE UPDATE OF turn_index, item_index ON items
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index)
 AND EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.thread_id = NEW.thread_id AND l.depth = 1
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
)
BEGIN
  SELECT RAISE(ABORT, 'item position precedes the fork cut');
END;

CREATE TRIGGER trg_items_fork_snapshot AFTER INSERT ON items
WHEN EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
) AND NOT EXISTS (
  SELECT 1 FROM thread_fork_hidden WHERE thread_id = NEW.thread_id AND item_id = NEW.id
) AND NOT EXISTS (
  SELECT 1 FROM thread_import_item_overrides WHERE thread_id = NEW.thread_id AND item_id = NEW.id
)
BEGIN
  INSERT OR IGNORE INTO thread_fork_hidden(thread_id, item_id)
  SELECT l.thread_id, NEW.id
    FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden h ON h.thread_id = nearer.ancestor_id AND h.item_id = NEW.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth);
END;

CREATE TRIGGER trg_items_fork_snapshot_move AFTER UPDATE OF turn_index, item_index ON items
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index)
 AND EXISTS (
  SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
     AND (OLD.turn_index, OLD.item_index) >= (l.cut_turn_index, l.cut_item_index)
)
BEGIN
  INSERT OR IGNORE INTO thread_fork_hidden(thread_id, item_id)
  SELECT l.thread_id, NEW.id
    FROM thread_fork_lineage l
   WHERE l.ancestor_id = NEW.thread_id
     AND (NEW.turn_index, NEW.item_index) < (l.cut_turn_index, l.cut_item_index)
     AND (OLD.turn_index, OLD.item_index) >= (l.cut_turn_index, l.cut_item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden h ON h.thread_id = nearer.ancestor_id AND h.item_id = NEW.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth);
END;

CREATE TRIGGER trg_items_shown_update BEFORE UPDATE OF id, summary, status, meta, kind, role, turn_index, item_index,
    payload_id, input_payload_id, parent_id, is_background, completion_of, tool_name, decision,
    created_at, updated_at ON items
WHEN (OLD.id IS NOT NEW.id OR OLD.summary IS NOT NEW.summary OR OLD.status IS NOT NEW.status
      OR OLD.meta IS NOT NEW.meta OR OLD.kind IS NOT NEW.kind OR OLD.role IS NOT NEW.role
      OR OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index
      OR OLD.payload_id IS NOT NEW.payload_id OR OLD.input_payload_id IS NOT NEW.input_payload_id
      OR OLD.parent_id IS NOT NEW.parent_id OR OLD.is_background IS NOT NEW.is_background
      OR OLD.completion_of IS NOT NEW.completion_of
      OR OLD.tool_name IS NOT NEW.tool_name OR OLD.decision IS NOT NEW.decision
      OR OLD.created_at IS NOT NEW.created_at OR OLD.updated_at IS NOT NEW.updated_at)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (OLD.turn_index, OLD.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = OLD.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = OLD.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_items_shown_delete BEFORE DELETE ON items
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (OLD.turn_index, OLD.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = OLD.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = OLD.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_payloads_shown_update BEFORE UPDATE OF data, meta ON payloads
WHEN (OLD.data IS NOT NEW.data OR OLD.meta IS NOT NEW.meta)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.thread_id)
 AND (EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.payload_id = OLD.id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.input_payload_id = OLD.id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
                CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
                CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
               WHERE ref_payload.id = OLD.id AND refs.thread_id = OLD.thread_id
                 AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
                 AND NOT EXISTS (
		       SELECT 1 FROM thread_import_item_overrides o
		        WHERE o.thread_id = refs.thread_id AND o.item_id = items.id
		   )
                 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = refs.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (items.turn_index, items.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = items.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = items.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_payload_chunks_shown_insert BEFORE INSERT ON payload_chunks
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = NEW.thread_id)
 AND (EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = NEW.thread_id AND ref.payload_id = NEW.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = NEW.thread_id AND ref.input_payload_id = NEW.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
                CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
                CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
               WHERE ref_payload.id = NEW.payload_id AND refs.thread_id = NEW.thread_id
                 AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
                 AND NOT EXISTS (
		       SELECT 1 FROM thread_import_item_overrides o
		        WHERE o.thread_id = refs.thread_id AND o.item_id = items.id
		   )
                 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = refs.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (items.turn_index, items.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = items.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = items.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_payload_chunks_shown_update BEFORE UPDATE OF chunk_index, start_offset, data ON payload_chunks
WHEN (OLD.chunk_index IS NOT NEW.chunk_index OR OLD.start_offset IS NOT NEW.start_offset OR OLD.data IS NOT NEW.data)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.thread_id)
 AND (EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.payload_id = OLD.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.input_payload_id = OLD.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
                CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
                CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
               WHERE ref_payload.id = OLD.payload_id AND refs.thread_id = OLD.thread_id
                 AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
                 AND NOT EXISTS (
		       SELECT 1 FROM thread_import_item_overrides o
		        WHERE o.thread_id = refs.thread_id AND o.item_id = items.id
		   )
                 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = refs.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (items.turn_index, items.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = items.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = items.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_payload_chunks_shown_delete BEFORE DELETE ON payload_chunks
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.thread_id)
 AND (EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.payload_id = OLD.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = OLD.thread_id AND ref.input_payload_id = OLD.payload_id
                AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ref.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (ref.turn_index, ref.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ref.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ref.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth)))
   OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
                CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
                CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
               WHERE ref_payload.id = OLD.payload_id AND refs.thread_id = OLD.thread_id
                 AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
                 AND NOT EXISTS (
		       SELECT 1 FROM thread_import_item_overrides o
		        WHERE o.thread_id = refs.thread_id AND o.item_id = items.id
		   )
                 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = refs.thread_id
     AND (l.cut_turn_index, l.cut_item_index) > (items.turn_index, items.item_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = items.id)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = items.id
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_turns_shown_update BEFORE UPDATE OF turn_index, started_at, completed_at, stop_reason,
    assistant_message_id, token_usage_json, error_message, provider_turn_id ON turns
WHEN (OLD.turn_index IS NOT NEW.turn_index OR OLD.started_at IS NOT NEW.started_at
      OR OLD.completed_at IS NOT NEW.completed_at OR OLD.stop_reason IS NOT NEW.stop_reason
      OR OLD.assistant_message_id IS NOT NEW.assistant_message_id
      OR OLD.token_usage_json IS NOT NEW.token_usage_json OR OLD.error_message IS NOT NEW.error_message
      OR OLD.provider_turn_id IS NOT NEW.provider_turn_id)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id AND l.cut_turn_index > OLD.turn_index
     AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = OLD.turn_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = OLD.turn_index
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

CREATE TRIGGER trg_turns_shown_delete BEFORE DELETE ON turns
WHEN EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = OLD.thread_id AND l.cut_turn_index > OLD.turn_index
     AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = OLD.turn_index)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = OLD.turn_index
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))
BEGIN
  SELECT RAISE(ABORT, 'history another thread shows is immutable');
END;

`
