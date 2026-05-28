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

// PagedItems is the return shape for windowed item loads. `Items` is sorted
// by (turn_index, item_index) ASC so callers can append the slice directly
// to a timeline. `OldestTurnIndex` is the inclusive floor of the returned
// set — the smallest turn_index that appears in `Items`. It is -1 when
// `Items` is empty. `HasMore` is true when the database has at least one
// item with `turn_index < OldestTurnIndex` for this thread, which is the
// signal the frontend uses to render "Load older messages".
type PagedItems struct {
	Items           []Item `json:"items"`
	OldestTurnIndex int    `json:"oldestTurnIndex"`
	HasMore         bool   `json:"hasMore"`
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
// turn_index) to load the entire thread. Empty threads return
// PagedItems{Items: nil, OldestTurnIndex: -1, HasMore: false}.
func (s *Store) ListRecentItemsWithAncestors(threadID string, floorTurnIndex int) (PagedItems, error) {
	items, err := s.queryPagedItems(threadID, int64(floorTurnIndex), openUpperBound, floorTurnIndex, "")
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
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
	empty := PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}
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
	return PagedItems{
		Items:           items,
		OldestTurnIndex: newFloor,
		HasMore:         hasMore,
	}, nil
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

// finalizePagedItems attaches OldestTurnIndex + HasMore to an items
// slice for the initial-load entry point. Empty results yield a stable
// empty slice so the JSON response shape matches the paged-load branch.
func (s *Store) finalizePagedItems(threadID string, items []Item) (PagedItems, error) {
	if len(items) == 0 {
		return PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}, nil
	}
	// Items are returned ORDER BY turn_index ASC, so the smallest
	// turn_index is on the first row. Using items[0] rather than a min
	// scan keeps this a constant-time read.
	oldest := items[0].TurnIndex
	hasMore, err := s.hasOlderTurns(threadID, oldest)
	if err != nil {
		return PagedItems{}, err
	}
	return PagedItems{Items: items, OldestTurnIndex: oldest, HasMore: hasMore}, nil
}

// hasOlderTurns answers "does the thread have any visible item with
// turn_index < floor?" in one probe, used to populate PagedItems.HasMore.
// Uses the idx_items_thread composite index so the EXISTS probe is an
// index lookup.
//
// Filters out `plan_update` notifications via the shared
// `visibleItemsFilter` to match every other loader (`queryPagedItems`,
// `floorTurnByItemBudget`, `walkSliceTurns`). Without the filter,
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

// ListThreadSliceAround loads a small slice of items around an anchor
// for the phase-1 fast path on thread switch. The slice contains roughly
// `targetItemCount` items (defaults to 50 when <= 0), split half above
// and half below the anchor's turn position. Subagent ancestors above
// the floor are stitched in via the same recursive CTE other paged loads
// use. When the anchor itself sits inside a subagent group
// (anchor.parent_id != ""), every sibling under that parent is included
// so the group renders intact even if some siblings live outside the
// turn window.
//
// When `anchorItemID` is "" or the item doesn't belong to `threadID`
// (bottom-snapshot restore, stale snapshot whose anchor has been
// deleted), the function returns the tail `targetItemCount` items.
//
// `OldestTurnIndex` and `HasMore` populate the same way the other paged
// loads do, so the frontend's pagination controls work without
// special-casing this entry point. Phase 2 of the switch always re-runs
// `ListRecentThreadItems` to fill in the full window — this slice is a
// fast first paint, not the canonical history view.
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

	floor, upper, err := s.pickSliceTurnRange(threadID, anchor.TurnIndex, targetItemCount)
	if err != nil {
		return PagedItems{}, err
	}
	items, err := s.queryPagedItems(threadID, int64(floor), int64(upper)+1, floor, anchor.ParentID)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// listTailSlice returns the newest `targetItemCount` items with subagent
