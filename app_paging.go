package main

import (
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// Windowed history constants. `sliceAroundDefaultItems` and
// `paginationItems` size the active-pane loads so SubagentGroup collapse
// (a heavy subagent turn rolls up to one card) doesn't leave the rendered
// timeline visually truncated. `initialTurnWindow` is kept for the legacy
// ListRecentThreadItems RPC, while `maxWindowItems` is the shared DoS cap
// for every history binding.
const (
	initialTurnWindow = 50
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

// ListRecentThreadItems loads a broad recent tail window. Active chat panes
// use ListThreadSliceAround for bounded switch/refresh loads; this method is
// retained for legacy callers and any future full-tail refresh surfaces.
func (a *App) ListRecentThreadItems(threadID string, turnLimit int) (store.PagedItems, error) {
	if turnLimit <= 0 {
		turnLimit = initialTurnWindow
	}
	// 500: floor on accumulated item count so an unusually sparse tail
	// (lots of empty turns, one-line plan_update bursts) still loads a
	// useful viewport.
	floor, _, err := a.store.PickInitialFloorTurn(threadID, turnLimit, 500, maxWindowItems)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list recent thread items: %w", err)
	}
	if floor < 0 {
		// Empty thread — no items, no turns.
		return store.PagedItems{
			Items:           []store.Item{},
			OldestCursor:    store.TimelineCursor{TurnIndex: -1, ItemIndex: -1},
			NewestCursor:    store.TimelineCursor{TurnIndex: -1, ItemIndex: -1},
			OldestTurnIndex: -1,
			NewestTurnIndex: -1,
			HasMore:         false,
			HasMoreOlder:    false,
			HasMoreNewer:    false,
		}, nil
	}
	paged, err := a.store.ListRecentItems(threadID, floor)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list recent thread items: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListThreadSliceAround loads the bounded active-pane window around an anchor.
// Roughly `targetItemCount` items are returned (defaulting to
// sliceAroundDefaultItems when <= 0): half at-or-before and half after the
// anchor's item coordinate. When `anchorItemID` is "" or no longer exists,
// the function returns the tail `targetItemCount` items — the bottom-snapshot
// restore case.
func (a *App) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount int) (store.PagedItems, error) {
	if targetItemCount <= 0 {
		targetItemCount = sliceAroundDefaultItems
	}
	// Cap at maxWindowItems so a malicious LAN-attached caller can't
	// request a slice covering the whole thread and OOM the process.
	// Active panes should stay on this bounded slice surface and page
	// older/newer history explicitly.
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

// ListItemsBeforeTurn is the legacy turn-floor pager. Active panes use
// ListItemsBeforeCursor so long single turns remain item-bounded. Keep this
// public compatibility surface item-bounded too: the synthetic cursor points
// below every possible index in beforeTurnIndex — head-healed prompts sit at
// NEGATIVE indexes, so index 0 is not the start of a turn — keeping the load
// strictly below that turn with a hard primary-row budget (mirror of
// ListItemsAfterTurn's MaxInt ceiling).
func (a *App) ListItemsBeforeTurn(threadID string, beforeTurnIndex, itemBudget int) (store.PagedItems, error) {
	if itemBudget <= 0 {
		itemBudget = paginationItems
	}
	if itemBudget > maxWindowItems {
		itemBudget = maxWindowItems
	}
	paged, err := a.store.ListItemsBeforeCursor(
		threadID,
		store.TimelineCursor{TurnIndex: beforeTurnIndex, ItemIndex: math.MinInt},
		itemBudget,
	)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items before turn: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListItemsAfterTurn is the legacy turn-ceiling pager. Active panes use
// ListItemsAfterCursor so long single turns remain item-bounded. The synthetic
// cursor points at the end of afterTurnIndex, so rows strictly above that turn
// are loaded with a hard primary-row budget.
func (a *App) ListItemsAfterTurn(threadID string, afterTurnIndex, itemBudget int) (store.PagedItems, error) {
	if itemBudget <= 0 {
		itemBudget = paginationItems
	}
	if itemBudget > maxWindowItems {
		itemBudget = maxWindowItems
	}
	if afterTurnIndex < 0 {
		paged, err := a.store.ListItemsAfterTurn(threadID, afterTurnIndex, itemBudget)
		if err != nil {
			return store.PagedItems{}, fmt.Errorf("list items after turn: %w", err)
		}
		paged.Items = slicesx.OrEmpty(paged.Items)
		return paged, nil
	}
	paged, err := a.store.ListItemsAfterCursor(
		threadID,
		store.TimelineCursor{TurnIndex: afterTurnIndex, ItemIndex: math.MaxInt},
		itemBudget,
	)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items after turn: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListItemsBeforeCursor loads older items on demand, strictly before the
// frontend's current item-coordinate window floor. The item budget is a hard
// primary-row cap; render-support ancestors can be stitched in above it, but
// same-turn rows outside the cursor range stay omitted until explicitly paged.
func (a *App) ListItemsBeforeCursor(threadID string, before store.TimelineCursor, itemBudget int) (store.PagedItems, error) {
	if itemBudget <= 0 {
		itemBudget = paginationItems
	}
	if itemBudget > maxWindowItems {
		itemBudget = maxWindowItems
	}
	paged, err := a.store.ListItemsBeforeCursor(threadID, before, itemBudget)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items before cursor: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListItemsAfterCursor loads newer items on demand, strictly after the
// frontend's current item-coordinate window ceiling. It is the forward pager
// companion to ListItemsBeforeCursor.
func (a *App) ListItemsAfterCursor(threadID string, after store.TimelineCursor, itemBudget int) (store.PagedItems, error) {
	if itemBudget <= 0 {
		itemBudget = paginationItems
	}
	if itemBudget > maxWindowItems {
		itemBudget = maxWindowItems
	}
	paged, err := a.store.ListItemsAfterCursor(threadID, after, itemBudget)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items after cursor: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// ListSubagentDescendants loads the full child transcript under a
// subagent launch row, on demand when its SubagentGroup card expands.
// History windows deliberately exclude rows with a parent_id (see
// internal/store/paging.go topLevelItemsFilter); this is the expansion
// path that hydrates them. The result is every visible transitive
// descendant in timeline order, capped store-side at the same scale as
// maxWindowItems (newest rows win) so a malicious LAN-attached caller
// can't stream an unbounded subtree per call.
func (a *App) ListSubagentDescendants(threadID, rootItemID string) ([]store.Item, error) {
	items, err := a.store.ListSubagentDescendants(threadID, rootItemID)
	if err != nil {
		return nil, fmt.Errorf("list subagent descendants: %w", err)
	}
	return slicesx.OrEmpty(items), nil
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
