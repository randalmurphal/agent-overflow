package app

import (
	"fmt"
	"math"
	"time"

	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// Windowed history constants. `sliceAroundDefaultItems` and
// `paginationItems` size the active-pane loads so SubagentGroup collapse
// (a heavy subagent turn rolls up to one card) doesn't leave the rendered
// timeline visually truncated. `initialTurnWindow` is kept for the legacy
// ListRecentThreadItems RPC, while `maxWindowItems` is the shared DoS cap
// for the item-window bindings (the composer history-recall read carries
// its own row cap below).
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
//
//ao:scope threads:read
func (a *App) ListRecentThreadItems(threadID string, turnLimit int, inlinePreviews bool) (store.PagedItems, error) {
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
	return projectPage(paged, inlinePreviews, keepNewest), nil
}

// ListThreadSliceAround loads the bounded active-pane window around an anchor.
// Roughly `targetItemCount` items are returned (defaulting to
// sliceAroundDefaultItems when <= 0): half at-or-before and half after the
// anchor's item coordinate. When `anchorItemID` is "" or no longer exists,
// the function returns the tail `targetItemCount` items — the bottom-snapshot
// restore case.
//
//ao:scope threads:read
func (a *App) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount int, inlinePreviews bool) (store.PagedItems, error) {
	paged, err := a.store.ListThreadSliceAround(threadID, anchorItemID, clampSliceItemBudget(targetItemCount))
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list thread slice around: %w", err)
	}
	return projectPage(paged, inlinePreviews, keepNewest), nil
}

// clampSliceItemBudget normalizes a caller-supplied slice-window budget:
// non-positive takes the pane default, and anything larger than
// maxWindowItems is capped so an unintended LAN-attached caller can't
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

// ListItemsBeforeTurn is the legacy turn-floor pager. Active panes use
// ListItemsBeforeCursor so long single turns remain item-bounded. Keep this
// public compatibility surface item-bounded too: the synthetic cursor points
// below every possible index in beforeTurnIndex — head-healed prompts sit at
// NEGATIVE indexes, so index 0 is not the start of a turn — keeping the load
// strictly below that turn with a hard primary-row budget (mirror of
// ListItemsAfterTurn's MaxInt ceiling).
//
//ao:scope threads:read
func (a *App) ListItemsBeforeTurn(threadID string, beforeTurnIndex, itemBudget int, inlinePreviews bool) (store.PagedItems, error) {
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
	return projectPage(paged, inlinePreviews, keepNewest), nil
}

// ListItemsAfterTurn is the legacy turn-ceiling pager. Active panes use
// ListItemsAfterCursor so long single turns remain item-bounded. The synthetic
// cursor points at the end of afterTurnIndex, so rows strictly above that turn
// are loaded with a hard primary-row budget.
//
//ao:scope threads:read
func (a *App) ListItemsAfterTurn(threadID string, afterTurnIndex, itemBudget int, inlinePreviews bool) (store.PagedItems, error) {
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
		return projectPage(paged, inlinePreviews, keepOldest), nil
	}
	paged, err := a.store.ListItemsAfterCursor(
		threadID,
		store.TimelineCursor{TurnIndex: afterTurnIndex, ItemIndex: math.MaxInt},
		itemBudget,
	)
	if err != nil {
		return store.PagedItems{}, fmt.Errorf("list items after turn: %w", err)
	}
	return projectPage(paged, inlinePreviews, keepOldest), nil
}

// ListItemsBeforeCursor loads older items on demand, strictly before the
// frontend's current item-coordinate window floor. The item budget is a hard
// primary-row cap; render-support ancestors can be stitched in above it, but
// same-turn rows outside the cursor range stay omitted until explicitly paged.
//
//ao:scope threads:read
func (a *App) ListItemsBeforeCursor(threadID string, before store.TimelineCursor, itemBudget int, inlinePreviews bool) (store.PagedItems, error) {
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
	return projectPage(paged, inlinePreviews, keepNewest), nil
}

// ListItemsAfterCursor loads newer items on demand, strictly after the
// frontend's current item-coordinate window ceiling. It is the forward pager
// companion to ListItemsBeforeCursor.
//
//ao:scope threads:read
func (a *App) ListItemsAfterCursor(threadID string, after store.TimelineCursor, itemBudget int, inlinePreviews bool) (store.PagedItems, error) {
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
	return projectPage(paged, inlinePreviews, keepOldest), nil
}

