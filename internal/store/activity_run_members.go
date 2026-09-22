package store

import (
	"context"
	"errors"
	"fmt"
)

// On-demand run members (docs/architecture/timeline-window-pages.md §3).
//
// A page ships a window of each run and a stub for the rest. When the
// reader mounts past that window — scrolling inside a collapsed run, or
// jumping to a member the page did not ship — the pane asks for the
// adjacent members and gets back the rows plus the stub for the span it
// holds AFTER the call.

// ErrActivityRunStale is returned when the run the caller describes is no
// longer the run the store has: it does not start where the caller says,
// or the span the caller claims to hold is not made of its members. The
// pane reports it and follows with a window reload; there is no partial
// answer that would be safe, because every count the caller would fold
// into its header and its held window depends on the span being real.
var ErrActivityRunStale = errors.New("store: activity run no longer matches the caller's span")

// ActivityRunMembersRequest asks for members of one run adjacent to the
// span the caller holds.
type ActivityRunMembersRequest struct {
	Selection TimelineSelection
	// RunFirstItemID identifies the run. The store refuses the call when
	// the run containing it no longer starts there.
	RunFirstItemID string
	// LoadedFirstItemID / LoadedLastItemID bound the members the caller
	// already holds. Both "" when it holds none, which is the shape a
	// collapsed run's first fetch has.
	LoadedFirstItemID string
	LoadedLastItemID  string
	// Direction is "before", "after" or "around".
	//
	// "before" and "after" EXTEND the loaded span outward from its edges;
	// with no loaded span they take the run's newest and oldest members
	// respectively, the same ends a page and a forward pager would take.
	//
	// "around" REPLACES the span, centered on AroundItemID: it is the jump
	// path for a target inside a run the pane already holds, and the rows
	// the caller was holding become unshipped in the returned stub.
	Direction string
	// AroundItemID is the member "around" centers on.
	AroundItemID string
	// Limit caps the members returned. 0 returns none and refreshes the
	// stub for the span the caller already holds.
	Limit int
}

// ActivityRunMembers is one answer: the rows to mount, and the stub that
// describes the run for the span the caller holds after mounting them.
type ActivityRunMembers struct {
	Items       []Item          `json:"items"`
	Stub        ActivityRunStub `json:"stub"`
	heldFirstID string
	heldLastID  string
	direction   string
}

// TrimFresh folds byte-rejected new members into the already computed stub.
// The existing held span remains mounted for extensions; an around request
// replaces it. This gives the app its smaller exact answer without scanning
// the whole run a second time after it measures projected item bytes.
func (members ActivityRunMembers) TrimFresh(from, to int) (ActivityRunMembers, error) {
	if from < 0 || from >= to || to > len(members.Items) {
		return ActivityRunMembers{}, fmt.Errorf("store: invalid activity member trim %d..%d of %d", from, to, len(members.Items))
	}
	first := members.Items[from].ID
	last := members.Items[to-1].ID
	switch members.direction {
	case ActivityRunMembersBefore:
		if members.heldLastID != "" {
			last = members.heldLastID
		}
	case ActivityRunMembersAfter:
		if members.heldFirstID != "" {
			first = members.heldFirstID
		}
	case ActivityRunMembersAround:
	default:
		return ActivityRunMembers{}, fmt.Errorf("store: invalid activity member trim direction %q", members.direction)
	}
	trimmed, ok := members.Stub.trimShipped(map[string]struct{}{first: {}, last: {}})
	if !ok || trimmed.LoadedFirstItemID != first || trimmed.LoadedLastItemID != last {
		return ActivityRunMembers{}, fmt.Errorf("store: activity member trim lost span %s..%s", first, last)
	}
	members.Items = members.Items[from:to]
	members.Stub = trimmed
	return members, nil
}

// Directions for ActivityRunMembersRequest.
const (
	ActivityRunMembersBefore = "before"
	ActivityRunMembersAfter  = "after"
	ActivityRunMembersAround = "around"
)

// maxActivityRunMemberLimit caps one members response. It is the run
// window's cap times a small factor: the pane fetches a chunk at a time,
// and an unintended LAN-attached caller must not be able to ask for a
// 5,000-row run in one call.
const maxActivityRunMemberLimit = 500

