package store

import (
	"fmt"

	"github.com/google/uuid"
)

// CloneThreadItems copies the visible timeline items from sourceThreadID into
// targetThreadID, preserving turn ordering while assigning new item IDs.
func (s *Store) CloneThreadItems(sourceThreadID, targetThreadID string) error {
	items, err := s.ListItems(sourceThreadID)
	if err != nil {
		return fmt.Errorf("store: list source items for fork %s: %w", sourceThreadID, err)
	}

	for _, item := range items {
		cloned := item
		cloned.ID = uuid.NewString()
		cloned.ThreadID = targetThreadID
		if err := s.InsertItem(cloned); err != nil {
			return fmt.Errorf("store: clone item %s into thread %s: %w", item.ID, targetThreadID, err)
		}
	}

	return nil
}
