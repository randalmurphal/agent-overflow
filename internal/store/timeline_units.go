package store

import (
	"fmt"
	"math"
)

// Page units (docs/architecture/timeline-window-pages.md §2.1 step 1-2).
//
// A page is composed of UNITS, not rows: a prose row, or a whole activity
// run. A window never splits a run, so every page edge lands on a run
// boundary or a prose row, and a held window is describable as "these
// edges, these shipped rows, these stubs".
//
// The membership rule is asymmetric, which is what the two walkers here
// are about. A rail row is a member on sight. A notification is a member
// only when the row IMMEDIATELY BEFORE it is one (an absorbed bell), so
// walking backward means holding a chain of undecided bells until a rail
// row admits them or a prose row refuses them, while walking forward can
// decide every row as it arrives.

// pageUnit is one unit of a page with the members this page ships.
type pageUnit struct {
	// rows is every physical row of the unit in (turn_index, item_index)
	// order: one row for prose, every member for a run.
	rows []activityScanRow
	// run distinguishes a run from a prose row. A one-member run is still
	// a run: the client's rail is a collapse control at every length.
	run bool
	// shippedFrom / shippedTo bound the members this page ships as a
	// half-open range over rows.
	shippedFrom, shippedTo int
}

func (u pageUnit) oldest() activityScanRow { return u.rows[0] }
func (u pageUnit) newest() activityScanRow { return u.rows[len(u.rows)-1] }

func (u pageUnit) shippedRows() int { return u.shippedTo - u.shippedFrom }

// shipWindow selects the members the page ships from this unit.
//
// Candidates are rows[lo:hi]. That is the whole unit except on a cursor
// pager's first unit, which may reach back across the requested cursor to
// stay whole: re-shipping rows the caller explicitly excluded would spend
// the page's budget on rows it already holds.
//
// `center` is the anchor coordinate when this is the anchor's unit, nil
// otherwise. The anchor's run ships its window centered on the newest
// member at or before that coordinate, so a jump lands on a mounted row
// and an anchor that is a subagent child centers on the row it renders
// inside; every other run ships its newest members, which is the end a
// reader arrives at.
func (u *pageUnit) shipWindow(runWindowRows, lo, hi int, center *TimelineCursor) {
	if lo < 0 {
		lo = 0
	}
	if hi > len(u.rows) {
		hi = len(u.rows)
	}
	if lo >= hi {
		u.shippedFrom, u.shippedTo = lo, lo
		return
	}
	if !u.run {
		u.shippedFrom, u.shippedTo = lo, hi
		return
	}
	width := runWindowRows
	if width > hi-lo {
		width = hi - lo
	}
	start := hi - width
	if center != nil {
		at := lo
		for i := lo; i < hi && !cursorBefore(*center, u.rows[i].cursor()); i++ {
			at = i
		}
		start = at - width/2
		if start < lo {
			start = lo
		}
		if start > hi-width {
			start = hi - width
		}
	}
	u.shippedFrom, u.shippedTo = start, start+width
}

// timelineTailBound is the exclusive bound an older-side walk starts from
// to reach a thread's newest row. Turn and item indexes are SQLite
// INTEGERs written from Go ints, so no row can sit at or past it.
func timelineTailBound() TimelineCursor {
	return TimelineCursor{TurnIndex: math.MaxInt32, ItemIndex: math.MaxInt32}
}

// inclusiveOlderBound turns a coordinate into the exclusive bound an
// older-side walk needs to INCLUDE that coordinate. Item indexes are
// dense within a turn only by convention — head-healed prompts go
// negative — so the bound is one step past the item index, never a turn
// boundary.
func inclusiveOlderBound(at TimelineCursor) TimelineCursor {
	return TimelineCursor{TurnIndex: at.TurnIndex, ItemIndex: at.ItemIndex + 1}
}

// cursorBefore reports whether a sits strictly older than b.
func cursorBefore(a, b TimelineCursor) bool {
	if a.TurnIndex != b.TurnIndex {
		return a.TurnIndex < b.TurnIndex
	}
	return a.ItemIndex < b.ItemIndex
}

