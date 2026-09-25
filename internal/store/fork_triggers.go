package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Pointer-fork triggers (docs/architecture/sqlite-store.md#triggers-and-stamps).
//
// A row a pointer fork shows belongs to the thread that holds it, and the
// fork's history is fixed when the fork is made. The guards below refuse a
// write that would change what a fork shows: a thread that changes such a
// row gives the forks a copy first (fork_reown.go), and one that must stop
// showing such rows moves them to a holder first (fork_holders.go).

// shownHistoryImmutable is the message every guard raises.
const shownHistoryImmutable = "history another thread shows is immutable"

// IsShownHistoryRefusal reports whether err is the store refusing a write
// to history a pointer fork shows: a guard refused it, or the copy the
// write gives the forks first needs a level past the depth cap
// (ErrForkChainTooDeep).
func IsShownHistoryRefusal(err error) bool {
	return err != nil && (errors.Is(err, ErrForkChainTooDeep) || strings.Contains(err.Error(), shownHistoryImmutable))
}

// fixShownHistoryTx runs fix, a migration's one-time data fix, in tx with
// the guards of shown payload content stood down: a data fix applies to
// every copy of the data, a payload a fork shows and a holder's included.
// The guards stand down while shown_history_fix holds a row. It writes the
// row before fix and deletes it after, whether fix fails or not, so the row
// never commits; tx runs on the single writer connection, so no other write
// runs while it is there.
func fixShownHistoryTx(tx *sql.Tx, fix func() error) error {
	if _, err := tx.Exec(`INSERT INTO shown_history_fix (id) VALUES (1)`); err != nil {
		return fmt.Errorf("store: stand down the shown-history guards: %w", err)
	}
	fixErr := fix()
	if _, err := tx.Exec(`DELETE FROM shown_history_fix`); err != nil {
		return errors.Join(fixErr, fmt.Errorf("store: restore the shown-history guards: %w", err))
	}
	return fixErr
}

// readerShowsItemSQL is true while some thread reads the items row of
// owner with the given id and position through its lineage: a lineage row
// naming owner whose cut follows the row, where neither the reader nor a
// nearer level hides the id. It is the lineage arms' visibility rule
// (inheritedItemVisibleSQL) seen from the owner, one range probe of
// idx_thread_fork_lineage_ancestor, which finds nothing for a row after
// every reader's cut.
func readerShowsItemSQL(owner, id, turn, item string) string {
	return `EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ` + owner + `
     AND (l.cut_turn_index, l.cut_item_index) > (` + turn + `, ` + item + `)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_hidden hidden
                      WHERE hidden.thread_id = l.thread_id AND hidden.item_id = ` + id + `)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN thread_fork_hidden hidden ON hidden.thread_id = nearer.ancestor_id AND hidden.item_id = ` + id + `
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth))`
}

// readerShowsTurnSQL is forkTurnVisibleSQL seen from the owner: some
// reader reads owner's row of the turn because the turn is below that
// level's cut, the reader holds no row of its own there and no nearer
// level has one the reader reads.
func readerShowsTurnSQL(owner, turn string) string {
	return `EXISTS (SELECT 1 FROM thread_fork_lineage l
   WHERE l.ancestor_id = ` + owner + ` AND l.cut_turn_index > ` + turn + `
     AND NOT EXISTS (SELECT 1 FROM turns held WHERE held.thread_id = l.thread_id AND held.turn_index = ` + turn + `)
     AND NOT EXISTS (SELECT 1 FROM thread_fork_lineage nearer
                       JOIN turns held ON held.thread_id = nearer.ancestor_id AND held.turn_index = ` + turn + `
                      WHERE nearer.thread_id = l.thread_id AND nearer.depth < l.depth
                        AND nearer.cut_turn_index > ` + turn + `))`
}

// readerShowsPayloadSQL is true while some thread shows a row of owner
// that renders payload id: a local row naming it through the two partial
// payload indexes, or an imported row of owner's chunks that names it. It
// starts with one probe for any reader of owner, so a thread nothing
// reads pays that probe alone.
func readerShowsPayloadSQL(owner, id string) string {
	return `EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = ` + owner + `)
 AND (EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + owner + ` AND ref.payload_id = ` + id + `
                AND ` + readerShowsItemSQL("ref.thread_id", "ref.id", "ref.turn_index", "ref.item_index") + `)
   OR EXISTS (SELECT 1 FROM items ref WHERE ref.thread_id = ` + owner + ` AND ref.input_payload_id = ` + id + `
                AND ` + readerShowsItemSQL("ref.thread_id", "ref.id", "ref.turn_index", "ref.item_index") + `)
   OR EXISTS (SELECT 1 FROM import_history_payloads ref_payload
                CROSS JOIN thread_import_chunks refs ON refs.chunk_id = ref_payload.chunk_id
                CROSS JOIN import_history_items items ON items.chunk_id = ref_payload.chunk_id
               WHERE ref_payload.id = ` + id + ` AND refs.thread_id = ` + owner + `
                 AND (items.payload_id = ref_payload.id OR items.input_payload_id = ref_payload.id)
                 AND ` + importedNotOverridden + `
                 AND ` + readerShowsItemSQL("refs.thread_id", "items.id", "items.turn_index", "items.item_index") + `))`
}

// itemContentChangedSQL lists the columns a shown row may not change:
// every column but the revision, which a reader does not read
// (importedItemRevExpr), and thread_id, which moves the row to a holder.
// A revision touch writes updated_at to itself.
const itemContentChangedSQL = `(OLD.id IS NOT NEW.id OR OLD.summary IS NOT NEW.summary OR OLD.status IS NOT NEW.status
      OR OLD.meta IS NOT NEW.meta OR OLD.kind IS NOT NEW.kind OR OLD.role IS NOT NEW.role
      OR OLD.turn_index IS NOT NEW.turn_index OR OLD.item_index IS NOT NEW.item_index
      OR OLD.payload_id IS NOT NEW.payload_id OR OLD.input_payload_id IS NOT NEW.input_payload_id
      OR OLD.parent_id IS NOT NEW.parent_id OR OLD.is_background IS NOT NEW.is_background
      OR OLD.completion_of IS NOT NEW.completion_of
      OR OLD.tool_name IS NOT NEW.tool_name OR OLD.decision IS NOT NEW.decision
      OR OLD.created_at IS NOT NEW.created_at OR OLD.updated_at IS NOT NEW.updated_at)`

// turnContentChangedSQL is itemContentChangedSQL for a turn row.
const turnContentChangedSQL = `(OLD.turn_index IS NOT NEW.turn_index OR OLD.started_at IS NOT NEW.started_at
      OR OLD.completed_at IS NOT NEW.completed_at OR OLD.stop_reason IS NOT NEW.stop_reason
      OR OLD.assistant_message_id IS NOT NEW.assistant_message_id
      OR OLD.token_usage_json IS NOT NEW.token_usage_json OR OLD.error_message IS NOT NEW.error_message
      OR OLD.provider_turn_id IS NOT NEW.provider_turn_id)`

// forkTriggersSQL is the latest DDL for the pointer-fork triggers: the set
// migration v125 installed, with v130's turn guards and v131's payload
// guards, less the revive trigger v133 dropped. RestoreFrom reinstalls it
// after the row copy, which runs without these triggers so restored rows
// are the snapshot's exactly.
//
//   - trg_threads_fork_source_delete: a thread forks read is never deleted;
//     its delete keeps it as a holder (DeleteThreadPaced).
//   - trg_items_fork_position / trg_items_fork_position_update: a fork's own
//     rows sit at or after its cut. The only exception is a row that
//     replaces an inherited one, which is hidden first.
//   - trg_items_fork_snapshot: a row an ancestor inserts below a fork's cut
//     after the fork was made (a background child, a late completion) is
//     not part of the fork's history, so the fork hides it. A row that
//     replaces one the inserting thread already showed under the same id
//     (a copy of a row it inherited, or its own imported row localized) is
//     the same history and stays visible to the forks.
//     A reader a nearer level already hides the id from (a holder that
//     took the thread's row of that id) gets no hide: its own hide would
//     remove the holder's row from it too.
//   - trg_items_fork_snapshot_move: the same rule for a row an ancestor
//     moves from at or after a fork's cut to before it.
//   - trg_items_shown_update / trg_items_shown_delete,
//     trg_payloads_shown_update, trg_payload_chunks_shown_*,
//     trg_turns_shown_update / trg_turns_shown_delete: a row, payload or
//     turn a fork shows keeps its content. Moving it to another thread
//     (thread_id) is allowed: that is how a holder takes it. The payload
//     guards stand down while a migration's data fix runs
//     (fixShownHistoryTx).
//   - trg_thread_fork_lineage_release: a holder no lineage row names any
//     more is marked deleting, for the app to delete (ListPendingThreadDeletes).
var forkTriggersSQL = forkGuardTriggersSQL + forkLineageReleaseTriggerSQL

// forkGuardTriggersSQL is forkTriggersSQL before the lineage release trigger.
var forkGuardTriggersSQL = `
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
WHEN ` + itemContentChangedSQL + `
 AND ` + readerShowsItemSQL("OLD.thread_id", "OLD.id", "OLD.turn_index", "OLD.item_index") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_items_shown_delete BEFORE DELETE ON items
WHEN ` + readerShowsItemSQL("OLD.thread_id", "OLD.id", "OLD.turn_index", "OLD.item_index") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_payloads_shown_update BEFORE UPDATE OF data, meta ON payloads
WHEN (OLD.data IS NOT NEW.data OR OLD.meta IS NOT NEW.meta)
 AND NOT EXISTS (SELECT 1 FROM shown_history_fix)
 AND ` + readerShowsPayloadSQL("OLD.thread_id", "OLD.id") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_payload_chunks_shown_insert BEFORE INSERT ON payload_chunks
WHEN NOT EXISTS (SELECT 1 FROM shown_history_fix)
 AND ` + readerShowsPayloadSQL("NEW.thread_id", "NEW.payload_id") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_payload_chunks_shown_update BEFORE UPDATE OF chunk_index, start_offset, data ON payload_chunks
