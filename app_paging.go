package main

import (
	"fmt"
	"time"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// Windowed history constants. `sliceAroundDefaultItems` and
// `paginationItems` size the user-facing loads so SubagentGroup
// collapse (a heavy subagent turn rolls up to one card) doesn't leave
// the rendered timeline visually truncated. `initialTurnWindow` /
// `minWindowItems` / `maxWindowItems` exist for the legacy
// `ListRecentThreadItems` transport-gap-recovery probe only.
const (
	initialTurnWindow = 50
	minWindowItems    = 500
	maxWindowItems    = 2000

	// paginationItems is the default item budget for an explicit
	// "load older" page. The backend walks turns DESC accumulating
	// item counts (excluding plan_update notifications) until
	// cumulative ≥ this budget, then returns that turn's items plus
	// every newer one strictly below the caller's floor. One click =
	// ~this many items prepended, regardless of per-turn density.
	paginationItems = 200

	// sliceAroundDefaultItems is the target size for the slice loaded
	// on thread switch. 200 items covers a desktop viewport
	// (10–15 rendered cards) with several screens of overscan, and is
	// large enough that one heavy subagent turn collapsing to a single
	// card doesn't leave the timeline visually empty.
	sliceAroundDefaultItems = 200

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
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListThreadSliceAround loads a small slice of items around an anchor
// for the phase-1 fast path on thread switch. Roughly `targetItemCount`
// items are returned (defaulting to sliceAroundDefaultItems when <= 0):
// half above and half below the anchor's turn position. When
// `anchorItemID` is "" or no longer exists, the function returns the
// tail `targetItemCount` items — the bottom-snapshot restore case.
//
// Phase 2 of the thread switch always re-runs `ListRecentThreadItems`
// to fill in the full window; this binding exists to paint the visible
// viewport quickly while phase 2 runs in parallel.
func (a *App) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount int) (store.PagedItems, error) {
	if targetItemCount <= 0 {
		targetItemCount = sliceAroundDefaultItems
	}
	// Cap at maxWindowItems so a malicious LAN-attached caller can't
	// request a slice covering the whole thread and OOM the process.
	// Phase 2 (ListRecentThreadItems) is the right surface for the full
	// window; this binding is for a viewport-sized fast path.
	if targetItemCount > maxWindowItems {
		targetItemCount = maxWindowItems
	}
	paged, err := a.store.ListThreadSliceAround(threadID, anchorItemID, targetItemCount)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list thread slice around: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListItemsBeforeTurn loads older items on demand, strictly below
// `beforeTurnIndex` (the frontend's current window floor). The third
// parameter is an **item budget**: the backend walks turns DESC,
// summing each turn's item count (excluding plan_update notifications),
// and stops at the first turn that pushes cumulative ≥ itemBudget.
// Defaults to paginationItems when <= 0, capped at maxWindowItems to
// defend against a malicious LAN-attached caller asking for the whole
// thread in one round-trip.
func (a *App) ListItemsBeforeTurn(threadID string, beforeTurnIndex, itemBudget int) (store.PagedItems, error) {
	if itemBudget <= 0 {
		itemBudget = paginationItems
	}
	if itemBudget > maxWindowItems {
		itemBudget = maxWindowItems
	}
	paged, err := a.store.ListItemsBeforeTurn(threadID, beforeTurnIndex, itemBudget)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items before turn: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListThreadProposedPlans returns the current proposed-plan item for a thread,
// outside the timeline window. It keeps the historical slice return shape for
// binding compatibility, but callers should treat it as 0-or-1 items.
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

// ListLiveBackgroundTasks returns running launches plus their
// recently-completed siblings (within the tray retention window) so the
// BackgroundTaskTray can render without scanning `pane.items`. SQLite
// rows cover persisted Claude launches and Codex subagent launches; the
// latter are projected as running tray rows while the chat-history spawn
// card remains completed. The triage router appends transient Codex
// unified-exec tasks that intentionally do not exist in chat history.
// Pending Codex unifiedExec launches surface here before they are known
// to be backgrounded.
func (a *App) ListLiveBackgroundTasks(threadID string) ([]store.Item, error) {
	now := time.Now().UnixMilli()
	cutoff := now - backgroundTaskRetentionMillis
	items, err := a.store.ListLiveBackgroundTasks(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list live background tasks: %w", err)
	}
	codexSubagents, err := a.store.ListLiveCodexSubagentLaunches(threadID)
	if err != nil {
		return nil, fmt.Errorf("list live Codex subagent launches: %w", err)
	}
	for _, item := range codexSubagents {
		item.Status = "running"
		items = append(items, item)
	}
	if a.triage != nil {
		items = append(items, a.triage.ListLiveCodexBackgroundTasks(threadID, now, cutoff)...)
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
