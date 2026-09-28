// Which rows of a long list the DOM holds. Browsers cap an element's
// height (Chromium and WebKit near 33.5M px, Firefox near 17.9M px), so a
// list taller than that cannot be one scroll surface. LongListVirtualizer
// hands the virtualizer a contiguous range of rows that fits, and moves
// the range as the reader approaches its ends or jumps outside it.
//
// The limit stays below every engine's cap and below 2^24 px, past which
// float positions lose whole pixels. A list that fits the limit is held
// whole. Otherwise the range spans about HELD_SPAN_PX around the row being
// read, so the reader crosses at least HELD_EDGE_PX before it moves again.

export const HELD_SPAN_PX = 8_000_000;
export const HELD_LIMIT_PX = 12_000_000;
export const HELD_EDGE_PX = 2_000_000;

/** Rows `[start, end)` of the list. */
export interface HeldRange {
  start: number;
  end: number;
}

export interface HeldLimits {
  span: number;
  limit: number;
  edge: number;
}

export const HELD_LIMITS: HeldLimits = { span: HELD_SPAN_PX, limit: HELD_LIMIT_PX, edge: HELD_EDGE_PX };

/** The height of rows `[start, end)`. */
export function heldHeight(sizeAt: (index: number) => number, start: number, end: number): number {
  let height = 0;
  for (let index = start; index < end; index += 1) height += sizeAt(index);
  return height;
}

/** Whether all `count` rows fit within `limit`. Stops at the first row
 * past it. */
export function fitsWhole(count: number, sizeAt: (index: number) => number, limit: number): boolean {
  let height = 0;
  for (let index = 0; index < count; index += 1) {
    height += sizeAt(index);
    if (height > limit) return false;
  }
  return true;
}

/**
 * About `span` px of rows around `focus`: up to half above it, the rest
 * from it down, and what an end of the list leaves over on the other
 * side. The focus row is always held, however tall.
 */
export function rangeAround(
  count: number,
  sizeAt: (index: number) => number,
  focus: number,
  span: number,
): HeldRange {
  if (count === 0) return { start: 0, end: 0 };
  const center = Math.max(0, Math.min(count - 1, focus));
  let start = center;
  let end = center + 1;
  let height = sizeAt(center);
  let above = 0;
  while (start > 0) {
    const size = sizeAt(start - 1);
    if (above + size > span / 2 || height + size > span) break;
    start -= 1;
    above += size;
    height += size;
  }
  while (end < count && height + sizeAt(end) <= span) {
    height += sizeAt(end);
    end += 1;
  }
  while (start > 0 && height + sizeAt(start - 1) <= span) {
    start -= 1;
    height += sizeAt(start);
  }
  return { start, end };
}

/**
 * Whether the held range should move: the reader is within `edge` of an
 * end that is not the list's own, or the held rows have grown past the
 * limit as they were measured.
 */
export function shouldMoveHeld(
  range: HeldRange,
  count: number,
  view: { offset: number; viewport: number; total: number },
  limits: HeldLimits,
): boolean {
  if (view.total > limits.limit && range.end - range.start > 1) return true;
  if (range.start > 0 && view.offset < limits.edge) return true;
  return range.end < count && view.offset + view.viewport > view.total - limits.edge;
}
