package store

import (
	"database/sql"
	"fmt"
	"log"
)

func (s *Store) InsertItem(item Item) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin insert item tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Summary,
		nilIfEmpty(item.PayloadID), item.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("store: insert item: %w", err)
	}
	// Touch the parent thread's updated_at.
	if _, err := tx.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, item.CreatedAt, item.ThreadID); err != nil {
		return fmt.Errorf("store: touch thread updated_at for %s: %w", item.ThreadID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit insert item tx: %w", err)
	}
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

func (s *Store) FindTurnItem(threadID string, turnIndex int, kind string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary,
		    COALESCE(payload_id, ''), created_at
		 FROM items
		 WHERE thread_id = ? AND turn_index = ? AND kind = ?
		 ORDER BY item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	)

	var item Item
	err := row.Scan(
		&item.ID, &item.ThreadID, &item.TurnIndex, &item.ItemIndex, &item.Kind,
		&item.Role, &item.Summary, &item.PayloadID, &item.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find turn item: %w", err)
	}
	return item, true, nil
}

// FirstUserMessage returns the summary of the first user text message in the
// given thread, or empty string if none exists.
func (s *Store) FirstUserMessage(threadID string) (string, error) {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary FROM items WHERE thread_id = ? AND role = 'user' AND kind = 'text' ORDER BY turn_index, item_index LIMIT 1`,
		threadID,
	).Scan(&summary)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: first user message for thread %s: %w", threadID, err)
	}
	return summary, nil
}

func (s *Store) UpdateItemPayload(id, payloadID, summary string, createdAt int64) error {
	_, err := s.db.Exec(
		`UPDATE items SET payload_id = ?, summary = ?, created_at = ? WHERE id = ?`,
		nilIfEmpty(payloadID), summary, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update item payload %s: %w", id, err)
	}

	if _, err := s.db.Exec(`UPDATE threads SET updated_at = ? WHERE id = (
		SELECT thread_id FROM items WHERE id = ?
	)`, createdAt, id); err != nil {
		log.Printf("store: touch thread updated_at for item %s: %v", id, err)
	}
	return nil
}
