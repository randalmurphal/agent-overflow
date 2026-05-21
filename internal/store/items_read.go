package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// FindStreamItemByProviderItemID resolves a streamed assistant row from the
// provider's item id stored in items.meta. It is intentionally a narrow
// fallback lookup for late completion events; the hot delta path keeps the
// in-memory item id and never pays this JSON predicate.
func (s *Store) FindStreamItemByProviderItemID(threadID string, turnIndex int, kind, parentID, providerItemID string) (Item, bool, error) {
	row := s.db.QueryRow(`
		SELECT `+itemColumns+`
		  FROM items
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 WHERE items.thread_id = ?
		   AND items.turn_index = ?
		   AND items.kind = ?
		   AND items.parent_id = ?
		   AND json_extract(items.meta, '$.provider_item_id') = ?
		 ORDER BY items.item_index ASC
		 LIMIT 1`,
		threadID, turnIndex, kind, parentID, providerItemID,
	)
	item, err := scanItemRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find stream item by provider item id: %w", err)
	}
	return item, true, nil
}

func (s *Store) ListItems(threadID string) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		  ORDER BY items.turn_index, items.item_index`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s: %w", threadID, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) ListItemsForTurn(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ?
		  ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) LatestAssistantTextSummaryForParent(threadID, parentID string) (string, bool, error) {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary
		   FROM items
		  WHERE thread_id = ?
		    AND parent_id = ?
		    AND kind = 'assistant_text'
		    AND summary <> ''
		  ORDER BY turn_index DESC, item_index DESC
		  LIMIT 1`,
		threadID,
		parentID,
	).Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: latest assistant text summary for parent %s/%s: %w", threadID, parentID, err)
	}
	return summary, true, nil
}

func (s *Store) LastTurnIndex(threadID string) (int, error) {
	var maxIndex sql.NullInt64
	err := s.db.QueryRow(
		`SELECT MAX(turn_index)
		   FROM (
		         SELECT turn_index FROM items WHERE thread_id = ?
		         UNION ALL
		         SELECT turn_index FROM turns WHERE thread_id = ?
		        )`,
		threadID, threadID,
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
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ? AND items.kind = ?
		 ORDER BY items.item_index DESC
		 LIMIT 1`,
		threadID, turnIndex, kind,
	)

	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find turn item: %w", err)
	}
	return item, true, nil
}

// FindToolCallItemByTaskID resolves a thread's tool_call row whose persisted
// items.meta JSON carries a top-level task_id matching taskID. Used by the
// background completion router when a Claude task_updated/task_notification
// event arrives without an inline tool_use_id — most commonly after a
// reconnect with a fresh parser, when the adapter's in-memory
// task_id ↔ tool_use_id map has been dropped.
//
// The query is O(log N) thanks to the partial expression index
// idx_items_meta_task_id (migration v17) which materialises
// json_extract(meta, '$.task_id') for the narrow subset of rows that
// actually carry a task_id. The kind filter stays in Go-space rather
// than the index because every row this function cares about is a
// tool_call by construction (only that kind sets task_id in meta), and
// adding kind to the index would bloat it for no planner benefit.
//
// Empty taskID returns (Item{}, false, nil) so callers can short-circuit
// without a DB round-trip.
func (s *Store) FindToolCallItemByTaskID(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND json_extract(items.meta, '$.task_id') = ?
		  ORDER BY items.updated_at DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find tool call by task id %s: %w", taskID, err)
	}
	return item, true, nil
}

// GetThreadItemByPayloadID returns the newest item on threadID whose
// payload_id OR input_payload_id matches payloadID, so a payload id is
// not usable outside the thread that references it. The two partial
// indexes (idx_items_payload_id, idx_items_input_payload_id) cover the
// two columns; UNION ALL keeps each branch index-friendly. A single
// OR-clause forces SQLite onto the broad thread_id index instead, which
// would scan every row in the thread on every lazy-load click.
func (s *Store) GetThreadItemByPayloadID(threadID, payloadID string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM (
		         SELECT items.* FROM items
		          WHERE items.thread_id = ? AND items.payload_id = ?
		         UNION ALL
		         SELECT items.* FROM items
		          WHERE items.thread_id = ? AND items.input_payload_id = ?
		   ) AS items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  ORDER BY items.updated_at DESC
		  LIMIT 1`,
		threadID, payloadID, threadID, payloadID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item by payload id %s on thread %s: %w", payloadID, threadID, err)
	}
	return item, true, nil
}

