package store

import (
	"fmt"
)

// visibleItemsFilter is the WHERE-clause fragment shared by every read
// path that walks the timeline. plan_update notifications are excluded
// because the frontend renders them out of band (live plan store) —
// counting them against item budgets would systematically under-deliver
// visible content, and a thread whose only sub-floor rows are plan_update
// notifications would otherwise flash a "Load older messages" button
// that loads zero rows. Both unqualified and items-qualified forms are
// needed: queries with a LEFT JOIN onto `payloads` (which also has a
// `kind` column) must qualify, the rest of the queries don't have the
// ambiguity. Change both at once if you ever add another excluded
// notification kind here.
const visibleItemsFilter = "NOT (kind = 'notification' AND tool_name = 'plan_update')"
const visibleItemsFilterQualified = "NOT (items.kind = 'notification' AND items.tool_name = 'plan_update')"

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
// item-coordinate bounds of the logical page. Render-support ancestors can be
// returned outside those bounds; they must not become pagination cursors.
// Cursor turn/item indexes are -1 when `Items` is empty.
//
// `OldestTurnIndex` / `NewestTurnIndex` are legacy turn-only aliases derived
// from the cursors. Active-pane callers should use the cursor fields so one
// dense turn cannot punch through the item window cap.
//
// HasMoreOlder / HasMoreNewer report whether visible items exist outside
// the cursor bounds. HasMore is the legacy older-history alias kept for
// frontend and transport compatibility while callers migrate to the explicit
// names.
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

// ancestorCTE is the recursive common table expression used by every paged
// items query to pull in subagent parents that live below the requested
// floor. The seed scan filters items by thread+turn range and uses
// migrate.go's idx_items_parent partial index to find the distinct parent
// ids; the recursive step seeks on the items primary-key id to walk up
// each parent edge AND re-filters by thread_id so a future migration that
// preserves ids across threads can't leak ancestors from another thread.
// Both hops are index-driven. Placeholders in order:
//
//  1. thread_id       (seed)
//  2. floorTurnIndex  (seed: turn_index >= floor)
//  3. upperBound      (seed: turn_index < upper) — when the caller wants
//     a bounded range; pass openUpperBound for open-ended initial loads.
//  4. thread_id       (recursive step) — the id-join gate.
//
// UNION (not UNION ALL) is load-bearing here: it dedups during recursion
// so a cycle (pathological parent_id pointing at itself or back to a
// descendant) terminates instead of looping forever. Items without a
// parent_id are filtered at each step so the walk terminates on the
// normal path too.
const ancestorCTE = `WITH RECURSIVE ancestors(id) AS (
    SELECT DISTINCT parent_id FROM items
     WHERE thread_id = ?
       AND turn_index >= ?
       AND turn_index < ?
       AND parent_id <> ''
    UNION
    SELECT i.parent_id FROM items i
      JOIN ancestors a ON i.id = a.id
     WHERE i.thread_id = ?
       AND i.parent_id <> ''
)`

// openUpperBound is the sentinel upper-turn bound used by queries that
// want "all turns at or above floor." 2^31 is larger than any practical
// turn_index — the schema only enforces CHECK(turn_index >= 0), so this
// isn't a hard cap, just a safe ceiling past anything realistic.
const openUpperBound = int64(1 << 31)

// ListRecentItemsWithAncestors loads every item for the thread whose
// turn_index is at or above `floorTurnIndex`, plus any item whose id is
// the transitive parent of an in-window item (even if its own turn_index
// is below the floor). The latter keeps subagent chains intact when
// paging — a child that lives in turn 45 with a parent in turn 20 will
// still render as a proper SubagentGroup instead of an orphan.
//
// Pass `floorTurnIndex = 0` (or any value ≤ the thread's smallest
// turn_index) to load the entire thread. Empty threads return a stable
// empty PagedItems shape with both turn bounds set to -1.
func (s *Store) ListRecentItemsWithAncestors(threadID string, floorTurnIndex int) (PagedItems, error) {
	items, err := s.queryPagedItems(threadID, int64(floorTurnIndex), openUpperBound, floorTurnIndex, "")
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItemsWithTurnRange(threadID, items, floorTurnIndex, int(openUpperBound))
}

