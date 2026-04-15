package store

import (
	"database/sql"
	"fmt"
)

func (s *Store) InsertItem(item Item) error {
	_, err := s.db.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Summary,
		nilIfEmpty(item.PayloadID), item.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert item: %w", err)
	}
	// Touch the parent thread's updated_at.
	_, _ = s.db.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, item.CreatedAt, item.ThreadID)
	return nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary, COALESCE(payload_id, ''), created_at
		 FROM items WHERE thread_id = ? ORDER BY turn_index, item_index`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex, &it.Kind, &it.Role, &it.Summary, &it.PayloadID, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) NextItemIndex(threadID string, turnIndex int) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(item_index) FROM items WHERE thread_id = ? AND turn_index = ?`,
		threadID, turnIndex,
	).Scan(&maxIndex)
	if err != nil {
		return 0, fmt.Errorf("store: next item index: %w", err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64) + 1, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(turn_index) FROM items WHERE thread_id = ?`, threadID,
	).Scan(&maxIndex)
	if err != nil {
		return 0, fmt.Errorf("store: last turn index: %w", err)
	}
	if !maxIndex.Valid {
		return 0, nil
	}
	return int(maxIndex.Int64), nil
}
