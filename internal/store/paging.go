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

// PagedItems is the return shape for windowed item loads
// (docs/architecture/timeline-window-pages.md §2).
//
// `Items` is sorted by (turn_index, item_index) ASC so callers can append
// or replace the slice directly in a timeline. It holds the page's PROSE
// rows plus, for each activity run in the page, only the members that
// would mount; `Runs` carries one stub per run in the range, counting
// every member the page did not ship.
//
// `OldestCursor` / `NewestCursor` are the inclusive item-coordinate bounds
// of the page's RANGE, which holds only whole units — prose rows and whole
// runs. Either cursor may therefore name a row that is not in `Items`: a
// page whose newest unit is a run ends at that run's last member even when
// the shipped span stops earlier. The cursor pagers accept such a cursor.
// Cursor turn/item indexes are -1 when the page is empty.
//
// Every physical row in [OldestCursor, NewestCursor] is either in `Items`
// or counted by exactly one stub. That invariant is what lets a client
// fold stubs into a held-window description (§5) and drop rows without
// asking the server (§6).
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
	Items           []Item            `json:"items"`
	Runs            []ActivityRunStub `json:"runs"`
	OldestCursor    TimelineCursor    `json:"oldestCursor"`
	NewestCursor    TimelineCursor    `json:"newestCursor"`
	OldestTurnIndex int               `json:"oldestTurnIndex"`
	NewestTurnIndex int               `json:"newestTurnIndex"`
	HasMore         bool              `json:"hasMore"`
	HasMoreOlder    bool              `json:"hasMoreOlder"`
	HasMoreNewer    bool              `json:"hasMoreNewer"`
}

// TrimShipped rebuilds the page around a contiguous sub-range of its own
// SHIPPED rows, `[from, to)` over `Items` — the byte backstop
// (docs/architecture/timeline-window-pages.md §2.2) is the one caller
// that shortens a page for its own reasons.
//
// A dropped row does not leave the page's range: if it is a run member it
// becomes unshipped and folds into its run's stub — digest, groups, side
// counts, span edges and the failed/running edges all move — so the
// invariant "every physical row in the range is shipped or counted by
// exactly one stub" survives the trim. Only when a unit loses every
// shipped row does it leave the range, which can happen at the far ends
// only, and then the cursor moves to the last surviving unit and the
// has-more flag on that side gains what was dropped.
//
// Trimming to nothing returns the empty page rather than a page with
// impossible cursors.
func (p PagedItems) TrimShipped(from, to int) PagedItems {
	if from <= 0 && to >= len(p.Items) {
		return p
	}
	if from < 0 {
		from = 0
	}
	if to > len(p.Items) {
		to = len(p.Items)
	}
	if from >= to {
		return emptyPagedItems()
	}
	kept := p.Items[from:to]
	keptIDs := make(map[string]struct{}, len(kept))
	for _, item := range kept {
		keptIDs[item.ID] = struct{}{}
	}

	runs := make([]ActivityRunStub, 0, len(p.Runs))
	for _, stub := range p.Runs {
		trimmed, survives := stub.trimShipped(keptIDs)
		if survives {
			runs = append(runs, trimmed)
		}
	}

	oldest := pageEdgeCursor(kept[0], runs, true)
	newest := pageEdgeCursor(kept[len(kept)-1], runs, false)
	hasMoreOlder := p.HasMoreOlder || cursorBefore(p.OldestCursor, oldest)
	hasMoreNewer := p.HasMoreNewer || cursorBefore(newest, p.NewestCursor)
	return PagedItems{
		Items:           kept,
		Runs:            runs,
		OldestCursor:    oldest,
		NewestCursor:    newest,
		OldestTurnIndex: oldest.TurnIndex,
		NewestTurnIndex: newest.TurnIndex,
		HasMore:         hasMoreOlder,
		HasMoreOlder:    hasMoreOlder,
		HasMoreNewer:    hasMoreNewer,
	}
}

// trimShipped folds every member this stub shipped that the trim dropped.
// survives=false means the run lost its whole shipped span, which by the
// trim's contiguity can only happen at a far end: the unit leaves the
// range entirely rather than lingering as a run nobody can reach.
func (p ActivityRunStub) trimShipped(keptIDs map[string]struct{}) (ActivityRunStub, bool) {
	firstKept, lastKept := -1, -1
	for i, fold := range p.shipped {
		if _, ok := keptIDs[fold.id]; !ok {
			continue
		}
		if firstKept < 0 {
			firstKept = i
		}
		lastKept = i
	}
	if firstKept < 0 {
		return ActivityRunStub{}, false
	}
	if firstKept == 0 && lastKept == len(p.shipped)-1 {
		return p, true
	}
	// Fold order is what foldShippedMember's running-edge rule relies on:
	// the older side folds oldest-first (each fold overwrites, the newest
	// stands), the newer side folds newest-first (the first fold stands).
	for i := 0; i < firstKept; i++ {
		p.foldShippedMember(p.shipped[i], true)
	}
	for i := len(p.shipped) - 1; i > lastKept; i-- {
		p.foldShippedMember(p.shipped[i], false)
	}
	p.shipped = p.shipped[firstKept : lastKept+1]
	p.LoadedFirstItemID = p.shipped[0].id
	p.LoadedLastItemID = p.shipped[len(p.shipped)-1].id
	return p, true
}

