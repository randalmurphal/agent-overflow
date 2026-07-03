// Prefix-sum size store for the timeline virtualizer: measured row sizes,
// lazily computed offsets behind a watermark, binary search, and
// locality-aware range computation.
//
// Portions derived from virtua (https://github.com/inokawa/virtua),
// Copyright (c) 2022 inokawa, MIT License — see VIRTUA_LICENSE in this
// directory. Ported from src/core/cache.ts at 0.49.1 with three changes:
//
//   - Unmeasured rows fall back to a per-index `estimate` function
//     (priors → kind table → flat default; utils/virtual/priors.ts)
//     instead of one flat default size, and the median-based
//     `estimateDefaultItemSize` is dropped — priors replace it.
//   - Head splices rebuild the offsets memo in one O(n+k) pass and keep
//     the watermark, instead of upstream's unshift-per-item (O(n·k))
//     plus full memo invalidation.
//   - Tail growth/shrink (`updateLength`) and head splices (`spliceHead`)
//     are separate entry points — the two have different compensation
//     semantics in the engine (tail changes never move content above the
//     viewport; head splices always do).
//
// Coordinates stay top-anchored prefix sums deliberately: the watermark
// (`min(index, watermark)` in setItemSize) makes tail mutations — the
// 60Hz streaming path — invalidate only the tail of the memo. Suffix sums
// would invert that cost onto the hot path (inventories validation
// note 1). Bottom-anchoring is engine/controller policy, not a coordinate
// system.

export const UNMEASURED = -1;

export interface SizeStore {
  length: number;
  /** Measured px per row; UNMEASURED until the row's RO delivers. */
  sizes: number[];
  /**
   * offsets[i] = top of row i; length+1 entries so offsets[length] is the
   * total size. Entries past the watermark are stale/UNMEASURED.
   */
  offsets: number[];
  /** Highest offsets index that is computed and valid (-1 = none). */
  offsetWatermark: number;
  /**
   * Fallback size for unmeasured rows. MUST be stable per index between
   * structural changes — computed offsets bake estimates in and are only
   * invalidated by setItemSize/updateLength/spliceHead.
   */
  estimate: (index: number) => number;
}

export const clamp = (value: number, minValue: number, maxValue: number): number =>
  Math.min(maxValue, Math.max(minValue, value));

export function initSizeStore(length: number, estimate: (index: number) => number): SizeStore {
  return {
    length,
    sizes: new Array<number>(length).fill(UNMEASURED),
    offsets: new Array<number>(length + 1).fill(UNMEASURED),
    offsetWatermark: -1,
    estimate,
  };
}

export function getItemSize(store: SizeStore, index: number): number {
  const size = store.sizes[index];
  return size === UNMEASURED ? store.estimate(index) : size;
}

export function isMeasured(store: SizeStore, index: number): boolean {
  return store.sizes[index] !== UNMEASURED;
}

/** Returns true when this is the row's first measurement. */
export function setItemSize(store: SizeStore, index: number, size: number): boolean {
  const isInitialMeasurement = store.sizes[index] === UNMEASURED;
  store.sizes[index] = size;
  // Offsets at or below `index` are unaffected; everything after is stale.
  store.offsetWatermark = Math.min(index, store.offsetWatermark);
  return isInitialMeasurement;
}

export function getItemOffset(store: SizeStore, index: number): number {
  if (!store.length) return 0;
  if (store.offsetWatermark >= index) {
    return store.offsets[index];
  }
  if (store.offsetWatermark < 0) {
    // First offset must be 0 to avoid returning NaN, which can cause
    // infinite rerender (virtua #160).
    store.offsets[0] = 0;
    store.offsetWatermark = 0;
  }
  let i = store.offsetWatermark;
  let top = store.offsets[i];
  while (i < index) {
    top += getItemSize(store, i);
    store.offsets[++i] = top;
  }
  store.offsetWatermark = index;
  return top;
}

