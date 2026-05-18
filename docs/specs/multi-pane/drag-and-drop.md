# Drag-and-Drop: Sidebar Thread → Pane Row

> Open a thread in a specific pane position via drag.

## Decision

- Drag a thread row from the sidebar to the pane row. **Always additive** — never replaces an existing pane's thread (replacement is the click-in-sidebar gesture).
- Drop zones (standard tiling pattern):
  - **Left half of a pane** → outline previews on the left side of the target pane; drop inserts new pane *before* it.
  - **Right half of a pane** → outline previews on the right side; drop inserts *after*.
  - **Inter-pane gap** → vertical insertion line at the gap; drop inserts at that position.
  - **Right end of the pane row** → outline at the rightmost edge; drop appends.
  - **Empty-state screen (0 panes)** → drop creates the first pane.
- **No-duplicates rule still wins**: if the dragged thread is already in another pane, drop completes by *focusing that pane*, ignoring drop position. Drop-zone outlines are suppressed; the already-holding pane gets a distinct highlight.
- **Auto-scroll during drag**: cursor near the left or right edge of the pane row triggers horizontal scroll to expose more drop targets. Standard pattern.

## Visual feedback

- **Drag ghost**: minimal thread row representation (title + status dot) following the cursor.
- **Drop-target highlights**:
  - Pane-half hover → outline on the relevant side of the target pane, sized to approximate the new pane's projected width.
  - Inter-pane gap hover → vertical insertion line at the gap.
  - Empty-state surface hover → full-pane-width outline.
  - Pane already holding the dragged thread → distinct highlight (pulsing accent border or similar), drop-zone outlines suppressed everywhere else.
- **Auto-scroll near edges** → visible scroll motion; no extra indicator needed (scrollbar reflects new position).

## Outline preview width math

The new pane will start with ratio = average of existing ratios (see [layout-model.md](./layout-model.md)). The outline width is the *projected* width after the drop:

```
projectedRatio = avg(existing_ratios)
newTotal = sum(existing_ratios) + projectedRatio
projectedWidth = (projectedRatio / newTotal) * paneRowWidth
```

clamped to `min-pane-width`. This is an approximation — the actual width after drop matches this exactly.

## Disambiguating from pane-header reorder drag

The pane row supports two drag sources that look similar:

| Source | What it does | Ghost | Drop preview |
|---|---|---|---|
| Sidebar thread row | Creates a new pane with that thread | Thread row (title + dot) | New-pane outline (transparent fill) |
| Pane header | Moves the existing pane left/right | Pane silhouette | Pane outline at the new position |

Different ghost + different drop-preview style keeps them visually distinct.

## Edge cases

- **Drag canceled** (`Esc`, drop outside any valid target) → no change.
- **Drag onto the very pane that already holds the thread** → no-op (or harmless re-focus).
- **Drag during streaming** → fine. Source thread keeps running on its provider process; the new pane just mounts a view of it.
- **Drag from sidebar onto sidebar** → no-op.
- **Drag onto sidebar from pane** → not a defined gesture; treat as drag-canceled.

## Implementation notes

- HTML5 drag-and-drop is sufficient. Pointer-events-based custom drag also works if more visual control is needed.
- Drop zones: each pane registers two halves (left/right) plus the row registers inter-pane gaps and the end-cap.
- Auto-scroll: listen for pointer-move with cursor near `PaneHost`'s left/right edge during a drag session; programmatically adjust `scrollLeft` with a velocity ramped by proximity.
- No-duplicates collision detection: on dragstart, compute the set of panes holding the dragged thread once; update drop-target highlight rendering accordingly.
- New gesture; no existing code to modify (clean addition to `PaneHost.svelte` and `ThreadRow.svelte`).
