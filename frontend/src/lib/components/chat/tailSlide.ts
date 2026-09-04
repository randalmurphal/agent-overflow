// Pure decision + motion core for TailClampedText's line-slide.
//
// The collapsed clamp has two motion regimes. While the text is under
// TAIL_CLAMP_LINES the BOX grows a line at a time and the enclosing
// scroller's spring glides that growth. Once the box is at max-h, a new
// line re-packs the flex-end content one line UP in a single frame — a
// zero-duration teleport of every visible line
// (bug-report-20260806T011635Z: at a burst-fed drain's ceiling rate
// those re-packs cluster and read as the think block snapping). The
// component turns each re-pack into motion: it inverts the jump with a
// translateY on the inner wrapper in the same frame the layout moved,
// then drains that offset back to zero over the following frames.
//
// The drain is a TRACKER, not a fixed-duration transition: every frame
// it takes a fixed fraction of whatever offset is pending
// (`stepSlide`), so lines arriving faster than one transition could
// absorb them accumulate offset and the text tickers faster instead of
// the offset saturating. The previous fixed 140ms per-line FLIP hit
// its one-window cap under short-line text (lists, code-ish reasoning,
// short sentences with paragraph breaks) and every further line then
// teleported (bug-report-20260904T184019Z).
//
// This module owns WHICH observations animate. Only append-driven clip
// advances slide; everything else recalibrates and snaps exactly as the
// pure-CSS anchor always did:
//
//   - the first observation after (re)mount or an armed reset — no
//     baseline to diff against;
//   - a width reflow — line boundaries re-derive, a translate can't
//     represent a re-wrap;
//   - an outer-box height change with no clip advance — the expanded
//     flip, a collapse, the clamp first engaging; a box that GROWS and
//     overflows in the same frame (a paragraph break plus the next word
//     landing in one reveal tick while the block is still short) slides
//     by the overflow only: the growth is the scroll spring's motion, but
//     the re-pack of the overflowed lines is a separate instant jump
//     nothing else animates;
//   - unusable geometry (hidden ancestor) — recalibrate so re-showing
//     doesn't misread the reappearance as an append.
//
// A clip advance of a full window or more slides through it (up to
// SLIDE_MAX_WINDOWS): no previously visible line survives, but a ticker
// through the new lines still reads as motion where a snap reads as a
// glitch.
//
// Kept pure (no DOM) so the guard matrix and the drain curve are
// unit-testable in the default vitest project; the component supplies
// real geometry and applies the styles.

/**
 * Fraction of the pending offset drained per 60Hz frame: an ease-out
 * whose speed is proportional to the lag. 35% is the old transition's
 * feel on a single line (~7px in the first frame of a 19.5px advance)
 * and, at a full-window offset, drains one line per frame — more than
 * the reveal smoother's ceiling (MAX_ADAPTIVE_CHARS_PER_SEC, ~5 chars a
 * frame) can land on any line longer than five characters, so the
 * offset settles below the cap instead of pinning on it.
 */
export const SLIDE_DRAIN_PER_FRAME = 0.35;

/**
 * The pending offset is capped at this many clamp windows. Past one
 * window the inversion starts on content that was never visible, but a
 * burst that lands two lines in one frame (a paragraph break plus the
 * next words at catch-up rate) would otherwise pin the offset at the cap
 * and teleport the excess every frame; a second window of catch-up
 * tickers through it instead. Beyond two windows per frame the rate is
 * unreadable regardless and the excess snaps.
 */
export const SLIDE_MAX_WINDOWS = 2;

/**
 * Minimum drain per 60Hz frame, so the exponential tail lands on zero
 * instead of asymptoting through sub-pixel translates for many frames.
 */
export const SLIDE_MIN_STEP_PX = 1;

const FRAME_MS = 1000 / 60;

/**
 * Sub-pixel slack for geometry comparisons. Fractional client rects can
 * wobble by well under this across engines; real signals (a ~19px line
 * crossing, a width oscillation that moves a line break) clear it by an
 * order of magnitude.
 */
const EPS = 0.5;

/** One frame's fractional geometry, as observed post-layout. */
export type SlideObservation = {
  /** Inner wrapper's border-box height (transform-independent). */
  innerH: number;
  /** Inner wrapper's border-box width. */
  innerW: number;
  /** Clamp box's height. */
  outerH: number;
};

export type SlideDecision =
  /** Update the baseline; leave any in-flight slide draining. */
  | { kind: 'none'; memory: SlideObservation | null }
  /** Discontinuity: update the baseline AND drop any in-flight slide. */
  | { kind: 'clear'; memory: SlideObservation | null }
  /** Append-driven line advance: set the pending offset to `startPx`. */
  | { kind: 'slide'; memory: SlideObservation; startPx: number };

const clipOf = (o: SlideObservation): number => Math.max(0, o.innerH - o.outerH);

/**
 * Classify one ResizeObserver delivery. `prev` is the last stored
 * baseline (null = recalibrate), `currentOffset` the offset still
 * pending from earlier advances — a new advance compounds onto it,
 * capped at SLIDE_MAX_WINDOWS clamp windows.
 */
export function slideDecision(
  prev: SlideObservation | null,
  next: SlideObservation,
  currentOffset: number,
): SlideDecision {
  if (next.outerH < 1 || next.innerH < 1) return { kind: 'clear', memory: null };
  if (!prev) return { kind: 'none', memory: next };
  if (Math.abs(next.innerW - prev.innerW) > EPS) return { kind: 'clear', memory: next };
  const delta = clipOf(next) - clipOf(prev);
  if (Math.abs(next.outerH - prev.outerH) > EPS && (next.outerH < prev.outerH || delta <= EPS)) {
    return { kind: 'clear', memory: next };
  }
  if (delta <= EPS) return { kind: 'none', memory: next };
  return { kind: 'slide', memory: next, startPx: Math.min(currentOffset + delta, SLIDE_MAX_WINDOWS * next.outerH) };
}

/**
 * One drain step of the pending offset after `dtMs` of wall time. Frame
 * independent: a 33ms frame drains what two 16.7ms frames would, so a
 * dropped frame never shows as a slower ticker.
 */
export function stepSlide(offset: number, dtMs: number): number {
  if (offset <= 0) return 0;
  const frames = Math.max(0, dtMs) / FRAME_MS;
  const drained = offset * (1 - Math.pow(1 - SLIDE_DRAIN_PER_FRAME, frames));
  const next = offset - Math.max(drained, SLIDE_MIN_STEP_PX * frames);
  return next > EPS ? next : 0;
}

/**
 * translateY of a COMPUTED transform value (always serialized as
 * `matrix(...)` / `matrix3d(...)`, never a transform-function list).
 * String-parsed rather than `DOMMatrixReadOnly` so it stays testable
 * without a layout engine.
 */
export function transformTranslateY(transform: string): number {
  if (!transform || transform === 'none' || !transform.startsWith('matrix')) return 0;
  const open = transform.indexOf('(');
  const close = transform.indexOf(')');
  if (open < 0 || close < 0) return 0;
  const parts = transform.slice(open + 1, close).split(',');
  const ty = Number.parseFloat(parts[transform.startsWith('matrix3d') ? 13 : 5] ?? '0');
  return Number.isFinite(ty) ? ty : 0;
}
