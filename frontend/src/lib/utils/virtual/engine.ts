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
  applyViewportResize(size: number): EngineUpdate | null;
  /** One RO delivery batch of [index, size] pairs. */
  applyMeasurements(
    entries: readonly (readonly [index: number, size: number])[],
  ): EngineUpdate | null;
  /** Data length changed to `count`; `headSplice` rows of that change were
   * inserted (+) / removed (−) at the head (the `shift` one-flush
   * contract), the rest is tail growth/shrink. */
  applyLength(count: number, headSplice?: number): EngineUpdate | null;

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

  function currentWindow(): ItemsRange {
    if (options.renderAll) return fullWindow(store);
    if (!hasScrollInput) return seedTailWindow(store, viewportSize, options.bufferSize);
    return computeWindow(
      store,
      { scrollOffset, viewportSize, bufferSize: options.bufferSize },
      window[0],
    );
  }

  // force: geometry under the mounted rows changed (measurements, length),
  // so the adapter must re-apply offsets even when the range is identical.
  function refresh(compensation: EngineCompensation | undefined, force: boolean): EngineUpdate | null {
    const next = currentWindow();
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

    applyViewportResize(size) {
      if (size === viewportSize) return null;
      viewportSize = size;
      return refresh(undefined, false);
    },

    applyMeasurements(entries) {
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
      let delta = 0;
      for (const [index, size] of changed) {
        const top = storeItemOffset(store, index);
        const oldSize = storeItemSize(store, index);
        if (top + oldSize <= scrollOffset) {
          delta += size - oldSize;
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

    applyLength(count, headSplice = 0) {
      if (count === store.length && !headSplice) return null;

      let compensation: EngineCompensation | undefined;
      if (headSplice > 0) {
        // Remap index-keyed estimates BEFORE a prepend, AFTER a removal —
        // spliceHead consults post-splice indices for inserted rows and
        // pre-splice indices for removed ones (sizes.ts contract).
        estimate.shiftBase(headSplice);
        const delta = spliceHead(store, headSplice);
        compensation = compensationFor('head-splice', delta);
      } else if (headSplice < 0) {
        const delta = spliceHead(store, headSplice);
        estimate.shiftBase(headSplice);
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
    isMeasuredAt: (index) => isMeasured(store, index),
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
