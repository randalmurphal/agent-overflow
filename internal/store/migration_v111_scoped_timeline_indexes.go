package store

const scopedTimelineIndexesV111SQL = `
DROP INDEX idx_items_parent;
CREATE INDEX idx_items_parent
 ON items(thread_id, parent_id, turn_index, item_index) WHERE parent_id <> '';
DROP INDEX idx_import_history_items_parent;
CREATE INDEX idx_import_history_items_parent
 ON import_history_items(chunk_id, parent_id, turn_index, item_index) WHERE parent_id <> '';

CREATE INDEX idx_items_scope_lifecycle
 ON items(thread_id, CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END, turn_index, item_index)
 WHERE kind = 'tool_call' AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END IS NOT NULL;
CREATE INDEX idx_import_history_items_scope_lifecycle
 ON import_history_items(chunk_id, CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END, turn_index, item_index)
 WHERE kind = 'tool_call' AND CASE WHEN json_valid(meta) THEN json_extract(meta, '$.transcript_root_id') END IS NOT NULL;
`
