// Sub-row attribution for the ONE row that can span the viewport top.
//
// The engine compensates rows lying entirely above the viewport top: their
// growth moves the reading position by exactly the size delta, so an equal
// scroll shift makes it invisible. Rows at or below the top need no
// compensation — growth there pushes content down, which is what the eye
// expects.
//
// A row STRADDLING the top is neither. Part of it is above the reading
// position and part below, and the two halves need opposite responses:
// growth in the off-screen-above part shifts everything visible down by
// that amount; growth in the visible part is ordinary reflow. The engine
// only ever sees whole-row `[index, height]` pairs from the adapter's
// ResizeObserver, so it cannot tell the two apart and has historically
// compensated neither — leaving up to one row's worth of uncompensated
// shift whenever a tall row's off-screen head grows (late KaTeX, mermaid,
// a decoding image, or a width reflow re-wrapping the whole window).
//
// This module supplies the missing information by measuring the DOM, and
// it measures ROW-RELATIVE on purpose: the offset of an anchor element
// from the top of its OWN row. Rows are absolutely positioned at engine
// offsets, so a row's own position is a pure function of the engine's
// above-rows arithmetic — which is already exact. Anything that moves the
// anchor relative to its row top is, by construction, intra-row growth
// above the reading position and nothing else. That makes the two
// corrections independent and impossible to double-count.
//
// Failure is always safe. No hit, an anchor that is the row itself, or a
// detached anchor all yield "no information", and the engine falls back to
// the historical behavior of compensating nothing for that row.

/** A resolved mounted row: its element and its live index. */
export interface AnchorRow {
  el: HTMLElement;
  index: number;
}

export interface ReadingAnchor {
  /** The mounted row element the anchor lives inside. */
  rowEl: HTMLElement;
  /** Deepest element painted at the viewport top. Always a strict
   *  descendant of `rowEl` — an anchor equal to the row carries no
   *  sub-row information, so sampling rejects it. */
  anchorEl: HTMLElement;
  /** `anchorEl` top minus `rowEl` top, in px, at sample time. */
  intraRowOffset: number;
}

export interface ReadingAnchorDeps {
  /** The scroll container. */
  scroller: HTMLElement;
  /** Resolve a hit-tested element to the mounted row that owns it.
   *  Undefined when the element belongs to no row (the scroller's own
   *  padding, a floating overlay, a portaled surface). */
  rowFor(el: Element): AnchorRow | undefined;
}

/**
 * Capture the element painted at the viewport top and its offset within
 * its own row. Call whenever the current layout is the one the next
 * measurement should be judged against — after a scroll, and after each
 * measurement pass has been applied and compensated.
 *
 * Returns null when no usable anchor exists; callers treat that as "no
 * sub-row information available".
 */
export function sampleReadingAnchor(deps: ReadingAnchorDeps): ReadingAnchor | null {
  const { scroller, rowFor } = deps;
  const scrollerRect = scroller.getBoundingClientRect();
  if (scrollerRect.width <= 0 || scrollerRect.height <= 0) return null;

  // One px inside the content box: `clientTop` skips the border, and a hit
  // landing in the scroller's own padding resolves to no row, which
  // correctly yields null rather than a bogus anchor.
  const y = scrollerRect.top + scroller.clientTop + 1;
  // Horizontal center — row gutters and margins sit at the edges, where a
  // hit would resolve to the row wrapper instead of its content.
  const x = scrollerRect.left + scrollerRect.width / 2;

  const hit = document.elementFromPoint(x, y);
  if (!(hit instanceof HTMLElement)) return null;

  const row = rowFor(hit);
  // `hit === row.el` means the point landed on the row wrapper itself with
  // no descendant painted there. Its intra-row offset is identically zero
  // and can never change, so it would only ever report a zero shift.
  if (!row || hit === row.el) return null;

  return {
    rowEl: row.el,
    anchorEl: hit,
    intraRowOffset: hit.getBoundingClientRect().top - row.el.getBoundingClientRect().top,
  };
}

/**
 * How far the anchor moved within its own row since it was sampled — i.e.
 * how much of that row's growth landed above the reading position.
 *
 * Null when the anchor can no longer be measured (either element left the
 * DOM — a keyed re-render, a Streamdown block remount, the row scrolling
 * out of the mount window).
 */
export function measureReadingAnchorShift(anchor: ReadingAnchor): number | null {
  if (!anchor.anchorEl.isConnected || !anchor.rowEl.isConnected) return null;
  const next =
    anchor.anchorEl.getBoundingClientRect().top - anchor.rowEl.getBoundingClientRect().top;
  const shift = next - anchor.intraRowOffset;
  return Number.isFinite(shift) ? shift : null;
}