func (s *Store) GetThreadItem(threadID, id string) (Item, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.id = ?`,
		threadID, id,
	)

	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: get item %s on thread %s: %w", id, threadID, err)
	}
	return item, true, nil
}

// FindNotificationItemByTaskID returns the newest notification row whose
// meta.task_id matches taskID. Claude task_notification rows use this to let
// later task terminals attach the durable output_file payload without treating
// the notification itself as lifecycle state.
func (s *Store) FindNotificationItemByTaskID(threadID, taskID string) (Item, bool, error) {
	if taskID == "" {
		return Item{}, false, nil
	}
	row := s.db.QueryRow(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ?
		    AND items.kind = 'notification'
		    AND json_extract(items.meta, '$.task_id') = ?
		  ORDER BY items.turn_index DESC, items.item_index DESC
		  LIMIT 1`,
		threadID, taskID,
	)
	item, err := scanItemRow(row)
	if err == sql.ErrNoRows {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, fmt.Errorf("store: find notification by task_id %s: %w", taskID, err)
	}
	return item, true, nil
}

func (s *Store) ListTurnItems(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumns+`
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.thread_id = ? AND items.turn_index = ?
		 ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn item row: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// ListTurnItemsSansPayload is a lighter sibling of ListTurnItems that
// skips the payloads LEFT JOIN. Use it on paths that read only the
// item-table columns (status, summary, kind, role, is_background) —
// the force-close safety net and the truncated-turn flip loop both
// qualify. For any caller that inspects PayloadKind / PayloadMeta
// (e.g. tool_result_diff_upgrade.loadSummaryOnlyToolResultCandidate)
// keep ListTurnItems, which hydrates them.
func (s *Store) ListTurnItemsSansPayload(threadID string, turnIndex int) ([]Item, error) {
	rows, err := s.db.Query(
		`SELECT `+itemColumnsSansPayload+`
		   FROM items
		  WHERE items.thread_id = ? AND items.turn_index = ?
		 ORDER BY items.item_index`,
		threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn items (sans payload) for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		it, err := scanItemRowSansPayload(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn item row (sans payload): %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (s *Store) HasMatchingSystemItem(threadID string, turnIndex int, kind, parentID, summary string) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(
			SELECT 1
			  FROM items
			 WHERE thread_id = ?
			   AND turn_index = ?
			   AND kind = ?
			   AND role = 'system'
			   AND parent_id = ?
			   AND summary = ?
			 LIMIT 1
		)`,
		threadID, turnIndex, kind, parentID, summary,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: matching system item for thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return exists != 0, nil
}

// LatestToolCallByName returns the most-recently-inserted tool_call row
// in (threadID, turnIndex) whose lower(tool_name) equals any of
// toolNames. Matches the iteration pattern in triage.findLatestToolCall
// but pushes the filter into SQLite so we don't deserialize every item
// in turns with a lot of tool calls. Returns (zero Item, false, nil)
// when no match exists.
//
// toolNames must be non-empty and are matched case-insensitively; the
// names are lowercased by the caller (to keep the SQL string short).
func (s *Store) LatestToolCallByName(threadID string, turnIndex int, toolNames []string) (Item, bool, error) {
	if len(toolNames) == 0 {
		return Item{}, false, nil
	}

	// Build a parametrized IN clause. SQLite has no native array type; we
	// use ? placeholders. Thread id + turn index stay as the final two
	// parameters so the SELECT works regardless of the tool-name slice
	// length. Performance-wise we rely on the items.thread_id +
	// turn_index covering index — the LIMIT 1 makes the scan minimal.
	placeholders := ""
	args := make([]any, 0, len(toolNames)+2)
	for i, name := range toolNames {
		if i > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		args = append(args, name)
	}
	args = append(args, threadID, turnIndex)

	query := `SELECT ` + itemColumns + `
		   FROM items
		   LEFT JOIN payloads ON payloads.id = items.payload_id
		  WHERE items.kind = 'tool_call'
		    AND lower(items.tool_name) IN (` + placeholders + `)
		    AND items.thread_id = ? AND items.turn_index = ?
		  ORDER BY items.item_index DESC
		  LIMIT 1`

	row := s.db.QueryRow(query, args...)
	it, err := scanItemRow(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Item{}, false, nil
		}
		return Item{}, false, fmt.Errorf("store: latest tool_call thread %s turn %d: %w", threadID, turnIndex, err)
	}
	return it, true, nil
}
