package store

import "fmt"

// ErrorActiveItemIfRevision settles only the live row the caller observed.
// A concurrent tool result, approval, or background transition requires a new
// read before deciding whether the row still belongs to the interrupted work.
func (s *Store) ErrorActiveItemIfRevision(threadID, id string, revision int64, summary string, now int64) (Item, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, false, fmt.Errorf("store: begin interrupt item: %w", err)
	}
	defer tx.Rollback()
	if err := requireMutableItemTx(tx, threadID, id, "store: interrupt item"); err != nil {
		return Item{}, false, err
	}
	result, err := tx.Exec(`UPDATE items SET status = 'errored', summary = ?, updated_at = ?
  WHERE thread_id = ? AND id = ? AND rev = ?
  AND status IN ('running', 'streaming') AND NOT (kind = 'tool_call' AND is_background = 1)`, summary, now, threadID, id, revision)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: interrupt item %s/%s: %w", threadID, id, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return Item{}, false, fmt.Errorf("store: interrupt item result: %w", err)
	}
	if changed == 0 {
		return Item{}, false, nil
	}
	if err := indexItemByIDTx(tx, threadID, id); err != nil {
		return Item{}, false, err
	}
	item, err := readBackItemTx(tx, threadID, id)
	if err != nil {
		return Item{}, false, fmt.Errorf("store: read interrupted item: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Item{}, false, fmt.Errorf("store: commit interrupt item: %w", err)
	}
	return item, true, nil
}
