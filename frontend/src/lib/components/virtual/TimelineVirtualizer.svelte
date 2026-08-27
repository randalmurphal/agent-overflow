<script lang="ts" generics="T">
  import { onDestroy, untrack, type Snippet } from 'svelte';
  import { createEngine, mergeCompensations } from '../../utils/virtual/engine';
  import {
    classifyKeyedSequenceMutation,
    keyedReorderPermutation,
  } from '../../utils/virtual/keys';
  import {
    projectVirtualPlane,
    type GlobalPlaneRow,
    type VirtualPlaneState,
  } from '../../utils/virtual/plane';
  import { createRowEstimate } from '../../utils/virtual/priors';
  import {
    measureReadingAnchorShift,
    sampleReadingAnchor,
    type ReadingAnchor,
  } from '../../utils/virtual/readingAnchor';
  import type {
    ContentGeometrySample,
    EngineCompensation,
    EngineUpdate,
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
  // Ownership contract (plan §2): this component NEVER writes scrollTop.
  // Geometry changes that would move content above the viewport surface
  // as `onCompensation` observations for the scroll controller's
  // resolver; imperative scrolls (scrollToIndex) go through the REQUIRED
  // `applyScrollTarget` prop so the controller performs and tags the
  // write — test harnesses pass a direct writer, so the exception lives
  // in test code, not here. The component is also the
  // controller's content-geometry source (`onContentGeometry`): the
  // spacer height it writes IS the content height, so chat runs no second
  // ResizeObserver over the content element.
  //
  // Constructor-once by design: `estimate`, `bufferSize`, and `renderAll`
  // configure the engine at mount. The `{#key pane.threadId}` remount is
  // the per-thread reset boundary — there is deliberately no live
  // snapshot-replay prop to resolve pre-mount (the footgun the old
  // cache-replay dance existed to feed).

  const DEFAULT_ESTIMATE_PX = 56;
  // Keep in sync with MessageTimeline's BUFFER_SIZE_PX — the rationale
  // (scroll-runway sizing; deliberately NOT a memory dial) lives on
  // that constant.
  const DEFAULT_BUFFER_PX = 1200;
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
  // How far the live position may sit from where a pending index scroll
  // left it before a convergence pass treats the viewport as taken over.
  // Covers fractional scrollTop quantization; real takeovers (a spring
  // frame, a wheel tick) move whole tens of pixels.
  const INDEX_SCROLL_TAKEOVER_TOLERANCE_PX = 2;

  interface Props {
    data: readonly T[];
    getKey: (item: T, index: number) => unknown;
    /** Exact-height content before the first data row. It stays outside
     * keyed row identity so a prepend cannot move the header to a new row
     * and silently change the retained row's geometry. */
    header?: Snippet;
    /** Rendered header height in CSS px. Required when `header` is set. */
    headerSize?: number;
    /** External scroll container. The engine never owns or styles it. */
    scrollRef?: HTMLElement;
    /** Per-row size estimate (priors → kind table → default). */
    estimate?: RowEstimate;
    /** Symmetric overscan px on each side of the visible range. */
    bufferSize?: number;
    /** Mount every row (happy-dom unit-test seam). */
    renderAll?: boolean;
    /** The stable mounted-row plane. */
    renderPlane?: HTMLDivElement;
    onscroll?: (offset: number) => void;
    onscrollend?: () => void;
    /** Engine compensation observations → scroll controller resolver. */
    onCompensation?: (compensation: EngineCompensation) => void;
    /** Performs an imperative scroll write (controller chokepoint).
     * Required — the adapter can never write scrollTop itself; test
     * harnesses pass a direct writer. */
    applyScrollTarget: (top: number) => void;
    /** Whether the viewport top is a READING position that must be held
     * stationary. False while the controller holds bottom-follow intent:
     * there the per-beat pin write already absorbs growth anywhere in the
     * content, so tracking a reading anchor would cost a hit-test per
     * scroll event and change nothing. Defaults to always-track. */
    trackReadingAnchor?: () => boolean;
    /** Engine-sourced content-geometry samples → the scroll controller's
     * `deliverContentGeometry` (replaces its contentEl ResizeObserver).
     * Delivered post-flush alongside compensations, and only when the
     * (height, width, settle) tuple actually changed. */
    onContentGeometry?: (sample: ContentGeometrySample) => void;
    children: Snippet<[item: T, index: number]>;
  }

  let {
    data,
    getKey,
    header,
    headerSize,
    scrollRef,
    estimate,
    bufferSize = DEFAULT_BUFFER_PX,
    renderAll = false,
    renderPlane = $bindable(),
    onscroll,
    onscrollend,
    onCompensation,
    applyScrollTarget,
    trackReadingAnchor,
    onContentGeometry,
    children,
  }: Props = $props();

  function validatedHeaderSize(snippet: Snippet | undefined, size: number | undefined): number {
    if (!snippet) {
      if (size !== undefined && size !== 0) {
        throw new Error('TimelineVirtualizer headerSize requires a header snippet');
      }
      return 0;
    }
    if (size === undefined || !Number.isFinite(size) || size < 0) {
      throw new Error('TimelineVirtualizer headerSize must be a finite non-negative number');
    }
    return size;
  }

  const renderHeaderSize = $derived(validatedHeaderSize(header, headerSize));
  let actualScrollOffset = 0;

  function engineOffsetFor(offset: number): number {
    return Math.max(0, offset - renderHeaderSize);
  }

  function publicCompensation(compensation: EngineCompensation): EngineCompensation {
    return {
      ...compensation,
      target: Math.max(0, actualScrollOffset + compensation.delta),
    };
  }

  // untrack: constructor-once by design (see the header block).
  const engineEstimate = untrack(
    () => estimate ?? createRowEstimate({ defaultSize: DEFAULT_ESTIMATE_PX }),
  );
  const engine = untrack(() =>
    createEngine({
      itemCount: data.length,
      estimate: engineEstimate,
      bufferSize,
      renderAll,
    }),
  );

  // Bumped on every engine update so mounted-row offsets re-read even
  // when the window range itself is unchanged (e.g. a mid-window
  // remeasure shifts the rows below it).
  let geometryVersion = $state(0);

  function applyUpdate(update: EngineUpdate | null): void {
    if (!update) return;
    geometryVersion++;
    if (update.compensation) queueCompensation(publicCompensation(update.compensation));
  }

  // ------------------------------------------------------------------
  // Post-flush write timing
  // ------------------------------------------------------------------
  // applyUpdate lands engine geometry in Svelte state; the DOM (spacer
  // height, row offsets) reflects it only after the template flush.
  // Compensation delivery and index-scroll convergence both end in a
  // scrollTop write against live DOM geometry: delivered pre-flush, a
  // target beyond the STALE scrollHeight clamps (the pinned tail visibly
  // shifts by the growth delta, or the spring inherits the whole
  // correction as a multi-frame chase), and the resolver samples a stale
  // bottom target. This effect delivers both after the DOM is
  // consistent, still before paint — the same timing virtua's patched
  // applier seam had, which the resolver tiers were calibrated against.
  let pendingCompensation: EngineCompensation | null = null;
  let deliveredGeometryVersion = -1;
  let deliveredDataSnapshot: object | null = null;

  function queueCompensation(next: EngineCompensation): void {
    const prior = pendingCompensation;
    if (!prior) {
      pendingCompensation = next;
      return;
    }
    // Two engine updates in one flush (e.g. a head splice and a
    // measurement batch). Both targets derive from the same engine scroll
    // offset, so the merge recomputes the exact combined target from the
    // summed deltas (see mergeCompensations).
    pendingCompensation = mergeCompensations(prior, next, actualScrollOffset);
  }

  // Header geometry is exact and therefore has no ResizeObserver delivery
  // to tell the engine that data rows moved. Report the move directly when
  // the reader is below the old header. At the top, scrollTop stays zero so
  // the header itself remains the reading anchor.
  let priorHeaderSize = untrack(() => validatedHeaderSize(header, headerSize));
  $effect.pre(() => {
    const next = renderHeaderSize;
    const prior = priorHeaderSize;
    if (next === prior) return;
    priorHeaderSize = next;
    geometryVersion++;
    if (actualScrollOffset >= prior) {
      const delta = next - prior;
      queueCompensation({
        kind: 'head-splice',
        delta,
        target: Math.max(0, actualScrollOffset + delta),
      });
    }
  });

  $effect(() => {
    const currentGeometryVersion = geometryVersion;
    const currentData = reconciledData;
    void contentGeometryTrigger;
    const geometryChanged = currentGeometryVersion !== deliveredGeometryVersion;
    const dataChanged = currentData !== deliveredDataSnapshot;
    deliveredGeometryVersion = currentGeometryVersion;
    deliveredDataSnapshot = currentData;
    const compensation = pendingCompensation;
    pendingCompensation = null;
    untrack(() => {
      if (compensation && onCompensation) {
        onCompensation(compensation);
        syncEngineToLiveScrollTop();
        // The controller's compensation write moves the position by the
        // content shift it preserves across; a pending index scroll's
        // takeover check must expect the same shift or it would read its
        // own side's write as a takeover and die mid-restore. Only when a
        // consumer is attached: undelivered compensation writes nothing,
        // so the expectation must not move either.
        noteCompensationForIndexScroll(compensation.delta);
      }
      convergeIndexScroll();
      const spliceCorrected = correctHeadSpliceAnchor();
      maybeDeliverContentGeometry();
      // Strictly last: row offsets and the container height are flushed
      // and the controller has performed any compensation write, so this
      // is the settled layout the NEXT measurement must be judged against.
      // Sampling before the write would bake the pre-compensation
      // position into the anchor and double-count it next pass.
      // A same-key content update must retain the PRE-update intra-row
      // reading anchor until ResizeObserver attributes the resulting size
      // delta. Refreshing here would sample the already-grown DOM and erase
      // the shift. Geometry updates run this effect again after measurement;
      // structural key changes need a fresh anchor immediately.
      const structuralChange = dataChanged && currentData.mutation.kind !== 'unchanged';
      if (geometryChanged || structuralChange) {
        const consumed = straddleShiftConsumed;
        straddleShiftConsumed = false;
        refreshReadingAnchor(
          actualScrollOffset,
          compensation !== null || spliceCorrected || consumed || structuralChange,
        );
      }
    });
  });

  // ------------------------------------------------------------------
  // Engine-sourced content geometry (the controller's contentRO merge)
  // ------------------------------------------------------------------
  // The spacer's explicit height makes the engine's totalSize identical
  // to the content element's height, so this component IS the content-
  // geometry source: one sample per actual change, delivered from the
  // post-flush effect above (DOM consistent, pre-paint — the same timing
  // argument as compensation delivery). Width comes from the scroller's
  // RO entry (the single async content-box width source — never a
  // synchronous layout read, per the width-oscillation contract). The
  // settle fields are warm-gate evidence: window fully measured + how far
  // first measurements landed from their estimates (a priors-hit revisit
  // measures ~0; a cold estimate cascade measures large corrections).
  //
  // Deliveries start only once the scroller RO has reported a width —
  // the same first-delivery timing the contentEl RO had. Samples repeat
  // only when the tuple changes, so scroll-driven effect runs (window
  // shifts at constant geometry) deliver nothing.
  let contentGeometryTrigger = $state(0);
  let scrollerContentWidth: number | undefined;
  let scrollerContentHeight: number | undefined;
  let maxFirstMeasureCorrectionPx = 0;
  let lastGeometrySample: ContentGeometrySample | undefined;

  function windowFullyMeasured(): boolean {
    const [start, end] = engine.getWindow();
    const lastIndex = Math.min(end, engine.getItemCount() - 1);
    const firstIndex = Math.max(0, start);
    // An empty window has measured nothing — never report settle
    // evidence for it (the empty-timeline paths use markAtBottom /
    // skipWarmup instead of the warm gate).
    if (lastIndex < firstIndex) return false;
    for (let index = firstIndex; index <= lastIndex; index++) {
      if (!engine.isMeasuredAt(index)) return false;
    }
    return true;
  }

  function maybeDeliverContentGeometry(): void {
    if (!onContentGeometry || scrollerContentWidth === undefined) return;
    const sample: ContentGeometrySample = {
      height: engine.getTotalSize() + renderHeaderSize,
      width: scrollerContentWidth,
      windowMeasured: windowFullyMeasured(),
      maxFirstMeasureCorrectionPx,
      viewportHeight: scrollerContentHeight,
    };
    const prev = lastGeometrySample;
    // Field-by-field on purpose — but TypeScript will NOT flag a field
    // added to ContentGeometrySample and missed here; a missed field
    // silently swallows deliveries that differ only in it. Update this
    // compare alongside the type.
    if (
      prev &&
      prev.height === sample.height &&
      prev.width === sample.width &&
      prev.windowMeasured === sample.windowMeasured &&
      prev.maxFirstMeasureCorrectionPx === sample.maxFirstMeasureCorrectionPx &&
      prev.viewportHeight === sample.viewportHeight
    ) {
      return;
    }
    lastGeometrySample = sample;
    onContentGeometry(sample);
  }

  // Recomputes on EVERY data identity change (each streaming beat passes
  // a fresh items array), even when the window and offsets held still.
  // That reactivity is load-bearing — a mounted row's item reference must
  // stay live for its content to update mid-stream — so gating this on
  // engine versions alone cannot work. The rebuild is O(window)
  // descriptors with matching keys; the keyed diff writes no DOM when
  // geometry is unchanged.
  interface RenderRow {
    item: T;
    key: unknown;
    index: number;
    offset: number;
    measured: boolean;
    observe: boolean;
  }

  let planeState: VirtualPlaneState<unknown> = {
    anchorKey: null,
    localOffsets: new Map(),
  };
  let planeDataSnapshot: object | null = null;

  // Rows from the previous projection pass, by key, for identity reuse
  // below. Plain Map, replaced wholesale each pass — entries for keys that
  // left the window die with the old Map.
  let rowReuse = new Map<unknown, RenderRow>();

  const projection = $derived.by(() => {
    void geometryVersion;
    const current = reconciledData;
    const currentData = current.items;
    const windowRange = engine.getWindow();
    const [start, end] = windowRange;
    const lastIndex = Math.min(end, currentData.length - 1);
    const rows: RenderRow[] = [];
    const globalRows: GlobalPlaneRow<unknown>[] = [];
    for (let index = Math.max(0, start); index <= lastIndex; index++) {
      const key = reconciledData.keys[index];
      const globalOffset = engine.getItemOffset(index);
      globalRows.push({ key, offset: globalOffset, size: engine.sizeAt(index) });
      rows.push({
        item: currentData[index],
        key,
        index,
        offset: globalOffset,
        measured: engine.isMeasuredAt(index),
        // Exact rows never attach a ResizeObserver — their estimate IS the
        // rendered height (RowEstimate.isExact contract).
        observe: !(engineEstimate.isExact?.(index) ?? false),
      });
    }
    const newDataSnapshot = current !== planeDataSnapshot;
    planeDataSnapshot = current;
    const plane = projectVirtualPlane(
      globalRows,
      planeState,
      newDataSnapshot && (current.mutation.kind === 'head' || current.mutation.kind === 'keyed'),
    );
    planeState = plane;
    // An unchanged row keeps its previous OBJECT, not just its values. This
    // derived re-runs on every reveal tick and every geometry bump, and the
    // keyed each writes each row into a per-key signal — a fresh object
    // there reads as a change, so every mounted row re-ran its wrapper
    // effects (position style, setRowIndex) and re-validated its snippet on
    // every streamed chunk, O(window) per tick with almost every value
    // identical (2026-08-26, the 165Hz frame-drop attribution — same
    // identity-churn class as the projection node caches). Reused objects
    // are never mutated: a candidate replaces the cached row unless EVERY
    // field matches.
    const reused = new Map<unknown, RenderRow>();
    for (let i = 0; i < rows.length; i++) {
      const candidate = rows[i];
      candidate.offset = plane.localOffsets.get(candidate.key) ?? 0;
      const previous = rowReuse.get(candidate.key);
      if (
        previous &&
        previous.item === candidate.item &&
        previous.index === candidate.index &&
        previous.offset === candidate.offset &&
        previous.measured === candidate.measured &&
        previous.observe === candidate.observe
      ) {
        rows[i] = previous;
      }
      reused.set(candidate.key, rows[i]);
    }
    rowReuse = reused;
    return { rows, plane: plane.geometry };
  });

  const rows = $derived(projection.rows);

  // ------------------------------------------------------------------
  // Data changes (tail append/trim, load-older head splice, reorders)
  // ------------------------------------------------------------------
  // Keys are compared every beat, not just lengths: a same-length upsert
  // can REORDER rows (e.g. a queued user message repositioned to its
  // turn tail by the interrupt promote), and a moved row keeps its DOM
  // size — no RO delivery follows to correct a position-keyed
  // measurement, so the stale offsets would persist until an unrelated
  // resize (the prose-overlaps-message bug, 2026-07-03). Pure head/tail
  // changes stay on the applyLength path (its head-splice compensation
  // kind is load-bearing for load-older); anything else remaps measured
  // sizes by row identity.
  function keysFor(items: readonly T[]): unknown[] {
    const keys = items.map((item, index) => getKey(item, index));
    const unique = new Set(keys);
    if (unique.size !== keys.length) {
      throw new Error('TimelineVirtualizer requires a unique key for every row');
    }
    return keys;
  }

  let prevKeys: readonly unknown[] = untrack(() => keysFor(data));

  // This reconciliation is a render dependency, not an effect. An effect.pre
  // still lets Svelte derive one intermediate template from NEW data and the
  // OLD engine range before the effect's state bump schedules its second
  // flush. On a large head prune that transient range is empty, so every
  // visible keyed row unmounts and remounts in one mutation batch. Reading
  // `reconciledData` is now the only door to the render data: it updates the
  // non-reactive engine first and returns that exact keyed snapshot, making an
  // unreconciled data/range pair structurally unavailable to the template.
  const reconciledData = $derived.by(() => {
    const items = data;
    const nextKeys = keysFor(data);
    const prev = prevKeys;
    prevKeys = nextKeys;
    const mutation = classifyKeyedSequenceMutation(prev, nextKeys);
    switch (mutation.kind) {
      case 'unchanged':
        break;
      case 'head':
        armHeadSpliceAnchor();
        queueUpdate(engine.applyLength(nextKeys.length, mutation.headSplice));
        break;
      case 'tail':
        queueUpdate(engine.applyLength(nextKeys.length, mutation.headSplice));
        break;
      case 'keyed':
        queueUpdate(engine.applyKeyedReorder(keyedReorderPermutation(prev, nextKeys)));
    }
    return { items, keys: nextKeys, mutation };
  });

  function queueUpdate(update: EngineUpdate | null): void {
    if (update?.compensation) queueCompensation(publicCompensation(update.compensation));
  }

  const renderTotalSize = $derived.by(() => {
    void geometryVersion;
    void reconciledData;
    return engine.getTotalSize();
  });

  const renderHeader = $derived.by(() => {
    void geometryVersion;
    void reconciledData;
    return header && (data.length === 0 || engine.getWindow()[0] === 0) ? header : undefined;
  });

  const plane = $derived(projection.plane);

  // ------------------------------------------------------------------
  // Measurement: one lazy ResizeObserver for the scroller + every row
  // ------------------------------------------------------------------
  let resizeObserver: ResizeObserver | undefined;
  const rowIndexes = new WeakMap<Element, number>();
  // Reverse lookup for headAnchorAt. Entries go stale when a head splice
  // re-indexes rows (the old index keeps pointing at its element until
  // another row claims it), so every read is verified against rowIndexes
  // before use.
  const rowElementByIndex = new Map<number, HTMLElement>();
  let observedScroller: HTMLElement | undefined;

  // ------------------------------------------------------------------
  // Reading anchor (sub-row attribution for the straddling row)
  // ------------------------------------------------------------------
  // Rationale lives in utils/virtual/readingAnchor.ts. The anchor is
  // sampled whenever the CURRENT layout is the one the next measurement
  // should be judged against — after a scroll, and after each measurement
  // pass has flushed and been compensated — so every batch is measured
  // against the state the previous batch left behind.
  let readingAnchor: ReadingAnchor | null = null;
  let anchorScrollTop = -1;
  // Set when the engine consumed a nonzero straddle shift this batch: the
  // sampled baseline was spent and MUST be re-sampled even when the batch
  // nets to zero compensation (a shift exactly cancelled by growth above
  // would otherwise leave a stale baseline that double-counts next pass).
  let straddleShiftConsumed = false;
  interface HeadSpliceAnchor {
    rowEl: HTMLElement;
    /** Row top relative to the viewport top, in content px:
     *  engine offset + header − scrollTop at sample time. Differences of
     *  two samples equal client-rect deltas (rows are absolutely
     *  positioned from engine offsets), without the forced-layout read. */
    relativeTop: number;
  }
  let latestViewportAnchor: HeadSpliceAnchor | null = null;
  let headSpliceAnchor: HeadSpliceAnchor | null = null;

  function rowFor(element: Element): { el: HTMLElement; index: number } | undefined {
    for (let node: Element | null = element; node; node = node.parentElement) {
      const index = rowIndexes.get(node);
      if (index !== undefined) return { el: node as HTMLElement, index };
      if (node === observedScroller) return undefined;
    }
    return undefined;
  }

  function readingAnchorWanted(): boolean {
    return trackReadingAnchor?.() ?? true;
  }

  // No DOM reads except the sub-row sample itself. This runs from the
  // post-flush effect with the whole document freshly dirtied by the
  // template flush, where ANY forced read (even a bare scrollTop) pays a
  // whole-document style recalc — measured at 1,091 elements per call
  // during two-pane streaming (perf trace 2026-08-25). The caller passes
  // the scrollTop it already knows; both anchors below are answered from
  // engine geometry.
  //
  // `forceSample` re-samples the sub-row anchor unconditionally (a scroll
  // moved the viewport, a compensation moved the content, the previous
  // baseline was consumed, or rows re-keyed). Without it a still-valid
  // baseline is KEPT: growth strictly below the viewport moves neither
  // the straddling row nor the anchor element inside it, so re-sampling
  // per streaming beat would buy nothing and cost a whole-document hit
  // test each time the reader sits in scrollback over a streaming tail.
  function refreshReadingAnchor(offset: number, forceSample: boolean): void {
    const scroller = observedScroller;
    if (!scroller) {
      readingAnchor = null;
      anchorScrollTop = -1;
      latestViewportAnchor = null;
      return;
    }
    if (!readingAnchorWanted()) {
      readingAnchor = null;
    } else if (
      forceSample ||
      !readingAnchor ||
      !readingAnchor.anchorEl.isConnected ||
      !readingAnchor.rowEl.isConnected
    ) {
      readingAnchor = sampleReadingAnchor({ scroller, rowFor });
    }
    anchorScrollTop = offset;
    // A retained-row anchor is only needed near the head, where a prepend
    // can occur, or while a measurement cascade is already being held.
    // Skip the bookkeeping on the bottom-follow streaming hot path.
    if (!headSpliceAnchor && engine.getWindow()[0] !== 0) {
      latestViewportAnchor = null;
      return;
    }
    latestViewportAnchor = headAnchorAt(offset);
  }

  // The mounted row spanning the viewport top, located by engine
  // arithmetic. Unmeasured rows answer null — they render
  // visibility:hidden, so the old hit test also resolved no row there.
  function headAnchorAt(offset: number): HeadSpliceAnchor | null {
    if (engine.getItemCount() === 0) return null;
    const index = engine.findItemIndex(engineOffsetFor(offset));
    if (!engine.isMeasuredAt(index)) return null;
    const rowEl = rowElementByIndex.get(index);
    if (!rowEl || rowIndexes.get(rowEl) !== index || !rowEl.isConnected) return null;
    return {
      rowEl,
      relativeTop: engine.getItemOffset(index) + renderHeaderSize - offset,
    };
  }

  function syncEngineToLiveScrollTop(): void {
    const top = observedScroller?.scrollTop;
    if (top === undefined) return;
    actualScrollOffset = top;
    engine.noteScrollOffset(engineOffsetFor(top));
  }

  // A head page can place the old reading row below one newly inserted,
  // still-unmeasured row. The normal straddling-row rule intentionally
  // leaves that row's below-viewport-top correction visible. During a head
  // splice that would move the retained reading row instead. Hold the live
  // retained DOM row across the inserted window's measurement cascade.
  // Scroll input refreshes `latestViewportAnchor` while the request is in
  // flight, so this never restores a position captured before the user moved.
  function armHeadSpliceAnchor(): void {
    const anchor = latestViewportAnchor;
    headSpliceAnchor = anchor?.rowEl.isConnected ? { ...anchor } : null;
  }

  // Returns whether a correction was issued (the caller must then treat
  // the reading-anchor baseline as spent). Delta comes from engine
  // offsets: the retained row's (offset − scrollTop) now versus at sample
  // time. Rows are absolutely positioned from those same offsets, so this
  // equals the old client-rect delta without the forced-layout read — and
  // it deliberately excludes scroll motion actualScrollOffset has not
  // seen yet (a spring frame's write mid-flush), which the rect read used
  // to fold into the "content shift". The one same-flush writer that must
  // be visible is a pending index scroll's own write (delivered before
  // this runs, its scroll event still in flight), so only then is the
  // live scrollTop read — a cold path.
  function correctHeadSpliceAnchor(): boolean {
    const anchor = headSpliceAnchor;
    if (!anchor) return false;
    const index = anchor.rowEl.isConnected ? rowIndexes.get(anchor.rowEl) : undefined;
    if (index === undefined) {
      headSpliceAnchor = null;
      return false;
    }
    const currentTop = pendingIndexScroll
      ? (observedScroller?.scrollTop ?? actualScrollOffset)
      : actualScrollOffset;
    const relativeNow = engine.getItemOffset(index) + renderHeaderSize - currentTop;
    const delta = relativeNow - anchor.relativeTop;
    let corrected = false;
    if (Math.abs(delta) > 0.01 && onCompensation) {
      onCompensation({
        kind: 'head-splice',
        delta,
        target: Math.max(0, currentTop + delta),
      });
      syncEngineToLiveScrollTop();
      noteCompensationForIndexScroll(delta);
      corrected = true;
    }
    if (windowFullyMeasured()) headSpliceAnchor = null;
    return corrected;
  }

  // Passed to the engine, which calls it ONLY for the row spanning the
  // viewport top and only when that row's size changed. Identity is
  // checked against the live element→index map rather than the index
  // captured at sample time, so a head splice between sample and
  // measurement can't misattribute the shift to a different row.
  //
  // The gate is re-read HERE, not merely at sample time: bottom-follow
  // intent can be regained without a scroll event or a measurement pass
  // (markAtBottom, forceStick, the resolver's own setIsAtBottom), which
  // would otherwise leave a live anchor armed from before the flip and
  // land a sub-row correction on top of the pin write. Checking at use
  // makes the sample-time check a pure optimization.
  function measureStraddleShift(index: number): number {
    const anchor = readingAnchor;
    if (!anchor || !readingAnchorWanted()) return 0;
    if (rowIndexes.get(anchor.rowEl) !== index) return 0;
    const shift = measureReadingAnchorShift(anchor) ?? 0;
    if (shift !== 0) straddleShiftConsumed = true;
    return shift;
  }

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
        if (viewportHeight !== scrollerContentHeight) {
          scrollerContentHeight = viewportHeight;
          // A content-box viewport move can race a browser scroll clamp.
          // The bumped delivery below carries the new viewportHeight so
          // the controller refreshes cached scrollTop even when virtual
          // content height did not change.
          contentGeometryTrigger++;
        }
        // Sibling width observer: MessageTimeline's
        // observeScrollSurfaceContentWidth watches this same scroller for
        // the size-priors validity key. It must outlive the `{#key}`
        // remount that recreates this component (and report before this
        // component mounts), so the two observations stay separate. Both
        // are async RO content-box reads; neither may become a sync
        // layout read (width-oscillation incident 2026-06-26).
        const width = entry.contentRect.width;
        if (width !== scrollerContentWidth) {
          scrollerContentWidth = width;
          // Bump the post-flush effect: a width-only reflow produces no
          // engine update (rows re-wrap at unchanged heights → all
          // measurement entries filter out), yet the controller needs the
          // width sample to open its width-reflow settle window. The
          // first fire is also what starts geometry deliveries.
          contentGeometryTrigger++;
        }
        continue;
      }
      const index = rowIndexes.get(target);
      if (index !== undefined) {
        // Content-box height: trailing row margins stay contained by the
        // [data-row-geometry-content] flow-root contract, so content-box
        // and visual extent agree (settle-flicker analysis).
        const height = entry.contentRect.height;
        if (!engine.isMeasuredAt(index)) {
          // First measurement of this row: how far it landed from its
          // estimate is the warm gate's settle evidence (~0 on a
          // priors-hit revisit, large during a cold estimate cascade).
          // No trigger bump needed — a first measurement always records
          // in the engine (UNMEASURED → size), so the applyMeasurements
          // below bumps geometryVersion in this same flush.
          const correction = Math.abs(height - engine.sizeAt(index));
          if (correction > maxFirstMeasureCorrectionPx) {
            maxFirstMeasureCorrectionPx = correction;
          }
        }
        resizes.push([index, height]);
      }
    }
    if (viewportHeight !== undefined) applyUpdate(engine.applyViewportResize(viewportHeight));
    if (resizes.length) applyUpdate(engine.applyMeasurements(resizes, measureStraddleShift));
  }

  function ensureResizeObserver(): ResizeObserver {
    return (resizeObserver ??= new ResizeObserver(handleResizeEntries));
  }

  // Registration is split from index bookkeeping so a head splice (which
  // re-indexes every mounted row) updates the WeakMap without an
  // unobserve/observe round trip per row — each observe() schedules a
  // fresh RO delivery, so re-registering would cost a spurious
  // O(window) delivery burst on every load-older prepend. Both are
  // stable references by design (see VirtualRow's Props doc).
  function registerRow(element: HTMLElement): () => void {
    ensureResizeObserver().observe(element);
    return () => {
      const index = rowIndexes.get(element);
      if (index !== undefined && rowElementByIndex.get(index) === element) {
        rowElementByIndex.delete(index);
      }
      rowIndexes.delete(element);
      resizeObserver?.unobserve(element);
    };
  }

  function setRowIndex(element: HTMLElement, index: number): void {
    rowIndexes.set(element, index);
    rowElementByIndex.set(index, element);
  }

  // ------------------------------------------------------------------
  // Scroll input + scrollend synthesis
  // ------------------------------------------------------------------
  let scrollEndTimer: ReturnType<typeof setTimeout> | undefined;
  let wheeling = false;
  let touching = false;
  let lastScrollTime = 0;

  // Deadline pattern, not clear+set per event: scroll events arrive once
  // per frame per pane while the spring glides (165/s on a 165Hz panel),
  // and re-arming the timeout on each one churned Blink's timer heap with
  // a clearTimeout+setTimeout pair per event (135 installs + 107 removes
  // per second in a 3-pane storm trace, 2026-08-26). One standing timer
  // checks how long ago the last scroll event landed and re-arms only for
  // the remainder, so a glide costs one timer per debounce window while
  // onscrollend still fires the same ~150ms after the last event.
  function armScrollEnd(): void {
    scrollEndTimer ??= setTimeout(fireScrollEnd, SCROLL_END_DEBOUNCE_MS);
  }

  function fireScrollEnd(): void {
    const remaining = SCROLL_END_DEBOUNCE_MS - (performance.now() - lastScrollTime);
    if (remaining > 0) {
      scrollEndTimer = setTimeout(fireScrollEnd, remaining);
      return;
    }
    if (wheeling || touching) {
      wheeling = false;
      scrollEndTimer = setTimeout(fireScrollEnd, SCROLL_END_DEBOUNCE_MS);
      return;
    }
    scrollEndTimer = undefined;
    onscrollend?.();
  }

  function handleScroll(): void {
    const scroller = observedScroller;
    if (!scroller) return;
    lastScrollTime = performance.now();
    const offset = scroller.scrollTop;
    if (headSpliceAnchor && Math.abs(offset - actualScrollOffset) > 0.01) {
      headSpliceAnchor = null;
    }
    actualScrollOffset = offset;
    applyUpdate(engine.applyScroll(engineOffsetFor(offset)));
    onscroll?.(offset);
    // The viewport top now sits over different content. Re-sample unless
    // the position is unchanged (the controller's own compensation write
    // already re-sampled from the post-flush effect, and its scroll event
    // arrives afterwards with nothing left to move).
    if (offset !== anchorScrollTop) refreshReadingAnchor(offset, true);
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

  // Also handles touchcancel: a system gesture / context menu can end a
  // touch without ever firing touchend, and a stuck `touching` flag would
  // re-arm the scrollend timer forever and never deliver onscrollend.
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
    scroller.addEventListener('touchcancel', handleTouchEnd, { passive: true });
    return () => {
      scroller.removeEventListener('scroll', handleScroll);
      scroller.removeEventListener('wheel', handleWheel);
      scroller.removeEventListener('touchstart', handleTouchStart);
      scroller.removeEventListener('touchend', handleTouchEnd);
      scroller.removeEventListener('touchcancel', handleTouchEnd);
      resizeObserver?.unobserve(scroller);
      observedScroller = undefined;
      clearTimeout(scrollEndTimer);
      scrollEndTimer = undefined;
    };
  });

  onDestroy(() => {
    clearIndexScroll();
    clearTimeout(scrollEndTimer);
    scrollEndTimer = undefined;
    resizeObserver?.disconnect();
    resizeObserver = undefined;
  });

  // ------------------------------------------------------------------
  // Imperative scrolls (handle)
  // ------------------------------------------------------------------
  interface PendingIndexScroll {
    index: number;
    align: ScrollToIndexAlign;
    extraOffset: number;
    lastTarget: number;
    /** Where the last pass's write left the viewport (clamped to the max
     * scroll of that layout), shifted by every compensation delivered
     * since. NaN until the first pass writes. */
    expectedPosition: number;
    /** The destination row's size at the first pass that wrote against a
     * MEASURED destination; NaN until such a pass. Align-end only: it is
     * the baseline that lets convergence exclude the destination's own
     * GROWTH from later targets — see convergeIndexScroll. */
    destinationSizeBaseline: number;
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

  function noteCompensationForIndexScroll(delta: number): void {
    const pending = pendingIndexScroll;
    if (pending && Number.isFinite(pending.expectedPosition)) {
      pending.expectedPosition += delta;
    }
  }

  // Destination rows may be unmeasured; each measurement batch can move
  // the target. Re-write until the target is stable, a settle window
  // passes without movement, or the pass budget runs out.
  //
  // A pass may only continue a journey nobody else has redirected. The
  // pending scroll survives across real time (settle windows, up to 8
  // passes), and an engine update inside that window used to re-fire the
  // stale absolute target over whatever motion had taken over — a spring
  // glide yanked back mid-animation by a late row re-measure was the
  // visible form (auto-collapse release restores armed exactly that).
  // So each pass first checks the live position against where its own
  // last write left it — adjusted for compensations the effect above
  // delivered, which move the position on the navigation's behalf. Moved
  // beyond tolerance means a reader gesture or the controller's follow
  // owns the viewport now: the navigation is stale and dies, it never
  // fights.
  function convergeIndexScroll(): void {
    const pending = pendingIndexScroll;
    if (!pending) return;
    const scroller = observedScroller;
    if (scroller && Number.isFinite(pending.expectedPosition)) {
      // Where should the viewport be if nobody but this navigation (and
      // the compensations delivered on its behalf) has moved it? The
      // expectation, re-clamped against the CURRENT layout: when the
      // content under the navigation shrinks (a run collapsing in view),
      // the browser clamps scrollTop with no write from anyone — that is
      // the navigation's own ground shifting, not a takeover, and the
      // restore pass that follows is exactly the one that puts the
      // anchored row back.
      const maxScroll = Math.max(
        0,
        engine.getTotalSize() + renderHeaderSize - engine.getViewportSize(),
      );
      const expected = Math.min(pending.expectedPosition, maxScroll);
      // Live DOM read, not engine.getScrollOffset(): the engine's offset
      // lags its own write until the scroll event lands, and a stale
      // offset here would read the navigation's own write as a takeover.
      // Cold path — only runs while a navigation is pending.
      if (Math.abs(scroller.scrollTop - expected) > INDEX_SCROLL_TAKEOVER_TOLERANCE_PX) {
        clearIndexScroll();
        return;
      }
    }
    let target =
      engine.targetOffsetFor(pending.index, pending.align, pending.extraOffset) + renderHeaderSize;
    // An align-end target decomposes as offset(index) + size(index) −
    // viewport, so it moves for two reasons with different owners. Rows
    // ABOVE moving (ΔOffset — a fold's RO landing, an estimate
    // correcting) is exactly what the passes exist to hold the anchor
    // across. The destination's OWN size growing past what the first
    // measured write saw (ΔSize > 0 — a streaming tail regrowing under a
    // bottom restore) is live content, and chasing it re-fired the stale
    // navigation as tagged instant writes: the stutter-then-snap when
    // prose resumed inside the settle window of an auto-collapse restore
    // (bug-report-20260731T211929Z). Excluding the growth makes a
    // growth-only pass compute the target it already wrote — the early
    // return below keeps the navigation quiet while the controller's
    // spring glides the growth — without giving up the ΔOffset holds. A
    // destination SHRINK is deliberately not excluded: it is either the
    // measurement correction the passes exist for or the destination
    // itself folding, and both must converge. Estimate-to-measured hops
    // stay live too — the baseline only exists once a write was computed
    // from a measured destination.
    if (pending.align === 'end' && Number.isFinite(pending.destinationSizeBaseline)) {
      const growth = engine.sizeAt(pending.index) - pending.destinationSizeBaseline;
      if (growth > 0) target -= growth;
    }
    if (target === pending.lastTarget) return;
    if (pending.passesLeft <= 0) {
      clearIndexScroll();
      return;
    }
    pending.passesLeft--;
    pending.lastTarget = target;
    if (pending.settleTimer !== undefined) clearTimeout(pending.settleTimer);
    pending.settleTimer = setTimeout(clearIndexScroll, INDEX_SCROLL_SETTLE_MS);
    applyScrollTarget(target);
    if (
      pending.align === 'end' &&
      !Number.isFinite(pending.destinationSizeBaseline) &&
      engine.isMeasuredAt(pending.index)
    ) {
      pending.destinationSizeBaseline = engine.sizeAt(pending.index);
    }
    // The browser clamps a write past the end of the CURRENT layout; the
    // engine's max is that same bound (spacer height IS totalSize, and
    // viewport excludes the padding both sides share).
    pending.expectedPosition = Math.min(
      Math.max(0, target),
      Math.max(0, engine.getTotalSize() + renderHeaderSize - engine.getViewportSize()),
    );
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
      expectedPosition: Number.NaN,
      destinationSizeBaseline: Number.NaN,
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
    // Content-box height, matching the RO path's contentRect unit.
    // clientHeight is the PADDING box, and chat's scroller carries
    // composer-clearance padding-bottom — feeding it here would inflate
    // the engine's viewport by the padding and land align-end targets
    // (scroll-to-bottom after a pane reorder) short by that amount.
    const style = getComputedStyle(scroller);
    // `|| 0` guards happy-dom's empty computed strings (NaN paddings
    // would poison the engine's viewport).
    const paddingTop = Number.parseFloat(style.paddingTop) || 0;
    const paddingBottom = Number.parseFloat(style.paddingBottom) || 0;
    applyUpdate(engine.applyViewportResize(scroller.clientHeight - paddingTop - paddingBottom));
    actualScrollOffset = scroller.scrollTop;
    applyUpdate(engine.applyScroll(engineOffsetFor(scroller.scrollTop)));
  }

  // ------------------------------------------------------------------
  // Read-only handle (see TimelineVirtualizerHandle in utils/virtual/types)
  // ------------------------------------------------------------------
  export function getScrollOffset(): number {
    return actualScrollOffset;
  }
  export function noteScrollTopWritten(top: number): void {
    actualScrollOffset = top;
    engine.noteScrollOffset(engineOffsetFor(top));
  }
  export function getViewportSize(): number {
    return engine.getViewportSize();
  }
  export function getScrollSize(): number {
    return Math.max(engine.getTotalSize() + renderHeaderSize, engine.getViewportSize());
  }
  export function getTotalSize(): number {
    return engine.getTotalSize() + renderHeaderSize;
  }
  export function findItemIndex(offset: number): number {
    return engine.findItemIndex(engineOffsetFor(offset));
  }
  export function getItemOffset(index: number): number {
    return engine.getItemOffset(index) + renderHeaderSize;
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
  style:height="{renderTotalSize + renderHeaderSize}px"
>
  {#if renderHeader}
    <div
      data-virtual-header
      style:position="absolute"
      style:left="0px"
      style:top="0px"
      style:width="100%"
      style:height="{renderHeaderSize}px"
    >
      {@render renderHeader()}
    </div>
  {/if}
  <div
    bind:this={renderPlane}
    data-virtual-row-plane
    style:position="absolute"
    style:left="0px"
    style:top="{plane.origin + renderHeaderSize}px"
    style:width="100%"
    style:height="{plane.size}px"
  >
    {#each rows as row (row.key)}
      <VirtualRow
        item={row.item}
        index={row.index}
        offset={row.offset}
        measured={row.measured}
        observe={row.observe}
        register={registerRow}
        {setRowIndex}
        {children}
      />
    {/each}
  </div>
</div>
