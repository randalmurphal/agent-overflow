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
var visibleItemsFilterQualified = visibleItemsFilterFor("items.")

// topLevelItemsFilter restricts a timeline read to top-level rows.
// Subagent children (rows with a non-empty parent_id) are deliberately
// not part of any history window, budget, or pagination probe: they
// render inside their anchor's SubagentGroup card, load on demand via
// ListSubagentDescendants when the card expands, and are summarised on
// the collapsed card by decorateSubagentAnchors. Counting them against
// windows used to make one subagent-heavy turn eat the entire item
// budget and flash "Load older messages" for rows that would never
// render as timeline rows.
const topLevelItemsFilter = "parent_id = ''"
const topLevelItemsFilterQualified = "items.parent_id = ''"

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

// openUpperBound is the sentinel upper-turn bound used by queries that
// want "all turns at or above floor." 2^31 is larger than any practical
// turn_index — the schema only enforces CHECK(turn_index >= 0), so this
// isn't a hard cap, just a safe ceiling past anything realistic.
const openUpperBound = int64(1 << 31)

// ListRecentItems loads every top-level item for the thread whose
// turn_index is at or above `floorTurnIndex`. Subagent children stay
// behind their anchor's collapsed card (see topLevelItemsFilter).
//
// Pass `floorTurnIndex = 0` (or any value ≤ the thread's smallest
// turn_index) to load the entire thread. Empty threads return a stable
// empty PagedItems shape with both turn bounds set to -1.
func (s *Store) ListRecentItems(threadID string, floorTurnIndex int) (PagedItems, error) {
	items, err := s.queryPagedItems(threadID, int64(floorTurnIndex), openUpperBound)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// ListItemsBeforeTurn loads older top-level items strictly below
// `beforeTurnIndex` until cumulative item count reaches `itemBudget`.
//
// The third parameter is an **item budget**, not a turn count: the
// backend walks turns DESC starting at `beforeTurnIndex - 1`, summing
// each turn's top-level item count (excluding `plan_update`
// notifications), and stops at the first turn that pushes cumulative ≥
// itemBudget. This keeps a page predictably sized for the frontend
// regardless of how many items any single turn happens to contain.
//
// `beforeTurnIndex` is exclusive — callers pass their current floor and
// get back items for turns strictly below it. A non-positive
// `itemBudget` is treated as "nothing to load" and returns an empty
// page.
//
// Returns PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}
// when no older turns exist or itemBudget is non-positive.
func (s *Store) ListItemsBeforeTurn(threadID string, beforeTurnIndex, itemBudget int) (PagedItems, error) {
	empty := emptyPagedItems()
	if itemBudget <= 0 {
		return empty, nil
	}

	newFloor, ok, err := s.floorTurnByItemBudget(threadID, int64(beforeTurnIndex), itemBudget)
	if err != nil {
		return PagedItems{}, err
	}
	if !ok {
		return empty, nil
	}

	items, err := s.queryPagedItems(threadID, int64(newFloor), int64(beforeTurnIndex))
	if err != nil {
		return PagedItems{}, err
	}
	// newFloor always has at least one selectable row (the budget walk
	// only stops on turns that contribute counts), so items[0].TurnIndex
	// == newFloor and finalizePagedItems reports the same turn bounds the
	// explicit newFloor bookkeeping used to.
	return s.finalizePagedItems(threadID, items)
}

// ListItemsAfterTurn loads newer top-level items strictly above
// `afterTurnIndex` until cumulative item count reaches `itemBudget`.
func (s *Store) ListItemsAfterTurn(threadID string, afterTurnIndex, itemBudget int) (PagedItems, error) {
	empty := emptyPagedItems()
	if itemBudget <= 0 {
		return empty, nil
	}

	upper, ok, err := s.ceilingTurnByItemBudget(threadID, int64(afterTurnIndex), itemBudget)
	if err != nil {
		return PagedItems{}, err
	}
	if !ok {
		return empty, nil
	}

	floor := afterTurnIndex + 1
	items, err := s.queryPagedItems(threadID, int64(floor), int64(upper)+1)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// ListItemsBeforeCursor loads older visible top-level items strictly
// before `before` until `itemBudget` rows have been selected.
func (s *Store) ListItemsBeforeCursor(threadID string, before TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(before) {
		return emptyPagedItems(), nil
	}
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND ` + topLevelItemsFilter + `
		   AND (turn_index < ? OR (turn_index = ? AND item_index < ?))
		 ORDER BY turn_index DESC, item_index DESC
		 LIMIT ?
	)`
	items, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, before.TurnIndex, before.TurnIndex, before.ItemIndex, itemBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// ListItemsAfterCursor loads newer visible top-level items strictly
// after `after` until `itemBudget` rows have been selected. It is the
// forward pager companion to ListItemsBeforeCursor.
func (s *Store) ListItemsAfterCursor(threadID string, after TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(after) {
		return emptyPagedItems(), nil
	}
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND ` + topLevelItemsFilter + `
		   AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		 ORDER BY turn_index ASC, item_index ASC
		 LIMIT ?
	)`
	items, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, after.TurnIndex, after.TurnIndex, after.ItemIndex, itemBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// queryPagedItems runs the "top-level items in [floor, upper)" query
// used by the turn-bounded loaders and decorates the rows for render.
//
// Placeholders in order:
//
//  1. thread_id
//  2. floor   (turn_index >= floor)
//  3. upper   (turn_index < upper)
func (s *Store) queryPagedItems(threadID string, floor, upper int64) ([]Item, error) {
	rows, err := s.reader().Query(`
		SELECT `+itemColumns+`
		  FROM items
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 WHERE items.thread_id = ?
		   AND `+visibleItemsFilterQualified+`
		   AND `+topLevelItemsFilterQualified+`
		   AND items.turn_index >= ? AND items.turn_index < ?
		 ORDER BY items.turn_index, items.item_index`,
		threadID, floor, upper,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query paged items for %s: %w", threadID, err)
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		it, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan paged item row: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate paged items for %s: %w", threadID, err)
	}
	return s.decoratePagedItems(threadID, items)
}

// querySelectedPagedItems runs the cursor-based paging shape used by
// active panes. `selectedSQL` must return a single `id` column
// containing the page's top-level row ids; the outer query hydrates and
// orders them for rendering.
func (s *Store) querySelectedPagedItems(threadID, selectedSQL string, selectedArgs ...any) ([]Item, error) {
	args := append([]any{}, selectedArgs...)
	args = append(args, threadID)
	rows, err := s.reader().Query(`
		WITH selected(id) AS (
			`+selectedSQL+`
		)
		SELECT `+itemColumns+`
		  FROM items
		  JOIN selected ON selected.id = items.id
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 WHERE items.thread_id = ?
		 ORDER BY items.turn_index, items.item_index`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("store: query selected paged items for %s: %w", threadID, err)
	}
	defer rows.Close()

	items := []Item{}
	for rows.Next() {
		item, err := scanItemRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan selected paged item row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate selected paged items for %s: %w", threadID, err)
	}
	return s.decoratePagedItems(threadID, items)
}

// decoratePagedItems applies the read-time meta decorations every
// frontend-bound window needs: proposed-plan state and subagent anchor
// aggregates (descendant count + collapsed-card preview).
func (s *Store) decoratePagedItems(threadID string, items []Item) ([]Item, error) {
	decorated, err := s.decorateProposedPlanItems(threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate paged proposed plans for %s: %w", threadID, err)
	}
	decorated, err = s.decorateSubagentAnchors(threadID, decorated)
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

func (s *Store) finalizePagedItems(threadID string, items []Item) (PagedItems, error) {
	if len(items) == 0 {
		return emptyPagedItems(), nil
	}
	oldest := cursorFromItem(items[0])
	newest := cursorFromItem(items[len(items)-1])
	hasMoreOlder, err := s.hasOlderItems(threadID, oldest)
	if err != nil {
		return PagedItems{}, err
	}
	hasMoreNewer, err := s.hasNewerItems(threadID, newest)
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

// hasOlderTurns answers "does the thread have any visible top-level item
// with turn_index < floor?" in one probe, used by PickInitialFloorTurn's
// hasMore report. Uses the idx_items_thread composite index so the
// EXISTS probe is an index lookup.
//
// Filters match every other loader (`queryPagedItems`,
// `floorTurnByItemBudget`, cursor pagers): plan_update notifications and
// subagent children are excluded. Without them, a thread whose only
// sub-floor rows are plan_update notifications or subagent children
// would report `hasMore=true`, the frontend would render a "Load older
// messages" button, and clicking it would load zero rows before the
// frontend's self-heal cleared the button.
func (s *Store) hasOlderTurns(threadID string, floorTurnIndex int) (bool, error) {
	var exists int
	err := s.reader().QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
		   WHERE thread_id = ? AND turn_index < ?
		     AND `+visibleItemsFilter+`
		     AND `+topLevelItemsFilter+`)`,
		threadID, floorTurnIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe older turns for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) hasOlderItems(threadID string, cursor TimelineCursor) (bool, error) {
	var exists int
	err := s.reader().QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
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

func (s *Store) hasNewerItems(threadID string, cursor TimelineCursor) (bool, error) {
	var exists int
	err := s.reader().QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
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

// floorTurnByItemBudget walks turns DESC strictly below `beforeTurnIndex`,
// summing each turn's top-level item count (excluding plan_update
// notifications), and returns the smallest turn_index reached once
// cumulative ≥ itemBudget. Returns (0, false, nil) when no items exist
// below `beforeTurnIndex`.
//
// One walker, two entry points: ListItemsBeforeTurn passes the caller's
// current floor (page-back); legacy tail loads pass openUpperBound. The
// filters and the cumulative-budget shape must stay aligned with
// `queryPagedItems` — counting filtered rows against the budget would
// systematically under-deliver visible content.
func (s *Store) floorTurnByItemBudget(threadID string, beforeTurnIndex int64, itemBudget int) (int, bool, error) {
	if itemBudget < 1 {
		itemBudget = 1
	}
	limit := boundedSliceTurnLimit(itemBudget)
	rows, err := s.reader().Query(
		`SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index < ?
		    AND `+visibleItemsFilter+`
		    AND `+topLevelItemsFilter+`
		  GROUP BY turn_index
		  ORDER BY turn_index DESC
		  LIMIT ?`,
		threadID, beforeTurnIndex, limit,
	)
	if err != nil {
		return 0, false, fmt.Errorf("store: floor turn by item budget for %s: %w", threadID, err)
	}
	defer rows.Close()

	cumulative := 0
	floor := 0
	saw := false
	for rows.Next() {
		var ti, cnt int
		if err := rows.Scan(&ti, &cnt); err != nil {
			return 0, false, fmt.Errorf("store: scan floor turn by item budget row: %w", err)
		}
		floor = ti
		saw = true
		cumulative += cnt
		if cumulative >= itemBudget {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("store: iterate floor turn by item budget for %s: %w", threadID, err)
	}
	return floor, saw, nil
}

// ceilingTurnByItemBudget is the newer-side companion to
// floorTurnByItemBudget. It walks turns ASC strictly above afterTurnIndex
// and returns the largest turn_index reached once cumulative ≥ itemBudget.
func (s *Store) ceilingTurnByItemBudget(threadID string, afterTurnIndex int64, itemBudget int) (int, bool, error) {
	if itemBudget < 1 {
		itemBudget = 1
	}
	limit := boundedSliceTurnLimit(itemBudget)
	rows, err := s.reader().Query(
		`SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index > ?
		    AND `+visibleItemsFilter+`
		    AND `+topLevelItemsFilter+`
		  GROUP BY turn_index
		  ORDER BY turn_index ASC
		  LIMIT ?`,
		threadID, afterTurnIndex, limit,
	)
	if err != nil {
		return 0, false, fmt.Errorf("store: ceiling turn by item budget for %s: %w", threadID, err)
	}
	defer rows.Close()

	cumulative := 0
	ceiling := 0
	saw := false
	for rows.Next() {
		var ti, cnt int
		if err := rows.Scan(&ti, &cnt); err != nil {
			return 0, false, fmt.Errorf("store: scan ceiling turn by item budget row: %w", err)
		}
		ceiling = ti
		saw = true
		cumulative += cnt
		if cumulative >= itemBudget {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("store: iterate ceiling turn by item budget for %s: %w", threadID, err)
	}
	return ceiling, saw, nil
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
	if targetItemCount <= 0 {
		targetItemCount = 50
	}
	if anchorItemID == "" {
		return s.listTailSlice(threadID, targetItemCount)
	}
	anchor, found, err := s.GetThreadItem(threadID, anchorItemID)
	if err != nil {
		return PagedItems{}, fmt.Errorf("store: list thread slice for %s anchor=%s: %w", threadID, anchorItemID, err)
	}
	if !found {
		return s.listTailSlice(threadID, targetItemCount)
	}

	atOrBeforeBudget := targetItemCount / 2
	if atOrBeforeBudget < 1 {
		atOrBeforeBudget = 1
	}
	afterBudget := targetItemCount - atOrBeforeBudget
	if afterBudget < 1 {
		afterBudget = 1
	}
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND ` + topLevelItemsFilter + `
		   AND (turn_index < ? OR (turn_index = ? AND item_index <= ?))
		 ORDER BY turn_index DESC, item_index DESC
		 LIMIT ?
	)
	UNION
	SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND ` + topLevelItemsFilter + `
		   AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		 ORDER BY turn_index ASC, item_index ASC
		 LIMIT ?
	)`
	items, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, atOrBeforeBudget,
		threadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, afterBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// listTailSlice returns the newest `targetItemCount` top-level items.
// Used when the snapshot is a bottom-restore or the anchor item has
// been deleted.
func (s *Store) listTailSlice(threadID string, targetItemCount int) (PagedItems, error) {
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND ` + topLevelItemsFilter + `
		 ORDER BY turn_index DESC, item_index DESC
		 LIMIT ?
	)`
	items, err := s.querySelectedPagedItems(threadID, selectedSQL, threadID, targetItemCount)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// boundedSliceTurnLimit caps the number of turn rows scanned by the legacy
// turn-budget pagers. Worst case (one item per turn) needs `budget` turns; a
// 4x overshoot keeps the planner honest on burst threads where a turn might be
// a no-item placeholder, and the absolute cap prevents pathological scans on
// multi-thousand-turn threads.
func boundedSliceTurnLimit(budget int) int {
	const overshoot = 4
	const absoluteCap = 5000
	limit := budget * overshoot
	if limit > absoluteCap {
		limit = absoluteCap
	}
	return limit
}
