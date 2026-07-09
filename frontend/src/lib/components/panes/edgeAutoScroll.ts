// Shared edge-proximity auto-scroll math for horizontal drags in the
// pane strip. Both gestures that scroll the strip while dragging — the
// pane reorder drag (usePaneThreadDrag) and the divider resize drag
// (PaneDivider) — derive their per-frame scroll step from this one
// function so they accelerate with the same feel.

export const AUTO_SCROLL_EDGE_MAX_PX = 96;
export const AUTO_SCROLL_EDGE_FRACTION = 4;
export const AUTO_SCROLL_MAX_STEP_PX = 18;

export interface HorizontalEdges {
  left: number;
  right: number;
  width: number;
}

/**
 * Signed px-per-frame scroll step for a pointer at `clientX` over a
 * host with the given edges: negative near the left edge, positive
 * near the right, 0 outside both zones. Magnitude ramps linearly with
 * proximity up to AUTO_SCROLL_MAX_STEP_PX.
 */
export function edgeAutoScrollVelocity(rect: HorizontalEdges, clientX: number): number {
  const edgeSize = Math.min(AUTO_SCROLL_EDGE_MAX_PX, rect.width / AUTO_SCROLL_EDGE_FRACTION);
  if (edgeSize <= 0) return 0;
  const leftProximity = Math.max(0, edgeSize - (clientX - rect.left));
  const rightProximity = Math.max(0, edgeSize - (rect.right - clientX));
  if (leftProximity > 0) return -Math.ceil((leftProximity / edgeSize) * AUTO_SCROLL_MAX_STEP_PX);
  if (rightProximity > 0) return Math.ceil((rightProximity / edgeSize) * AUTO_SCROLL_MAX_STEP_PX);
  return 0;
}
