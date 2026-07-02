// Pure windowing policy: which rows to mount for a scroll position.
//
// The overscan is direction-independent and symmetric — deliberately NOT
// virtua's directional buffer trim (drop the backward overscan while
// scrolling down). That trim is the failure class the old pnpm-patch
// marking existed to suppress; keeping the buffer symmetric deletes the
// class by construction (plan §2 Ownership model).

import { computeRange, getTotalSize, type SizeStore } from './sizes';
import type { ItemsRange } from './types';

export const EMPTY_WINDOW: ItemsRange = [0, -1];

export interface Viewport {
  /** Current scrollTop in content coordinates. */
  scrollOffset: number;
  /** Visible height px. */
  viewportSize: number;
  /** Symmetric overscan px on EACH side of the visible range. */
  bufferSize: number;
}

/** Rows overlapping the viewport ± buffer, clamped to the store. */
export function computeWindow(
  store: SizeStore,
  viewport: Viewport,
  prevStartIndex: number,
): ItemsRange {
  if (!store.length || viewport.viewportSize <= 0) return EMPTY_WINDOW;
  const startOffset = Math.max(0, viewport.scrollOffset - viewport.bufferSize);
  const endOffset = Math.max(
    0,
    viewport.scrollOffset + viewport.viewportSize + viewport.bufferSize,
  );
  const [start, end] = computeRange(
    store,
    startOffset,
    endOffset,
    Math.max(0, prevStartIndex),
  );
  return [Math.max(0, start), Math.min(end, store.length - 1)];
}

/**
 * First mount of a bottom-anchored timeline: mount the TAIL window (the
 * last viewport + buffer px) so the rows that determine the user-visible
 * landing measure first and above-viewport estimate error cannot move the
 * initial viewport (plan §2 "Mount seeding").
 */
export function seedTailWindow(
  store: SizeStore,
  viewportSize: number,
  bufferSize: number,
): ItemsRange {
  if (!store.length || viewportSize <= 0) return EMPTY_WINDOW;
  const total = getTotalSize(store);
  const startOffset = Math.max(0, total - viewportSize - bufferSize);
  const [start] = computeRange(store, startOffset, total, store.length - 1);
  return [Math.max(0, start), store.length - 1];
}

/** renderAll mode (unit tests under happy-dom's zero geometry). */
export function fullWindow(store: SizeStore): ItemsRange {
  return store.length ? [0, store.length - 1] : EMPTY_WINDOW;
}

export function rangesEqual(a: ItemsRange, b: ItemsRange): boolean {
  return a[0] === b[0] && a[1] === b[1];
}