// ancestors stitched in. Used when the snapshot is a bottom-restore or
// the anchor item has been deleted.
func (s *Store) listTailSlice(threadID string, targetItemCount int) (PagedItems, error) {
	floor, found, err := s.floorTurnByItemBudget(threadID, openUpperBound, targetItemCount)
	if err != nil {
		return PagedItems{}, err
	}
	if !found {
		return PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}, nil
	}
	items, err := s.queryPagedItems(threadID, int64(floor), openUpperBound, floor, "")
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// pickSliceTurnRange returns inclusive (floor, upper) turn_index bounds
// covering enough turns above and below the anchor to total roughly
// `targetItemCount` items, split evenly. Either side may fall short if
// the thread has fewer items on that side — the missing items are filled
// in by phase 2 of the thread switch.
func (s *Store) pickSliceTurnRange(threadID string, anchorTurnIndex, targetItemCount int) (floor, upper int, err error) {
	half := targetItemCount / 2
	if half < 1 {
		half = 1
	}
	floor, err = s.walkSliceTurns(threadID, anchorTurnIndex, half, sliceWalkAtOrBelow)
	if err != nil {
		return 0, 0, err
	}
	upper, err = s.walkSliceTurns(threadID, anchorTurnIndex, half, sliceWalkAbove)
	if err != nil {
		return 0, 0, err
	}
	return floor, upper, nil
}

type sliceWalkDir int

const (
	sliceWalkAtOrBelow sliceWalkDir = iota
	sliceWalkAbove
)

// walkSliceTurns scans turn rows around an anchor in the given direction,
// accumulating item counts until cumulative >= budget, and returns the
// outermost turn_index reached. For sliceWalkAtOrBelow the scan is
// `turn_index <= anchor` ORDER BY turn_index DESC; for sliceWalkAbove
// it's `turn_index > anchor` ORDER BY turn_index ASC. When the side is
// empty the anchor's own turn_index is returned, which is harmless
// because the caller's [floor, upper] window still includes the anchor
// turn via the other walk.
func (s *Store) walkSliceTurns(threadID string, anchorTurnIndex, budget int, dir sliceWalkDir) (int, error) {
	if budget < 1 {
		budget = 1
	}
	limit := boundedSliceTurnLimit(budget)
	var query string
	switch dir {
	case sliceWalkAtOrBelow:
		query = `SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index <= ?
		    AND ` + visibleItemsFilter + `
		  GROUP BY turn_index
		  ORDER BY turn_index DESC
		  LIMIT ?`
	case sliceWalkAbove:
		query = `SELECT turn_index, COUNT(*) AS item_count
		   FROM items
		  WHERE thread_id = ? AND turn_index > ?
		    AND ` + visibleItemsFilter + `
		  GROUP BY turn_index
		  ORDER BY turn_index ASC
		  LIMIT ?`
	default:
		return 0, fmt.Errorf("store: walk slice turns invalid direction %d", dir)
	}
	rows, err := s.db.Query(query, threadID, anchorTurnIndex, limit)
	if err != nil {
		return 0, fmt.Errorf("store: walk slice turns for %s: %w", threadID, err)
	}
	defer rows.Close()
	cumulative := 0
	outer := anchorTurnIndex
	for rows.Next() {
		var ti, cnt int
		if err := rows.Scan(&ti, &cnt); err != nil {
			return 0, fmt.Errorf("store: scan slice turn count: %w", err)
		}
		outer = ti
		cumulative += cnt
		if cumulative >= budget {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate slice turns for %s: %w", threadID, err)
	}
	return outer, nil
}

// boundedSliceTurnLimit caps the number of turn rows scanned per slice
// walk. Worst case (one item per turn) needs `budget` turns; a 4×
// overshoot keeps the planner honest on burst threads where a turn might
// be a no-item placeholder, and the absolute cap prevents pathological
// scans on multi-thousand-turn threads.
func boundedSliceTurnLimit(budget int) int {
	const overshoot = 4
	const absoluteCap = 5000
	limit := budget * overshoot
	if limit > absoluteCap {
		limit = absoluteCap
	}
	return limit
}
