package app

import (
	"fmt"

	"agent-overflow/internal/itemwire"
	"agent-overflow/internal/settings"
	"agent-overflow/internal/slicesx"
	"agent-overflow/internal/store"
)

// itemWindowMaxBytes is the byte backstop every item-window load carries
// beside its row-count budget. Count alone bounds reducer churn; bytes
// alone bounds a small number of very large rows; neither substitutes
// for the other, and neither substitutes for not sending the field
// (docs/specs/remote-access.md §14).
//
// 512 KiB is deliberately well above the heaviest window measured on a
// real thread (330 KB raw for 200 rows, before the field projection that
// ships with this constant). Cutting rows trades visible history for
// bytes and cutting an unrendered field trades nothing, so the row count
// is what this must NOT reach for: it is a ceiling on the pathological
// page — a handful of rows carrying tens of KB each — not a second
// window size.
//
// It is also the ceiling a caller's own PageShape.MaxBytes is clamped to:
// a client may ask for less, never for more.
const itemWindowMaxBytes = 512 << 10

// PageShape is how a caller asks for a history page to be SHAPED, as
// opposed to which rows it covers (docs/architecture/
// timeline-window-pages.md §2.3). Every field is a per-client property
// this process cannot read for itself: one backend serves several
// clients that disagree about previews, about how many rows of a run
// their screen mounts, and about how many bytes a page may cost them.
//
// It rides `ListThreadSliceAround`, `ListItemsBeforeCursor`,
// `ListItemsAfterCursor` and `ListActivityRunMembers` as a parameter, and
// `SyncThreadWindow` as three fields of its flat JSON request body.
type PageShape struct {
	// InlinePreviews is true when the client paints inline diff previews
	// on arrival, false when they sit behind a chevron
	// (`collapseDiffPreviews`, the default) and none of the patch text is
	// rendered until clicked.
	InlinePreviews bool `json:"inlinePreviews,omitempty"`
	// RunWindowRows is the client's `activityRunWindowRows`: how many
	// members of each activity run in the page it can mount. 0 (unset)
	// takes the setting's default; anything else is clamped to the
	// settings bounds.
	RunWindowRows int `json:"runWindowRows,omitempty"`
	// MaxBytes is the projected-byte ceiling the caller wants the page
	// trimmed to. 0 means itemWindowMaxBytes; anything larger is capped
	// to it.
	MaxBytes int `json:"maxBytes,omitempty"`
}

// normalize is the one place a caller-supplied shape becomes the shape a
// read is served under. The store clamps `RunWindowRows` again, and this
// clamp is what the contract documents: every binding goes through here,
// so a client cannot make one page read a wider run window or spend more
// bytes than another.
func (s PageShape) normalize() PageShape {
	if s.RunWindowRows == 0 {
		s.RunWindowRows = settings.DefaultActivityRunWindowRows
	}
	if s.RunWindowRows < settings.MinActivityRunWindowRows {
		s.RunWindowRows = settings.MinActivityRunWindowRows
	}
	if s.RunWindowRows > settings.MaxActivityRunWindowRows {
		s.RunWindowRows = settings.MaxActivityRunWindowRows
	}
	if s.MaxBytes <= 0 || s.MaxBytes > itemWindowMaxBytes {
		s.MaxBytes = itemWindowMaxBytes
	}
	return s
}

// projectPage is the single wire projection for a paged item load: every
// SHIPPED row is bounded field-wise, then the page is bounded byte-wise
// around the row at `anchor`, the row the reader is positioned on. A
// cursor page anchors at the end nearest the pane's held window; an
// anchored slice anchors at the item it was asked for.
//
// Trimming goes through store.PagedItems.TrimShipped, so a row this
// drops does not leave the page's RANGE: if it is a run member it folds
// into that run's stub and the page still accounts for every physical
// row between its cursors (§2.2).
//
// `shape` must already be normalized: this is the byte ceiling the caller
// asked for, not the process-wide one.
func projectPage(paged store.PagedItems, shape PageShape, anchor int) store.PagedItems {
	paged.Scope = projectScopeContext(paged.Scope, shape)
	paged.Items = itemwire.ProjectItems(slicesx.OrEmpty(paged.Items), shape.InlinePreviews)
	from, to := admittedRange(paged.Items, anchor, shape.MaxBytes-scopeContextBytes(paged.Scope))
	return paged.TrimShipped(from, to)
}

// projectItemSlice is projectPage for the loads that return a bare slice
// (subagent descendants, proposed plans, the tray feed, the unwindowed
// list). They carry no cursors and no run stubs, so an over-budget slice
// keeps the rows nearest the reader, which is the newest end, and drops
// the rest rather than reporting a boundary it cannot describe. None of
// them is a history page, so none carries a caller shape: the backstop
// they spend is the process ceiling.
func projectItemSlice(items []store.Item, inlinePreviews bool) []store.Item {
	items = itemwire.ProjectItems(slicesx.OrEmpty(items), inlinePreviews)
	from, to := admittedRange(items, newestIndex(items), itemWindowMaxBytes)
	return items[from:to]
}