WHEN (OLD.chunk_index IS NOT NEW.chunk_index OR OLD.start_offset IS NOT NEW.start_offset OR OLD.data IS NOT NEW.data)
 AND NOT EXISTS (SELECT 1 FROM shown_history_fix)
 AND ` + readerShowsPayloadSQL("OLD.thread_id", "OLD.payload_id") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_payload_chunks_shown_delete BEFORE DELETE ON payload_chunks
WHEN NOT EXISTS (SELECT 1 FROM shown_history_fix)
 AND ` + readerShowsPayloadSQL("OLD.thread_id", "OLD.payload_id") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_turns_shown_update BEFORE UPDATE OF turn_index, started_at, completed_at, stop_reason,
    assistant_message_id, token_usage_json, error_message, provider_turn_id ON turns
WHEN ` + turnContentChangedSQL + `
 AND ` + readerShowsTurnSQL("OLD.thread_id", "OLD.turn_index") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

CREATE TRIGGER trg_turns_shown_delete BEFORE DELETE ON turns
WHEN ` + readerShowsTurnSQL("OLD.thread_id", "OLD.turn_index") + `
BEGIN
  SELECT RAISE(ABORT, '` + shownHistoryImmutable + `');
END;

`

// forkLineageReleaseTriggerSQL is forkTriggersSQL after the guards.
const forkLineageReleaseTriggerSQL = `CREATE TRIGGER trg_thread_fork_lineage_release AFTER DELETE ON thread_fork_lineage
WHEN NOT EXISTS (SELECT 1 FROM thread_fork_lineage WHERE ancestor_id = OLD.ancestor_id)
BEGIN
  UPDATE threads SET deleting = 1 WHERE id = OLD.ancestor_id AND mode = 'holder' AND deleting = 0;
END;
`

// dropForkTriggersSQL drops every trigger forkTriggersSQL creates.
const dropForkTriggersSQL = `DROP TRIGGER IF EXISTS trg_threads_fork_source_delete;
DROP TRIGGER IF EXISTS trg_items_fork_position;
DROP TRIGGER IF EXISTS trg_items_fork_position_update;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot;
DROP TRIGGER IF EXISTS trg_items_fork_snapshot_move;
DROP TRIGGER IF EXISTS trg_items_shown_update;
DROP TRIGGER IF EXISTS trg_items_shown_delete;
DROP TRIGGER IF EXISTS trg_payloads_shown_update;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_insert;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_update;
DROP TRIGGER IF EXISTS trg_payload_chunks_shown_delete;
DROP TRIGGER IF EXISTS trg_turns_shown_update;
DROP TRIGGER IF EXISTS trg_turns_shown_delete;
DROP TRIGGER IF EXISTS trg_thread_fork_lineage_release;`
