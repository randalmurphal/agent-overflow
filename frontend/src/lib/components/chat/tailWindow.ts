// Wrap-stable sliding window for TailClampedText's collapsed streaming
// tail.
//
// While a reasoning stream runs, the collapsed row's text node is
// replaced on every reveal tick (~50Hz) and the browser re-line-breaks
// everything the node contains — CSS clipping bounds what is PAINTED,
// not what is laid out. An unbounded monotonically-growing tail
// therefore costs O(total thinking length) of line-breaking per tick.
//
// The window bounds that cost without moving a visible pixel by only
// cutting at WRAP-STABLE offsets. Browsers break lines greedily: breaks
// are decided left-to-right, appended text never changes breaks above
// it, and a suffix sliced exactly at an existing line start lays out
// identically to the same lines inside the full string. Two offsets
// qualify:
//
//   - just after a hard '\n' (wrapping restarts there unconditionally);
//   - a MEASURED rendered line start, for a single paragraph that
//     outgrows the cap without containing a newline — found by
//     binary-searching character rects, a few dozen Range reads once
//     per few thousand appended characters, never per tick.
//
// Everything above a cut is guaranteed invisible: the clamp shows the
// bottom 3 lines and every cut keeps comfortably more than that.

/** Kept-window size that triggers a cut. ~50–60 rendered lines. */
export const TAIL_WINDOW_CAP_CHARS = 8192;

/**
 * Minimum characters a newline cut must keep. Covers 3 rendered lines
 * at any plausible pane width (an extreme 2560px pane fits ~350
 * chars/line — 3 lines ≈ 1050), with margin for the clamp's clipped
 * fourth line.
 */
export const TAIL_WINDOW_MIN_KEEP_CHARS = 2048;

/**
 * Rendered lines a measured cut keeps. 4× the 3-line clamp, so the
 * window survives the pane widening (fewer, longer lines) without
 * dropping below the visible count before the next cut re-anchors.
 */
export const TAIL_WINDOW_KEEP_LINES = 12;

/**
 * After a failed measurement (no geometry — e.g. happy-dom, or a
 * hidden ancestor), don't retry until the text has grown by this many
 * characters, so an unmeasurable element doesn't re-attempt per tick.
 */
export const TAIL_WINDOW_MEASURE_RETRY_CHARS = 1024;

/**
 * Wrap-stable cut at a hard newline: the offset just after the last
 * '\n' that still keeps at least `minKeep` characters. Returns an
 * absolute offset strictly greater than `cutOffset`, or null when no
 * newline qualifies (the advanceable region is one giant paragraph).
 *
 * Scans backward only down to `cutOffset` — this runs on every reveal
 * tick while the window is over cap, and `String.lastIndexOf` would
 * otherwise walk the entire already-cut prefix on every miss,
 * reintroducing the O(total) per-tick cost the window exists to kill.
 */
export function newlineCutOffset(text: string, cutOffset: number, minKeep: number): number | null {
  const limit = text.length - minKeep; // a cut at `limit` keeps exactly minKeep
  for (let i = limit - 1; i >= cutOffset; i--) {
    if (text.charCodeAt(i) === 10 /* '\n' */) return i + 1;
  }
  return null;
}

/**
 * O(1) monotonic-append check for the window's cut bookkeeping: is
 * `text` an append of the previously seen string (length `prevLen`,
 * final char code `prevLastCharCode`) with the cut boundary intact
 * (char code `cutFirstCharCode` still at `cutOffset`)?
 *
 * This is a sentinel, not a proof — a same-or-longer replacement that
 * happens to preserve both probed characters is misclassified as an
 * append and would render a stale (but wrap-valid) window. Accepted:
 * callers contractually feed a monotonically-growing tail, and the
 * real replacements — the swaps to the rune-trimmed summary when the
 * retained tail is dropped (offscreen prune, budget eviction,
 * post-settle summary overwrite; see threadStreamingReveal.svelte.ts) —
 * are always caught by the length probe, because summaries are trimmed
 * to 400 runes (≤ 800 UTF-16 units) while a live cut keeps at least
 * TAIL_WINDOW_MIN_KEEP_CHARS (2048): the swap can only shrink the text.
 */
