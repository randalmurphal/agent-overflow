package store

import (
	"fmt"

	"github.com/google/uuid"
)

// CloneThreadItems copies the visible timeline items from sourceThreadID into
// targetThreadID, preserving turn ordering while assigning new item IDs.
//
// When throughTurnIndex is non-nil, only items whose turn_index is <= *throughTurnIndex
// are copied — used for fork-at-point so the forked thread starts truncated
// at the chosen turn. nil means clone every turn (existing fork-at-tail
// behavior).
//
// Rows with `is_background=1 AND status='running'` are SKIPPED: those
// point at PTYs / subagents owned by the source session's provider
// subprocess, and the fork gets its own subprocess that can never reach
// them. Copying them would strand the forked thread with ghost rows
// that can never complete. The parent thread is untouched — its
// backgrounded launches keep running under its own session.
//
// Completed backgrounded rows and non-background running rows copy
// normally; the filter is deliberately narrow.
//
// All inserts run in a single transaction so a 200-row clone takes one
// fsync instead of 200. Per-row InsertItem would commit individually
// and dominate the fork wall-clock for large threads.
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string, throughTurnIndex *int) error {
	items, err := s.ListItems(sourceThreadID)
	if err != nil {
		return fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin clone items tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("store: prepare clone insert: %w", err)
	}
	defer stmt.Close()

	var maxUpdatedAt int64
	cloned := 0
	for _, item := range items {
		if item.IsBackground && item.Status == "running" {
			continue
		}
		if throughTurnIndex != nil && item.TurnIndex > *throughTurnIndex {
			continue
		}
		// Items returned by ListItems are already defaulted by the
		// store's CHECK constraints — no need to re-default per row.
		item.ID = uuid.NewString()
		item.ThreadID = targetThreadID

		if _, err := stmt.Exec(
			item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex,
			item.Kind, item.Role, item.Status, item.Summary,
			nilIfEmpty(item.PayloadID), item.ParentID,
			boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
			item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return fmt.Errorf("store: clone item into thread %s: %w", targetThreadID, err)
		}
		if item.UpdatedAt > maxUpdatedAt {
			maxUpdatedAt = item.UpdatedAt
		}
		cloned++
	}

	// Touch the destination thread's updated_at once at the end (mirrors
	// per-row InsertItem's touch semantics, batched).
	if cloned > 0 {
		if _, err := tx.Exec(`UPDATE threads SET updated_at = ? WHERE id = ?`, maxUpdatedAt, targetThreadID); err != nil {
			return fmt.Errorf("store: touch fork thread %s updated_at: %w", targetThreadID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit clone items tx: %w", err)
	}
	return nil
}