// errRunTooLong is the refusal §2.1 step 3 requires. It names the run so
// an operator can find it; a truncated stub would be a silent wrong
// answer instead.
func errRunTooLong(threadID string, first activityScanRow) error {
	return fmt.Errorf(
		"store: activity run starting at %s/%s exceeds %d rows; refusing to build a truncated stub",
		threadID, first.ID, maxActivityRunScanRows)
}

// nextOlderUnit consumes the unit immediately older than the walk's
// position. ok=false means the walk reached the start of history.
//
// A run is accumulated newest-first: each round looks past a chain of
// undecided notifications for a rail row, and admits the whole chain only
// when it finds one. A chain that ends at prose or at the start of
// history is not absorbed, and the first of those bells is a unit of its
// own.
func nextOlderUnit(w *activityScanWalk) (pageUnit, bool, error) {
	first, ok, err := w.peekAt(0)
	if err != nil || !ok {
		return pageUnit{}, false, err
	}
	var members []activityScanRow
	for {
		bells := 0
		admit := false
		for {
			row, ok, err := w.peekAt(bells)
			if err != nil {
				return pageUnit{}, false, err
			}
			if !ok {
				break
			}
			if row.isRailRow() {
				admit = true
				break
			}
			if row.Kind != notificationKind {
				break
			}
			bells++
			if bells+len(members) > maxActivityRunScanRows {
				return pageUnit{}, false, errRunTooLong(w.threadID, first)
			}
		}
		if !admit {
			break
		}
		for i := 0; i <= bells; i++ {
			row, ok, err := w.peekAt(0)
			if err != nil {
				return pageUnit{}, false, err
			}
			if !ok {
				return pageUnit{}, false, fmt.Errorf(
					"store: activity run walk lost a peeked row on %s", w.threadID)
			}
			members = append(members, row)
			w.take(1)
		}
		if len(members) > maxActivityRunScanRows {
			return pageUnit{}, false, errRunTooLong(w.threadID, first)
		}
	}
	if len(members) == 0 {
		w.take(1)
		return pageUnit{rows: []activityScanRow{first}}, true, nil
	}
	reverseActivityScanRows(members)
	return pageUnit{rows: members, run: true}, true, nil
}

// nextNewerUnit consumes the unit immediately newer than the walk's
// position. `prevIsMember` is whether the row before the walk's first row
// is a run member, which is the only thing a leading notification's
// membership depends on.
func nextNewerUnit(w *activityScanWalk, prevIsMember bool) (pageUnit, bool, error) {
	first, ok, err := w.peekAt(0)
	if err != nil || !ok {
		return pageUnit{}, false, err
	}
	w.take(1)
	if !first.isRailRow() && !(prevIsMember && first.Kind == notificationKind) {
		return pageUnit{rows: []activityScanRow{first}}, true, nil
	}
	unit := pageUnit{rows: []activityScanRow{first}, run: true}
	if err := absorbNewerMembers(w, &unit); err != nil {
		return pageUnit{}, false, err
	}
	return unit, true, nil
}

// absorbNewerMembers extends a run forward over the walk: every rail row
// joins, and so does every notification, because a bell whose predecessor
// is a member is absorbed and stays absorbed once prose settles the run.
func absorbNewerMembers(w *activityScanWalk, unit *pageUnit) error {
	for {
		row, ok, err := w.peekAt(0)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if !row.isRailRow() && row.Kind != notificationKind {
			return nil
		}
		unit.rows = append(unit.rows, row)
		w.take(1)
		if len(unit.rows) > maxActivityRunScanRows {
			return errRunTooLong(w.threadID, unit.rows[0])
		}
	}
}

// unitAtOrBefore resolves the whole unit that owns the newest visible
// top-level row at or older than `at`, and leaves both walks positioned
// on its edges so the page can keep walking outward from it.
//
// `at` may name a row that is not in a window at all — the anchor can be
// a subagent child, whose coordinates position the window even though the
// row itself never renders as a timeline row.
func unitAtOrBefore(
	q sqlQueryer,
	threadID string,
	at TimelineCursor,
) (unit pageUnit, older *activityScanWalk, newer *activityScanWalk, found bool, err error) {
	older = newActivityScanWalk(q, threadID, inclusiveOlderBound(at), false)
	unit, found, err = nextOlderUnit(older)
	if err != nil || !found {
		return pageUnit{}, nil, nil, false, err
	}
	newer = newActivityScanWalk(q, threadID, unit.newest().cursor(), true)
	if unit.run {
		if err := absorbNewerMembers(newer, &unit); err != nil {
			return pageUnit{}, nil, nil, false, err
		}
	}
	return unit, older, newer, true, nil
}