/** Total content height. offsets[length] via the same memo. */
export function getTotalSize(store: SizeStore): number {
  return getItemOffset(store, store.length);
}

/**
 * Finds the index of the row whose computed offset is closest to (at or
 * before) the given offset. Binary search over the lazily-filled memo.
 */
export function findIndex(
  store: SizeStore,
  offset: number,
  low: number = 0,
  high: number = store.length - 1,
): number {
  let found = low;
  while (low <= high) {
    const mid = Math.floor((low + high) / 2);
    if (getItemOffset(store, mid) <= offset) {
      found = mid;
      low = mid + 1;
    } else {
      high = mid - 1;
    }
  }
  return clamp(found, 0, store.length - 1);
}

/**
 * Rows overlapping [startOffset, endOffset], searched from the previous
 * start index for locality (steady scrolling touches a handful of rows).
 */
export function computeRange(
  store: SizeStore,
  startOffset: number,
  endOffset: number,
  prevStartIndex: number,
): [startIndex: number, endIndex: number] {
  // Clamp because prevStartIndex may exceed the limit when rows decreased
  // a lot after scrolling.
  prevStartIndex = Math.min(prevStartIndex, store.length - 1);

  if (getItemOffset(store, prevStartIndex) <= startOffset) {
    // Search forward: start <= end, prevStartIndex <= start.
    const end = findIndex(store, endOffset, prevStartIndex);
    return [findIndex(store, startOffset, prevStartIndex, end), end];
  }
  // Search backward: start <= end, start <= prevStartIndex.
  const start = findIndex(store, startOffset, undefined, prevStartIndex);
  return [start, findIndex(store, endOffset, start)];
}

/**
 * Measured sizes only (UNMEASURED where the row never measured) — the
 * per-thread priors persistence payload (utils/virtual/priors.ts).
 */
export function takeSizeSnapshot(store: SizeStore): number[] {
  return store.sizes.slice();
}

/**
 * Tail growth/shrink to `length`. Returns the total-size delta (estimates
 * for unmeasured rows). Head changes go through spliceHead instead.
 */
export function updateLength(store: SizeStore, length: number): number {
  const diff = length - store.length;
  const oldLength = store.length;
  store.offsetWatermark = Math.min(length - 1, store.offsetWatermark);
  store.length = length;

  if (diff > 0) {
    for (let i = 0; i < diff; i++) {
      store.sizes.push(UNMEASURED);
      store.offsets.push(UNMEASURED);
    }
    let added = 0;
    for (let i = oldLength; i < length; i++) {
      added += store.estimate(i);
    }
    return added;
  }

  let removed = 0;
  for (let i = length; i < oldLength; i++) {
    const size = store.sizes[i];
    removed += size === UNMEASURED ? store.estimate(i) : size;
  }
  store.sizes.splice(diff);
  store.offsets.splice(diff);
  return -removed;
}

/**
 * Keyed remap: row identities moved to new positions (a same-length
 * reorder, a mid-list insert/removal, or any combination the head/tail
 * entry points can't express). `prevIndexByNewIndex[i]` is the row's
 * pre-change index, or -1 for a key that wasn't present before.
 *
 * Measured sizes travel WITH their rows. This matters because a moved
 * row keeps its DOM size, so no ResizeObserver delivery follows the move
 * — a position-keyed stale entry would never self-correct and rows below
 * the move point would render at wrong offsets (overlap) until an
 * unrelated resize. Rows whose key is new come in UNMEASURED.
 *
 * Offsets up to the first changed index stay memoized (they depend only
 * on the identity-mapped prefix). Returns true when anything changed.
 */
