package store

import (
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) InsertItem(item Item) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin insert item tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, parent_tool_use_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Summary,
		nilIfEmpty(item.PayloadID), item.ParentToolUseID, item.CreatedAt,
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

// AppendItem inserts an item at the next available item_index for
// (thread, turn), computed atomically inside the transaction. Unlike
// InsertItem, the caller does not pass item_index — the store derives it
// as MAX(item_index)+1 within the same transaction as the insert, so two
// concurrent AppendItem calls for the same (thread, turn) cannot land on
// the same slot. Returns the assigned item_index.
//
// Use this when the caller's intent is "add a new timeline entry" and any
// monotonic index is acceptable. Use InsertItem when the caller must
// control the exact index (e.g. CloneThreadItems preserving source
// ordering, migrations replaying a fixed sequence).
func (s *Store) AppendItem(item Item) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin append item tx: %w", err)
	}
	defer tx.Rollback()

	var maxIndex sql.NullInt64
	if err := tx.QueryRow(
		`SELECT MAX(item_index) FROM items WHERE thread_id = ? AND turn_index = ?`,
		item.ThreadID, item.TurnIndex,
	).Scan(&maxIndex); err != nil {
		return 0, fmt.Errorf("store: append item next index: %w", err)
	}
	next := 0
	if maxIndex.Valid {
		next = int(maxIndex.Int64) + 1
	}
	item.ItemIndex = next

	if _, err := tx.Exec(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, summary, payload_id, parent_tool_use_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex, item.Kind, item.Role, item.Summary,
		nilIfEmpty(item.PayloadID), item.ParentToolUseID, item.CreatedAt,
	); err != nil {
		return 0, fmt.Errorf("store: append item insert: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		item.CreatedAt, item.ThreadID,
	); err != nil {
		return 0, fmt.Errorf("store: append item touch thread %s: %w", item.ThreadID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit append item tx: %w", err)
	}
	return next, nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary, COALESCE(payload_id, ''), parent_tool_use_id, created_at
		 FROM items WHERE thread_id = ? ORDER BY turn_index, item_index`, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex, &it.Kind, &it.Role, &it.Summary, &it.PayloadID, &it.ParentToolUseID, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// NextItemIndex returns the next available item_index for the given
// (thread, turn). Prefer AppendItem for anything that follows the read
// with an insert: NextItemIndex + InsertItem is a read-then-write pair
// that two goroutines can race on, landing both rows at the same
// index. Wave A's UNIQUE(thread_id, turn_index, item_index) guards
// against actual corruption, but a caller that treats NextItemIndex as
// "reserve a slot" will see UNIQUE errors instead of a clean ordered
// append.
//
// NextItemIndex is fine when the caller only needs to read the current
// tail (e.g. choosing which payload to update) and does not then
// insert using the returned value. For "append a new item" use
// AppendItem, which computes MAX+1 and inserts inside the same
// transaction.
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

func (s *Store) HasItems(threadID string) (bool, error) {
	var exists int
	if err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items WHERE thread_id = ? LIMIT 1)`,
		threadID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: has items for thread %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) FindTurnItem(threadID string, turnIndex int, kind string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary,
		    COALESCE(payload_id, ''), parent_tool_use_id, created_at
		 FROM items
		 WHERE thread_id = ? AND turn_index = ? AND kind = ?
		 ORDER BY item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	)

	var item Item
	err := row.Scan(
		&item.ID, &item.ThreadID, &item.TurnIndex, &item.ItemIndex, &item.Kind,
		&item.Role, &item.Summary, &item.PayloadID, &item.ParentToolUseID, &item.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find turn item: %w", err)
	}
	return item, true, nil
}

func (s *Store) GetItem(id string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary,
		    COALESCE(payload_id, ''), parent_tool_use_id, created_at
		 FROM items
		 WHERE id = ?`,
		id,
	)

	var item Item
	err := row.Scan(
		&item.ID, &item.ThreadID, &item.TurnIndex, &item.ItemIndex, &item.Kind,
		&item.Role, &item.Summary, &item.PayloadID, &item.ParentToolUseID, &item.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item %s: %w", id, err)
	}
	return item, true, nil
}

func (s *Store) ListTurnItems(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT id, thread_id, turn_index, item_index, kind, role, summary, COALESCE(payload_id, ''), parent_tool_use_id, created_at
		 FROM items
		 WHERE thread_id = ? AND turn_index = ?
		 ORDER BY item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex, &it.Kind, &it.Role, &it.Summary, &it.PayloadID, &it.ParentToolUseID, &it.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan turn item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// DeleteItemsAfterTurn removes every item with turn_index strictly greater
// than keepThroughTurn for the given thread, and bumps the thread's
// updated_at inside the same transaction. Returns the number of items
// deleted. keepThroughTurn is the last turn to preserve — to drop all
// items from a thread pass -1 (though callers should not typically do
// that).
//
// Used by the revert flow in app_checkpoint.go: after a checkpoint
// revert the timeline must match the post-revert conversation state, so
// every item the user reverted past is dropped from the store.
//
// Payload rows are not cascade-deleted here. They become orphaned but
// reachable only by a deleted item's old payload_id, which no caller
// will look up. A future GC pass can reclaim them; no correctness
// impact in the meantime.
func (s *Store) DeleteItemsAfterTurn(threadID string, keepThroughTurn int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin delete items after turn tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`DELETE FROM items WHERE thread_id = ? AND turn_index > ?`,
		threadID, keepThroughTurn,
	)
	if err != nil {
		return 0, fmt.Errorf("store: delete items after turn for thread %s: %w", threadID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete items after turn rows affected: %w", err)
	}

	if _, err := tx.Exec(
		`UPDATE threads SET updated_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), threadID,
	); err != nil {
		return 0, fmt.Errorf("store: touch thread %s after item truncation: %w", threadID, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit delete items after turn tx: %w", err)
	}
	return int(n), nil
}

// UpdateItemPayload updates a single item's payload link, summary, and
// timestamp, and bumps the parent thread's updated_at so the sidebar
// reshuffles. Both updates run inside one transaction so the thread's
// updated_at never drifts out of sync with the item it describes. The
// thread-touch error used to be log-only; a write failure there is just
// as meaningful as a failure on the item update and callers deserve to
// see it.
//
// Returns an error if the item does not exist or its thread has been
// deleted (caught by RowsAffected on each UPDATE), instead of silently
// succeeding on a no-op update.
func (s *Store) UpdateItemPayload(id, payloadID, summary string, createdAt int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin update item payload tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE items SET payload_id = ?, summary = ?, created_at = ? WHERE id = ?`,
		nilIfEmpty(payloadID), summary, createdAt, id,
	)
	if err != nil {
		return fmt.Errorf("store: update item payload %s: %w", id, err)
	}
	if err := requireRowsAffected(
		result,
		fmt.Sprintf("store: update item payload %s", id),
	); err != nil {
		return err
	}

	threadResult, err := tx.Exec(`UPDATE threads SET updated_at = ? WHERE id = (
		SELECT thread_id FROM items WHERE id = ?
	)`, createdAt, id)
	if err != nil {
		return fmt.Errorf("store: touch thread updated_at for item %s: %w", id, err)
	}
	if err := requireRowsAffected(
		threadResult,
		fmt.Sprintf("store: touch thread updated_at for item %s", id),
	); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit update item payload tx: %w", err)
	}
	return nil
}