// newestIndex is the anchor for a page read from its newest end: a
// backward cursor page, a tail slice, or a bare slice.
func newestIndex(items []store.Item) int {
	return len(items) - 1
}

// pageAnchorIndex resolves the SHIPPED row a slice's byte trim grows out
// from. The anchor a caller names need not be shipped: a subagent child
// anchors a window it can never appear in, and a run member the page did
// not ship (a deleted anchor's replacement, a member outside the run
// window) is counted by a stub instead. In those cases the reader is
// still positioned at the anchor's COORDINATE, so the trim grows from the
// newest shipped row at or before it — not from the newest row of the
// page, which can be thousands of rows away.
//
// Resolving that coordinate costs one indexed point read, and only on the
// unshipped-anchor path: `ListThreadSliceAround` takes the anchor by id
// and the page it returns carries no coordinate for a row it did not
// ship, so there is nothing cheaper to read it from. A vanished anchor is
// the store's tail fallback, which anchors at the newest row like any
// other tail read.
func (a *App) pageAnchorIndex(threadID, anchorItemID string, items []store.Item) (int, error) {
	if anchorItemID == "" || len(items) == 0 {
		return newestIndex(items), nil
	}
	if i := indexOfItemID(items, anchorItemID); i >= 0 {
		return i, nil
	}
	anchor, found, err := a.store.GetThreadItem(threadID, anchorItemID)
	if err != nil {
		return 0, fmt.Errorf("resolve page anchor %s: %w", anchorItemID, err)
	}
	if !found {
		return newestIndex(items), nil
	}
	return newestShippedAtOrBefore(items, anchor), nil
}

// indexOfItemID is the index of the row with this id, or -1.
func indexOfItemID(items []store.Item, id string) int {
	for i := range items {
		if items[i].ID == id {
			return i
		}
	}
	return -1
}

// newestShippedAtOrBefore is the index of the last row of `items` whose
// timeline coordinate is at or before `anchor`'s, or 0 when the anchor
// precedes every shipped row — the nearest shipped row in that case is
// the oldest one. `items` is in (turn_index, item_index) order.
func newestShippedAtOrBefore(items []store.Item, anchor store.Item) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].TurnIndex < anchor.TurnIndex ||
			(items[i].TurnIndex == anchor.TurnIndex && items[i].ItemIndex <= anchor.ItemIndex) {
			return i
		}
	}
	return 0
}

// admittedRange grows a contiguous range outward from `anchor`, one row
// per side per step, admitting rows until `maxBytes` is reached on that
// side. The anchor is admitted unconditionally: a page whose anchor is
// the row it dropped would leave a jump with nothing to land on and a
// cursor page with a boundary that never moves, so an oversized anchor
// ships alone rather than not at all.
func admittedRange(items []store.Item, anchor, maxBytes int) (int, int) {
	if len(items) == 0 {
		return 0, 0
	}
	anchor = min(max(anchor, 0), len(items)-1)
	lo, hi := anchor, anchor+1
	spent := itemwire.EncodedBytes(items[anchor])
	olderOpen, newerOpen := true, true
	for olderOpen || newerOpen {
		if olderOpen {
			if cost := costAt(items, lo-1); cost >= 0 && spent+cost <= maxBytes {
				spent += cost
				lo--
			} else {
				olderOpen = false
			}
		}
		if newerOpen {
			if cost := costAt(items, hi); cost >= 0 && spent+cost <= maxBytes {
				spent += cost
				hi++
			} else {
				newerOpen = false
			}
		}
	}
	return lo, hi
}

// costAt is the wire cost of items[i], or -1 past either end.
func costAt(items []store.Item, i int) int {
	if i < 0 || i >= len(items) {
		return -1
	}
	return itemwire.EncodedBytes(items[i])
}

func projectScopeContext(context *store.TimelineScopeContext, shape PageShape) *store.TimelineScopeContext {
	if context == nil {
		return nil
	}
	projected := *context
	projected.Root = itemwire.Project(context.Root, shape.InlinePreviews)
	projected.Lifecycle = itemwire.Project(context.Lifecycle, shape.InlinePreviews)
	if context.Completion != nil {
		completion := itemwire.Project(*context.Completion, shape.InlinePreviews)
		projected.Completion = &completion
	}
	return &projected
}

func scopeContextBytes(scope *store.TimelineScopeContext) int {
	if scope == nil {
		return 0
	}
	cost := itemwire.EncodedBytes(scope.Root) + itemwire.EncodedBytes(scope.Lifecycle)
	if scope.Completion != nil {
		cost += itemwire.EncodedBytes(*scope.Completion)
	}
	return cost
}
