package store

import (
	"fmt"

	"github.com/google/uuid"
)

// CloneThreadItems copies the visible timeline items from sourceThreadID into
// targetThreadID, preserving turn ordering while assigning new item IDs.
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
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string) error {
	items, err := s.ListItems(sourceThreadID)
	if err != nil {
		return fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	for _, item := range items {
		if item.IsBackground && item.Status == "running" {
			continue
		}
		cloned := item
		cloned.ID = uuid.NewString()
		cloned.ThreadID = targetThreadID
		if err := s.InsertItem(cloned); err != nil {
			return fmt.Errorf("store: clone item %s into thread %s: %w", item.ID, targetThreadID, err)
		}
	}

	return nil
}
