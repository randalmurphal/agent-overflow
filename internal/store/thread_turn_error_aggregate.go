package store

import (
	"database/sql"
	"fmt"
)

// A thread's Failed pill is lit by its turn errors: the visible `error`
// rows (both timeline arms) whose turn_index is at or after the newest turn
// row, or 0 before the first turn. An orphan error has no turn row of its
// own, so rows past the newest turn count too.
//
// The thread row carries two aggregates of that row set, maintained by the
// triggers below so a thread-row read costs no timeline probe:
//
//   - newest_turn_error_at: MAX(created_at). threadColumns compares it with
//     last_read_at for hasFailedTurn and MarkThreadReadNow clamps its read
//     stamp to it, so the pill and the read that clears it cannot disagree.
//   - newest_turn_error_turn: MAX(turn_index). A new newest turn past it
//     empties the set without a recompute, which is the common case: the
//     turn after a failed one.
//
// Both are NULL when the set is empty. A write that adds rows raises the
// pair with them. A write that can remove a counted row recomputes it: one
// idx_items_thread_error probe, plus one idx_import_history_items_error
// probe per attached chunk whose turn range reaches the newest turn, found
// through idx_thread_import_chunks_turns. That happens only when a counted
// row leaves the set or the newest turn moves back: an error row's delete
// or key change, a turn delete, a counted imported row hidden by an
// override, a chunk with a counted row detached. A new newest turn
// recomputes only when an error already sits at or past it.
//
// The triggers do not consult history_bulk_load: none of them walks the
// thread, and the bulk paths (imports, thread deletion, transferred
// history) move exactly the rows these aggregates count.
//
// A pointer fork also counts the error rows it reads through its lineage,
// which sit at or before its cut turn. No trigger sees a change to that set
// (the fork's creation, a revert of inherited rows, a hidden row, a detached
// source); each such writer recomputes the pair with the lineage arms
// (recomputeTurnErrorsTx). A copy of an inherited row changes no set: the
// insert raises the pair with a row it already counts.
//
// thread_import_chunks rows are detached before an unreferenced chunk is
// collected (trg_thread_import_chunks_gc), and SQLite does not order
// triggers, so the detach trigger runs BEFORE the delete while the chunk's
// rows still exist and recomputes without that chunk.

// turnErrorNewestTurnSQL is the turn the set is measured from.
func turnErrorNewestTurnSQL(thread string) string {
	return `COALESCE((SELECT MAX(turns.turn_index) FROM turns WHERE turns.thread_id = ` + thread + `), 0)`
}

// turnErrorNotOverriddenSQL keeps an imported row that a local overlay row
// replaces out of the imported arm, as the timeline_items view does.
func turnErrorNotOverriddenSQL(thread string) string {
	return `NOT EXISTS (SELECT 1 FROM thread_import_item_overrides overrides
	              WHERE overrides.thread_id = ` + thread + ` AND overrides.item_id = imported.id)`
}

// turnErrorRowsSQL selects (created_at, turn_index) of the rows in thread's
// set. skipChunk, when set, names an attached chunk to leave out.
func turnErrorRowsSQL(thread, skipChunk string) string {
	skip := ""
	if skipChunk != "" {
		skip = `
	   AND refs.chunk_id <> ` + skipChunk
	}
	return `SELECT errors.created_at, errors.turn_index
	  FROM items errors
	 WHERE errors.thread_id = ` + thread + ` AND errors.kind = 'error'
	   AND errors.turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	UNION ALL
	SELECT imported.created_at, imported.turn_index
	  FROM thread_import_chunks refs
	  CROSS JOIN import_history_items imported ON imported.chunk_id = refs.chunk_id
	 WHERE refs.thread_id = ` + thread + skip + `
	   AND refs.max_turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND imported.kind = 'error'
	   AND imported.turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND ` + turnErrorNotOverriddenSQL(thread)
}

// turnErrorLineageRowsSQL is turnErrorRowsSQL's arms for the rows a pointer
// fork reads from its ancestors, under the timeline arms' visibility rule.
// A thread without lineage rows probes them once.
func turnErrorLineageRowsSQL(thread string) string {
	return `SELECT items.created_at, items.turn_index
	  FROM thread_fork_lineage l
	  CROSS JOIN items ON items.thread_id = l.ancestor_id
	 WHERE l.thread_id = ` + thread + ` AND items.kind = 'error'
	   AND items.turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND ` + inheritedItemVisibleSQL + `
	UNION ALL
	SELECT items.created_at, items.turn_index
	  FROM thread_fork_lineage l
	  CROSS JOIN thread_import_chunks refs ON refs.thread_id = l.ancestor_id
	  CROSS JOIN import_history_items items ON items.chunk_id = refs.chunk_id
	 WHERE l.thread_id = ` + thread + `
	   AND refs.max_turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND items.kind = 'error'
	   AND items.turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND NOT EXISTS (SELECT 1 FROM thread_import_item_overrides overrides
	              WHERE overrides.thread_id = l.ancestor_id AND overrides.item_id = items.id)
	   AND ` + inheritedItemVisibleSQL
}