export function remapSizes(store: SizeStore, prevIndexByNewIndex: readonly number[]): boolean {
  const oldLength = store.length;
  const newLength = prevIndexByNewIndex.length;

  let firstChanged = -1;
  for (let i = 0; i < newLength; i++) {
    if (prevIndexByNewIndex[i] !== i) {
      firstChanged = i;
      break;
    }
  }
  if (firstChanged === -1) {
    if (newLength === oldLength) return false;
    // Identity prefix with a pure length change: the tail entry point
    // would also work, but handle it so callers don't need to split.
    firstChanged = Math.min(newLength, oldLength);
  }

  const oldSizes = store.sizes;
  const sizes = new Array<number>(newLength);
  for (let i = 0; i < newLength; i++) {
    const prev = prevIndexByNewIndex[i];
    sizes[i] = prev >= 0 && prev < oldLength ? oldSizes[prev] : UNMEASURED;
  }

  const offsets = new Array<number>(newLength + 1).fill(UNMEASURED);
  const keepWatermark = Math.min(store.offsetWatermark, firstChanged);
  for (let i = 0; i <= keepWatermark; i++) {
    offsets[i] = store.offsets[i];
  }

  store.length = newLength;
  store.sizes = sizes;
  store.offsets = offsets;
  store.offsetWatermark = keepWatermark;
  return true;
}

/**
 * Insert (`count > 0`) or remove (`count < 0`) rows at the head. Returns
 * the total-size delta: +sum of inserted estimates, or −sum of removed
 * sizes/estimates.
 *
 * A prepend consults `estimate` for the NEW head indices [0, count); a
 * removal consults the REMOVED pre-splice indices. Neither needs an
 * ordering contract with the caller: `RowEstimate.at` (utils/virtual/priors.ts)
 * resolves per-row against live data — a content signature, not a
 * position — so there is no index-keyed estimate state left to remap
 * across the splice.
 */
export function spliceHead(store: SizeStore, count: number): number {
  if (count === 0) return 0;
  return count > 0 ? prependHead(store, count) : removeHead(store, -count);
}

function prependHead(store: SizeStore, count: number): number {
  const oldOffsets = store.offsets;
  const oldWatermark = store.offsetWatermark;
  store.length += count;
  store.sizes = new Array<number>(count).fill(UNMEASURED).concat(store.sizes);

  // Rebuild the memo in one pass instead of discarding it: the new head
  // rows are estimate-sized prefix entries, and every previously computed
  // offset shifts by exactly their sum.
  const offsets = new Array<number>(store.length + 1).fill(UNMEASURED);
  let top = 0;
  for (let i = 0; i < count; i++) {
    offsets[i] = top;
    top += store.estimate(i);
  }
  offsets[count] = top; // old offsets[0] was 0 by definition
  for (let i = 1; i <= oldWatermark; i++) {
    offsets[count + i] = oldOffsets[i] + top;
  }
  store.offsets = offsets;
  store.offsetWatermark = Math.max(count, oldWatermark + count);
  return top;
}

function removeHead(store: SizeStore, count: number): number {
  count = Math.min(count, store.length);

  if (store.offsetWatermark >= count) {
    // The memo already knows the removed prefix's exact extent; shift the
    // surviving entries down by it and keep the watermark.
    const removedSize = store.offsets[count];
    const oldOffsets = store.offsets;
    const newWatermark = store.offsetWatermark - count;
    store.length -= count;
    store.sizes = store.sizes.slice(count);
    const offsets = new Array<number>(store.length + 1).fill(UNMEASURED);
    for (let i = 0; i <= newWatermark; i++) {
      offsets[i] = oldOffsets[i + count] - removedSize;
    }
    store.offsets = offsets;
    store.offsetWatermark = newWatermark;
    return -removedSize;
  }

  // Watermark inside the removed prefix: sum the removed rows directly and
  // drop the memo.
  let removedSize = 0;
  for (let i = 0; i < count; i++) {
    const size = store.sizes[i];
    removedSize += size === UNMEASURED ? store.estimate(i) : size;
  }
  store.length -= count;
  store.sizes = store.sizes.slice(count);
  store.offsets = new Array<number>(store.length + 1).fill(UNMEASURED);
  store.offsetWatermark = -1;
  return -removedSize;
}
