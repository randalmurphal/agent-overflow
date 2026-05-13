package store

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// BuildForkedThread returns a Thread row populated from source plus the
// fork-only fields: a fresh UUID, a "(fork)"-suffixed title, the
// `ForkedFromThreadID` linkage, and a `created_at` / `updated_at` pair
// at the current millisecond. The session-state fields
// (`SessionRef`, `PendingForkRef`) are left empty — the app-side fork
// saga sets them once the provider-specific resume reference is known.
// AutoCompactStandard/Extended Percent are intentionally NOT copied —
// a fork starts with zero overrides so it picks up the live Settings
// value on the first session start (the same default-resolution path a
// brand-new thread follows).
//
// Pure: this only builds the row. The caller persists it (CreateThread)
// and pairs it with the side-effecting clone steps.
func BuildForkedThread(source Thread) Thread {
	now := time.Now().UnixMilli()
	return Thread{
		ID:                 uuid.NewString(),
		ProjectID:          source.ProjectID,
		Title:              source.Title + " (fork)",
		Provider:           source.Provider,
		WorkspacePath:      source.WorkspacePath,
		Model:              source.Model,
		WorktreePath:       source.WorktreePath,
		Branch:             source.Branch,
		Mode:               source.Mode,
		ReasoningEffort:    source.ReasoningEffort,
		FastMode:           source.FastMode,
		ContextWindow:      source.ContextWindow,
		RuntimeMode:        source.RuntimeMode,
		ForkedFromThreadID: source.ID,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
}

// CloneThreadItems copies the visible timeline items from sourceThreadID into
// targetThreadID, preserving turn ordering while assigning new item IDs. The
// returned map is source item id -> cloned item id for every copied row.
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
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string, throughTurnIndex *int) (map[string]string, error) {
	items, err := s.ListItems(sourceThreadID)
	if err != nil {
		return nil, fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	clonedItems := make([]Item, 0, len(items))
	idMap := make(map[string]string, len(items))
	for _, item := range items {
		if item.IsBackground && item.Status == "running" {
			continue
		}
		if throughTurnIndex != nil && item.TurnIndex > *throughTurnIndex {
			continue
		}
		oldID := item.ID
		item.ID = uuid.NewString()
		item.ThreadID = targetThreadID
		idMap[oldID] = item.ID
		clonedItems = append(clonedItems, item)
	}
	for i := range clonedItems {
		if next, ok := idMap[clonedItems[i].ParentID]; ok {
			clonedItems[i].ParentID = next
		}
		if next, ok := idMap[clonedItems[i].CompletionOf]; ok {
			clonedItems[i].CompletionOf = next
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("store: begin clone items tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO items (id, thread_id, turn_index, item_index, kind, role, status, summary,
		    payload_id, input_payload_id, parent_id, is_background, completion_of, tool_name, decision, meta,
		    created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return nil, fmt.Errorf("store: prepare clone insert: %w", err)
	}
	defer stmt.Close()

	var maxUpdatedAt int64
	cloned := 0
	for _, item := range clonedItems {
		if _, err := stmt.Exec(
			item.ID, item.ThreadID, item.TurnIndex, item.ItemIndex,
			item.Kind, item.Role, item.Status, item.Summary,
			nilIfEmpty(item.PayloadID), nilIfEmpty(item.InputPayloadID), item.ParentID,
			boolToInt(item.IsBackground), item.CompletionOf, item.ToolName, item.Decision, item.Meta,
			item.CreatedAt, item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: clone item into thread %s: %w", targetThreadID, err)
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
			return nil, fmt.Errorf("store: touch fork thread %s updated_at: %w", targetThreadID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit clone items tx: %w", err)
	}
	return idMap, nil
}