// recomputeForkTurnErrorsSQL rewrites one thread's pair from its own rows
// and the rows it reads through its lineage.
var recomputeForkTurnErrorsSQL = `UPDATE threads
	   SET (newest_turn_error_at, newest_turn_error_turn) = (
	       SELECT MAX(created_at), MAX(turn_index) FROM (` + turnErrorRowsSQL("threads.id", "") + `
	UNION ALL
	` + turnErrorLineageRowsSQL("threads.id") + `))
	 WHERE id = ?`

// recomputeTurnErrorsTx rewrites threadID's pair, for a write that changes
// which rows a pointer fork reads through its lineage.
func recomputeTurnErrorsTx(tx *sql.Tx, threadID string) error {
	if _, err := tx.Exec(recomputeForkTurnErrorsSQL, threadID); err != nil {
		return fmt.Errorf("store: recompute turn errors of %s: %w", threadID, err)
	}
	return nil
}

// turnErrorRecomputeSQL rewrites the pair of the threads rows `where`
// selects from their rows.
func turnErrorRecomputeSQL(where, skipChunk string) string {
	return `UPDATE threads
	   SET (newest_turn_error_at, newest_turn_error_turn) = (
	       SELECT MAX(created_at), MAX(turn_index) FROM (` + turnErrorRowsSQL("threads.id", skipChunk) + `))
	 WHERE ` + where + `;`
}

// turnErrorRaiseSQL folds the rows `source` selects (created_at,
// turn_index, already restricted to the set) into thread's pair.
func turnErrorRaiseSQL(thread, source string) string {
	return `UPDATE threads
	   SET newest_turn_error_at = MAX(COALESCE(newest_turn_error_at, added.created_at), added.created_at),
	       newest_turn_error_turn = MAX(COALESCE(newest_turn_error_turn, added.turn_index), added.turn_index)
	  FROM (SELECT MAX(created_at) AS created_at, MAX(turn_index) AS turn_index FROM (` + source + `)) AS added
	 WHERE threads.id = ` + thread + ` AND added.created_at IS NOT NULL;`
}

// turnErrorChunkRowsSQL selects a chunk's rows in thread's set.
func turnErrorChunkRowsSQL(thread, chunk string) string {
	return `SELECT imported.created_at, imported.turn_index
	  FROM import_history_items imported
	 WHERE imported.chunk_id = ` + chunk + ` AND imported.kind = 'error'
	   AND imported.turn_index >= ` + turnErrorNewestTurnSQL(thread) + `
	   AND ` + turnErrorNotOverriddenSQL(thread)
}

// turnErrorImportedRowSQL selects the imported row item in thread's set,
// ignoring overrides: the override triggers ask about the row an override
// hides or releases.
func turnErrorImportedRowSQL(thread, item string) string {
	return `SELECT imported.created_at, imported.turn_index
	  FROM import_history_items imported
	  CROSS JOIN thread_import_chunks refs ON refs.chunk_id = imported.chunk_id AND refs.thread_id = ` + thread + `
	 WHERE imported.id = ` + item + ` AND imported.kind = 'error'
	   AND imported.turn_index >= ` + turnErrorNewestTurnSQL(thread)
}

// threadTurnErrorSchemaSQL adds the pair, its two probe indexes, the
// backfill and the triggers. Migration v121 installs it.
var threadTurnErrorSchemaSQL = `
ALTER TABLE threads ADD COLUMN newest_turn_error_at INTEGER;
ALTER TABLE threads ADD COLUMN newest_turn_error_turn INTEGER;

CREATE INDEX idx_items_thread_error
    ON items(thread_id, turn_index, created_at) WHERE kind = 'error';

CREATE INDEX idx_import_history_items_error
    ON import_history_items(chunk_id, turn_index, created_at) WHERE kind = 'error';

` + turnErrorRecomputeSQL("1", "") + `

` + threadTurnErrorTriggersSQL

// dropThreadTurnErrorTriggersSQL removes the triggers below; RestoreFrom
// copies the snapshot's pairs verbatim with them off.
const dropThreadTurnErrorTriggersSQL = `DROP TRIGGER IF EXISTS trg_items_turn_error_insert;
DROP TRIGGER IF EXISTS trg_items_turn_error_delete;
DROP TRIGGER IF EXISTS trg_items_turn_error_update;
DROP TRIGGER IF EXISTS trg_turns_turn_error_insert;
DROP TRIGGER IF EXISTS trg_turns_turn_error_delete;
DROP TRIGGER IF EXISTS trg_turns_turn_error_update;
DROP TRIGGER IF EXISTS trg_thread_import_chunks_turn_error_insert;
DROP TRIGGER IF EXISTS trg_thread_import_chunks_turn_error_delete;
DROP TRIGGER IF EXISTS trg_thread_import_chunks_turn_error_update;
DROP TRIGGER IF EXISTS trg_thread_import_item_overrides_turn_error_insert;
DROP TRIGGER IF EXISTS trg_thread_import_item_overrides_turn_error_delete;
DROP TRIGGER IF EXISTS trg_thread_import_item_overrides_turn_error_update;`

