<script lang="ts" generics="T">
  import { onDestroy, untrack, type Snippet } from 'svelte';
  import { createEngine } from '../../utils/virtual/engine';
  import { createRowEstimate } from '../../utils/virtual/priors';
  import type {
    EngineCompensation,
    EngineUpdate,
    ItemsRange,
    RowEstimate,
    ScrollToIndexAlign,
  } from '../../utils/virtual/types';
  import VirtualRow from './VirtualRow.svelte';

  // The bespoke timeline virtualizer adapter: binds the pure engine
  // (utils/virtual/) to the DOM. One lazy ResizeObserver measures the
  // scroller (viewport) and every mounted row; scroll events feed the
  // engine's range computation; engine updates come back as one window +
  // container height application per input batch.
  //
  // Ownership contract (plan §2): this component NEVER writes scrollTop
  // on its own behalf. Geometry changes that would move content above the
  // viewport surface as `onCompensation` observations for the scroll
  // controller's resolver; imperative scrolls (scrollToIndex) go through
  // `applyScrollTarget` so the controller performs and tags the write.
  // The one default: without `applyScrollTarget` (test harnesses), the
  // target is written directly to the scroller.
  //
  // Constructor-once by design: `estimate`, `bufferSize`, and `renderAll`
  // configure the engine at mount. The `{#key pane.threadId}` remount is
  // the per-thread reset boundary — there is deliberately no live
  // snapshot-replay prop to resolve pre-mount (the footgun the old
  // cache-replay dance existed to feed).

  const DEFAULT_ESTIMATE_PX = 56;
  const DEFAULT_BUFFER_PX = 1800;
  // Matches upstream virtua's scrollend timing, which the app's snapshot
  // persistence and the browser harness quiet-waits already assume.
  const SCROLL_END_DEBOUNCE_MS = 150;
  // Wheel events landing 50–150ms after the last scroll event mean the
  // gesture is still alive even though scroll events paused (dropped
  // frames); hold the synthetic scrollend until it drains.
  const WHEEL_CONTINUATION_MIN_MS = 50;
  const WHEEL_CONTINUATION_MAX_MS = 150;
  // An imperative index scroll re-converges while destination rows
  // measure; each pass renews a 150ms settle window (upstream timing).
  const INDEX_SCROLL_SETTLE_MS = 150;
  const INDEX_SCROLL_MAX_PASSES = 8;

  interface Props {
    data: readonly T[];
    getKey: (item: T, index: number) => unknown;
    /** External scroll container. The engine never owns or styles it. */
    scrollRef?: HTMLElement;
    /** Per-row size estimate (priors → kind table → default). */
    estimate?: RowEstimate;
    /** Symmetric overscan px on each side of the visible range. */
    bufferSize?: number;
    /** One-flush head-splice hint: the CURRENT data-length change happened
     * at the head (load-older prepend), not the tail. */
    shift?: boolean;
    /** Mount every row (happy-dom unit-test seam). */
    renderAll?: boolean;
    onscroll?: (offset: number) => void;
    onscrollend?: () => void;
    /** Engine compensation observations → scroll controller resolver. */
    onCompensation?: (compensation: EngineCompensation) => void;
    /** Performs an imperative scroll write (controller chokepoint). */
    applyScrollTarget?: (top: number) => void;
    children: Snippet<[item: T, index: number]>;
  }

  let {
    data,
    getKey,
    scrollRef,
    estimate,
    bufferSize = DEFAULT_BUFFER_PX,
    shift = false,
    renderAll = false,
    onscroll,
    onscrollend,
    onCompensation,
    applyScrollTarget,
    children,
  }: Props = $props();

  // untrack: constructor-once by design (see the header block).
  const engine = untrack(() =>
    createEngine({
      itemCount: data.length,
      estimate: estimate ?? createRowEstimate({ defaultSize: DEFAULT_ESTIMATE_PX }),
      bufferSize,
      renderAll,
    }),
  );

  let windowRange = $state<ItemsRange>(engine.getWindow());
  let totalSize = $state(engine.getTotalSize());
  // Bumped on every engine update so mounted-row offsets re-read even
  // when the window range itself is unchanged (e.g. a mid-window
  // remeasure shifts the rows below it).
  let geometryVersion = $state(0);

  function applyUpdate(update: EngineUpdate | null): void {
    if (!update) return;
    windowRange = update.window;
    totalSize = update.totalSize;
    geometryVersion++;
    if (update.compensation) onCompensation?.(update.compensation);
    convergeIndexScroll();
  }

  const rows = $derived.by(() => {
    void geometryVersion;
    const [start, end] = windowRange;
    const lastIndex = Math.min(end, data.length - 1);
    const out: { item: T; index: number; offset: number; measured: boolean }[] = [];
    for (let index = Math.max(0, start); index <= lastIndex; index++) {
      out.push({
        item: data[index],
        index,
        offset: engine.getItemOffset(index),
        measured: engine.isMeasuredAt(index),
      });
    }
    return out;
  });

  // ------------------------------------------------------------------
  // Data-length changes (append/trim at tail, load-older at head)
  // ------------------------------------------------------------------
  // $effect.pre so the engine's window is clamped to the new length
  // before the row loop renders against it.
  $effect.pre(() => {
    const length = data.length;
    if (length === engine.getItemCount()) return;
    const headSplice = shift ? length - engine.getItemCount() : 0;
    applyUpdate(engine.applyLength(length, headSplice));
  });

  // ------------------------------------------------------------------
  // Measurement: one lazy ResizeObserver for the scroller + every row
  // ------------------------------------------------------------------
  let resizeObserver: ResizeObserver | undefined;
  const rowIndexes = new WeakMap<Element, number>();
  let observedScroller: HTMLElement | undefined;

  function handleResizeEntries(entries: ResizeObserverEntry[]): void {
    let viewportHeight: number | undefined;
    const resizes: [number, number][] = [];
    for (const entry of entries) {
      const target = entry.target as HTMLElement;
      // Zero-sized rects arrive for display:none subtrees (hidden pane);
      // measuring them would collapse every row to 0.
      if (!target.offsetParent) continue;
      if (target === observedScroller) {
        viewportHeight = entry.contentRect.height;
        continue;
      }
      const index = rowIndexes.get(target);
      if (index !== undefined) {
        // Content-box height: trailing row margins stay contained by the
        // [data-row-geometry-content] flow-root contract, so content-box
        // and visual extent agree (settle-flicker analysis).
        resizes.push([index, entry.contentRect.height]);
      }
    }
    if (viewportHeight !== undefined) applyUpdate(engine.applyViewportResize(viewportHeight));
    if (resizes.length) applyUpdate(engine.applyMeasurements(resizes));
  }

  function ensureResizeObserver(): ResizeObserver {
    return (resizeObserver ??= new ResizeObserver(handleResizeEntries));
  }

  function observeRow(element: HTMLElement, index: number): () => void {
    rowIndexes.set(element, index);
    ensureResizeObserver().observe(element);
    return () => {
      rowIndexes.delete(element);
      resizeObserver?.unobserve(element);
    };
  }

  // ------------------------------------------------------------------
  // Scroll input + scrollend synthesis
  // ------------------------------------------------------------------
  let scrollEndTimer: ReturnType<typeof setTimeout> | undefined;
  let wheeling = false;
  let touching = false;
  let lastScrollTime = 0;

  function armScrollEnd(): void {
    clearTimeout(scrollEndTimer);
    scrollEndTimer = setTimeout(() => {
      if (wheeling || touching) {
        wheeling = false;
        armScrollEnd();
        return;
      }
      onscrollend?.();
    }, SCROLL_END_DEBOUNCE_MS);
  }

  function handleScroll(): void {
    const scroller = observedScroller;
    if (!scroller) return;
    lastScrollTime = performance.now();
    const offset = scroller.scrollTop;
    applyUpdate(engine.applyScroll(offset));
    onscroll?.(offset);
    armScrollEnd();
  }

  function handleWheel(event: WheelEvent): void {
    // ctrlKey wheel is pinch-to-zoom, not scrolling.
    if (wheeling || event.ctrlKey || !event.deltaY) return;
    const sinceLastScroll = performance.now() - lastScrollTime;
    if (
      sinceLastScroll > WHEEL_CONTINUATION_MIN_MS &&
      sinceLastScroll < WHEEL_CONTINUATION_MAX_MS
    ) {
      wheeling = true;
    }
  }

  function handleTouchStart(): void {
    touching = true;
  }

  function handleTouchEnd(): void {
    touching = false;
  }

  $effect(() => {
    const scroller = scrollRef;
    if (!scroller) return;
    observedScroller = scroller;
    ensureResizeObserver().observe(scroller);
    scroller.addEventListener('scroll', handleScroll, { passive: true });
    scroller.addEventListener('wheel', handleWheel, { passive: true });
    scroller.addEventListener('touchstart', handleTouchStart, { passive: true });
    scroller.addEventListener('touchend', handleTouchEnd, { passive: true });
    return () => {
      scroller.removeEventListener('scroll', handleScroll);
      scroller.removeEventListener('wheel', handleWheel);
      scroller.removeEventListener('touchstart', handleTouchStart);
      scroller.removeEventListener('touchend', handleTouchEnd);
      resizeObserver?.unobserve(scroller);
      observedScroller = undefined;
      clearTimeout(scrollEndTimer);
    };
  });

  onDestroy(() => {
    clearIndexScroll();
    clearTimeout(scrollEndTimer);
    resizeObserver?.disconnect();
    resizeObserver = undefined;
  });

  // ------------------------------------------------------------------
  // Imperative scrolls (handle)
  // ------------------------------------------------------------------
  function writeScrollTarget(top: number): void {
    if (applyScrollTarget) {
      applyScrollTarget(top);
      return;
    }
    if (scrollRef) scrollRef.scrollTop = top;
  }

  interface PendingIndexScroll {
    index: number;
    align: ScrollToIndexAlign;
    extraOffset: number;
    lastTarget: number;
    passesLeft: number;
    settleTimer: ReturnType<typeof setTimeout> | undefined;
  }
  let pendingIndexScroll: PendingIndexScroll | undefined;

  function clearIndexScroll(): void {
    if (pendingIndexScroll?.settleTimer !== undefined) {
      clearTimeout(pendingIndexScroll.settleTimer);
    }
    pendingIndexScroll = undefined;
  }

  // Destination rows may be unmeasured; each measurement batch can move
  // the target. Re-write until the target is stable, a settle window
  // passes without movement, or the pass budget runs out.
  function convergeIndexScroll(): void {
    const pending = pendingIndexScroll;
    if (!pending) return;
    const target = engine.targetOffsetFor(pending.index, pending.align, pending.extraOffset);
    if (target === pending.lastTarget) return;
    if (pending.passesLeft <= 0) {
      clearIndexScroll();
      return;
    }
    pending.passesLeft--;
    pending.lastTarget = target;
    if (pending.settleTimer !== undefined) clearTimeout(pending.settleTimer);
    pending.settleTimer = setTimeout(clearIndexScroll, INDEX_SCROLL_SETTLE_MS);
    writeScrollTarget(target);
  }

  export function scrollToIndex(
    index: number,
    opts: { align?: ScrollToIndexAlign; offset?: number } = {},
  ): void {
    clearIndexScroll();
    pendingIndexScroll = {
      index,
      align: opts.align ?? 'start',
      extraOffset: opts.offset ?? 0,
      lastTarget: Number.NaN,
      passesLeft: INDEX_SCROLL_MAX_PASSES,
      settleTimer: undefined,
    };
    convergeIndexScroll();
  }

  // Explicit geometry recheck after a host-layout move (pane reorder /
  // drawer resize can reparent the scroller between RO deliveries).
  // Cold-path DOM reads by design — replaces the scrollTo(getScrollOffset())
  // self-rewrite hack.
  export function revalidate(): void {
    const scroller = scrollRef;
    if (!scroller) return;
    applyUpdate(engine.applyViewportResize(scroller.clientHeight));
    applyUpdate(engine.applyScroll(scroller.scrollTop));
  }

  // ------------------------------------------------------------------
  // Read-only handle (see TimelineVirtualizerHandle in utils/virtual/types)
  // ------------------------------------------------------------------
  export function getScrollOffset(): number {
    return engine.getScrollOffset();
  }
  export function getViewportSize(): number {
    return engine.getViewportSize();
  }
  export function getScrollSize(): number {
    return Math.max(engine.getTotalSize(), engine.getViewportSize());
  }
  export function getTotalSize(): number {
    return engine.getTotalSize();
  }
  export function findItemIndex(offset: number): number {
    return engine.findItemIndex(offset);
  }
  export function getItemOffset(index: number): number {
    return engine.getItemOffset(index);
  }
  export function sizeAt(index: number): number {
    return engine.sizeAt(index);
  }
  export function isMeasuredAt(index: number): boolean {
    return engine.isMeasuredAt(index);
  }
  export function takeSnapshot(): number[] {
    return engine.takeSnapshot();
  }
</script>

<!-- Container style contract (ported from virtua, each line a shipped
     upstream fix): `contain: size style` decouples container layout from
     row churn; `overflow-anchor: none` opts out of native scroll anchoring
     (it fights the controller's anchoring); `flex: none` because flex
     sizing breaks the explicit height. No pointer-events toggling — that
     upstream behavior is deliberately dropped (plan §2 DOM contracts). -->
<div
  style:contain="size style"
  style:overflow-anchor="none"
  style:flex="none"
  style:position="relative"
  style:width="100%"
  style:height="{totalSize}px"
>
  {#each rows as row (getKey(row.item, row.index))}
    <VirtualRow
      item={row.item}
      index={row.index}
      offset={row.offset}
      measured={row.measured}
      observe={observeRow}
      {children}
    />
  {/each}
</div>
