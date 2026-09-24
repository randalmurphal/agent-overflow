package store

import (
	"database/sql"
	"fmt"
)

// ErrorActiveItemIfRevision settles only the live row the caller observed.
// A concurrent tool result, approval, or background transition requires a new
// read before deciding whether the row still belongs to the interrupted work.
// card is the card of the row's parent (OpenSubagentCard), or nil for a row
// without one; a row a card previews needs it.
func (s *Store) ErrorActiveItemIfRevision(threadID, id string, revision int64, summary string, now int64, card *SubagentCard) (Item, bool, error) {
	var item Item
	var changed bool
	err := s.writeItems(threadID, card, "interrupt item", func(tx *sql.Tx, w *cardWrite) error {
		old, err := readMutableSubagentRowTx(tx, threadID, id, "store: interrupt item")
		if err != nil {
			return err
		}
		result, err := tx.Exec(`UPDATE items SET status = 'errored', summary = ?, updated_at = ?
  WHERE thread_id = ? AND id = ? AND rev = ?
  AND status IN ('running', 'streaming') AND NOT (kind = 'tool_call' AND is_background = 1)`, summary, now, threadID, id, revision)
		if err != nil {
			return fmt.Errorf("store: interrupt item %s/%s: %w", threadID, id, err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: interrupt item result: %w", err)
		}
		if n == 0 {
			return nil
		}
		changed = true
		row := old
		row.status, row.summary = "errored", summary
		if err := w.updated(old, row); err != nil {
			return err
		}
		if err := indexItemByIDTx(tx, threadID, id); err != nil {
			return err
		}
		if err := w.finish(); err != nil {
			return err
		}
		if item, err = readBackItemTx(tx, threadID, id); err != nil {
			return fmt.Errorf("store: read interrupted item: %w", err)
		}
		return nil
	})
	if err != nil || !changed {
		return Item{}, false, err
	}
	return item, true, nil
}
