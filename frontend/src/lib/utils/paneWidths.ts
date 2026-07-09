// Pure math for pane widths. Pane layout items carry an absolute pixel
// width: the pane row renders each pane at that width, stretches all
// panes proportionally when the window is wider than their sum, and
// horizontal-scrolls when it is narrower. All drag semantics resolve
// here so they can be tested without DOM.

import type { PaneDensityMode } from '../types/settings';

// Minimum pane width per density mode. Lives here (pure data) so this
// module stays store-free; stores/paneDensity.svelte.ts re-exports it
// as the settings-facing API.
export const PANE_DENSITY_MIN_WIDTHS: Record<PaneDensityMode, number> = {
  compact: 560,
  comfortable: 880,
  spacious: 1400,
};

export const MAX_PANE_WIDTH_PX = 10_000;
// The compact density minimum: a garbage width degrades to the
// smallest legitimate pane rather than something invisible or enormous.
export const FALLBACK_PANE_WIDTH_PX = PANE_DENSITY_MIN_WIDTHS.compact;

// scrollWidth/clientWidth are rounded independently; treat sub-pixel
// overflow as "fits".
export const OVERFLOW_EPSILON_PX = 1;

// Rendered width of each divider / end-handle strip, and its flex basis.
// The fit-gate in paneLayout subtracts one strip per pane from the
// measured host width, and PaneDivider renders exactly this width — a
// single source so the estimate and the DOM cannot drift.
export const PANE_DIVIDER_WIDTH_PX = 4;

export function normalizePaneWidthPx(width: number): number {
  // Sub-1px widths are as much garbage as non-positive ones — nothing
  // in the app can legitimately produce them.
  if (!Number.isFinite(width) || width < 1) return FALLBACK_PANE_WIDTH_PX;
  return Math.min(width, MAX_PANE_WIDTH_PX);
}

export interface PaneBoundaryDrag {
  /** Measured pane widths at drag start, in layout order. */
  widths: readonly number[];
  /**
   * Index of the pane immediately left of the grabbed boundary. The end
   * handle (right edge of the whole strip) is `widths.length - 1` with
   * `hasRightPane: false`.
   */
  leftIndex: number;
  hasRightPane: boolean;
  /**
   * Pointer travel since drag start in content space (viewport delta
   * plus any scroll the drag itself caused). Positive = rightward.
   */
  deltaPx: number;
  minPaneWidth: number;
  /** Horizontal overflow (scrollWidth - clientWidth) at drag start. */
  overflowPx: number;
  /** Alt-drag: trade space with the neighbor, keeping the total width. */
  zeroSum: boolean;
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}


/**
 * Resolve the pane widths a boundary drag produces. Pure in `deltaPx`:
 * re-resolving from the same start snapshot retraces exactly, so a
 * drag out and back within one gesture is a no-op.
 *
 * Normal drag: the boundary follows the pointer and resizes the pane on
 * its left. While the strip overflows, panes past the boundary keep
 * their widths and shift — a grow-then-shrink round trip across two
 * gestures restores the exact starting layout. Growing while everything
 * fits first takes space from the right neighbor (down to its minimum),
 * then pushes into overflow; shrinking first consumes the overflow,
 * then hands the freed space to the neighbor (panes always fill the
 * window). The end handle behaves the same with the left neighbor as
 * the recipient.
 *
 * Zero-sum drag (Alt): classic divider behavior — the two panes at the
 * boundary trade space, nothing shifts, total width is unchanged.
 *
 * Returns null only for invalid input. A resolution identical to the
 * start snapshot is still returned: mid-gesture the store may hold
 * different widths (the pointer moved out and back), and the caller
 * compares against its CURRENT state to decide whether to write.
 */
export function resolvePaneBoundaryDrag(drag: PaneBoundaryDrag): number[] | null {
  const { widths, leftIndex, hasRightPane, deltaPx, minPaneWidth } = drag;
  if (leftIndex < 0 || leftIndex >= widths.length) return null;
  if (hasRightPane && leftIndex + 1 >= widths.length) return null;
  if (widths.some((width) => !Number.isFinite(width) || width <= 0)) return null;
  if (!Number.isFinite(deltaPx)) return null;
  const next = widths.slice();
  const left = widths[leftIndex];

  if (drag.zeroSum) {
    const neighborIndex = hasRightPane ? leftIndex + 1 : leftIndex - 1;
    if (neighborIndex < 0) return null;
    const combined = left + widths[neighborIndex];
    if (combined < minPaneWidth * 2) return null;
    // Lower bound also keeps the NEIGHBOR under MAX so the trade stays
    // total-preserving even when both panes sit near the cap.
    const newLeft = clamp(
      left + deltaPx,
      Math.max(minPaneWidth, combined - MAX_PANE_WIDTH_PX),
      Math.min(combined - minPaneWidth, MAX_PANE_WIDTH_PX),
    );
    next[leftIndex] = newLeft;
    next[neighborIndex] = combined - newLeft;
    return next;
  }

  if (deltaPx >= 0) {
    next[leftIndex] = Math.min(left + deltaPx, MAX_PANE_WIDTH_PX);
    if (hasRightPane && drag.overflowPx <= OVERFLOW_EPSILON_PX) {
      const rightIndex = leftIndex + 1;
      next[rightIndex] = Math.max(minPaneWidth, widths[rightIndex] - deltaPx);
    }
    return next;
  }

  const newLeft = Math.max(minPaneWidth, left + deltaPx);
  next[leftIndex] = newLeft;
  const shrunkBy = left - newLeft;
  const beyondOverflow = Math.max(0, shrunkBy - Math.max(0, drag.overflowPx));
  if (beyondOverflow > 0) {
    const recipientIndex = hasRightPane ? leftIndex + 1 : leftIndex - 1;
    if (recipientIndex >= 0 && recipientIndex < widths.length) {
      next[recipientIndex] = Math.min(
        widths[recipientIndex] + beyondOverflow,
        MAX_PANE_WIDTH_PX,
      );
    }
  }
  return next;
}

/**
 * Canonicalize fit-mode widths by scaling them down so the smallest
 * pane sits exactly at the minimum. While the strip fits the window,
 * rendering only depends on the widths' proportions (the flex row
 * stretches them to fill), so this is visually a no-op — but it keeps
 * headroom: opening another pane squeezes the existing ones toward
 * their minimums instead of immediately spilling into scroll. Must NOT
 * be applied while the strip overflows: there the absolute values ARE
 * the scroll width the user built.
 *
 * Returns null when nothing changes.
 */
export function minAnchorPaneWidths(
  widths: readonly number[],
  minPaneWidth: number,
): number[] | null {
  if (widths.length === 0) return null;
  const smallest = Math.min(...widths);
  if (!Number.isFinite(smallest) || smallest <= minPaneWidth) return null;
  const scale = minPaneWidth / smallest;
  return widths.map((width) => Math.max(minPaneWidth, width * scale));
}
