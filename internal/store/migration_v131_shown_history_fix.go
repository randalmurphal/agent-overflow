package store

// Migration v131 lets a migration's one-time data fix reach every copy of
// the data it fixes. The pointer-fork guards of payload content stand down
// while shown_history_fix holds a row, which a data fix writes and deletes
// inside its own transaction (fixShownHistoryTx), so the fix empties a
// payload a fork shows and a holder's as it does the source's.

const shownHistoryFixV131SQL = `
CREATE TABLE shown_history_fix (
  id INTEGER PRIMARY KEY CHECK (id = 1)
);

DROP TRIGGER trg_payloads_shown_update;
DROP TRIGGER trg_payload_chunks_shown_insert;
DROP TRIGGER trg_payload_chunks_shown_update;
DROP TRIGGER trg_payload_chunks_shown_delete;

CREATE TRIGGER trg_payloads_shown_update BEFORE UPDATE OF data, meta ON payloads
WHEN (OLD.data IS NOT NEW.data OR OLD.meta IS NOT NEW.meta)
 AND NOT EXISTS (SELECT 1 FROM shown_history_fix)
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
WHEN NOT EXISTS (SELECT 1 FROM shown_history_fix)
 AND EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = NEW.thread_id)
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
 AND NOT EXISTS (SELECT 1 FROM shown_history_fix)
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
WHEN NOT EXISTS (SELECT 1 FROM shown_history_fix)
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
`