export function isMonotonicAppend(
  text: string,
  prevLen: number,
  prevLastCharCode: number,
  cutOffset: number,
  cutFirstCharCode: number,
): boolean {
  return (
    text.length >= prevLen &&
    (prevLen === 0 || text.charCodeAt(prevLen - 1) === prevLastCharCode) &&
    (cutOffset === 0 || text.charCodeAt(cutOffset) === cutFirstCharCode)
  );
}

/**
 * Wrap-stable cut inside rendered text with no usable hard newline:
 * the offset (RELATIVE TO `textNode`) of the first character of the
 * rendered line `keepLines` line-heights above the last character's
 * line. Greedy line breaking makes any rendered line start a
 * wrap-stable cut, and — because appends never move breaks above them
 * — the offset stays a line start as the stream keeps growing.
 *
 * Returns null when geometry is unavailable (zero-height rects: no
 * layout engine, detached or display:none ancestor), when `lineHeight`
 * isn't a usable pixel value, or when fewer than `keepLines` lines are
 * rendered.
 */
export function measuredLineStartOffset(
  textNode: Text,
  keepLines: number,
  lineHeightPx: number,
): number | null {
  const len = textNode.length;
  if (len < 2 || !Number.isFinite(lineHeightPx) || lineHeightPx <= 0) return null;

  const range = document.createRange();
  const rectAt = (i: number): DOMRect => {
    range.setStart(textNode, i);
    range.setEnd(textNode, i + 1);
    return range.getBoundingClientRect();
  };

  // Half a pixel of slack absorbs sub-pixel rect rounding; the ~19px
  // line height dwarfs it, so thresholds can only snap between whole
  // lines, never inside one.
  const EPS = 0.5;

  const lastRect = rectAt(len - 1);
  if (lastRect.height === 0) return null; // no layout geometry
  const firstTop = rectAt(0).top;
  const targetTop = lastRect.top - keepLines * lineHeightPx;
  if (firstTop >= targetTop - EPS) return null; // fewer than keepLines lines

  // Character tops are monotonically non-decreasing in document order,
  // so binary-search the smallest index at/below the target line.
  let lo = 0; // rectAt(lo).top < targetTop - EPS
  let hi = len - 1; // rectAt(hi).top >= targetTop - EPS
  while (hi - lo > 1) {
    const mid = (lo + hi) >> 1;
    if (rectAt(mid).top >= targetTop - EPS) hi = mid;
    else lo = mid;
  }

  // Snap to the found line's FIRST character: the smallest index that
  // shares (within slack) the found character's line top. Guards the
  // cut against any within-line rect-top wobble.
  const lineTop = rectAt(hi).top;
  let lo2 = lo;
  let hi2 = hi;
  while (hi2 - lo2 > 1) {
    const mid = (lo2 + hi2) >> 1;
    if (rectAt(mid).top >= lineTop - EPS) hi2 = mid;
    else lo2 = mid;
  }

  // Never cut between the halves of a surrogate pair. Engines don't
  // break lines inside one, but the rect of a half-pair Range is
  // engine-defined — step back to the high surrogate rather than trust
  // it. Slicing mid-pair would render a lone surrogate at the window
  // top (and a corrupt first glyph).
  while (hi2 > 0 && (textNode.data.charCodeAt(hi2) & 0xfc00) === 0xdc00) hi2--;

  // A zero cut means the epsilon band swallowed the whole advance —
  // report "no cut" so the caller throttles retries instead of
  // re-measuring on every tick for a no-op.
  return hi2 > 0 ? hi2 : null;
}
