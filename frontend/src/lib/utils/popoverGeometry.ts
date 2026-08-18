// Pure placement + clip math for the Popover primitive. Everything here is
// viewport-space arithmetic with no DOM access, so the fit/flip/clamp rules
// and the clip-boundary rules are unit-testable without a component render.
// Popover.svelte owns WHEN to measure and re-fit; this module owns what the
// numbers mean once measured.

export type PopoverPlacement =
  | 'bottom-start'
  | 'bottom-end'
  | 'top-start'
  | 'top-end'
  | 'right-start'
  | 'left-start';

/** A viewport-space rectangle expressed by its edges (DOMRect-compatible). */
export interface EdgeRect {
  top: number;
  left: number;
  right: number;
  bottom: number;
}

export interface FloatSize {
  width: number;
  height: number;
}

export interface PopoverPosition {
  top: number;
  left: number;
}

/** Minimum gap kept between a fitted popover and the viewport edge. */
export const POPOVER_VIEWPORT_MARGIN = 8;

// Core placement math. Given the anchor rect and the floating rect, return
// {top,left} for each placement. Pure so flip can retry with a different
// placement without touching state.
export function placePopover(
  anchor: EdgeRect,
  float: FloatSize,
  placement: PopoverPlacement,
  offset: number,
): PopoverPosition {
  switch (placement) {
    case 'bottom-start':
      return { top: anchor.bottom + offset, left: anchor.left };
    case 'bottom-end':
      return { top: anchor.bottom + offset, left: anchor.right - float.width };
    case 'top-start':
      return { top: anchor.top - float.height - offset, left: anchor.left };
    case 'top-end':
      return { top: anchor.top - float.height - offset, left: anchor.right - float.width };
    case 'right-start':
      return { top: anchor.top, left: anchor.right + offset };
    case 'left-start':
      return { top: anchor.top, left: anchor.left - float.width - offset };
  }
}

// Flip rules: if the preferred placement overflows the viewport, try its
// natural opposite. Intentionally simple — two candidates per axis. If both
// overflow, the caller sticks with preferred rather than cascading through
// every direction (keeps behaviour predictable in tiny viewports).
export function oppositeOf(placement: PopoverPlacement): PopoverPlacement {
  switch (placement) {
    case 'bottom-start': return 'top-start';
    case 'bottom-end':   return 'top-end';
    case 'top-start':    return 'bottom-start';
    case 'top-end':      return 'bottom-end';
    case 'right-start':  return 'left-start';
    case 'left-start':   return 'right-start';
  }
}

export function overflowsPrimaryAxis(
  pos: PopoverPosition,
  float: FloatSize,
  placement: PopoverPlacement,
  viewportWidth: number,
  viewportHeight: number,
): boolean {
  switch (placement) {
    case 'bottom-start':
    case 'bottom-end':
      return pos.top + float.height > viewportHeight;
    case 'top-start':
    case 'top-end':
      return pos.top < 0;
    case 'right-start':
      return pos.left + float.width > viewportWidth;
    case 'left-start':
      return pos.left < 0;
  }
}

function clamp(value: number, min: number, max: number): number {
  if (max < min) return min;
  return Math.min(Math.max(value, min), max);
}

/**
 * Clamp a placed position into the fit bounds: the viewport inset by
 * {@link POPOVER_VIEWPORT_MARGIN}, intersected with the clip boundary when
 * one applies. The intersection is load-bearing — the open-time fit must
 * land the popover inside the plane it will be CLIPPED to, or an end-aligned
 * popover in a narrow leftmost pane opens with columns already cut off
 * behind the neighbouring surface, permanently invisible with no scroll to
 * reveal them. `clip: null` reproduces plain viewport clamping exactly.
 *
 * A boundary narrower than the floating element pins it to the boundary's
 * near edge; the overhang is the clip-path's problem, not the clamp's.
 */
export function clampPopoverPosition(
  pos: PopoverPosition,
  float: FloatSize,
  viewportWidth: number,
  viewportHeight: number,
  clip: EdgeRect | null,
): PopoverPosition & { maxHeight: number | undefined } {
  const minTop = Math.max(POPOVER_VIEWPORT_MARGIN, clip?.top ?? POPOVER_VIEWPORT_MARGIN);
  const minLeft = Math.max(POPOVER_VIEWPORT_MARGIN, clip?.left ?? POPOVER_VIEWPORT_MARGIN);
  const maxRight = Math.min(
    viewportWidth - POPOVER_VIEWPORT_MARGIN,
    clip?.right ?? viewportWidth - POPOVER_VIEWPORT_MARGIN,
  );
  const maxBottom = Math.min(
    viewportHeight - POPOVER_VIEWPORT_MARGIN,
    clip?.bottom ?? viewportHeight - POPOVER_VIEWPORT_MARGIN,
  );
  const top = clamp(pos.top, minTop, maxBottom - float.height);
  const left = clamp(pos.left, minLeft, maxRight - float.width);
  const availableHeight = Math.max(0, maxBottom - minTop);
  const needsHeightLimit = float.height > availableHeight || pos.top !== top;
  return { top, left, maxHeight: needsHeightLimit ? availableHeight : undefined };
}

/**
 * The clip boundary's rect intersected with the viewport, or null when no
 * clip applies. Both degenerate shapes deliberately answer null — "no
 * clip", never "clip everything": a zero-size boundary is what happy-dom
 * and mid-teardown layouts measure, and an empty viewport intersection
 * would otherwise produce an inverted rect whose clamp math is garbage.
 */
export function intersectClipBoundary(
  boundary: EdgeRect,
  viewportWidth: number,
  viewportHeight: number,
): EdgeRect | null {
  if (boundary.right - boundary.left <= 0 || boundary.bottom - boundary.top <= 0) return null;
  const top = Math.max(boundary.top, 0);
  const left = Math.max(boundary.left, 0);
  const right = Math.min(boundary.right, viewportWidth);
  const bottom = Math.min(boundary.bottom, viewportHeight);
  if (right <= left || bottom <= top) return null;
  return { top, left, right, bottom };
}

/**
 * The `clip-path` declaration cutting a floating element at its clip
 * boundary, or '' when the element sits fully inside it. `inset()` measures
 * from the element's own box — argument order top/right/bottom/left — hence
 * the position/size arithmetic. All four sides are computed: horizontal
 * strip scrolling only ever cuts left/right, but a boundary shorter than
 * the viewport (future vertical scroller) cuts top/bottom the same way.
 */
export function clipPathRule(
  clip: EdgeRect,
  top: number,
  left: number,
  width: number,
  height: number,
): string {
  const insetTop = Math.max(0, clip.top - top);
  const insetLeft = Math.max(0, clip.left - left);
  const insetRight = Math.max(0, left + width - clip.right);
  const insetBottom = Math.max(0, top + height - clip.bottom);
  if (insetTop === 0 && insetLeft === 0 && insetRight === 0 && insetBottom === 0) return '';
  return `clip-path: inset(${insetTop}px ${insetRight}px ${insetBottom}px ${insetLeft}px);`;
}
