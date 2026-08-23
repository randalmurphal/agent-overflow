package store

import "fmt"

// EditDiffItem is one tool call whose persisted payload carries a
// unified diff — an Edit/Write/apply_patch or captured command inline
// diff (payload kind `tool_result`), or a legacy Claude EventDiff
// attach (kind `diff`). Metadata only: the diff bytes load on
// selection via GetPayloadData, keeping the review pane's edit list
// as cheap as the commit list.
type EditDiffItem struct {
	ItemID      string
	PayloadID   string
	TurnIndex   int
	ItemIndex   int
	CreatedAt   int64
	PayloadKind string
	PayloadMeta string
}

// ListEditDiffItems returns every edit-diff tool call of a thread in
// timeline order, including subagent children — a subagent's edit is
// as real as the parent's.
func (s *Store) ListEditDiffItems(threadID string) ([]EditDiffItem, error) {
	rows, err := s.reader().Query(`
		WITH edit_items AS (
			SELECT items.id, items.payload_id, items.turn_index, items.item_index,
			       items.created_at, payloads.kind, payloads.meta, payloads.data
			  FROM items AS items
			  JOIN payloads AS payloads
			    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
			 WHERE items.thread_id = ?
			UNION ALL
			SELECT items.id, items.payload_id, items.turn_index, items.item_index,
			       items.created_at,
			       COALESCE(local_payloads.kind, imported_payloads.kind),
			       COALESCE(local_payloads.meta, imported_payloads.meta),
			       COALESCE(local_payloads.data, imported_payloads.data)
			  FROM thread_import_chunks AS refs
			  JOIN import_history_items AS items ON items.chunk_id = refs.chunk_id
			  LEFT JOIN payloads AS local_payloads
			    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
			  LEFT JOIN import_history_payloads AS imported_payloads
			    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
			  LEFT JOIN thread_import_item_overrides AS overrides
			    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
			 WHERE refs.thread_id = ? AND overrides.item_id IS NULL
		)
		SELECT id, payload_id, turn_index, item_index, created_at, kind, meta
		  FROM edit_items
		 WHERE kind IN ('tool_result', 'diff') AND length(data) > 0
		 ORDER BY turn_index ASC, item_index ASC`,
		threadID, threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list edit diff items for %s: %w", threadID, err)
	}
	defer rows.Close()

	var entries []EditDiffItem
	for rows.Next() {
		var entry EditDiffItem
		if err := rows.Scan(
			&entry.ItemID, &entry.PayloadID, &entry.TurnIndex, &entry.ItemIndex,
			&entry.CreatedAt, &entry.PayloadKind, &entry.PayloadMeta,
		); err != nil {
			return nil, fmt.Errorf("store: scan edit diff item for %s: %w", threadID, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate edit diff items for %s: %w", threadID, err)
	}
	return entries, nil
}

// TurnEditDiffPatch is one edit payload's diff bytes plus the payload
// id, so callers can pair the patch with its persisted span blob.
type TurnEditDiffPatch struct {
	PayloadID string
	Data      []byte
}

// ListTurnEditDiffPatches returns the diff payloads of one turn's
// edit-diff tool calls in item order — the sequential story of what
// the turn changed. Callers concatenate; nothing here is merged or
// deduplicated (the same file edited twice yields two patch sections).
func (s *Store) ListTurnEditDiffPatches(threadID string, turnIndex int) ([]TurnEditDiffPatch, error) {
	rows, err := s.reader().Query(`
		WITH edit_items AS (
			SELECT items.payload_id, items.item_index, payloads.kind, payloads.data
			  FROM items AS items
			  JOIN payloads AS payloads
			    ON payloads.thread_id = items.thread_id AND payloads.id = items.payload_id
			 WHERE items.thread_id = ? AND items.turn_index = ?
			UNION ALL
			SELECT items.payload_id, items.item_index,
			       COALESCE(local_payloads.kind, imported_payloads.kind),
			       COALESCE(local_payloads.data, imported_payloads.data)
			  FROM thread_import_chunks AS refs
			  JOIN import_history_items AS items ON items.chunk_id = refs.chunk_id
			  LEFT JOIN payloads AS local_payloads
			    ON local_payloads.thread_id = refs.thread_id AND local_payloads.id = items.payload_id
			  LEFT JOIN import_history_payloads AS imported_payloads
			    ON imported_payloads.chunk_id = items.chunk_id AND imported_payloads.id = items.payload_id
			  LEFT JOIN thread_import_item_overrides AS overrides
			    ON overrides.thread_id = refs.thread_id AND overrides.item_id = items.id
			 WHERE refs.thread_id = ? AND items.turn_index = ? AND overrides.item_id IS NULL
		)
		SELECT payload_id, data
		  FROM edit_items
		 WHERE kind IN ('tool_result', 'diff') AND length(data) > 0
		 ORDER BY item_index ASC`,
		threadID, turnIndex, threadID, turnIndex,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn edit diff patches for %s/%d: %w", threadID, turnIndex, err)
	}
	defer rows.Close()

	var patches []TurnEditDiffPatch
	for rows.Next() {
		var patch TurnEditDiffPatch
		if err := rows.Scan(&patch.PayloadID, &patch.Data); err != nil {
			return nil, fmt.Errorf("store: scan turn edit diff patch for %s/%d: %w", threadID, turnIndex, err)
		}
		patches = append(patches, patch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate turn edit diff patches for %s/%d: %w", threadID, turnIndex, err)
	}
	return patches, nil
}

// TurnUserSummary labels a turn with its first user prompt's summary,
// for grouping the edit selector by turn.
type TurnUserSummary struct {
	TurnIndex int
	Summary   string
}

// ListTurnUserSummaries returns each turn's first reader-authored
// user_text summary. Turns without one (rare recovery shapes) are simply
// absent.
//
// readerAuthoredUserTextFilter, not a bare kind test: a subagent's own
// prompt is a `user_text` row too, nested under its launch and carrying
// the launch's turn index, and a turn whose only user row is one would
// otherwise label the edit selector with what an agent was told rather
// than what the reader asked for.
func (s *Store) ListTurnUserSummaries(threadID string) ([]TurnUserSummary, error) {
	// SQLite resolves the bare summary column against the row that
	// carries MIN(item_index) — the first prompt of the turn (a steer
	// or queued flush lands later in the same turn).
	rows, err := s.reader().Query(`
		SELECT turn_index, summary, MIN(item_index)
		  FROM timeline_items
		 WHERE thread_id = ?
		   AND `+readerAuthoredUserTextFilter+`
		 GROUP BY turn_index
		 ORDER BY turn_index ASC`,
		threadID,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list turn user summaries for %s: %w", threadID, err)
	}
	defer rows.Close()

	var summaries []TurnUserSummary
	for rows.Next() {
		var entry TurnUserSummary
		var minIndex int
		if err := rows.Scan(&entry.TurnIndex, &entry.Summary, &minIndex); err != nil {
			return nil, fmt.Errorf("store: scan turn user summary for %s: %w", threadID, err)
		}
		summaries = append(summaries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate turn user summaries for %s: %w", threadID, err)
	}
	return summaries, nil
}
