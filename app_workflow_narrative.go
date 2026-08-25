package main

import (
	"errors"
)

// workflowAssistantTextKind is the item kind a provider's prose is persisted
// under. The literal matches the triage write path's own constant; store exports
// no item-kind vocabulary, and inventing one for a single reader would widen
// that package's surface for nothing.
const workflowAssistantTextKind = "assistant_text"

// threadAssistantTexts returns one thread's top-level assistant prose, oldest
// first. Subagent rows (`parent_id` set) are excluded: an element's narrative is
// what it said, not what something it launched said.
func (a *App) threadAssistantTexts(threadID string) ([]string, error) {
	if threadID == "" {
		return nil, errors.New("thread id is required")
	}
	items, err := a.store.ListItems(threadID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, 4)
	for _, item := range items {
		if item.Kind != workflowAssistantTextKind || item.ParentID != "" || item.Summary == "" {
			continue
		}
		texts = append(texts, item.Summary)
	}
	return texts, nil
}