// ListActivityRunMembers returns up to `Limit` members of one run
// adjacent to the caller's loaded span, plus the stub describing the run
// for the span the caller holds after this call.
//
// The run is re-derived from the database on every call: the store
// resolves RunFirstItemID's coordinate, expands the whole run around it,
// and refuses the request unless the run still starts there and every id
// the caller claims to hold is a member. Nothing the caller sends is
// trusted as a description of the run.
func (s *Store) ListActivityRunMembers(ctx context.Context, threadID string, req ActivityRunMembersRequest) (ActivityRunMembers, error) {
	return readSnapshotContext(ctx, s.reader(), "activity members read", func(q sqlQueryer) (ActivityRunMembers, error) {
		return s.listActivityRunMembers(q, threadID, req)
	})
}

func (s *Store) listActivityRunMembers(q sqlQueryer, threadID string, req ActivityRunMembersRequest) (ActivityRunMembers, error) {
	if threadID == "" || req.RunFirstItemID == "" {
		return ActivityRunMembers{}, fmt.Errorf(
			"store: list activity run members needs a thread and a run: %w", ErrActivityRunStale)
	}
	if req.Limit < 0 || req.Limit > maxActivityRunMemberLimit {
		return ActivityRunMembers{}, fmt.Errorf(
			"store: activity run member limit %d for %s is outside 0..%d",
			req.Limit, threadID, maxActivityRunMemberLimit)
	}
	scope, err := s.resolveTimelineScope(q, threadID, req.Selection)
	if err != nil {
		return ActivityRunMembers{}, err
	}
	rows, err := s.activityRunMembers(q, threadID, req.RunFirstItemID, scope)
	if err != nil {
		return ActivityRunMembers{}, err
	}
	from, to, err := activityRunSpan(threadID, rows, req)
	if err != nil {
		return ActivityRunMembers{}, err
	}
	// Only the rows the caller does not hold ship: an extension returns
	// the members past the old span's edge, a replacement returns the new
	// span, a stub refresh (Limit 0) returns none. The stub describes the
	// whole new span either way.
	fresh, err := newlyMounted(rows, from, to, req)
	if err != nil {
		return ActivityRunMembers{}, err
	}
	items, err := s.hydratePageItems(q, threadID, activityRunMemberIDs(fresh))
	if err != nil {
		return ActivityRunMembers{}, err
	}
	return ActivityRunMembers{
		Items:       items,
		Stub:        buildActivityRunStub(rows, from, to),
		heldFirstID: req.LoadedFirstItemID,
		heldLastID:  req.LoadedLastItemID,
		direction:   req.Direction,
	}, nil
}

// activityRunMembers expands the whole run that starts at firstItemID,
// and refuses anything else: a run is identified by its first member, so
// a run that now starts earlier is a different run to every client rule
// that reads the id.
func (s *Store) activityRunMembers(q sqlQueryer, threadID, firstItemID string, scope timelineScope) ([]activityScanRow, error) {
	first, found, err := s.getThreadItem(q, threadID, firstItemID)
	if err != nil {
		return nil, fmt.Errorf("store: resolve activity run %s/%s: %w", threadID, firstItemID, err)
	}
	if !found {
		return nil, fmt.Errorf("store: activity run %s/%s is gone: %w",
			threadID, firstItemID, ErrActivityRunStale)
	}
	unit, _, _, found, err := unitAtOrBefore(q, threadID, cursorFromItem(first), scope)
	if err != nil {
		return nil, err
	}
	if !found || !unit.run || unit.oldest().ID != firstItemID {
		return nil, fmt.Errorf("store: activity run %s/%s no longer starts there: %w",
			threadID, firstItemID, ErrActivityRunStale)
	}
	return unit.rows, nil
}

