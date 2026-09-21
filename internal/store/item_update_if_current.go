package store

import "fmt"

// UpdateItemIfRevision updates an existing row only while its observed revision
// is current. The caller must re-read and recompute after a concurrent write.
func (s *Store) UpdateItemIfRevision(item Item) (Item, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Item{}, false, fmt.Errorf("store: begin conditional item update: %w", err)
	}
	defer tx.Rollback()
	if err := requireMutableItemTx(tx, item.ThreadID, item.ID, "store: conditional item update"); err != nil {
		return Item{}, false, err
	}
	current, err := readItemRevTx(tx, item.ThreadID, item.ID)
	if err != nil {
		return Item{}, false, err
	}
	if current != item.Rev {
		return Item{}, false, nil
	}
	if err := updateExistingItem(tx, item); err != nil {
		return Item{}, false, err
	}
	persisted, err := readBackItemTx(tx, item.ThreadID, item.ID)
	if err != nil {
		return Item{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Item{}, false, fmt.Errorf("store: commit conditional item update: %w", err)
	}
	return persisted, true, nil
}
