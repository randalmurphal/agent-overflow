package store

import (
	"database/sql"
	"fmt"
)

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
	items, err := s.queryPagedItems(threadID, int64(floorTurnIndex), openUpperBound, floorTurnIndex)
	if err != nil {
		return PagedItems{}, err
	}
	return s.finalizePagedItems(threadID, items)
}

// ListItemsBeforeTurn loads the next `turnLimit` turns strictly below
// `beforeTurnIndex`, returning only items that weren't already above the
// caller's existing floor. Ancestor items below the new floor are pulled
// in via the same recursive CTE ListRecentItemsWithAncestors uses so
// subagent chains stay consistent across paging boundaries.
//
// `beforeTurnIndex` is exclusive — callers pass their current floor and
// get back items for turns strictly below it. `turnLimit` is the maximum
// number of distinct turn_index values to pull in; a non-positive value
// is treated as "nothing to load" and returns an empty page.
//
// Returns PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}
// when no older turns exist or turnLimit is non-positive.
func (s *Store) ListItemsBeforeTurn(threadID string, beforeTurnIndex, turnLimit int) (PagedItems, error) {
	empty := PagedItems{Items: []Item{}, OldestTurnIndex: -1, HasMore: false}
	if turnLimit <= 0 {
		return empty, nil
	}

	newFloor, ok, err := s.floorTurnIndexBefore(threadID, beforeTurnIndex, turnLimit)
	if err != nil {
		return PagedItems{}, err
	}
	if !ok {
		return empty, nil
	}

	// Constrain ancestors to those strictly below newFloor so items that
	// were already in the caller's previously-loaded window (turn_index
	// >= beforeTurnIndex) don't duplicate when prepended.
	items, err := s.queryPagedItems(threadID, int64(newFloor), int64(beforeTurnIndex), newFloor)
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

// queryItemsWithAncestors runs the "items in [floor, upper) plus any
// ancestor" query. `ancestorCutoff` is the turn_index below which
// ancestors are accepted; pass the same value as `floor` for the
// initial-load case (any ancestor that happens to live above floor is
// already captured by the primary clause). For paged loads pass the
// new floor so ancestors above it — already in the caller's window — are
// excluded.
//
// `threadID` is bound into every scope of the query: the seed, the
// recursive step, the outer SELECT, and — defence-in-depth — the
// `ancestors IN (…)` subquery too. `items.id` is not globally unique
// (see migrate.go: PRIMARY KEY on id alone, but identical ids across
// threads are schema-allowed), so without the outer `items.thread_id`
// guard a cross-thread id collision would leak rows from another thread.
func (s *Store) queryPagedItems(threadID string, floor, upper int64, ancestorCutoff int) ([]Item, error) {
	rows, err := s.db.Query(ancestorCTE+`
		SELECT `+itemColumns+`
		  FROM items
		  LEFT JOIN payloads ON payloads.id = items.payload_id
		 WHERE items.thread_id = ?
		   AND (
		     (items.turn_index >= ? AND items.turn_index < ?)
		     OR (items.id IN (SELECT id FROM ancestors)
		         AND items.turn_index < ?)
		   )
		 ORDER BY items.turn_index, items.item_index`,
		threadID, floor, upper, threadID,
		threadID, floor, upper, int64(ancestorCutoff),
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

// hasOlderTurns answers "does the thread have any item with
// turn_index < floor?" in one probe, used to populate PagedItems.HasMore.
// Uses the idx_items_thread composite index so the EXISTS probe is an
// index lookup.
func (s *Store) hasOlderTurns(threadID string, floorTurnIndex int) (bool, error) {
	var exists int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM items WHERE thread_id = ? AND turn_index < ?)`,
		threadID, floorTurnIndex,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("store: probe older turns for %s: %w", threadID, err)
	}
	return exists != 0, nil
}

// floorTurnIndexBefore returns the Nth-from-the-top turn_index strictly
// below `beforeTurnIndex`, where N = turnLimit. Serves as the "new floor"
// for ListItemsBeforeTurn. Returns (0, false, nil) when no older turns
// exist for the thread.
//
// Reads from the `turns` table (not `items`) so the query hits the
// compact `turns_thread_index` and works even when a turn has no items
// yet (newly-started active turn on a fresh thread).
func (s *Store) floorTurnIndexBefore(threadID string, beforeTurnIndex, turnLimit int) (int, bool, error) {
	// Prefer the turns table when present. It's small and indexed.
	row := s.db.QueryRow(
		`SELECT turn_index FROM turns
		  WHERE thread_id = ? AND turn_index < ?
		  ORDER BY turn_index DESC
		  LIMIT 1 OFFSET ?`,
		threadID, beforeTurnIndex, turnLimit-1,
	)
	var ti int
	if err := row.Scan(&ti); err != nil {
		if err == sql.ErrNoRows {
			// Either the turns table is sparse for this thread or there
			// are fewer than turnLimit older turns. Fall back to "the
			// smallest turn_index that exists below beforeTurnIndex."
			return s.smallestTurnIndexBefore(threadID, beforeTurnIndex)
		}
		return 0, false, fmt.Errorf("store: pick floor before turn for %s: %w", threadID, err)
	}
	return ti, true, nil
}

// smallestTurnIndexBefore returns the minimum turn_index across both the
// turns and items tables that is strictly below `beforeTurnIndex`. Used
// when `floorTurnIndexBefore` runs out of turn rows — we still want to
// include every older item that exists on `items` alone.
//
// Returns (0, false, nil) when neither table has an older row.
func (s *Store) smallestTurnIndexBefore(threadID string, beforeTurnIndex int) (int, bool, error) {
	row := s.db.QueryRow(
		`SELECT MIN(ti) FROM (
		    SELECT MIN(turn_index) AS ti FROM turns
		     WHERE thread_id = ? AND turn_index < ?
		    UNION ALL
		    SELECT MIN(turn_index) AS ti FROM items
		     WHERE thread_id = ? AND turn_index < ?
		 ) WHERE ti IS NOT NULL`,
		threadID, beforeTurnIndex,
		threadID, beforeTurnIndex,
	)
	var ti sql.NullInt64
	if err := row.Scan(&ti); err != nil {
		return 0, false, fmt.Errorf("store: smallest turn before for %s: %w", threadID, err)
	}
	if !ti.Valid {
		return 0, false, nil
	}
	return int(ti.Int64), true, nil
}
