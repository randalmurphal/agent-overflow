// Thumb geometry and drag math for an overlay scrollbar.
//
// Why an overlay bar exists at all: `app.css` styles `::-webkit-scrollbar`
// globally at 10px, so every native bar in this app is a classic,
// space-CONSUMING bar. On a surface whose overflow toggles often — an
// activity run crosses its cap, inflates on expansion, trims, prepends,
// collapses — that means the content re-wraps on every one of those
// transitions.
//
// `scrollbar-gutter` is the outer scroller's fix and does not transfer:
// WebKitGTK only reserves a single-edge gutter while the bar is present,
// so a non-shifting native bar needs `both-edges` — 20px, whose left half
// would push the run's rows off the rail the run itself draws. Native
// costs width AND alignment.
//
// So: suppress the native bar to zero width and draw the affordance out of
// flow, in padding the column already has. Absolute positioning cannot
// affect width in any state, which is the whole point.
//
// Pure on purpose — every number below is testable without a DOM.

export interface ScrollMetrics {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
}

export interface ThumbMetrics {
  topPx: number;
  heightPx: number;
  /** False when there is nothing to scroll — the caller hides the track. */
  visible: boolean;
}

/** Below this the thumb is too small to grab. */
export const MIN_THUMB_PX = 24;

/** Sub-pixel overflow is rounding noise, not a scrollable surface. */
const OVERFLOW_EPSILON_PX = 1;

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value));
}

function scrollableRange(metrics: ScrollMetrics): number {
  return metrics.scrollHeight - metrics.clientHeight;
}

export function thumbMetrics(metrics: ScrollMetrics, trackPx: number): ThumbMetrics {
  const scrollable = scrollableRange(metrics);
  if (scrollable <= OVERFLOW_EPSILON_PX || trackPx <= 0) {
    return { topPx: 0, heightPx: 0, visible: false };
  }
  const proportional = (metrics.clientHeight / metrics.scrollHeight) * trackPx;
  const heightPx = clamp(proportional, Math.min(MIN_THUMB_PX, trackPx), trackPx);
  const travel = trackPx - heightPx;
  const topPx = travel <= 0 ? 0 : (metrics.scrollTop / scrollable) * travel;
  return { topPx: clamp(topPx, 0, travel), heightPx, visible: true };
}

export interface DragOrigin {
  scrollTop: number;
  pointerY: number;
}

/**
 * Where a drag puts `scrollTop`.
 *
 * Scaled by the exact inverse of the thumb mapping (`scrollable / travel`),
 * not by `scrollHeight / clientHeight`: with a thumb clamped up to
 * MIN_THUMB_PX the two differ, and only the inverse guarantees that
 * dragging the thumb to the end of the track lands exactly at the end of
 * the content.
 */
export function scrollTopForDrag(
  origin: DragOrigin,
  pointerY: number,
  metrics: ScrollMetrics,
  trackPx: number,
): number {
  const scrollable = scrollableRange(metrics);
  const { heightPx, visible } = thumbMetrics(metrics, trackPx);
  const travel = trackPx - heightPx;
  if (!visible || travel <= 0) return origin.scrollTop;
  const moved = ((pointerY - origin.pointerY) / travel) * scrollable;
  return clamp(origin.scrollTop + moved, 0, scrollable);
}

/**
 * Where a click on the track (not the thumb) puts `scrollTop`: one page
 * toward the click, matching every platform's track-click behavior. A
 * click that lands on the thumb is a no-op — that gesture is a drag.
 */
export function scrollTopForTrackClick(
  offsetY: number,
  metrics: ScrollMetrics,
  trackPx: number,
): number {
  const { topPx, heightPx, visible } = thumbMetrics(metrics, trackPx);
  if (!visible) return metrics.scrollTop;
  const direction = offsetY < topPx ? -1 : offsetY > topPx + heightPx ? 1 : 0;
  if (direction === 0) return metrics.scrollTop;
  return clamp(
    metrics.scrollTop + direction * metrics.clientHeight,
    0,
    scrollableRange(metrics),
  );
}

export function readScrollMetrics(el: Element): ScrollMetrics {
  return {
    scrollTop: el.scrollTop,
    scrollHeight: el.scrollHeight,
    clientHeight: el.clientHeight,
  };
}
