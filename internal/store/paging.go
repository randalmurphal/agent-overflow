package store

import (
	"fmt"
)

// visibleItemsFilterFor builds the WHERE-clause fragment shared by every
// read path that walks the timeline, qualified with the given table
// alias ("", "items.", "i."). plan_update notifications are excluded
// because the frontend renders them out of band (live plan store) —
// counting them against item budgets would systematically under-deliver
// visible content, and a thread whose only sub-floor rows are plan_update
// notifications would otherwise flash a "Load older messages" button
// that loads zero rows. One template so the aliased copies (needed
// because some queries LEFT JOIN `payloads`, which also has a `kind`
// column) cannot drift when an excluded kind is added.
func visibleItemsFilterFor(alias string) string {
	return fmt.Sprintf("NOT (%[1]skind = 'notification' AND %[1]stool_name = 'plan_update')", alias)
}

var visibleItemsFilter = visibleItemsFilterFor("")

// topLevelItemsFilterFor restricts a timeline read to top-level rows.
// Subagent children (rows with a non-empty parent_id) are deliberately
// not part of any history window, budget, or pagination probe: they
// render inside their anchor's SubagentGroup card, load on demand via
// ListSubagentDescendants when the card expands, and are summarised on
// the collapsed card by decorateSubagentAnchors. Counting them against
// windows used to make one subagent-heavy turn eat the entire item
// budget and flash "Load older messages" for rows that would never
// render as timeline rows.
//
// The aliased form exists for the same reason visibleItemsFilterFor's
// does: a read written as physical timeline arms (timeline_arms.go) has
// a second table in scope and must qualify every column.
func topLevelItemsFilterFor(alias string) string {
	return alias + "parent_id = ''"
}

var topLevelItemsFilter = topLevelItemsFilterFor("")

// windowedTimelineFilter is the predicate pair every history window,
// budget, and probe shares — visible rows, top-level only — qualified
// for the physical timeline arms (timeline_arms.go), which always alias
// the row source `items`.
var windowedTimelineFilter = visibleItemsFilterFor("items.") + `
		   AND ` + topLevelItemsFilterFor("items.")

// TimelineCursor is a stable position in a thread timeline. The item id is
// carried for diagnostics/snapshot readability; ordering is by
// (turn_index, item_index), which is the store's unique timeline coordinate.
type TimelineCursor struct {
	TurnIndex int    `json:"turnIndex"`
	ItemIndex int    `json:"itemIndex"`
	ItemID    string `json:"itemId"`
}

// PagedItems is the return shape for windowed item loads. `Items` is sorted
// by (turn_index, item_index) ASC so callers can append or replace the slice
// directly in a timeline. `OldestCursor` / `NewestCursor` are the inclusive
// item-coordinate bounds of the page. Cursor turn/item indexes are -1 when
// `Items` is empty.
//
// `OldestTurnIndex` / `NewestTurnIndex` are legacy turn-only aliases derived
// from the cursors. Active-pane callers should use the cursor fields so one
// dense turn cannot punch through the item window cap.
//
// HasMoreOlder / HasMoreNewer report whether visible top-level items exist
// outside the cursor bounds. HasMore is the legacy older-history alias kept
// for frontend and transport compatibility while callers migrate to the
// explicit names.
type PagedItems struct {
	Items           []Item         `json:"items"`
	OldestCursor    TimelineCursor `json:"oldestCursor"`
	NewestCursor    TimelineCursor `json:"newestCursor"`
	OldestTurnIndex int            `json:"oldestTurnIndex"`
	NewestTurnIndex int            `json:"newestTurnIndex"`
	HasMore         bool           `json:"hasMore"`
	HasMoreOlder    bool           `json:"hasMoreOlder"`
	HasMoreNewer    bool           `json:"hasMoreNewer"`
}