// ListItemsBeforeTurn loads older items strictly below `beforeTurnIndex`
// until cumulative item count reaches `itemBudget`. Ancestor items below
// the new floor are pulled in via the same recursive CTE
// ListRecentItemsWithAncestors uses so subagent chains stay consistent
// across paging boundaries.
//
// The third parameter is an **item budget**, not a turn count: the
// backend walks turns DESC starting at `beforeTurnIndex - 1`, summing
// each turn's item count (excluding `plan_update` notifications), and
// stops at the first turn that pushes cumulative ≥ itemBudget. This
// keeps a page predictably sized for the frontend regardless of how
// many items any single turn happens to contain.
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

	// Constrain ancestors to those strictly below newFloor so items that
	// were already in the caller's previously-loaded window (turn_index
	// >= beforeTurnIndex) don't duplicate when prepended.
	items, err := s.queryPagedItems(threadID, int64(newFloor), int64(beforeTurnIndex), newFloor, "")
	if err != nil {
		return PagedItems{}, err
	}

	hasMore, err := s.hasOlderTurns(threadID, newFloor)
	if err != nil {
		return PagedItems{}, err
	}
	primaryItems := itemsInTurnRange(items, newFloor, beforeTurnIndex)
	if len(primaryItems) == 0 {
		return empty, nil
	}
	oldestCursor := cursorFromItem(primaryItems[0])
	newestCursor := cursorFromItem(primaryItems[len(primaryItems)-1])
	newest := newestCursor.TurnIndex
	hasMoreNewer, err := s.hasNewerTurns(threadID, newest)
	if err != nil {
		return PagedItems{}, err
	}
	return PagedItems{
		Items:           items,
		OldestCursor:    oldestCursor,
		NewestCursor:    newestCursor,
		OldestTurnIndex: newFloor,
		NewestTurnIndex: newest,
		HasMore:         hasMore,
		HasMoreOlder:    hasMore,
		HasMoreNewer:    hasMoreNewer,
	}, nil
}

// ListItemsAfterTurn loads newer items strictly above `afterTurnIndex`
// until cumulative item count reaches `itemBudget`. Ancestor items below
// the new floor are stitched in so subagent chains stay renderable when
// the caller has pruned older context out of the active window.
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
	items, err := s.queryPagedItems(threadID, int64(floor), int64(upper)+1, floor, "")
	if err != nil {
		return PagedItems{}, err
	}

	primaryItems := itemsInTurnRange(items, floor, upper+1)
	if len(primaryItems) == 0 {
		return empty, nil
	}
	oldestCursor := cursorFromItem(primaryItems[0])
	newestCursor := cursorFromItem(primaryItems[len(primaryItems)-1])
	hasMoreNewer, err := s.hasNewerTurns(threadID, newestCursor.TurnIndex)
	if err != nil {
		return PagedItems{}, err
	}
	hasMoreOlder, err := s.hasOlderTurns(threadID, oldestCursor.TurnIndex)
	if err != nil {
		return PagedItems{}, err
	}
	return PagedItems{
		Items:           items,
		OldestCursor:    oldestCursor,
		NewestCursor:    newestCursor,
		OldestTurnIndex: oldestCursor.TurnIndex,
		NewestTurnIndex: newestCursor.TurnIndex,
		HasMore:         hasMoreOlder,
		HasMoreOlder:    hasMoreOlder,
		HasMoreNewer:    hasMoreNewer,
	}, nil
}

// ListItemsBeforeCursor loads older visible items strictly before `before`
// until `itemBudget` primary rows have been selected. Ancestors needed to
// render those rows are stitched into Items but do not affect the returned
// cursors or HasMore probes.
func (s *Store) ListItemsBeforeCursor(threadID string, before TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(before) {
		return emptyPagedItems(), nil
	}
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND (turn_index < ? OR (turn_index = ? AND item_index < ?))
		 ORDER BY turn_index DESC, item_index DESC
		 LIMIT ?
	)`
	items, primaryItems, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, before.TurnIndex, before.TurnIndex, before.ItemIndex, itemBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItemsWithPrimary(threadID, items, primaryItems)
}

// ListItemsAfterCursor loads newer visible items strictly after `after`
// until `itemBudget` primary rows have been selected. Ancestors below the
// page are stitched in as render support for partially loaded subagent
// groups, but cursor bounds remain tied to the selected primary rows.
func (s *Store) ListItemsAfterCursor(threadID string, after TimelineCursor, itemBudget int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(after) {
		return emptyPagedItems(), nil
	}
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		   AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		 ORDER BY turn_index ASC, item_index ASC
		 LIMIT ?
	)`
	items, primaryItems, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, after.TurnIndex, after.TurnIndex, after.ItemIndex, itemBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItemsWithPrimary(threadID, items, primaryItems)
}