// activityRunSpan resolves the half-open member range this call returns.
// A loaded span the caller names must be a real span of this run: ids
// that are not members, or an inverted pair, mean the caller's picture and
// the store's have diverged and every count derived from either is wrong.
func activityRunSpan(threadID string, rows []activityScanRow, req ActivityRunMembersRequest) (int, int, error) {
	first, last, center := -1, -1, -1
	for i, row := range rows {
		if req.LoadedFirstItemID != "" && row.ID == req.LoadedFirstItemID {
			first = i
		}
		if req.LoadedLastItemID != "" && row.ID == req.LoadedLastItemID {
			last = i
		}
		if req.AroundItemID != "" && row.ID == req.AroundItemID {
			center = i
		}
	}
	loadedFrom, loadedTo := -1, -1
	if req.LoadedFirstItemID != "" || req.LoadedLastItemID != "" {
		if first < 0 || last < 0 || first > last {
			return 0, 0, fmt.Errorf(
				"store: loaded span %s..%s is not a span of activity run %s/%s: %w",
				req.LoadedFirstItemID, req.LoadedLastItemID, threadID, req.RunFirstItemID,
				ErrActivityRunStale)
		}
		loadedFrom, loadedTo = first, last+1
	}

	switch req.Direction {
	case ActivityRunMembersAround:
		if center < 0 {
			return 0, 0, fmt.Errorf(
				"store: %s is not a member of activity run %s/%s: %w",
				req.AroundItemID, threadID, req.RunFirstItemID, ErrActivityRunStale)
		}
		if req.Limit == 0 {
			// Nothing to mount, so the span the caller ends up holding is
			// the one it already has.
			return clampActivityRunSpan(loadedFrom, loadedTo, len(rows))
		}
		width := min(req.Limit, len(rows))
		start := center - width/2
		if start < 0 {
			start = 0
		}
		if start > len(rows)-width {
			start = len(rows) - width
		}
		return start, start + width, nil
	case ActivityRunMembersBefore:
		if req.Limit == 0 {
			return clampActivityRunSpan(loadedFrom, loadedTo, len(rows))
		}
		if loadedFrom < 0 {
			// No span held: "before" the end of the run is its newest
			// members, the same end a page ships.
			start := len(rows) - min(req.Limit, len(rows))
			return start, len(rows), nil
		}
		start := loadedFrom - req.Limit
		if start < 0 {
			start = 0
		}
		return start, loadedTo, nil
	case ActivityRunMembersAfter:
		if req.Limit == 0 {
			return clampActivityRunSpan(loadedFrom, loadedTo, len(rows))
		}
		if loadedTo < 0 {
			return 0, min(req.Limit, len(rows)), nil
		}
		end := loadedTo + req.Limit
		if end > len(rows) {
			end = len(rows)
		}
		return loadedFrom, end, nil
	default:
		return 0, 0, fmt.Errorf(
			"store: activity run member direction %q is not before, after or around", req.Direction)
	}
}

// newlyMounted is the part of the resolved span [from, to) the caller does
// not already hold. "before" and "after" extend a held span, so the rows
// inside it are excluded; "around" replaces the span and ships all of it.
func newlyMounted(rows []activityScanRow, from, to int, req ActivityRunMembersRequest) ([]activityScanRow, error) {
	if req.Limit == 0 {
		return nil, nil
	}
	if req.Direction == ActivityRunMembersAround || req.LoadedFirstItemID == "" {
		return rows[from:to], nil
	}
	if req.Direction == ActivityRunMembersBefore {
		for i := from; i < to; i++ {
			if rows[i].ID == req.LoadedFirstItemID {
				return rows[from:i], nil
			}
		}
	}
	if req.Direction == ActivityRunMembersAfter {
		for i := from; i < to; i++ {
			if rows[i].ID == req.LoadedLastItemID {
				return rows[i+1 : to], nil
			}
		}
	}
	return nil, fmt.Errorf("store: loaded activity span has no extension boundary: %w", ErrActivityRunStale)
}

// clampActivityRunSpan is the stub-only answer's span: whatever the caller
// holds, or nothing when it holds nothing.
func clampActivityRunSpan(from, to, members int) (int, int, error) {
	if from < 0 || to < 0 {
		return 0, 0, nil
	}
	if to > members {
		to = members
	}
	return from, to, nil
}

func activityRunMemberIDs(rows []activityScanRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}
