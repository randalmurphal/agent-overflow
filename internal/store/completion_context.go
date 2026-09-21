package store

import "fmt"

// decorateCompletionLaunches carries a completion's launch as presentation
// context. It is not a timeline member: shipping it as a row would create
// holes in an activity run's loaded span and count the launch twice.
func (s *Store) decorateCompletionLaunches(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	ids := make([]string, 0)
	seen := make(map[string]bool)
	for i := range items {
		items[i].CompletionLaunch = nil
		item := items[i]
		if item.Kind == "tool_completion" && item.CompletionOf != "" && !seen[item.CompletionOf] {
			ids = append(ids, item.CompletionOf)
			seen[item.CompletionOf] = true
		}
	}
	if len(ids) == 0 {
		return items, nil
	}
	selection, args := idListSelection(ids)
	launches, err := queryHydratedTimelineItems(q, threadID, selection, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read completion context for %s: %w", threadID, err)
	}
	byID := make(map[string]*Item, len(launches))
	for i := range launches {
		byID[launches[i].ID] = &launches[i]
	}
	for i := range items {
		if items[i].Kind == "tool_completion" {
			items[i].CompletionLaunch = byID[items[i].CompletionOf]
		}
	}
	return items, nil
}