// ListSubagentDescendants loads the full child transcript under a
// subagent launch row, on demand when its SubagentGroup card expands.
// History windows deliberately exclude rows with a parent_id (see
// internal/store/paging.go topLevelItemsFilter); this is the expansion
// path that hydrates them. The result is every visible transitive
// descendant in timeline order, capped store-side at the same scale as
// maxWindowItems (newest rows win) so an unintended LAN-attached caller
// can't stream an unbounded subtree per call.
//
//ao:scope threads:read
func (a *App) ListSubagentDescendants(threadID, rootItemID string, inlinePreviews bool) ([]store.Item, error) {
	items, err := a.store.ListSubagentDescendants(threadID, rootItemID)
	if err != nil {
		return nil, fmt.Errorf("list subagent descendants: %w", err)
	}
	return projectItemSlice(items, inlinePreviews, keepNewest), nil
}

// ListThreadProposedPlans returns the current proposed-plan item for a thread,
// outside the timeline window. It keeps the historical slice return shape for
// binding compatibility, but callers should treat it as 0-or-1 items.
//
//ao:scope threads:read
func (a *App) ListThreadProposedPlans(threadID string) ([]store.Item, error) {
	items, err := a.store.ListThreadProposedPlans(threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread proposed plans: %w", err)
	}
	// 0-or-1 plan rows, never a diff carrier: the projection is here so
	// no item reaches a client unprojected, not because these rows have
	// bytes to give up. Previews stay on for the same reason.
	return projectItemSlice(items, true, keepNewest), nil
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
//
//ao:scope threads:read
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
	// Running launches, so no completed diff previews to weigh: the
	// projection is here to keep the "no item reaches a client
	// unprojected" rule total, not for the bytes.
	return projectItemSlice(items, true, keepNewest), nil
}

// GetThreadUserMessageTicks returns every reader-authored user message
// in the thread (id + position), oldest first — the message-nav rail's
// baseline, covering the WHOLE thread rather than the loaded window.
// Wire-only context injections are excluded. A few bytes per row, read
// once per thread switch.
//
//ao:scope threads:read
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
//
//ao:scope threads:read
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
//
//ao:scope threads:read
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
//
//ao:scope threads:read
func (a *App) GetThreadItem(threadID, itemID string) (store.Item, error) {
	item, found, err := a.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return store.Item{}, fmt.Errorf("get thread item: %w", err)
	}
	if !found {
		return store.Item{}, nil
	}
	// One row, fetched to resolve a scroll target. Previews stay on:
	// there is no window here for them to crowd out.
	return itemwire.Project(item, true), nil
}

// ItemProjectionSource carries the complete STORED values of the three
// item fields the wire projection may shorten. It is the recovery route
// every marker the projection writes points at: the persisted record was
// never truncated, so a client that hits a marker can always ask for
// what it did not receive.
//
// All three ride one response because they are read together — an
// expanded diff card needs the patch text and the spans that highlight
// it — and because each is a few KB for a single item. The projection
// exists because 109 of them ride one window, not because any one of
// them is large.
type ItemProjectionSource struct {
	ItemID              string `json:"itemId"`
	Meta                string `json:"meta,omitempty"`
	PayloadMeta         string `json:"payloadMeta,omitempty"`
	PayloadPreviewSpans string `json:"payloadPreviewSpans,omitempty"`
}

// GetThreadItemProjectionSource returns one item's unprojected meta,
// payload meta and preview spans. Thread-scoped for the same reason
// GetThreadItem is: an item id must not be a key to arbitrary history.
// Returns the zero value when no row matches — callers check ItemID != "".
//
// Deliberately NOT routed through the projection: this IS the route out
// of it, and a projection applied here would make an elided field
// unrecoverable, which is exactly the failure remote-access.md §14 names
// (a promised on-demand endpoint that never arrived turns an accepted
// temporary loss into a permanent one).
//
//ao:scope threads:read
func (a *App) GetThreadItemProjectionSource(threadID, itemID string) (ItemProjectionSource, error) {
	item, found, err := a.store.GetThreadItem(threadID, itemID)
	if err != nil {
		return ItemProjectionSource{}, fmt.Errorf("get thread item projection source: %w", err)
	}
	if !found {
		return ItemProjectionSource{}, nil
	}
	return ItemProjectionSource{
		ItemID:              item.ID,
		Meta:                item.Meta,
		PayloadMeta:         item.PayloadMeta,
		PayloadPreviewSpans: item.PayloadPreviewSpans,
	}, nil
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
//
//ao:scope threads:read
func (a *App) ListRecentTurns(threadID string, limit int) ([]store.Turn, error) {
	return a.store.ListRecentTurns(threadID, limit)
}
