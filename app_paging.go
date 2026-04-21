package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/store"
)

// Windowed history constants. Tuned for the common chat-thread shape:
// 50 turns covers most continuous sessions, min 500 items keeps short
// burst threads from paying a round-trip per Q&A, max 2000 items caps
// memory when a single agent turn produces hundreds of rows.
const (
	initialTurnWindow = 50
	minWindowItems    = 500
	maxWindowItems    = 2000
	paginationTurns   = 50

	// backgroundTaskRetentionMillis matches
	// COMPLETION_RETENTION_MS in BackgroundTaskTray.svelte. Completed
	// background rows older than this cutoff don't appear in the tray
	// feed; the backend enforces the same rule so the tray can be a
	// thin renderer over the server response instead of maintaining
	// its own time-windowed state.
	backgroundTaskRetentionMillis = 2000
)

// ListRecentThreadItems loads the tail of a thread's history into the
// timeline pane: the last `turnLimit` turns (defaulting to
// initialTurnWindow when <= 0) plus enough surrounding turns to keep
// the total item count in [minWindowItems, maxWindowItems], plus any
// subagent ancestors those items reference. This is the binding the
// frontend calls on thread switch.
func (a *App) ListRecentThreadItems(threadID string, turnLimit int) (store.PagedItems, error) {
	if turnLimit <= 0 {
		turnLimit = initialTurnWindow
	}
	floor, _, err := a.store.PickInitialFloorTurn(threadID, turnLimit, minWindowItems, maxWindowItems)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list recent thread items: %w", err)
	}
	if floor < 0 {
		// Empty thread — no items, no turns.
		return store.PagedItems{Items: []store.Item{}, OldestTurnIndex: -1, HasMore: false}, nil
	}
	paged, err := a.store.ListRecentItemsWithAncestors(threadID, floor)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list recent thread items: %w", err)
	}
	return normalizePagedItems(paged), nil
}

// ListItemsBeforeTurn loads older turns on demand, strictly below
// `beforeTurnIndex` (the frontend's current window floor). `turnLimit`
// defaults to paginationTurns when <= 0.
func (a *App) ListItemsBeforeTurn(threadID string, beforeTurnIndex, turnLimit int) (store.PagedItems, error) {
	if turnLimit <= 0 {
		turnLimit = paginationTurns
	}
	paged, err := a.store.ListItemsBeforeTurn(threadID, beforeTurnIndex, turnLimit)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items before turn: %w", err)
	}
	return normalizePagedItems(paged), nil
}

// ListThreadProposedPlans returns every proposed-plan item for a thread,
// newest-first. Backs PlanSidebar, which needs the full plan history
// regardless of the timeline window.
func (a *App) ListThreadProposedPlans(threadID string) ([]store.Item, error) {
	items, err := a.store.ListThreadProposedPlans(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread proposed plans: %w", err)
	}
	if items == nil {
		return []store.Item{}, nil
	}
	return items, nil
}

// ListThreadDiffPayloads returns every diff or tool_result item for a
// thread so the diff panel's cumulative view can summarize the entire
// thread independent of the timeline window.
func (a *App) ListThreadDiffPayloads(threadID string) ([]store.Item, error) {
	items, err := a.store.ListThreadDiffPayloads(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread diff payloads: %w", err)
	}
	if items == nil {
		return []store.Item{}, nil
	}
	return items, nil
}

// ListLiveBackgroundTasks returns running background launches plus
// recently-completed completions (within the tray retention window) so
// the BackgroundTaskTray can render without scanning `pane.items`.
func (a *App) ListLiveBackgroundTasks(threadID string) ([]store.Item, error) {
	cutoff := time.Now().UnixMilli() - backgroundTaskRetentionMillis
	items, err := a.store.ListLiveBackgroundTasks(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list live background tasks: %w", err)
	}
	if items == nil {
		return []store.Item{}, nil
	}
	return items, nil
}

// GetThreadItem returns a single item by id, scoped to a thread for
// safety so a compromised caller can't read arbitrary thread history
// via id enumeration. Used by the frontend's scroll-to-item flow to
// discover an out-of-window item's turn_index before loading back.
// Returns the zero Item when no row matches — callers check ID != "".
func (a *App) GetThreadItem(threadID, itemID string) (store.Item, error) {
	item, found, err := a.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return store.Item{}, fmt.Errorf("get thread item: %w", err)
	}
	if !found {
		return store.Item{}, nil
	}
	return item, nil
}

// normalizePagedItems forces a nil Items slice to an empty slice so the
// JSON shape the frontend consumes is stable. Matches the nil→[]
// normalization SearchThreadMessages does in app_search.go.
func normalizePagedItems(p store.PagedItems) store.PagedItems {
	if p.Items == nil {
		p.Items = []store.Item{}
	}
	return p
}
