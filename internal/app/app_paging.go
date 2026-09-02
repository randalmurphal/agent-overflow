package app

import (
	"fmt"
	"time"

	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// Windowed history constants. `sliceAroundDefaultItems` and
// `paginationItems` size the active-pane loads so SubagentGroup collapse
// (a heavy subagent turn rolls up to one card) doesn't leave the rendered
// timeline visually truncated. `maxWindowItems` is the shared DoS cap for
// the item-window bindings (the composer history-recall read carries its
// own row cap below).
const (
	maxWindowItems = 2000

	// paginationItems is the default item budget for an explicit
	// "load older" page: the cursor pagers select this many visible
	// top-level rows strictly outside the caller's current window. One
	// click = ~this many items prepended, regardless of per-turn
	// density.
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

// ListThreadSliceAround loads the bounded active-pane window around an anchor.
// Roughly `targetItemCount` items are returned (defaulting to
// sliceAroundDefaultItems when <= 0): half at-or-before and half after the
// anchor's item coordinate. When `anchorItemID` is "" or no longer exists,
// the function returns the tail `targetItemCount` items — the bottom-snapshot
// restore case.
func (a *App) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount int) (store.PagedItems, error) {
	paged, err := a.store.ListThreadSliceAround(threadID, anchorItemID, clampSliceItemBudget(targetItemCount))
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list thread slice around: %w", err)
	}
	paged.Items = slicesx.OrEmpty(paged.Items)
	return paged, nil
}

// clampSliceItemBudget normalizes a caller-supplied slice-window budget:
// non-positive takes the pane default, and anything larger than
// maxWindowItems is capped so a malicious LAN-attached caller can't
// request a slice covering the whole thread and OOM the process. Active
// panes stay on this bounded slice surface and page older/newer history
// explicitly. Shared by ListThreadSliceAround and SyncThreadWindow, which
// return the same window and must therefore bound it identically.
func clampSliceItemBudget(targetItemCount int) int {
	if targetItemCount <= 0 {
		return sliceAroundDefaultItems
	}
	if targetItemCount > maxWindowItems {
		return maxWindowItems
	}
	return targetItemCount
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
// BackgroundTaskTray can render without scanning `pane.items`. The store
// leg lists by BACKGROUNDED ANCESTRY, not top-level-ness (invariant 24):
// nested background launches and the agent launches between them and a
// background root are included, so the tray can indent by walking
// parentId within the result. SQLite
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
	codexSubagents, err := a.store.ListLiveCodexSubagentLaunchesForTray(threadID)
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

// GetThreadUserMessageTicks returns every reader-authored user message
// in the thread (id + position), oldest first — the message-nav rail's
// baseline, covering the WHOLE thread rather than the loaded window.
// Wire-only context injections are excluded. A few bytes per row, read
// once per thread switch.
func (a *App) GetThreadUserMessageTicks(threadID string) ([]store.UserMessageTick, error) {
	ticks, err := a.store.ListThreadUserMessageTicks(threadID)
	if err != nil {
		return nil, fmt.Errorf("get thread user message ticks: %w", err)
	}
	return ticks, nil
}

// Composer history-recall read bounds: how many past messages one
// ArrowUp session can walk by default, and the cap a caller-supplied
// limit is clamped to. The cap bounds the ROW count only — bodies are
// deliberately uncapped (a recalled message is re-sent verbatim), so a
// call's size is bounded by how much prose those rows hold.
const (
	userMessageHistoryDefaultLimit = 50
	userMessageHistoryMaxLimit     = 200
)

// GetThreadUserMessageHistory returns the thread's newest reader-authored
// user messages with their full text, newest first — the composer's
// ArrowUp history-recall baseline, which the frontend merges with the
// loaded window's live rows and the pending send queue. Wire-only
// context injections and subagent child prompts are excluded.
func (a *App) GetThreadUserMessageHistory(threadID string, limit int) ([]store.UserMessageHistoryEntry, error) {
	if limit <= 0 {
		limit = userMessageHistoryDefaultLimit
	}
	if limit > userMessageHistoryMaxLimit {
		limit = userMessageHistoryMaxLimit
	}
	entries, err := a.store.ListThreadUserMessageHistory(threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("get thread user message history: %w", err)
	}
	return slicesx.OrEmpty(entries), nil
}

// GetThreadTurnPreview resolves the nav rail's hover card for a turn
// whose rows are not loaded in the frontend window: the reader's ask
// plus the turn's final top-level assistant reply, both rune-capped at
// the wire. Returns the zero preview when the item is not a
// reader-authored user message on this thread — callers check
// userText != "".
func (a *App) GetThreadTurnPreview(threadID, itemID string) (store.TurnPreview, error) {
	preview, found, err := a.store.ThreadTurnPreview(threadID, itemID)
	if err != nil {
		return store.TurnPreview{}, fmt.Errorf("get thread turn preview: %w", err)
	}
	if !found {
		return store.TurnPreview{}, nil
	}
	return preview, nil
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

// ListRecentTurns returns the N most recent turn records for the given
// thread, newest first. Used by the frontend on thread-switch to rehydrate
// the latest settled-turn projection.
//
// The frontend MUST NOT light up the working indicator from these rows —
// an in-flight (completed_at=NULL) row from a prior session/crash is
// historical, not live. Only a fresh `provider:turn_started` push can set
// `pane.activeTurn`. See docs/architecture/invariants.md #22 and
// docs/architecture/turn-lifecycle.md §Frontend state shape.
func (a *App) ListRecentTurns(threadID string, limit int) ([]store.Turn, error) {
	return a.store.ListRecentTurns(threadID, limit)
}
