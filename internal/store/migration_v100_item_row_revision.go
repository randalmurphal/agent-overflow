package store

// Per-row history revisions (docs/architecture/thread-replica-sync.md §3.1).
//
// `items.rev` carries the thread's history_rev as of the last write that
// changed what a read of that row returns, so a client can describe the
// window it already holds as (id, rev) pairs and get a page-less `fresh`
// without holding an attested thread stamp.
//
// Four parts, all of which must land together:
//
//   - the column, defaulting to 0. Existing rows are NOT backfilled: 0 is
//     the honest "not stamped since the column existed", and it is safe
//     because any later write to such a row stamps it with a value greater
//     than zero. A backfill would rewrite every item row in a multi-GB
//     history to change no answer.
//   - the partial expression index the stamp's carrier leg probes
//     (idx_items_transcript_root). transcriptRootExpr is its expression
//     and the trigger's, so the planner can serve one from the other.
//   - the three history triggers, replayed drop-then-create, because their
//     bodies now stamp rows as well as the thread. historyRevTriggersSQL is
//     the single source RestoreFrom reinstalls from, so this migration and
//     a restored database cannot end up with different trigger text.
//   - the `timeline_items` view, recreated with the column. The view is the
//     logical row set, and `rev` is now part of a logical row; the imported
//     arm reads -1 because shared immutable chunks have no thread-scoped
//     place to stamp (importedItemRevExpr).
var itemRowRevisionV100SQL = `
ALTER TABLE items ADD COLUMN rev INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_items_transcript_root
    ON items(thread_id, ` + transcriptRootExpr + `)
 WHERE ` + transcriptRootExpr + ` IS NOT NULL;

` + dropHistoryRevTriggersSQL + historyRevTriggersSQL + `

DROP VIEW timeline_items;

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
    items.input_payload_id,
    items.rev
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
    imported.input_payload_id,
    ` + importedItemRevExpr + `
  FROM thread_import_chunks refs
  JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
 WHERE NOT EXISTS (
     SELECT 1 FROM thread_import_item_overrides overrides
      WHERE overrides.thread_id = refs.thread_id AND overrides.item_id = imported.id
 );
`