// ListItemsBeforeCursor loads older visible top-level items strictly
// before `before` until `itemBudget` rows have been selected.
func (s *Store) ListItemsBeforeCursor(threadID string, before TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(before) {
		return emptyPagedItems(), nil
	}
	selectedSQL, selectedArgs := timelineIDSelection(threadID, timelineSelection{
		Where: windowedTimelineFilter + `
		   AND (items.turn_index < ? OR (items.turn_index = ? AND items.item_index < ?))`,
		WhereArgs: []any{before.TurnIndex, before.TurnIndex, before.ItemIndex},
		OrderBy:   "turn_index DESC, item_index DESC",
		Limit:     itemBudget,
	})
	q := s.reader()
	items, err := s.querySelectedPagedItems(q, threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(q, threadID, items)
}

// ListItemsAfterCursor loads newer visible top-level items strictly
// after `after` until `itemBudget` rows have been selected. It is the
// forward pager companion to ListItemsBeforeCursor.
func (s *Store) ListItemsAfterCursor(threadID string, after TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(after) {
		return emptyPagedItems(), nil
	}
	selectedSQL, selectedArgs := timelineIDSelection(threadID, timelineSelection{
		Where: windowedTimelineFilter + `
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`,
		WhereArgs: []any{after.TurnIndex, after.TurnIndex, after.ItemIndex},
		OrderBy:   "turn_index ASC, item_index ASC",
		Limit:     itemBudget,
	})
	q := s.reader()
	items, err := s.querySelectedPagedItems(q, threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(q, threadID, items)
}

// querySelectedPagedItems runs the cursor-based paging shape used by
// active panes. `selectedSQL` must return a single `id` column
// containing the page's top-level row ids; the outer query hydrates and
// orders them for rendering.
func (s *Store) querySelectedPagedItems(q sqlQueryer, threadID, selectedSQL string, selectedArgs ...any) ([]Item, error) {
	items, err := queryHydratedTimelineItems(q, threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: query selected paged items for %s: %w", threadID, err)
	}
	return s.decoratePagedItems(q, threadID, items)
}

// decoratePagedItems applies the read-time meta decorations every
// frontend-bound window needs: proposed-plan state and subagent anchor
// aggregates (descendant count + collapsed-card preview).
func (s *Store) decoratePagedItems(q sqlQueryer, threadID string, items []Item) ([]Item, error) {
	decorated, err := s.decorateProposedPlanItems(q, threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate paged proposed plans for %s: %w", threadID, err)
	}
	decorated, err = s.decorateSubagentAnchors(q, threadID, decorated)
	if err != nil {
		return nil, fmt.Errorf("store: decorate paged subagent anchors for %s: %w", threadID, err)
	}
	return decorated, nil
}

func emptyPagedItems() PagedItems {
	return PagedItems{
		Items:           []Item{},
		OldestCursor:    emptyTimelineCursor(),
		NewestCursor:    emptyTimelineCursor(),
		OldestTurnIndex: -1,
		NewestTurnIndex: -1,
		HasMore:         false,
		HasMoreOlder:    false,
		HasMoreNewer:    false,
	}
}

func emptyTimelineCursor() TimelineCursor {
	return TimelineCursor{TurnIndex: -1, ItemIndex: -1}
}

func cursorFromItem(item Item) TimelineCursor {
	return TimelineCursor{
		TurnIndex: item.TurnIndex,
		ItemIndex: item.ItemIndex,
		ItemID:    item.ID,
	}
}

// cursorIsValid distinguishes real cursors from the empty sentinel by
// TurnIndex alone: turn indexes are never negative, but item indexes
// can be — head-healed prompts persist at negative indexes
// (UpsertItemAtTurnHead), and a page bounded by one must keep paging.
func cursorIsValid(cursor TimelineCursor) bool {
	return cursor.TurnIndex >= 0
}

func (s *Store) finalizePagedItems(q sqlQueryer, threadID string, items []Item) (PagedItems, error) {
	if len(items) == 0 {
		return emptyPagedItems(), nil
	}
	oldest := cursorFromItem(items[0])
	newest := cursorFromItem(items[len(items)-1])
	hasMoreOlder, err := s.hasOlderItems(q, threadID, oldest)
	if err != nil {
		return PagedItems{}, err
	}
	hasMoreNewer, err := s.hasNewerItems(q, threadID, newest)
	if err != nil {
		return PagedItems{}, err
	}
	return PagedItems{
		Items:           items,
		OldestCursor:    oldest,
		NewestCursor:    newest,
		OldestTurnIndex: oldest.TurnIndex,
		NewestTurnIndex: newest.TurnIndex,
		HasMore:         hasMoreOlder,
		HasMoreOlder:    hasMoreOlder,
		HasMoreNewer:    hasMoreNewer,
	}, nil
}

func (s *Store) hasOlderItems(q sqlQueryer, threadID string, cursor TimelineCursor) (bool, error) {
	var exists int
	err := q.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM timeline_items
		   WHERE thread_id = ?
		     AND `+visibleItemsFilter+`
		     AND `+topLevelItemsFilter+`
		     AND (turn_index < ? OR (turn_index = ? AND item_index < ?)))`,
		threadID, cursor.TurnIndex, cursor.TurnIndex, cursor.ItemIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe older items for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) hasNewerItems(q sqlQueryer, threadID string, cursor TimelineCursor) (bool, error) {
	var exists int
	err := q.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM timeline_items
		   WHERE thread_id = ?
		     AND `+visibleItemsFilter+`
		     AND `+topLevelItemsFilter+`
		     AND (turn_index > ? OR (turn_index = ? AND item_index > ?)))`,
		threadID, cursor.TurnIndex, cursor.TurnIndex, cursor.ItemIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe newer items for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

// ListThreadSliceAround loads the bounded active-pane window around an
// anchor. The slice contains roughly `targetItemCount` top-level items
// (defaults to 50 when <= 0), split half at-or-before and half after the
// anchor's item coordinate. The anchor may be a subagent child: its
// coordinates still position the window even though child rows
// themselves load through ListSubagentDescendants.
//
// When `anchorItemID` is "" or the item doesn't belong to `threadID`
// (bottom-snapshot restore, stale snapshot whose anchor has been
// deleted), the function returns the tail `targetItemCount` items.
func (s *Store) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount int) (PagedItems, error) {
	return s.listThreadSliceAround(s.reader(), threadID, anchorItemID, targetItemCount)
}

// listThreadSliceAround is ListThreadSliceAround against a caller-chosen
// queryer, so SyncThreadWindow can run the same window inside the
// transaction its stamps are read in.
func (s *Store) listThreadSliceAround(q sqlQueryer, threadID, anchorItemID string, targetItemCount int) (PagedItems, error) {
	if targetItemCount <= 0 {
		targetItemCount = 50
	}
	if anchorItemID == "" {
		return s.listTailSlice(q, threadID, targetItemCount)
	}
	anchor, found, err := s.getThreadItem(q, threadID, anchorItemID)
	if err != nil {
		return PagedItems{}, fmt.Errorf("store: list thread slice for %s anchor=%s: %w", threadID, anchorItemID, err)
	}
	if !found {
		return s.listTailSlice(q, threadID, targetItemCount)
	}

	atOrBeforeBudget := targetItemCount / 2
	if atOrBeforeBudget < 1 {
		atOrBeforeBudget = 1
	}
	afterBudget := targetItemCount - atOrBeforeBudget
	if afterBudget < 1 {
		afterBudget = 1
	}
	atOrBeforeSQL, atOrBeforeArgs := timelineIDSelection(threadID, timelineSelection{
		Where: windowedTimelineFilter + `
		   AND (items.turn_index < ? OR (items.turn_index = ? AND items.item_index <= ?))`,
		WhereArgs: []any{anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex},
		OrderBy:   "turn_index DESC, item_index DESC",
		Limit:     atOrBeforeBudget,
	})
	afterSQL, afterArgs := timelineIDSelection(threadID, timelineSelection{
		Where: windowedTimelineFilter + `
		   AND (items.turn_index > ? OR (items.turn_index = ? AND items.item_index > ?))`,
		WhereArgs: []any{anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex},
		OrderBy:   "turn_index ASC, item_index ASC",
		Limit:     afterBudget,
	})
	items, err := s.querySelectedPagedItems(
		q,
		threadID,
		atOrBeforeSQL+"\nUNION\n"+afterSQL,
		append(atOrBeforeArgs, afterArgs...)...,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(q, threadID, items)
}

// listTailSlice returns the newest `targetItemCount` top-level items.
// Used when the snapshot is a bottom-restore or the anchor item has
// been deleted.
func (s *Store) listTailSlice(q sqlQueryer, threadID string, targetItemCount int) (PagedItems, error) {
	selectedSQL, selectedArgs := timelineIDSelection(threadID, timelineSelection{
		Where:   windowedTimelineFilter,
		OrderBy: "turn_index DESC, item_index DESC",
		Limit:   targetItemCount,
	})
	items, err := s.querySelectedPagedItems(q, threadID, selectedSQL, selectedArgs...)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(q, threadID, items)
}
