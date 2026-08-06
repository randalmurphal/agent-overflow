// Pure decision core for TailClampedText's line-slide FLIP.
//
// The collapsed clamp has two motion regimes. While the text is under
// TAIL_CLAMP_LINES the BOX grows a line at a time and the enclosing
// scroller's spring glides that growth. Once the box is at max-h, a new
// line re-packs the flex-end content one line UP in a single frame — a
// zero-duration teleport of every visible line
// (bug-report-20260806T011635Z: at a burst-fed drain's ceiling rate
// those re-packs cluster and read as the think block snapping). The
// component turns each re-pack into motion: FLIP on the inner wrapper —
// invert the jump with a transform in the same frame the layout moved,
// then transition back to zero.
//
// This module owns WHICH observations animate. Only append-driven clip
// advances slide; everything else recalibrates and snaps exactly as the
// pure-CSS anchor always did:
//
//   - the first observation after (re)mount or an armed reset — no
//     baseline to diff against;
//   - a width reflow — line boundaries re-derive, a translate can't
//     represent a re-wrap;
//   - an outer-box height change — the expanded flip, the clamp first
//     engaging, or a burst that grows the box AND overflows it in one
//     frame (the box growth is already the scroll spring's motion;
//     stacking a slide on top would double-ease the same pixels);
//   - a clip advance of a full window or more — no visible line
//     survives, so there is no continuity to animate;
//   - unusable geometry (hidden ancestor) — recalibrate so re-showing
//     doesn't misread the reappearance as an append.
//
// Kept pure (no DOM) so the guard matrix is unit-testable in the
// default vitest project; the component supplies real geometry and
// applies the styles.

/** Duration of the line-slide transition. */
export const SLIDE_MS = 140;

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
  /** Update the baseline; leave any in-flight slide running. */
  | { kind: 'none'; memory: SlideObservation | null }
  /** Discontinuity: update the baseline AND drop any in-flight slide. */
  | { kind: 'clear'; memory: SlideObservation | null }
  /** Append-driven line advance: invert from `startPx` and release. */
  | { kind: 'slide'; memory: SlideObservation; startPx: number };

const clipOf = (o: SlideObservation): number => Math.max(0, o.innerH - o.outerH);

/**
 * Classify one ResizeObserver delivery. `prev` is the last stored
 * baseline (null = recalibrate), `currentTy` the wrapper's in-flight
 * translateY — the live interpolated value, which is why the caller
 * reads it from computed style rather than remembering a target.
 */
export function slideDecision(
  prev: SlideObservation | null,
  next: SlideObservation,
  currentTy: number,
): SlideDecision {
  if (next.outerH < 1 || next.innerH < 1) return { kind: 'clear', memory: null };
  if (!prev) return { kind: 'none', memory: next };
  if (Math.abs(next.innerW - prev.innerW) > EPS) return { kind: 'clear', memory: next };
  if (Math.abs(next.outerH - prev.outerH) > EPS) return { kind: 'clear', memory: next };
  const delta = clipOf(next) - clipOf(prev);
  if (delta <= EPS) return { kind: 'none', memory: next };
  if (delta >= next.outerH - EPS) return { kind: 'clear', memory: next };
  // Compound onto an in-flight slide, capped at one full window — past
  // that the inversion would start on content that was never visible.
  return { kind: 'slide', memory: next, startPx: Math.min(currentTy + delta, next.outerH) };
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
