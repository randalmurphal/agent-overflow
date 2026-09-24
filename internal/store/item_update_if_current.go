package store

import (
	"database/sql"
	"fmt"
)

// UpdateItemIfRevision updates an existing row only while its observed revision
// is current. The caller must re-read and recompute after a concurrent write.
func (s *Store) UpdateItemIfRevision(item Item) (Item, bool, error) {
	var persisted Item
	var updated bool
	err := s.writeItemsReportingForks(item.ThreadID, item.SubagentCard, "conditional item update", func(tx *sql.Tx, w *cardWrite) error {
		old, err := readMutableSubagentRowTx(tx, item.ThreadID, item.ID, "store: conditional item update")
		if err != nil {
			return err
		}
		current, err := readItemRevTx(tx, item.ThreadID, item.ID)
		if err != nil {
			return err
		}
		if current != item.Rev {
			return nil
		}
		if err := updateExistingItem(tx, w, item, old); err != nil {
			return err
		}
		if err := w.finish(); err != nil {
			return err
		}
		if persisted, err = readBackItemTx(tx, item.ThreadID, item.ID); err != nil {
			return fmt.Errorf("store: read conditionally updated item: %w", err)
		}
		updated = true
		return nil
	})
	if err != nil || !updated {
		return Item{}, false, err
	}
	return persisted, true, nil
}
