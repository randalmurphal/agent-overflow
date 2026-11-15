package store

import (
	"database/sql"
	"fmt"
)

// Migration v152 clears the pendingFlush mark from user rows that are
// history, and lets a one-time data fix reach an items row a pointer fork
// shows.
//
// A quiet row carries pendingFlush only while its send is pending. Earlier
// versions let a session Stop sweep the pending send and keep the mark, so
// the row stayed marked after later turns ran. The boot restore takes a
// marked row back only when it is residue (unconfirmedUserRowResidueSQL);
// this fix removes the mark from every other main-timeline user row, so a
// row's meta says what happened to it. A row the rule calls residue keeps
// its mark for the boot restore. Removing the key, not storing false, is
// what itemmeta.SetPendingFlush(false) does.
//
// The fix applies to every copy of the row, through fixShownHistoryTx, so
// the items guard of shown history stands down while shown_history_fix holds
// a row, as the payload guards do since v131. The item revision trigger
// bumps each changed row's thread.
const stalePendingFlushMigrationVersion = 152

const stalePendingFlushV152SQL = `
DROP TRIGGER trg_items_shown_update;

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
 AND NOT EXISTS (SELECT 1 FROM shown_history_fix)
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
`

// clearStalePendingFlushV152SQL removes the mark from every marked
// main-timeline user row the residue rule calls history: one a later turn of
// its thread follows, or one older than its thread. A meta left with no key
// stays the JSON object '{}': the user-row triggers read meta as JSON.
const clearStalePendingFlushV152SQL = `
UPDATE items AS u
   SET meta = json_remove(u.meta, '$.pendingFlush')
 WHERE u.kind = 'user_text' AND u.parent_id = ''
   AND instr(u.meta, '"pendingFlush"') > 0 AND json_valid(u.meta)
   AND json_extract(u.meta, '$.pendingFlush') = 1
   AND (u.created_at < (SELECT t.created_at FROM threads t WHERE t.id = u.thread_id)
        OR EXISTS (SELECT 1 FROM turns later WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index)
        OR EXISTS (SELECT 1 FROM items later
                    WHERE later.thread_id = u.thread_id AND later.turn_index > u.turn_index AND later.parent_id = ''))`

func clearStalePendingFlushMarks(tx *sql.Tx) error {
	return fixShownHistoryTx(tx, func() error {
		if _, err := tx.Exec(clearStalePendingFlushV152SQL); err != nil {
			return fmt.Errorf("store: clear pendingFlush from history rows: %w", err)
		}
		return nil
	})
}
