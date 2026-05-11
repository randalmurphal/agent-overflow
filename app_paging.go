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

	// sliceAroundDefaultItems is the target size for the phase-1
	// fast-path slice loaded on thread switch. ~50 items covers a
	// desktop-sized viewport (10–15 items) plus enough overscan above
	// and below for virtua's measurement loop to land cleanly on the
	// bottom or anchor. Phase 2 always re-runs the full
	// initialTurnWindow load, so undershooting here is corrected within
	// a frame.
	sliceAroundDefaultItems = 50

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
// rows cover persisted Claude / Codex subagent launches; the triage
// router appends transient Codex unified-exec tasks that intentionally
// do not exist in chat history. Pending Codex unifiedExec launches
// surface here before they are known to be backgrounded.
//
// For Claude background launches whose host process has exited but
// whose chat sibling has not landed yet (the gap between
// `system/task_updated` and the agent observation event), the store
// also returns synthetic `tool_completion` items derived from the
// `pending_background_task_terminals` stash — mirroring the Codex
// tracker pattern so the tray reflects process state immediately
// without waiting for the chat-side write. When the real sibling
// eventually lands the synthetic stops being returned.
func (a *App) ListLiveBackgroundTasks(threadID string) ([]store.Item, error) {
	now := time.Now().UnixMilli()
	cutoff := now - backgroundTaskRetentionMillis
	items, err := a.store.ListLiveBackgroundTasks(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list live background tasks: %w", err)
	}
	pending, err := a.store.ListPendingBackgroundCompletionsAsItems(threadID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list pending background completions: %w", err)
	}
	items = append(items, pending...)
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

// normalizePagedItems forces a nil Items slice to an empty slice so the
// JSON shape the frontend consumes is stable. Matches the nil→[]
// normalization SearchThreadMessages does in app_search.go.
func normalizePagedItems(p store.PagedItems) store.PagedItems {
	if p.Items == nil {
		p.Items = []store.Item{}
	}
	return p
}