// firstNewerUnit resolves the unit that owns the oldest visible top-level
// row strictly newer than `after`, expanded WHOLE — backward across the
// cursor when that row continues a run the caller's page ended on.
//
// A page range holds only whole units, so a run that grew past the
// caller's last page comes back whole rather than as a headless fragment
// the client would mint a second run record for. The rows the caller
// already excluded are still in the range; they are counted by the stub,
// not shipped again.
func firstNewerUnit(
	q sqlQueryer,
	threadID string,
	after TimelineCursor,
) (unit pageUnit, newer *activityScanWalk, found bool, err error) {
	newer = newActivityScanWalk(q, threadID, after, true)
	first, ok, err := newer.peekAt(0)
	if err != nil || !ok {
		return pageUnit{}, nil, false, err
	}
	if !first.isRailRow() && first.Kind != notificationKind {
		newer.take(1)
		return pageUnit{rows: []activityScanRow{first}}, newer, true, nil
	}
	older := newActivityScanWalk(q, threadID, inclusiveOlderBound(after), false)
	previous, hasPrevious, err := nextOlderUnit(older)
	if err != nil {
		return pageUnit{}, nil, false, err
	}
	if hasPrevious && previous.run {
		if err := absorbNewerMembers(newer, &previous); err != nil {
			return pageUnit{}, nil, false, err
		}
		return previous, newer, true, nil
	}
	next, found, err := nextNewerUnit(newer, false)
	if err != nil || !found {
		return pageUnit{}, nil, false, err
	}
	return next, newer, true, nil
}

// lastOlderUnit is firstNewerUnit's mirror: the unit that owns the newest
// visible top-level row strictly older than `before`, expanded whole —
// forward across the cursor when that row belongs to a run the caller's
// page starts with.
func lastOlderUnit(
	q sqlQueryer,
	threadID string,
	before TimelineCursor,
) (unit pageUnit, older *activityScanWalk, found bool, err error) {
	older = newActivityScanWalk(q, threadID, before, false)
	unit, found, err = nextOlderUnit(older)
	if err != nil || !found {
		return pageUnit{}, nil, false, err
	}
	if unit.run {
		newer := newActivityScanWalk(q, threadID, unit.newest().cursor(), true)
		if err := absorbNewerMembers(newer, &unit); err != nil {
			return pageUnit{}, nil, false, err
		}
	}
	return unit, older, true, nil
}

// walkUnitsOlder admits whole units older than the walk's position until
// the shipped-row budget is reached. The unit that crosses the budget is
// admitted whole — a run is never split — which is bounded because a run
// unit ships at most runWindowRows rows.
//
// Units come back in page order, oldest first.
func walkUnitsOlder(w *activityScanWalk, budget, runWindowRows int) ([]pageUnit, error) {
	var units []pageUnit
	for shipped := 0; shipped < budget; {
		unit, found, err := nextOlderUnit(w)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		unit.shipWindow(runWindowRows, 0, len(unit.rows), nil)
		shipped += unit.shippedRows()
		units = append(units, unit)
	}
	for i, j := 0, len(units)-1; i < j; i, j = i+1, j-1 {
		units[i], units[j] = units[j], units[i]
	}
	return units, nil
}

// walkUnitsNewer is walkUnitsOlder's mirror. Units come back in page
// order, which is walk order on this side.
func walkUnitsNewer(w *activityScanWalk, budget, runWindowRows int) ([]pageUnit, error) {
	var units []pageUnit
	for shipped := 0; shipped < budget; {
		unit, found, err := nextNewerUnit(w, false)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		unit.shipWindow(runWindowRows, 0, len(unit.rows), nil)
		shipped += unit.shippedRows()
		units = append(units, unit)
	}
	return units, nil
}
