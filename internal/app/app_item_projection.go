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

// windowEnd names which end of a page survives the byte backstop: the
// end the reader is anchored at. A backward or anchored load is read
// from the bottom, so the newest rows are the ones on screen; a forward
// load is read from the top of what the pane already holds.
type windowEnd int

const (
	keepNewest windowEnd = iota
	keepOldest
)

// projectPage is the single wire projection for a paged item load: every
// row is bounded field-wise, then the page is bounded byte-wise.
//
// inlinePreviews is the caller's stated preference, forwarded from the
// client. The server never reads `collapseDiffPreviews` itself — that is
// a per-client setting and this process may be serving several clients
// that disagree.
func projectPage(paged store.PagedItems, inlinePreviews bool, keep windowEnd) store.PagedItems {
	paged.Items = itemwire.ProjectItems(slicesx.OrEmpty(paged.Items), inlinePreviews)
	return admitByBytes(paged, keep)
}

// projectItemSlice is projectPage for the loads that return a bare slice
// (subagent descendants, proposed plans, the tray feed). They carry no
// cursors, so an over-budget slice keeps the rows nearest the reader and
// drops the rest rather than reporting a boundary it cannot describe.
func projectItemSlice(items []store.Item, inlinePreviews bool, keep windowEnd) []store.Item {
	items = itemwire.ProjectItems(slicesx.OrEmpty(items), inlinePreviews)
	from, to := admittedRange(items, keep)
	return items[from:to]
}

// admitByBytes shortens a page to the byte budget, keeping the rows at
// `keep` and reporting the dropped ones through the page's has-more
// flags so pagination stays honest.
func admitByBytes(paged store.PagedItems, keep windowEnd) store.PagedItems {
	from, to := admittedRange(paged.Items, keep)
	return paged.TrimToRange(from, to)
}

// admittedRange walks from the anchored end admitting rows until the
// byte budget is reached.
//
// One oversized row is always admitted when nothing else has been: a
// page that refused every row would stall pagination on that item
// forever, and the reader would sit at a boundary that never moves.
func admittedRange(items []store.Item, keep windowEnd) (int, int) {
	if len(items) == 0 {
		return 0, 0
	}
	spent := 0
	if keep == keepOldest {
		for i := range items {
			cost := itemwire.EncodedBytes(items[i])
			if i > 0 && spent+cost > itemWindowMaxBytes {
				return 0, i
			}
			spent += cost
		}
		return 0, len(items)
	}
	for i := len(items) - 1; i >= 0; i-- {
		cost := itemwire.EncodedBytes(items[i])
		if i < len(items)-1 && spent+cost > itemWindowMaxBytes {
			return i + 1, len(items)
		}
		spent += cost
	}
	return 0, len(items)
}
