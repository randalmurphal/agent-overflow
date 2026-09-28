<script lang="ts" module>
  import type { ScrollToIndexAlign } from '../../utils/virtual/types';

  /** The handle of a LongListVirtualizer. Indices are indices of `data`;
   * offsets are scroll offsets of the rows the DOM holds. */
  export interface LongListHandle {
    scrollToIndex(index: number, opts?: { align?: ScrollToIndexAlign; offset?: number }): void;
    revalidate(): void;
    /** The row at a scroll offset. */
    findItemIndex(offset: number): number;
    /** Whether the row is held, and so has a scroll offset. */
    holds(index: number): boolean;
    /** A held row's scroll offset. Throws for a row that is not held. */
    getItemOffset(index: number): number;
  }
</script>

<script lang="ts" generics="T">
  import { tick, untrack, type Snippet } from 'svelte';
  import TimelineVirtualizer from './TimelineVirtualizer.svelte';
  import {
    fitsWhole,
    HELD_LIMITS,
    heldHeight,
    rangeAround,
    shouldMoveHeld,
    type HeldLimits,
    type HeldRange,
  } from '../../utils/virtual/heldRows';
  import { UNMEASURED } from '../../utils/virtual/sizes';
  import type {
    EngineCompensation,
    RowEstimate,
    TimelineVirtualizerHandle,
  } from '../../utils/virtual/types';

  // A TimelineVirtualizer over a list that may be taller than a browser
  // can lay out. The virtualizer holds a contiguous range of rows that
  // fits (utils/virtual/heldRows.ts); scrolling near an end of the range
  // or jumping outside it moves the range. The virtualizer sees the move
  // as a keyed change and keeps the row under the viewport top in place,
  // so scrolling across a move is continuous. A list that fits is passed
  // through as is.
  //
  // Across a data change the range keeps the rows it held, and grows with
  // the list at an end it shares with it. Heights come from the
  // virtualizer's measurements where it has them, else from the estimate.

  interface Props {
    data: readonly T[];
    getKey: (item: T, index: number) => unknown;
    estimate: RowEstimate;
    scrollRef?: HTMLElement;
    renderAll?: boolean;
    applyScrollTarget: (top: number) => void;
    onCompensation?: (compensation: EngineCompensation) => void;
    onscroll?: (offset: number) => void;
    onscrollend?: () => void;
    /** Test seam: smaller limits so a test list can pass them. */
    limits?: HeldLimits;
    children: Snippet<[item: T]>;
  }

  let {
    data,
    getKey,
    estimate,
    scrollRef,
    renderAll = false,
    applyScrollTarget,
    onCompensation,
    onscroll,
    onscrollend,
    limits = HELD_LIMITS,
    children: renderRow,
  }: Props = $props();

  let list: TimelineVirtualizerHandle | undefined = $state();

  // What the virtualizer was last handed. Updated when it reads `held`,
  // so these always describe the rows its engine holds.
  let heldStart = 0;
  let heldKeys: unknown[] = [];
  let heldCount = 0;

  // A request to hold the rows around a key: a scroll near an end, rows
  // grown past the limit, or a jump. Each request is a new object, and a
  // plan consumes it once.
  let request: { key: unknown } | null = $state(null);
  let planned: { key: unknown } | null = null;
  // The last scroll request, to skip asking again for the range already
  // held when a tall row keeps the reader near an end.
  let lastAsked: { key: unknown; total: number } | null = null;

  // Stable identities for the virtualizer's constructor, reading the
  // range its engine holds.
  const localEstimate: RowEstimate = {
    at: (index) => estimate.at(index + heldStart),
    isExact: (index) => estimate.isExact?.(index + heldStart) ?? false,
  };
  const localKey = (item: T, index: number) => getKey(item, index + heldStart);

  const held = $derived.by(() => {
    const items = data;
    const move = request;
    return untrack(() => {
      const keys = items.map((item, index) => getKey(item, index));
      const range = plan(keys, move);
      heldStart = range.start;
      heldKeys = keys.slice(range.start, range.end);
      heldCount = items.length;
      return range.start === 0 && range.end === items.length ? items : items.slice(range.start, range.end);
    });
  });

  function plan(keys: readonly unknown[], move: { key: unknown } | null): HeldRange {
    const fresh = move !== null && move !== planned;
    planned = move;
    const count = keys.length;
    const sizeAt = sizesFor(keys);
    if (fitsWhole(count, sizeAt, limits.limit)) return { start: 0, end: count };
    if (fresh) {
      const focus = keys.indexOf(move.key);
      if (focus >= 0) return rangeAround(count, sizeAt, focus, limits.span);
    }
    // Keep the held rows; an end shared with the list follows the list.
    const indexOf = new Map<unknown, number>();
    for (let index = 0; index < count; index += 1) indexOf.set(keys[index], index);
    let first = -1;
    let last = -1;
    for (const key of heldKeys) {
      const index = indexOf.get(key);
      if (index === undefined) continue;
      if (first < 0) first = index;
      last = index;
    }
    const top = topRow(indexOf);
    if (first >= 0) {
      const start = heldStart === 0 ? 0 : first;
      const end = heldStart + heldKeys.length === heldCount ? count : last + 1;
      const holdsTop = top < 0 || (top >= start && top < end);
      if (start < end && holdsTop && heldHeight(sizeAt, start, end) <= limits.limit) return { start, end };
    }
    return rangeAround(count, sizeAt, Math.max(0, top >= 0 ? top : first), limits.span);
  }

  /** Row heights for a plan: what the virtualizer measured for the rows
   * it holds, by key, else the estimate. */
  function sizesFor(keys: readonly unknown[]): (index: number) => number {
    const measured = new Map<unknown, number>();
    const sizes = list?.takeSnapshot() ?? [];
    for (let local = 0; local < sizes.length && local < heldKeys.length; local += 1) {
      if (sizes[local] !== UNMEASURED) measured.set(heldKeys[local], sizes[local]);
    }
    if (measured.size === 0) return (index) => estimate.at(index);
    return (index) => measured.get(keys[index]) ?? estimate.at(index);
  }

  /** The new index of the row under the viewport top, else of the first
   * row below it that survived, else -1. */
  function topRow(indexOf: ReadonlyMap<unknown, number>): number {
    const inner = list;
    if (!inner || heldKeys.length === 0) return -1;
    for (let local = inner.findItemIndex(inner.getScrollOffset()); local < heldKeys.length; local += 1) {
      const index = indexOf.get(heldKeys[local]);
      if (index !== undefined) return index;
    }
    return -1;
  }

  function handleScroll(offset: number): void {
    onscroll?.(offset);
    const inner = list;
    if (!inner || heldKeys.length === 0) return;
    const range = { start: heldStart, end: heldStart + heldKeys.length };
    const view = { offset, viewport: inner.getViewportSize(), total: inner.getTotalSize() };
    if (!shouldMoveHeld(range, heldCount, view, limits)) return;
    const key = heldKeys[inner.findItemIndex(offset + view.viewport / 2)];
    if (lastAsked && key === lastAsked.key && view.total === lastAsked.total) return;
    lastAsked = { key, total: view.total };
    request = { key };
  }

  let jumpSeq = 0;

  export function scrollToIndex(index: number, opts: { align?: ScrollToIndexAlign; offset?: number } = {}): void {
    const inner = list;
    if (!inner || data.length === 0) return;
    const seq = ++jumpSeq;
    const target = Math.max(0, Math.min(data.length - 1, index));
    // A move still pending could leave the target behind.
    if (holds(target) && request === planned) {
      inner.scrollToIndex(target - heldStart, opts);
      return;
    }
    const key = getKey(data[target], target);
    request = { key };
    void tick().then(() => {
      if (seq !== jumpSeq) return;
      const local = heldKeys.indexOf(key);
      if (local >= 0) list?.scrollToIndex(local, opts);
    });
  }

  export function revalidate(): void {
    list?.revalidate();
  }

  export function findItemIndex(offset: number): number {
    return (list?.findItemIndex(offset) ?? 0) + heldStart;
  }

  export function holds(index: number): boolean {
    return index >= heldStart && index < heldStart + heldKeys.length;
  }

  export function getItemOffset(index: number): number {
    if (!holds(index)) throw new RangeError(`LongListVirtualizer: row ${index} is not held`);
    return list?.getItemOffset(index - heldStart) ?? 0;
  }
</script>

<TimelineVirtualizer
  bind:this={list}
  data={held}
  getKey={localKey}
  {scrollRef}
  estimate={localEstimate}
  {renderAll}
  {applyScrollTarget}
  {onCompensation}
  onscroll={handleScroll}
  {onscrollend}
>
  {#snippet children(item: T)}
    {@render renderRow(item)}
  {/snippet}
</TimelineVirtualizer>
