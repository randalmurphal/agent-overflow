package store

// Descendant walks discover children before checking thread membership. The
// chunk-first parent index still serves direct scoped pages within a chunk.
const importedParentLookupV115SQL = `
CREATE INDEX idx_import_history_items_parent_lookup
 ON import_history_items(parent_id,chunk_id) WHERE parent_id <> '';
`