// pageEdgeCursor is the page range's edge on one side: the surviving edge
// ITEM when it is prose, or its whole run's physical edge when it is a run
// member, which may be a row the page does not ship.
func pageEdgeCursor(edge Item, runs []ActivityRunStub, oldestSide bool) TimelineCursor {
	for _, stub := range runs {
		for _, fold := range stub.shipped {
			if fold.id != edge.ID {
				continue
			}
			if oldestSide {
				return stub.first
			}
			return stub.last
		}
	}
	return cursorFromItem(edge)
}

// ListItemsBeforeCursor loads older visible top-level items strictly
// before `before`, in whole units, until `itemBudget` shipped rows have
// been selected. `runWindowRows` is how many members of each run the page
// ships; it is clamped to the settings bounds.
//
// The page's newest unit is expanded whole, so its range can reach back
// across `before` when the row immediately older than the cursor belongs
// to a run that continues past it. Those rows are counted by the run's
// stub, never re-shipped.
func (s *Store) ListItemsBeforeCursor(threadID string, before TimelineCursor, itemBudget, runWindowRows int) (PagedItems, error) {
	return readSnapshot(s.reader(), "before cursor page", func(q sqlQueryer) (PagedItems, error) {
		return s.listItemsBeforeCursor(q, threadID, before, itemBudget, runWindowRows)
	})
}

