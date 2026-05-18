# Layout Model

> How panes are sized, laid out, and how the pane row overflows.

## Decision

- The pane row is a horizontal flex container (`PaneHost.svelte`), `overflow-x-auto`.
- Each pane carries a **ratio** (float). Available pane-row width is divided by `sum(ratios)`; each pane receives `(its ratio / total) * pane-row-width`, then clamped to `min-pane-width`.
- `min-pane-width` is set by the **density mode** (see [density-modes.md](./density-modes.md)):
  - Compact: **560px**
  - Comfortable: **880px**
  - Spacious: **1400px**
- If the sum of `min-pane-width` across all panes exceeds the pane row's width, panes lock at min and the row overflows horizontally (scroll engaged).
- Dragging a divider between two adjacent panes trades ratio between them; their combined width stays constant, others unaffected.
- Adding a pane: new pane's ratio = average of existing ratios; existing ratios unchanged; widths redistribute proportionally.
- Closing a pane: remove its ratio; remaining ratios unchanged; widths redistribute proportionally.

## Behavior

- **Single pane on a wide monitor**: pane fills the entire pane-row width. Chat content centers at its `max-w-62rem` (992px) inside the pane; the rest of the pane is empty space (or RHS panel room).
- **N panes that fit**: widths divide proportionally by ratio.
- **N panes that don't fit**: each clamps to `min-pane-width`, horizontal scroll engages, attention dots for off-screen panes park at the visible edge (see [attention-indicators.md](./attention-indicators.md)).
- **Window resize**: ratios stay constant, widths recompute against the new pane-row width. If it shrinks past `min × paneCount`, overflow scroll engages naturally.
- **Density mode change**: `min-pane-width` updates immediately; existing pane widths reflow; horizontal scroll engages/disengages as appropriate.

## Ratio math, worked

Two panes with default ratios 1.0 and 1.0, available width 1600px:

- total = 2.0, each gets `(1.0 / 2.0) * 1600 = 800px`.

User drags the divider so left gets +200, right gets -200:

- Left ratio becomes `(800 + 200) / 1600 = 0.625`, right becomes `0.375`. Sum still 1.0; their combined width is unchanged at 1600px.

Add a third pane. New ratio = average of existing = `(0.625 + 0.375) / 2 = 0.5`. New total = 1.5. Available width still 1600:

- Left: `(0.625 / 1.5) * 1600 ≈ 667px`
- Right: `(0.375 / 1.5) * 1600 = 400px`
- New: `(0.5 / 1.5) * 1600 ≈ 533px`

The 0.625 : 0.375 relationship between original panes is preserved (still 1.67:1).

## Implementation notes

- `frontend/src/lib/stores/paneLayout.svelte.ts` already supports the array shape. Needs:
  - An `addPaneLayoutItem(item, insertIndex?)` function (only `removePaneLayoutItem` exists today).
  - Allow the array to become empty (line 28–35 currently re-inserts a default `main` pane; relax this — see [pane-lifecycle.md](./pane-lifecycle.md)).
  - Add a `ratio` field on each layout item (today's `minWidth` constant moves out to the density-mode store).
- `frontend/src/lib/components/panes/PaneHost.svelte:57` already sets `overflow-x-auto`. The flex layout needs ratio-based `flex-grow` and `min-width` set per-item from the layout store.
- `frontend/src/lib/stores/layoutMetrics.svelte.ts` already publishes per-pane measured widths via ResizeObserver — no change needed for measurement.
- `DEFAULT_THREAD_PANE_MIN_WIDTH = 560` in `paneLayout.svelte.ts:10` becomes the Compact-mode constant; sibling constants for Comfortable (880) and Spacious (1400) sit alongside.
- A vertical resizer component between adjacent panes is new; reuse `frontend/src/lib/utils/resizeGesture.svelte.ts` (already shared by `SidebarResizer`, `RhsSidebarResizer`, `DesignSplitResizer`).
