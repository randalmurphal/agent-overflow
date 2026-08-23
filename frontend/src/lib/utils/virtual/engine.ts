// The virtualizer engine reducer: data-length changes, row measurements,
// viewport resizes, and scroll positions in → mount window + totalSize +
// at most one compensation observation out (plan §2 "The engine ↔
// controller seam").
//
// Ownership: the engine NEVER writes scrollTop and never touches the DOM.
// It answers geometry questions from its own model; when geometry changes
// in a way that would move content above the viewport, it reports the
// fact as an EngineCompensation and the scroll controller's resolver
// decides. There is no scroll-direction latch and no scroll-mode protocol
// here — user-vs-programmatic knowledge lives in the controller's intent
// machine, which arbitrates what the engine reports.

import {
  clamp,
  findIndex,
  getItemOffset as storeItemOffset,
  getItemSize as storeItemSize,
  getTotalSize as storeTotalSize,
  initSizeStore,
  isMeasured,
  remapSizes,
  setItemSize,
  spliceHead,
  takeSizeSnapshot,
  updateLength,
} from './sizes';
import { computeWindow, EMPTY_WINDOW, fullWindow, rangesEqual, seedTailWindow } from './window';
import type {
  EngineCompensation,
  EngineUpdate,
  ItemsRange,
  RowEstimate,
  ScrollToIndexAlign,
} from './types';

export interface EngineOptions {
  itemCount: number;
  estimate: RowEstimate;
  /** Symmetric overscan px on each side of the visible range. */
  bufferSize: number;
  /** Mount every row regardless of geometry (happy-dom test seam). */
  renderAll?: boolean;
}

export interface VirtualEngine {
  /** Scroll position changed. Null when the window is unchanged — the
   * range-equality early-out that makes per-scroll-event work near-free. */
  applyScroll(offset: number): EngineUpdate | null;
  /**
   * The controller wrote scrollTop; `offset` is the browser's readback.
   * Updates the offset every compensation is computed from WITHOUT
   * recomputing the window — the scroll event that follows still does
   * that, one frame later, exactly as before.
   *
   * Without this the offset lags authored writes by a frame: a spring
   * glide writes in rAF, the scroll event lands next frame, and a row
   * re-measuring above the viewport in between computes its
   * compensation target from the pre-write offset. The controller lands
   * that target verbatim mid-chase, so the viewport is pulled back by
   * the glide's last frame of travel (2px at glide start, ~17px at
   * peak) — the yank the auto-collapse release suite guards against.
   */
  noteScrollOffset(offset: number): void;
  applyViewportResize(size: number): EngineUpdate | null;
  /** One RO delivery batch of [index, size] pairs.
   *
   * `measureStraddleShift` supplies sub-row attribution for the at-most-one
   * row spanning the viewport top: how much of that row's delta landed
   * ABOVE the reading position. The engine calls it only for that row and
   * only when its size actually changed, so there is no call sequence in
   * which a caller can attribute a shift to the wrong row. Omit it (or
   * return 0) to compensate nothing for the straddling row. See
   * `readingAnchor.ts` for the DOM-side measurement. */
  applyMeasurements(
    entries: readonly (readonly [index: number, size: number])[],
    measureStraddleShift?: (index: number) => number,
  ): EngineUpdate | null;
  /** Data length changed to `count`; `headSplice` rows of that change were
   * inserted (+) / removed (-) at the head, and the rest is tail
   * growth/shrink. The keyed adapter infers this value. */
  applyLength(count: number, headSplice?: number): EngineUpdate | null;
  /** Row identities moved to new indices (same-length reorder or a keyed
   * change the head/tail entry points can't express).
   * `prevIndexByNewIndex[i]` = the row's pre-change index, -1 for a new
   * key. Measured sizes travel with their rows — a moved row keeps its
   * DOM size, so no RO delivery would ever correct a position-keyed
   * stale entry. */
  applyKeyedReorder(prevIndexByNewIndex: readonly number[]): EngineUpdate | null;

  getWindow(): ItemsRange;
  getTotalSize(): number;
  getViewportSize(): number;
  getScrollOffset(): number;
  getItemCount(): number;
  /** Index of the row at a content offset (first-visible-item math). */
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
  /** Effective row height: measured px, or the estimate until measured. */
  sizeAt(index: number): number;
  isMeasuredAt(index: number): boolean;
  /** Measured sizes for priors persistence (UNMEASURED where unmeasured). */
  takeSnapshot(): number[];
  /** Pure scrollToIndex target math; the WRITE goes through the scroll
   * controller's chokepoint, never through the engine. */
  targetOffsetFor(index: number, align?: ScrollToIndexAlign, extraOffset?: number): number;
}

/**
 * Combine two compensations that landed in the same adapter flush (e.g. a
 * head splice and a measurement batch). Both targets were computed by
 * `compensationFor` from the SAME engine scroll offset — no scroll event
 * lands mid-flush — so the exact combined target is recomputed from the
 * summed deltas. Deriving it from `next.target + prior.delta` instead
 * would inflate the result whenever `next.target` was already clamped at
 * 0 (an above-viewport shrink larger than the current offset, near the
 * top of the thread).
 */