func (s *Store) listItemsBeforeCursor(q sqlQueryer, threadID string, before TimelineCursor, itemBudget, runWindowRows int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(before) {
		return emptyPagedItems(), nil
	}
	runWindowRows = clampActivityRunWindowRows(runWindowRows)
	unit, older, found, err := lastOlderUnit(q, threadID, before)
	if err != nil {
		return PagedItems{}, err
	}
	if !found {
		return emptyPagedItems(), nil
	}
	unit.shipWindow(runWindowRows, 0, countRowsBefore(unit.rows, before), nil)
	units, err := walkUnitsOlder(older, itemBudget-unit.shippedRows(), runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	return s.composePagedUnits(q, threadID, append(units, unit))
}

// ListItemsAfterCursor loads newer visible top-level items strictly after
// `after`, in whole units, until `itemBudget` shipped rows have been
// selected. It is the forward pager companion to ListItemsBeforeCursor
// and expands its oldest unit whole for the same reason.
func (s *Store) ListItemsAfterCursor(threadID string, after TimelineCursor, itemBudget, runWindowRows int) (PagedItems, error) {
	return readSnapshot(s.reader(), "after cursor page", func(q sqlQueryer) (PagedItems, error) {
		return s.listItemsAfterCursor(q, threadID, after, itemBudget, runWindowRows)
	})
}

func (s *Store) listItemsAfterCursor(q sqlQueryer, threadID string, after TimelineCursor, itemBudget, runWindowRows int) (PagedItems, error) {
	if itemBudget <= 0 || !cursorIsValid(after) {
		return emptyPagedItems(), nil
	}
	runWindowRows = clampActivityRunWindowRows(runWindowRows)
	unit, newer, found, err := firstNewerUnit(q, threadID, after)
	if err != nil {
		return PagedItems{}, err
	}
	if !found {
		return emptyPagedItems(), nil
	}
	unit.shipWindow(runWindowRows, countRowsAtOrBefore(unit.rows, after), len(unit.rows), nil)
	units, err := walkUnitsNewer(newer, itemBudget-unit.shippedRows(), runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	return s.composePagedUnits(q, threadID, append([]pageUnit{unit}, units...))
}

// countRowsBefore / countRowsAtOrBefore split a unit's rows at a cursor.
// They bound what a cursor pager's first unit may ship: the unit stays
// whole in the range, but the caller asked for rows outside the cursor and
// the budget belongs to those.
func countRowsBefore(rows []activityScanRow, at TimelineCursor) int {
	count := 0
	for _, row := range rows {
		if !cursorBefore(row.cursor(), at) {
			break
		}
		count++
	}
	return count
}

func countRowsAtOrBefore(rows []activityScanRow, at TimelineCursor) int {
	count := 0
	for _, row := range rows {
		if cursorBefore(at, row.cursor()) {
			break
		}
		count++
	}
	return count
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
	return s.decorateCompletionLaunches(q, threadID, decorated)
}

func emptyPagedItems() PagedItems {
	return PagedItems{
		Items:           []Item{},
		Runs:            []ActivityRunStub{},
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

func hasOlderItems(q sqlQueryer, threadID string, cursor TimelineCursor) (bool, error) {
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

func hasNewerItems(q sqlQueryer, threadID string, cursor TimelineCursor) (bool, error) {
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
// anchor. The slice ships roughly `targetItemCount` top-level rows
// (defaults to 50 when <= 0), split half at-or-before and half after the
// anchor's item coordinate, and composed in whole units so no run is
// split. `runWindowRows` is how many members of each run the page ships,
// clamped to the settings bounds.
//
// The ANCHOR's run ships its window centered on the anchor, so a jump
// lands on a mounted row; every other run ships its newest members. The
// anchor may be a subagent child: its coordinates still position the
// window even though child rows themselves load through
// ListSubagentDescendants.
//
// When `anchorItemID` is "" or the item doesn't belong to `threadID`
// (bottom-snapshot restore, stale snapshot whose anchor has been
// deleted), the function returns the tail window.
func (s *Store) ListThreadSliceAround(threadID, anchorItemID string, targetItemCount, runWindowRows int) (PagedItems, error) {
	return readSnapshot(s.reader(), "thread slice", func(q sqlQueryer) (PagedItems, error) {
		return s.listThreadSliceAround(q, threadID, anchorItemID, targetItemCount, runWindowRows)
	})
}

// listThreadSliceAround is ListThreadSliceAround against a caller-chosen
// queryer, so SyncThreadWindow can run the same window inside the
// transaction its stamps are read in.
func (s *Store) listThreadSliceAround(q sqlQueryer, threadID, anchorItemID string, targetItemCount, runWindowRows int) (PagedItems, error) {
	if targetItemCount <= 0 {
		targetItemCount = 50
	}
	runWindowRows = clampActivityRunWindowRows(runWindowRows)
	if anchorItemID == "" {
		return s.listTailSlice(q, threadID, targetItemCount, runWindowRows)
	}
	anchor, found, err := s.getThreadItem(q, threadID, anchorItemID)
	if err != nil {
		return PagedItems{}, fmt.Errorf("store: list thread slice for %s anchor=%s: %w", threadID, anchorItemID, err)
	}
	if !found {
		return s.listTailSlice(q, threadID, targetItemCount, runWindowRows)
	}

	atOrBeforeBudget := targetItemCount / 2
	if atOrBeforeBudget < 1 {
		atOrBeforeBudget = 1
	}
	afterBudget := targetItemCount - atOrBeforeBudget
	if afterBudget < 1 {
		afterBudget = 1
	}
	anchorCursor := cursorFromItem(anchor)
	unit, older, newer, found, err := unitAtOrBefore(q, threadID, anchorCursor)
	if err != nil {
		return PagedItems{}, err
	}
	if !found {
		// Nothing visible at or before the anchor — a child anchor in the
		// thread's first turn. The whole budget goes to the newer side.
		return s.listSliceAfter(q, threadID, anchorCursor, targetItemCount, runWindowRows)
	}
	unit.shipWindow(runWindowRows, 0, len(unit.rows), &anchorCursor)
	olderUnits, err := walkUnitsOlder(older, atOrBeforeBudget-unit.shippedRows(), runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	newerUnits, err := walkUnitsNewer(newer, afterBudget, runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	units := append(olderUnits, unit)
	return s.composePagedUnits(q, threadID, append(units, newerUnits...))
}

// listSliceAfter is the whole-budget forward window, used when an anchor
// coordinate has no visible top-level row at or before it.
func (s *Store) listSliceAfter(q sqlQueryer, threadID string, after TimelineCursor, targetItemCount, runWindowRows int) (PagedItems, error) {
	w := newActivityScanWalk(q, threadID, after, true)
	units, err := walkUnitsNewer(w, targetItemCount, runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	return s.composePagedUnits(q, threadID, units)
}

// listTailSlice returns the newest units of the thread, shipping each
// run's newest `runWindowRows` members, until `targetItemCount` rows have
// been selected. Used when the snapshot is a bottom-restore or the anchor
// item has been deleted.
func (s *Store) listTailSlice(q sqlQueryer, threadID string, targetItemCount, runWindowRows int) (PagedItems, error) {
	w := newActivityScanWalk(q, threadID, timelineTailBound(), false)
	units, err := walkUnitsOlder(w, targetItemCount, runWindowRows)
	if err != nil {
		return PagedItems{}, err
	}
	return s.composePagedUnits(q, threadID, units)
}