// queryPagedItems runs the "items in [floor, upper) plus any ancestor"
// query. `ancestorCutoff` is the turn_index below which ancestors are
// accepted; pass the same value as `floor` for the initial-load case
// (any ancestor that happens to live above floor is already captured by
// the primary clause). For paged loads pass the new floor so ancestors
// above it — already in the caller's window — are excluded.
//
// When `anchorParentID` is non-empty, the WHERE clause grows an extra
// disjunct `OR items.parent_id = ?` so a subagent group containing the
// anchor renders intact even if some siblings sit outside [floor,
// upper). Pass `""` for the non-slice cases (initial load, paginate
// older).
//
// `threadID` is bound into every scope of the query: the seed, the
// recursive step, the outer SELECT, and — defence-in-depth — the
// `ancestors IN (…)` subquery too. `items.id` is not globally unique
// (see migrate.go: PRIMARY KEY on id alone, but identical ids across
// threads are schema-allowed), so without the outer `items.thread_id`
// guard a cross-thread id collision would leak rows from another thread.
//
// Placeholders in order:
//
//  1. thread_id      (CTE seed)
//  2. floor          (CTE seed: turn_index >= floor)
//  3. upper          (CTE seed: turn_index < upper)
//  4. thread_id      (CTE recursive step)
//  5. thread_id      (outer)
//  6. floor          (outer turn range)
//  7. upper          (outer turn range)
//  8. ancestorCutoff
//  9. anchorParentID (only when non-empty)
func (s *Store) queryPagedItems(threadID string, floor, upper int64, ancestorCutoff int, anchorParentID string) ([]Item, error) {
	siblingClause := ""
	args := []any{
		threadID, floor, upper,
		threadID,
		threadID,
		floor, upper, int64(ancestorCutoff),
	}
	if anchorParentID != "" {
		siblingClause = ` OR items.parent_id = ?`
		args = append(args, anchorParentID)
	}
	rows, err := s.db.Query(ancestorCTE+`
		SELECT `+itemColumns+`
		  FROM items
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 WHERE items.thread_id = ?
		   AND `+visibleItemsFilterQualified+`
		   AND (
		     (items.turn_index >= ? AND items.turn_index < ?)
		     OR (items.id IN (SELECT id FROM ancestors)
		         AND items.turn_index < ?)`+siblingClause+`
		   )
		 ORDER BY items.turn_index, items.item_index`,
		args...,
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
	decorated, err := s.decorateProposedPlanItems(threadID, items)
	if err != nil {
		return nil, fmt.Errorf("store: decorate paged proposed plans for %s: %w", threadID, err)
	}
	return decorated, nil
}

// querySelectedPagedItems runs the cursor-based paging shape used by active
// panes. `selectedSQL` must return a single `id` column containing only the
// primary page rows. This function adds the recursive ancestor stitch and
// returns both:
//
//   - all returned items, sorted for rendering, including ancestors
//   - primary page items only, sorted by timeline coordinate, for cursors
//
// Keeping the selected/primary distinction is what lets a page render a
// subagent shell from an older parent without letting that parent move the
// logical pagination floor.
func (s *Store) querySelectedPagedItems(threadID, selectedSQL string, selectedArgs ...any) ([]Item, []Item, error) {
	args := append([]any{}, selectedArgs...)
	args = append(args,
		threadID, // ancestor seed
		threadID, // ancestor recursion
		threadID, // outer SELECT
	)
	rows, err := s.db.Query(`
		WITH RECURSIVE selected(id) AS (
			`+selectedSQL+`
		),
		ancestors(id) AS (
			SELECT DISTINCT i.parent_id
			  FROM items i
			  JOIN selected s ON s.id = i.id
			 WHERE i.thread_id = ?
			   AND i.parent_id <> ''
			UNION
			SELECT i.parent_id
			  FROM items i
			  JOIN ancestors a ON i.id = a.id
			 WHERE i.thread_id = ?
			   AND i.parent_id <> ''
		)
		SELECT `+itemColumns+`,
		       CASE WHEN selected.id IS NULL THEN 0 ELSE 1 END AS selected_item
		  FROM items
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		  LEFT JOIN selected ON selected.id = items.id
		 WHERE items.thread_id = ?
		   AND `+visibleItemsFilterQualified+`
		   AND (
		     selected.id IS NOT NULL
		     OR items.id IN (SELECT id FROM ancestors)
		   )
		 ORDER BY items.turn_index, items.item_index`,
		args...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("store: query selected paged items for %s: %w", threadID, err)
	}
	defer rows.Close()

	items := []Item{}
	primaryItems := []Item{}
	for rows.Next() {
		item, selected, err := scanPagedItemRow(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("store: scan selected paged item row: %w", err)
		}
		items = append(items, item)
		if selected {
			primaryItems = append(primaryItems, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: iterate selected paged items for %s: %w", threadID, err)
	}
	decorated, err := s.decorateProposedPlanItems(threadID, items)
	if err != nil {
		return nil, nil, fmt.Errorf("store: decorate selected paged proposed plans for %s: %w", threadID, err)
	}
	if len(decorated) != len(items) {
		return decorated, primaryItems, nil
	}
	primaryByID := make(map[string]struct{}, len(primaryItems))
	for _, item := range primaryItems {
		primaryByID[item.ID] = struct{}{}
	}
	primaryItems = primaryItems[:0]
	for _, item := range decorated {
		if _, ok := primaryByID[item.ID]; ok {
			primaryItems = append(primaryItems, item)
		}
	}
	return decorated, primaryItems, nil
}

func scanPagedItemRow(scanner interface{ Scan(...any) error }) (Item, bool, error) {
	var it Item
	var isBackground int
	var selected int
	if err := scanner.Scan(
		&it.ID, &it.ThreadID, &it.TurnIndex, &it.ItemIndex,
		&it.Kind, &it.Role, &it.Status, &it.Summary,
		&it.PayloadID, &it.PayloadKind, &it.PayloadMeta,
		&it.InputPayloadID,
		&it.ParentID, &isBackground, &it.CompletionOf,
		&it.ToolName, &it.Decision, &it.Meta, &it.CreatedAt, &it.UpdatedAt,
		&selected,
	); err != nil {
		return Item{}, false, err
	}
	it.IsBackground = isBackground != 0
	return it, selected != 0, nil
}

func (s *Store) finalizePagedItemsWithTurnRange(
	threadID string,
	items []Item,
	floorTurnIndex int,
	upperTurnIndex int,
) (PagedItems, error) {
	primaryItems := itemsInTurnRange(items, floorTurnIndex, upperTurnIndex)
	if len(primaryItems) == 0 {
		return emptyPagedItems(), nil
	}
	oldest := cursorFromItem(primaryItems[0])
	newest := cursorFromItem(primaryItems[len(primaryItems)-1])
	hasMoreOlder, err := s.hasOlderTurns(threadID, oldest.TurnIndex)
	if err != nil {
		return PagedItems{}, err
	}
	hasMoreNewer, err := s.hasNewerTurns(threadID, newest.TurnIndex)
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

func cursorIsValid(cursor TimelineCursor) bool {
	return cursor.TurnIndex >= 0 && cursor.ItemIndex >= 0
}

func (s *Store) finalizePagedItemsWithPrimary(threadID string, items []Item, primaryItems []Item) (PagedItems, error) {
	if len(primaryItems) == 0 {
		return emptyPagedItems(), nil
	}
	oldest := cursorFromItem(primaryItems[0])
	newest := cursorFromItem(primaryItems[len(primaryItems)-1])
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

func itemsInTurnRange(items []Item, floorTurnIndex, upperTurnIndex int) []Item {
	primaryItems := make([]Item, 0, len(items))
	for _, item := range items {
		if item.TurnIndex < floorTurnIndex || item.TurnIndex >= upperTurnIndex {
			continue
		}
		primaryItems = append(primaryItems, item)
	}
	return primaryItems
}

// hasOlderTurns answers "does the thread have any visible item with
// turn_index < floor?" in one probe, used to populate PagedItems.HasMore.
// Uses the idx_items_thread composite index so the EXISTS probe is an
// index lookup.
//
// Filters out `plan_update` notifications via the shared
// `visibleItemsFilter` to match every other loader (`queryPagedItems`,
// `floorTurnByItemBudget`, cursor pagers). Without the filter,
// threads whose only sub-floor rows are plan_update notifications would
// report `hasMore=true`, the frontend would render a "Load older
// messages" button, and clicking it would load zero rows before the
// frontend's self-heal cleared the button.
func (s *Store) hasOlderTurns(threadID string, floorTurnIndex int) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
		   WHERE thread_id = ? AND turn_index < ?
		     AND `+visibleItemsFilter+`)`,
		threadID, floorTurnIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe older turns for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

// hasNewerTurns answers "does the thread have any visible item with
// turn_index > ceiling?" in one probe. It is the newer-side companion to
// hasOlderTurns and drives the frontend's bottom history-gap affordance.
func (s *Store) hasNewerTurns(threadID string, ceilingTurnIndex int) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
		   WHERE thread_id = ? AND turn_index > ?
		     AND `+visibleItemsFilter+`)`,
		threadID, ceilingTurnIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe newer turns for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

func (s *Store) hasOlderItems(threadID string, cursor TimelineCursor) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
		   WHERE thread_id = ?
		     AND `+visibleItemsFilter+`
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
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items
		   WHERE thread_id = ?
		     AND `+visibleItemsFilter+`
		     AND (turn_index > ? OR (turn_index = ? AND item_index > ?)))`,
		threadID, cursor.TurnIndex, cursor.TurnIndex, cursor.ItemIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe newer items for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

// floorTurnByItemBudget walks turns DESC strictly below `beforeTurnIndex`,
// summing each turn's item count (excluding plan_update notifications),
// and returns the smallest turn_index reached once cumulative ≥ itemBudget.
// Returns (0, false, nil) when no items exist below `beforeTurnIndex`.
//
// One walker, two entry points: ListItemsBeforeTurn passes the caller's
// current floor (page-back), listTailSlice passes openUpperBound (initial
// tail load). The plan_update filter and the cumulative-budget shape must
// stay aligned with `queryPagedItems` — counting filtered rows against
// the budget would systematically under-deliver visible content.
func (s *Store) floorTurnByItemBudget(threadID string, beforeTurnIndex int64, itemBudget int) (int, bool, error) {
	if itemBudget < 1 {
		itemBudget = 1
	}
	limit := boundedSliceTurnLimit(itemBudget)
	rows, err := s.db.Query(
		`SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index < ?
		    AND `+visibleItemsFilter+`
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
	rows, err := s.db.Query(
		`SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index > ?
		    AND `+visibleItemsFilter+`
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
// anchor. The primary slice contains roughly `targetItemCount` items
// (defaults to 50 when <= 0), split half at-or-before and half after the
// anchor's item coordinate. Subagent ancestors below the selected item
// window are stitched in so partially loaded groups still have their shell;
// siblings outside the item window deliberately stay omitted so one
// several-hour subagent turn cannot bypass the active-pane memory cap.
//
// When `anchorItemID` is "" or the item doesn't belong to `threadID`
// (bottom-snapshot restore, stale snapshot whose anchor has been
// deleted), the function returns the tail `targetItemCount` items.
//
// `OldestCursor` / `NewestCursor` report the logical slice bounds, not any
// stitched ancestors that render groups around the slice, so the frontend's
// pagination controls continue to expose omitted items.
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
		   AND (turn_index > ? OR (turn_index = ? AND item_index > ?))
		 ORDER BY turn_index ASC, item_index ASC
		 LIMIT ?
	)`
	items, primaryItems, err := s.querySelectedPagedItems(
		threadID,
		selectedSQL,
		threadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, atOrBeforeBudget,
		threadID, anchor.TurnIndex, anchor.TurnIndex, anchor.ItemIndex, afterBudget,
	)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItemsWithPrimary(threadID, items, primaryItems)
}

// listTailSlice returns the newest `targetItemCount` items with subagent
// ancestors stitched in. Used when the snapshot is a bottom-restore or
// the anchor item has been deleted.
func (s *Store) listTailSlice(threadID string, targetItemCount int) (PagedItems, error) {
	selectedSQL := `SELECT id FROM (
		SELECT id
		  FROM items
		 WHERE thread_id = ?
		   AND ` + visibleItemsFilter + `
		 ORDER BY turn_index DESC, item_index DESC
		 LIMIT ?
	)`
	items, primaryItems, err := s.querySelectedPagedItems(threadID, selectedSQL, threadID, targetItemCount)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItemsWithPrimary(threadID, items, primaryItems)
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