var threadTurnErrorTriggersSQL = `CREATE TRIGGER trg_items_turn_error_insert AFTER INSERT ON items
WHEN NEW.kind = 'error'
BEGIN
  ` + turnErrorRaiseSQL("NEW.thread_id", `SELECT NEW.created_at AS created_at, NEW.turn_index AS turn_index
	 WHERE NEW.turn_index >= `+turnErrorNewestTurnSQL("NEW.thread_id")) + `
END;

CREATE TRIGGER trg_items_turn_error_delete AFTER DELETE ON items
WHEN OLD.kind = 'error' AND OLD.turn_index >= ` + turnErrorNewestTurnSQL("OLD.thread_id") + `
BEGIN
  ` + turnErrorRecomputeSQL("id = OLD.thread_id", "") + `
END;

CREATE TRIGGER trg_items_turn_error_update AFTER UPDATE OF thread_id, kind, turn_index, created_at ON items
WHEN (OLD.kind = 'error' OR NEW.kind = 'error')
 AND (OLD.thread_id IS NOT NEW.thread_id OR OLD.kind IS NOT NEW.kind
      OR OLD.turn_index IS NOT NEW.turn_index OR OLD.created_at IS NOT NEW.created_at)
BEGIN
  ` + turnErrorRecomputeSQL("id IN (OLD.thread_id, NEW.thread_id)", "") + `
END;

CREATE TRIGGER trg_turns_turn_error_insert AFTER INSERT ON turns
WHEN NEW.turn_index = (SELECT MAX(turns.turn_index) FROM turns WHERE turns.thread_id = NEW.thread_id)
BEGIN
  UPDATE threads SET newest_turn_error_at = NULL, newest_turn_error_turn = NULL
   WHERE id = NEW.thread_id AND newest_turn_error_turn < NEW.turn_index;
  ` + turnErrorRecomputeSQL("id = NEW.thread_id AND newest_turn_error_turn >= NEW.turn_index", "") + `
END;

CREATE TRIGGER trg_turns_turn_error_delete AFTER DELETE ON turns
WHEN OLD.turn_index > ` + turnErrorNewestTurnSQL("OLD.thread_id") + `
BEGIN
  ` + turnErrorRecomputeSQL("id = OLD.thread_id", "") + `
END;

CREATE TRIGGER trg_turns_turn_error_update AFTER UPDATE OF thread_id, turn_index ON turns
WHEN OLD.thread_id IS NOT NEW.thread_id OR OLD.turn_index IS NOT NEW.turn_index
BEGIN
  ` + turnErrorRecomputeSQL("id IN (OLD.thread_id, NEW.thread_id)", "") + `
END;

CREATE TRIGGER trg_thread_import_chunks_turn_error_insert AFTER INSERT ON thread_import_chunks
BEGIN
  ` + turnErrorRaiseSQL("NEW.thread_id", turnErrorChunkRowsSQL("NEW.thread_id", "NEW.chunk_id")) + `
END;

CREATE TRIGGER trg_thread_import_chunks_turn_error_delete BEFORE DELETE ON thread_import_chunks
WHEN EXISTS (` + turnErrorChunkRowsSQL("OLD.thread_id", "OLD.chunk_id") + `)
BEGIN
  ` + turnErrorRecomputeSQL("id = OLD.thread_id", "OLD.chunk_id") + `
END;

CREATE TRIGGER trg_thread_import_chunks_turn_error_update AFTER UPDATE OF thread_id, chunk_id ON thread_import_chunks
WHEN OLD.thread_id IS NOT NEW.thread_id OR OLD.chunk_id IS NOT NEW.chunk_id
BEGIN
  ` + turnErrorRecomputeSQL("id IN (OLD.thread_id, NEW.thread_id)", "") + `
END;

CREATE TRIGGER trg_thread_import_item_overrides_turn_error_insert AFTER INSERT ON thread_import_item_overrides
WHEN EXISTS (` + turnErrorImportedRowSQL("NEW.thread_id", "NEW.item_id") + `)
BEGIN
  ` + turnErrorRecomputeSQL("id = NEW.thread_id", "") + `
END;

CREATE TRIGGER trg_thread_import_item_overrides_turn_error_delete AFTER DELETE ON thread_import_item_overrides
BEGIN
  ` + turnErrorRaiseSQL("OLD.thread_id", turnErrorImportedRowSQL("OLD.thread_id", "OLD.item_id")) + `
END;

CREATE TRIGGER trg_thread_import_item_overrides_turn_error_update AFTER UPDATE ON thread_import_item_overrides
WHEN OLD.thread_id IS NOT NEW.thread_id OR OLD.item_id IS NOT NEW.item_id
BEGIN
  ` + turnErrorRecomputeSQL("id IN (OLD.thread_id, NEW.thread_id)", "") + `
END;`