export function mergeCompensations(
  prior: EngineCompensation,
  next: EngineCompensation,
  scrollOffset: number,
): EngineCompensation {
  return {
    kind: prior.kind === 'head-splice' || next.kind === 'head-splice' ? 'head-splice' : next.kind,
    delta: prior.delta + next.delta,
    target: Math.max(0, scrollOffset + prior.delta + next.delta),
  };
}

/**
 * Clamp a measured straddle shift to what physics allows: the part of a
 * row's growth that landed above the reading position is a PART of that
 * row's own delta, so it shares its sign and cannot exceed its magnitude.
 *
 * This is what keeps the DOM measurement from being load-bearing. A stale
 * anchor, a re-rendered subtree, or a row whose internals moved for an
 * unrelated reason can only ever pull the correction back toward zero —
 * i.e. toward the historical behavior of compensating nothing — never past
 * the row's own delta into an over-correction. NaN degrades the same way.
 */
export function boundStraddleShift(shift: number, rowDelta: number): number {
  if (!Number.isFinite(shift)) return 0;
  return rowDelta >= 0
    ? Math.min(Math.max(shift, 0), rowDelta)
    : Math.max(Math.min(shift, 0), rowDelta);
}

export function createEngine(options: EngineOptions): VirtualEngine {
  const estimate = options.estimate;
  const store = initSizeStore(options.itemCount, (index) => estimate.at(index));

  let scrollOffset = 0;
  let viewportSize = 0;
  let window: ItemsRange = options.renderAll ? fullWindow(store) : EMPTY_WINDOW;
  // Until the controller's first write (pin or restore) produces a scroll
  // input, the window stays anchored to the TAIL — bottom-anchored mount
  // seeding, so above-viewport estimate error cannot move the landing.
  let hasScrollInput = false;

  function currentWindow(atOffset = scrollOffset): ItemsRange {
    if (options.renderAll) return fullWindow(store);
    if (!hasScrollInput) return seedTailWindow(store, viewportSize, options.bufferSize);
    const boundedOffset = clamp(
      atOffset,
      0,
      Math.max(0, storeTotalSize(store) - viewportSize),
    );
    return computeWindow(
      store,
      { scrollOffset: boundedOffset, viewportSize, bufferSize: options.bufferSize },
      window[0],
    );
  }

  // force: geometry under the mounted rows changed (measurements, length),
  // so the adapter must re-apply offsets even when the range is identical.
  function refresh(compensation: EngineCompensation | undefined, force: boolean): EngineUpdate | null {
    // A structural shrink can leave the last observed scrollOffset beyond the
    // new document until the controller's compensation write emits its scroll
    // event. Rendering a window from that impossible old coordinate produces
    // one empty/intersectless flush and remounts surviving rows. Compute this
    // update from the position the compensation is about to establish. Keep
    // the observed offset itself unchanged until the real scroll event lands.
    const next = currentWindow(compensation?.target ?? scrollOffset);
    if (!force && !compensation && rangesEqual(next, window)) return null;
    window = next;
    const update: EngineUpdate = { window, totalSize: storeTotalSize(store) };
    if (compensation) update.compensation = compensation;
    return update;
  }

  function compensationFor(kind: EngineCompensation['kind'], delta: number): EngineCompensation {
    return { kind, delta, target: Math.max(0, scrollOffset + delta) };
  }

  return {
    applyScroll(offset) {
      scrollOffset = offset;
      hasScrollInput = true;
      return refresh(undefined, false);
    },

    noteScrollOffset(offset) {
      // Deliberately leaves `hasScrollInput` alone: tail seeding ends on
      // the first scroll EVENT, as it always has.
      scrollOffset = offset;
    },

    applyViewportResize(size) {
      if (size === viewportSize) return null;
      viewportSize = size;
      return refresh(undefined, false);
    },

    applyMeasurements(entries, measureStraddleShift) {
      const changed: [number, number][] = [];
      for (const [index, size] of entries) {
        // Stale RO deliveries can trail a removal; drop out-of-range rows.
        if (index < 0 || index >= store.length) continue;
        if (store.sizes[index] === size) continue;
        changed.push([index, size]);
      }
      if (!changed.length) return null;

      // Pass 1 — compensation delta against PRE-mutation geometry: growth
      // of rows entirely above the viewport top moves the reading
      // position; rows at or below it don't (while pinned, the per-beat
      // pin write covers the tail growth instead).
      //
      // The at-most-one row SPANNING the top is the mixed case: only the
      // part of its delta above the reading position shifts what the eye
      // is holding. Whole-row `[index, size]` cannot express that split,
      // so the adapter measures it in the DOM (readingAnchor.ts) and the
      // result is bounded here.
      let delta = 0;
      for (const [index, size] of changed) {
        const top = storeItemOffset(store, index);
        const oldSize = storeItemSize(store, index);
        const rowDelta = size - oldSize;
        if (top + oldSize <= scrollOffset) {
          delta += rowDelta;
        } else if (measureStraddleShift && rowDelta !== 0 && top < scrollOffset) {
          // rowDelta !== 0 skips a first measurement that landed exactly on
          // its estimate (UNMEASURED → same px still counts as changed).
          // Its bounded shift is necessarily 0, so asking would only cost
          // the measurer a DOM read.
          delta += boundStraddleShift(measureStraddleShift(index), rowDelta);
        }
      }

      // Pass 2 — apply.
      for (const [index, size] of changed) {
        setItemSize(store, index, size);
      }

      return refresh(
        delta !== 0 ? compensationFor('remeasure-above', delta) : undefined,
        true,
      );
    },

    applyKeyedReorder(prevIndexByNewIndex) {
      // Pass 1 — anchor selection against PRE-mutation geometry: the row
      // under the viewport top is the reading anchor, and compensation
      // keeps it stationary — delta = its post-remap offset minus its
      // pre-remap offset. When the anchor row itself was removed, the
      // nearest surviving row after it anchors instead (keeping content
      // below the removal from jumping); when nothing at or after the
      // anchor survives, the browser's own scrollTop clamp is the right
      // outcome and no compensation is reported.
      //
      // An earlier version telescoped per-position size deltas over the
      // above-viewport prefix. That is exact only for same-length
      // reorders: a mid-list splice (review-pane collapse/expand, fold
      // eviction) shifts every row after the splice point, which a
      // fixed-position comparison under-counts.
      const oldLength = store.length;
      const newLength = prevIndexByNewIndex.length;
      const newIndexByOldIndex = new Int32Array(oldLength).fill(-1);
      for (let i = 0; i < newLength; i++) {
        const prev = prevIndexByNewIndex[i];
        if (prev >= 0 && prev < oldLength) newIndexByOldIndex[prev] = i;
      }
      let survivorOldIndex = -1;
      if (oldLength > 0) {
        for (let i = findIndex(store, scrollOffset); i < oldLength; i++) {
          if (newIndexByOldIndex[i] >= 0) {
            survivorOldIndex = i;
            break;
          }
        }
      }
      const survivorOldTop =
        survivorOldIndex >= 0 ? storeItemOffset(store, survivorOldIndex) : 0;

      if (!remapSizes(store, prevIndexByNewIndex)) return null;

      const delta =
        survivorOldIndex >= 0
          ? storeItemOffset(store, newIndexByOldIndex[survivorOldIndex]) - survivorOldTop
          : 0;
      return refresh(
        delta !== 0 ? compensationFor('remeasure-above', delta) : undefined,
        true,
      );
    },

    applyLength(count, headSplice = 0) {
      if (count === store.length && !headSplice) return null;

      let compensation: EngineCompensation | undefined;
      if (headSplice !== 0) {
        // No estimate remap needed here: priors resolve per-row against
        // live data (a content signature, not a position — see
        // utils/virtual/priors.ts), so `estimate.at()` already reads
        // correctly against post-splice indices without help.
        const delta = spliceHead(store, headSplice);
        compensation = compensationFor('head-splice', delta);
      }
      if (count !== store.length) {
        updateLength(store, count);
      }
      return refresh(compensation, true);
    },

    getWindow: () => window,
    getTotalSize: () => storeTotalSize(store),
    getViewportSize: () => viewportSize,
    getScrollOffset: () => scrollOffset,
    getItemCount: () => store.length,
    findItemIndex: (offset) => findIndex(store, offset),
    getItemOffset: (index) => storeItemOffset(store, index),
    sizeAt: (index) => storeItemSize(store, index),
    isMeasuredAt: (index) =>
      isMeasured(store, index) || (estimate.isExact?.(index) ?? false),
    takeSnapshot: () => takeSizeSnapshot(store),

    targetOffsetFor(index, align = 'start', extraOffset = 0) {
      if (!store.length) return 0;
      index = clamp(index, 0, store.length - 1);
      const top = storeItemOffset(store, index);
      const size = storeItemSize(store, index);
      const maxOffset = Math.max(0, storeTotalSize(store) - viewportSize);

      let target: number;
      switch (align) {
        case 'start':
          target = top;
          break;
        case 'end':
          target = top + size - viewportSize;
          break;
        case 'center':
          target = top + (size - viewportSize) / 2;
          break;
        case 'nearest': {
          if (top < scrollOffset) {
            target = top;
          } else if (top + size > scrollOffset + viewportSize) {
            target = top + size - viewportSize;
          } else {
            // Already fully visible: stay put (scrollIntoView semantics).
            return clamp(scrollOffset, 0, maxOffset);
          }
          break;
        }
      }
      return clamp(target + extraOffset, 0, maxOffset);
    },
  };
}
