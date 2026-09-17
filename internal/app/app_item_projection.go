package app

import (
	"agent-overflow/internal/itemwire"
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
const itemWindowMaxBytes = 512 << 10

// projectPage is the single wire projection for a paged item load: every
// row is bounded field-wise, then the page is bounded byte-wise around
// the row at `anchor`, the row the reader is positioned on. A cursor page
// anchors at the end nearest the pane's held window; an anchored slice
// anchors at the item it was asked for (`anchorIndex`).
//
// inlinePreviews is the caller's stated preference, forwarded from the
// client. The server never reads `collapseDiffPreviews` itself — that is
// a per-client setting and this process may be serving several clients
// that disagree.
func projectPage(paged store.PagedItems, inlinePreviews bool, anchor int) store.PagedItems {
	paged.Items = itemwire.ProjectItems(slicesx.OrEmpty(paged.Items), inlinePreviews)
	from, to := admittedRange(paged.Items, anchor)
	return paged.TrimToRange(from, to)
}

// projectItemSlice is projectPage for the loads that return a bare slice
// (subagent descendants, proposed plans, the tray feed). They carry no
// cursors, so an over-budget slice keeps the rows nearest the reader,
// which is the newest end, and drops the rest rather than reporting a
// boundary it cannot describe.
func projectItemSlice(items []store.Item, inlinePreviews bool) []store.Item {
	items = itemwire.ProjectItems(slicesx.OrEmpty(items), inlinePreviews)
	from, to := admittedRange(items, newestIndex(items))
	return items[from:to]
}

// newestIndex is the anchor for a page read from its newest end: a
// backward cursor page, a tail slice, or a bare slice.
func newestIndex(items []store.Item) int {
	return len(items) - 1
}

// anchorIndex resolves the row a slice was asked for. An empty or absent
// id is the store's tail fallback (ListThreadSliceAround), so the page
// anchors at its newest end like any other tail read.
func anchorIndex(items []store.Item, itemID string) int {
	if itemID != "" {
		for i := range items {
			if items[i].ID == itemID {
				return i
			}
		}
	}
	return newestIndex(items)
}

// admittedRange grows a contiguous range outward from `anchor`, one row
// per side per step, admitting rows until the byte budget is reached on
// that side. The anchor is admitted unconditionally: a page whose anchor
// is the row it dropped would leave a jump with nothing to land on and a
// cursor page with a boundary that never moves, so an oversized anchor
// ships alone rather than not at all.
func admittedRange(items []store.Item, anchor int) (int, int) {
	if len(items) == 0 {
		return 0, 0
	}
	anchor = min(max(anchor, 0), len(items)-1)
	lo, hi := anchor, anchor+1
	spent := itemwire.EncodedBytes(items[anchor])
	olderOpen, newerOpen := true, true
	for olderOpen || newerOpen {
		if olderOpen {
			if cost := costAt(items, lo-1); cost >= 0 && spent+cost <= itemWindowMaxBytes {
				spent += cost
				lo--
			} else {
				olderOpen = false
			}
		}
		if newerOpen {
			if cost := costAt(items, hi); cost >= 0 && spent+cost <= itemWindowMaxBytes {
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
