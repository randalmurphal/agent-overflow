<script lang="ts">
  import { onDestroy, tick } from 'svelte';
  import { Virtualizer, type VirtualizerHandle } from 'virtua/svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { createUseStickToBottomController } from '../../utils/useStickToBottom.svelte';
  import {
    getThreadScrollSnapshot,
    setThreadScrollSnapshot,
    type ScrollSnapshot,
  } from '../../utils/threadScrollSnapshots';
  import {
    groupItemsBySubagent,
    timelineNodeKey,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { timelineRowDecorations } from './timelineRows';
  import { codexSubagentReceiverLabels } from '../../utils/subagentLaunch';
  import { PROVIDER_DEFINITIONS } from '../../providers/catalog';
  import { filterRedundantNotifications } from '../../utils/notificationFilter';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import InlineSubagentGroup from './InlineSubagentGroup.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import WaitGroup from './WaitGroup.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { recordTimelineRenderTrace, startTimelineRowResizeTrace } from './messageTimelineTrace';
  import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';
  import type { UserMessageActions } from './userMessageActions';
  import {
    captureTimelineAnchor,
    centeredScrollTop,
    createAutoLoadOlderGate,
    resolveVisibleTimelineNodeIndex,
    timelineRowElementForIndex,
  } from './timelineScroll';
  import { createTimelineTargetFlash } from './timelineTargetFlash.svelte';

  // Initial item-size estimate for virtua. Real sizes come from the
  // per-item ResizeObserver virtua wraps each row in; this constant only
  // matters for the first render before measurements stabilise.
  const ESTIMATED_ROW_SIZE = 90;
  // Extra buffer rendered above + below the viewport. Sized for two viewports
  // worth of rows on each side so fast scrolls (trackpad fling, scrollbar
  // drag) don't outrun the rendered window — that was the source of the
  // "text disappears under the composer then reappears" flicker. Each
  // ~90px row × 1800px = ~20 extra rows per side. Trade ~4MB of mounted
  // DOM/component state for the smoother scroll. Revisit only if it's
  // ever measured to hurt mount-time on first-open.
  const BUFFER_SIZE_PX = 1800;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 24;
  const TARGET_FLASH_MS = 900;
  // Auto-load-older trigger thresholds. When the user scrolls within
  // AUTO_LOAD_OFFSET_PX of the top AND the topmost rendered row is one
  // of the first AUTO_LOAD_INDEX_THRESHOLD items, fire `pane.loadOlder()`
  // so the next batch slots in before the user runs out of buffer. The
  // index gate is what keeps an idle small-thread render from auto-
  // loading just because the whole thing fits in viewport.
  const AUTO_LOAD_OFFSET_PX = 800;
  const AUTO_LOAD_INDEX_THRESHOLD = 5;
  // happy-dom returns 0 for clientHeight/clientWidth, which makes virtua
  // mount zero rows. In test runs we ask virtua to mount everything via
  // ssrCount so test assertions can find the rendered DOM. Production
  // (vite dev/build) always sees the default `undefined`, leaving virtua
  // free to virtualize.
  const IS_TEST = import.meta.env.MODE === 'test';

  let {
    pane,
    onImageExpand,
    userMessageActions,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
  } = $props();

  // Inner scroll container. We own scrolling here; <Virtualizer> renders
  // its measured rows inside `contentEl` and reads/writes via scrollRef.
  // The element is wrapped in a non-scrolling `relative flex h-full
  // flex-col` shell that anchors the floating <ScrollToBottomButton>
  // outside the scroll content (see template comment for why).
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Wrapper around <Virtualizer>. The controller's content RO observes
  // this element so any size change (row growth, expand toggle, virtua's
  // totalSize bump) fires synchronously before the same paint that
  // displays the change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into virtua. Set once Virtualizer mounts.
  let listRef: VirtualizerHandle | undefined = $state(undefined);

  let restoredThreadId: string | null = $state(null);
  // Tracks which thread we last persisted into the snapshot store via
  // the thread-switch effect — separate from `restoredThreadId` so a
  // thread switch can dispose the previous snapshot before the next
  // restore completes.
  let scrollSnapshotThreadId: string | null = $state(null);
  // Token bumped on every external "interrupt" — thread switch, user
  // scroll, programmatic scrollToItem — so async restore work can detect
  // staleness and bail.
  let restoreToken = 0;
  const autoLoadOlderGate = createAutoLoadOlderGate({
    offsetThreshold: AUTO_LOAD_OFFSET_PX,
    indexThreshold: AUTO_LOAD_INDEX_THRESHOLD,
  });
  const targetFlash = createTimelineTargetFlash(TARGET_FLASH_MS);

  let groupedNodes = $derived<TimelineNode[]>(
    groupItemsBySubagent(filterRedundantNotifications(pane.items)),
  );
  let codexReceiverLabels = $derived(
    pane.thread?.provider === PROVIDER_DEFINITIONS.codex.id
      ? codexSubagentReceiverLabels(pane.items)
      : new Map<string, string>(),
  );

  let rowDecorations = $derived(
    timelineRowDecorations(groupedNodes, getActiveTurn(pane.threadId)?.turnIndex ?? null),
  );

  // Animation mode is keyed on whether the thread has an in-flight
  // turn. Streaming chunks come in fast enough that the contentRO
  // sync-pin would land them invisibly; the spring chase gives the
  // user the familiar "viewport follows the text as it streams in" UX.
  // When the turn settles, mode flips back to 'instant' — late
  // Streamdown typesetting on completed content doesn't move the
  // viewport. The controller's warm gate (quiescence-based, with a
  // 2.5s failsafe) defends against the e00723f regression where
  // mount-time virtua remeasurement + Streamdown typesetting would
  // spring-chase a thread restore visibly.
  const stick = createUseStickToBottomController({
    animationMode: () => (getActiveTurn(pane.threadId) ? 'spring' : 'instant'),
  });

  // Hide contentEl during the initial measurement cascade on UNCACHED
  // loads. Without `pane.cachedVirtuaCache`, virtua starts with
  // `ESTIMATED_ROW_SIZE × N` for totalSize; per-row ResizeObservers
  // then correct each row's actual height across the next ~25ms, which
  // shifts every row's Y-offset and clamps scrollTop by a fraction of
  // a page (216-item thread sample: 461px clamp). The controller's
  // sync-pin re-pins scrollTop correctly, but the user still sees the
  // row-content shift between the two paints. Hiding contentEl until
  // the controller's warmup gate fires (QUIET_MS=100ms of contentRO
  // silence, or FAILSAFE_MS=2500ms ceiling — whichever first) covers
  // the cascade with a brief blank-then-correct reveal instead of a
  // visible "land wrong, jump to correct" sequence.
  //
  // Cached loads skip this — virtua has correct totalSize from frame 0
  // and there's no measurement cascade to hide. The scroll controller
  // and composer overlay stay visible in both branches; only the
  // virtualizer's contentEl is hidden.
  let hideContentForWarmup = $derived(
    !stick.isWarm && pane.cachedVirtuaCache === undefined,
  );

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the pane reference changes (rare — ChatView re-keys on thread
  // switch, which remounts the timeline) and on component teardown, so
  // a stale pointer to a torn-down controller can't leak.
  $effect(() => {
    pane.attachScrollController(stick);
    return () => pane.detachScrollController(stick);
  });

  // Bind the controller to the actual DOM elements. The content RO and
  // wheel/scroll/keydown/touch listeners all start here. Re-runs if
  // either ref changes (thread switch / HMR).
  $effect(() => {
    if (!scrollEl || !contentEl) return;
    stick.attach(scrollEl, contentEl);
  });

  // Publish a virtua-cache getter so `pane.snapshotOutgoingPane` can
  // capture the OUTGOING thread's per-row sizes into the LRU before
  // {#key pane.threadId} unmounts the <Virtualizer>. The next switch
  // back replays the cache via `cache={pane.cachedVirtuaCache}` below,
  // so virtua starts with the correct totalSize at first paint instead
  // of the ESTIMATED_ROW_SIZE × N underestimate that lazy mount-time
  // measurement would otherwise produce.
  $effect(() => {
    const getter = () => listRef?.getCache();
    pane.attachVirtuaCacheGetter(getter);
    return () => pane.detachVirtuaCacheGetter(getter);
  });

  // Diagnostic UI render trace — extracted to messageTimelineTrace.ts.
  // Production builds short-circuit at isUiRenderTraceEnabled() inside
  // the helper, so this $effect's only steady-state cost is the reactive
  // dep tracking.
  $effect(() => {
    pane.threadId;
    pane.items.length;
    pane.timelineRevision;
    groupedNodes.length;
    recordTimelineRenderTrace(pane, groupedNodes, scrollEl, listRef);
  });

  // Trace virtua remount transitions: listRef goes undefined → defined
  // when the {#key pane.threadId} block remounts the Virtualizer. This
  // is the seam where virtua's deferred scroller attach
  // (`tick().then(observe)` in Virtualizer.svelte) fires; a stale
  // scrollTop from the outgoing thread can be visible here.
  $effect(() => {
    if (!isUiRenderTraceEnabled()) return;
    const bound = listRef !== undefined;
    recordUiTrace('timeline.listRef.bind', {
      bound,
      threadId: pane.threadId,
      restoredThreadId,
      scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
      scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      groupedNodesLength: groupedNodes.length,
    });
  });

  // Per-row resize tracker. Diagnostic-only — gated on the trace flag, so
  // production builds skip the observer wiring entirely. Observes every
  // [data-row-index] wrapper virtua mounts and records each height delta
  // alongside a small tag fingerprint of suspect descendants (shiki,
  // skeleton, mermaid, katex, sd-code, img, approval/todo/working). This
  // is the surface that names which row(s) and which child element class
  // is responsible for an unexpected ±N px oscillation on thread re-entry.
  $effect(() => {
    if (!isUiRenderTraceEnabled() || !contentEl) return;
    return startTimelineRowResizeTrace(contentEl);
  });

  // ============================================================
  // Helpers
  // ============================================================
  // Pure helpers live alongside the TimelineNode type in
  // subagentGrouping.ts; the local thin wrappers below adapt them to the
  // current groupedNodes array so the template doesn't have to thread
  // `groupedNodes` into every call site.

  function findTimelineNodeIndex(itemId: string): number {
    return resolveVisibleTimelineNodeIndex(groupedNodes, pane.items, itemId);
  }

  function rowElementForIndex(index: number): HTMLElement | null {
    return timelineRowElementForIndex(contentEl, index);
  }

  function centeredScrollTopForIndex(index: number): number {
    if (!scrollEl || !listRef) return 0;
    const rowTop = listRef.getItemOffset(index);
    const rowHeight = rowElementForIndex(index)?.getBoundingClientRect().height ?? ESTIMATED_ROW_SIZE;
    return centeredScrollTop(rowTop, rowHeight, scrollEl.clientHeight);
  }

  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleVirtuaScroll(offset: number): void {
    saveScrollSnapshot();
    maybeAutoLoadOlder(offset);
  }

  function handleVirtuaScrollEnd(): void {
    saveScrollSnapshot();
  }

  // Auto-load-older trigger. Fires `pane.loadOlder()` when the user is
  // reading near the top of the loaded window, so older items page in
  // before they hit a wall. The "Load older messages" button at the
  // top of the timeline is the explicit fallback when auto-load is
  // bypassed (no progress, fast-skip past the threshold, etc.).
  function maybeAutoLoadOlder(offset: number): void {
    if (!listRef) return;
    if (!autoLoadOlderGate.shouldLoad({
      offset,
      hasMoreHistory: pane.hasMoreHistory,
      loadingOlder: pane.loadingOlder,
      oldestLoadedTurnIndex: pane.oldestLoadedTurnIndex,
      restoredThreadId,
      threadId: pane.threadId,
      findFirstVisibleIndex: (top) => listRef!.findItemIndex(top),
    })) return;
    void handleLoadOlder();
  }

  // ============================================================
  // Snapshot save/restore (per-thread)
  // ============================================================

  function snapshotThreadId(): string | null {
    return pane.threadId || null;
  }

  function saveScrollSnapshot(): void {
    const threadId = snapshotThreadId();
    if (!threadId) return;
    saveScrollSnapshotForThread(threadId);
  }

  function saveScrollSnapshotForThread(threadId: string): void {
    // The `restoredThreadId !== threadId` guard already covers the
    // "ignore scroll events fired before restoration" case — once
    // restoration runs (which happens as soon as the timeline has
    // items, including the cache-hit fast path), saves are allowed.
    // No separate loading check is needed.
    if (!listRef || restoredThreadId !== threadId) return;
    // virtua's internal ref can be in a teardown state where any geometry
    // read throws (the inner ref is null while our outer handle is still
    // bound). The TypeError is the documented teardown shape — swallow
    // exactly that and re-throw anything else so a real regression in a
    // future virtua version doesn't disappear silently.
    try {
      if (stick.isAtBottom) {
        setThreadScrollSnapshot(threadId, { kind: 'bottom' });
        return;
      }
      const offset = listRef.getScrollOffset();
      // Negative when the anchor row's top has scrolled above the viewport
      // top by `-offsetTop` pixels. Restoration recreates exactly this
      // relationship via scrollToIndex({ align:'start', offset: -offsetTop }).
      const anchor = captureTimelineAnchor(groupedNodes, listRef, offset, { clampIndex: true });
      if (!anchor) return;
      setThreadScrollSnapshot(threadId, { kind: 'anchor', ...anchor });
    } catch (err) {
      if (err instanceof TypeError) {
        // Expected during teardown when virtua's inner ref is nulled
        // while our outer handle is still bound. Skip this save; the
        // next scroll will refresh the snapshot.
        return;
      }
      throw err;
    }
  }

  // Reset restoration tracking on thread change BEFORE the new thread's
  // effects run, AND suspend auto-follow until restoreToBottom (or
  // restoreAnchor) takes over. Setting escapedFromLockState=true synchronously here
  // freezes the controller until the new thread's restoration runs.
  //
  // We do NOT call saveScrollSnapshotForThread for the outgoing thread
  // here. By the time this effect runs, switchThread has already mutated
  // pane.items to the incoming thread's cached items — so virtua's
  // listRef.findItemIndex would return an index in the WRONG thread's
  // array, and the saved anchor would carry the incoming thread's item
  // id under the outgoing thread's snapshot key. The continuous
  // scroll-event-driven saves (handleVirtuaScroll, handleVirtuaScrollEnd)
  // already keep the outgoing thread's snapshot fresh — the most recent
  // user scroll IS the snapshot.
  $effect.pre(() => {
    const nextThreadId = pane.threadId;
    if (scrollSnapshotThreadId !== nextThreadId) {
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.effectPre', {
          oldThreadId: scrollSnapshotThreadId,
          newThreadId: nextThreadId,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
          paneItems: pane.items.length,
          paneLoading: pane.loading,
          hasCachedVirtuaCache: pane.cachedVirtuaCache !== undefined,
        });
      }
      if (scrollSnapshotThreadId) {
        restoredThreadId = null;
        restoreToken += 1;
      }
      autoLoadOlderGate.reset();
      stick.setEscapedFromLock(true);
      // Re-arm the warm-up gate BEFORE the DOM update flushes. The
      // restore $effect calls forceStick() (which also arms the gate),
      // but that runs AFTER DOM update — so without this $effect.pre
      // reset, the first paint of the new thread would inherit the
      // outgoing thread's settled `isWarm=true`, making
      // hideContentForWarmup=false during the new thread's measurement
      // cascade. attach() can't carry this load: scrollEl/contentEl
      // don't change across switches (MessageTimeline isn't keyed on
      // threadId), so the attach $effect early-returns. This was the
      // flaky-fix bug: cache-miss switches off a long-settled prior
      // thread reproduced the visible "lands wrong, jumps to correct"
      // sequence; cache-miss switches off an unsettled prior thread
      // (warm=false coincidentally) hid the cascade and looked fine.
      stick.armWarmup();
    }
    scrollSnapshotThreadId = nextThreadId;
  });

  $effect(() => {
    const threadId = pane.threadId;
    const itemsLength = pane.items.length;
    const loading = pane.loading;
    if (!threadId) return;
    if (restoredThreadId === threadId) return;
    // Restore as soon as we have items to anchor against — that's the
    // cache-hit fast path. For the cache-miss case where the thread
    // turned out to be genuinely empty, fall through when loading
    // flips false so the bottom-snapshot branch can still call
    // markAtBottom for streaming arrival.
    if (itemsLength === 0 && loading) return;
    const hasTimelineRows = groupedNodes.length > 0;
    if (hasTimelineRows && !listRef) return;
    restoredThreadId = threadId;
    // Branch synchronously on snapshot kind. The bottom branch only
    // needs scrollEl (forceStick) — running it inline keeps the
    // controller's pauseAutoScroll lease and the `await tick()`
    // microtask boundary out of the critical "switch in → land at
    // bottom" path. Awaiting tick gates the contentRO sync-pin off
    // (pauseDepth>0) for an extra microtask while virtua's deferred
    // scroller attach (Virtualizer.svelte's `tick().then(observe)`)
    // races to read scrollEl.scrollTop — the visible flash on long
    // threads was virtua reading the carry-over scrollTop from the
    // outgoing thread before our forceStick landed.
    const snap = getThreadScrollSnapshot(threadId);
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.effect', {
        threadId,
        snapKind: snap?.kind ?? null,
        snapItemId: snap?.kind === 'anchor' ? snap.itemId : null,
        snapOffsetTop: snap?.kind === 'anchor' ? snap.offsetTop : null,
        itemsLength,
        loading,
        groupedNodesLength: groupedNodes.length,
        hasListRef: listRef !== undefined,
        hasScrollEl: scrollEl !== undefined,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      });
    }
    if (!snap || snap.kind === 'bottom') {
      restoreToBottom();
      return;
    }
    void restoreAnchor(threadId, snap, ++restoreToken);
  });

  // Bottom restore. Two cases:
  //
  // - Empty timeline (no rows yet): just flip the controller's intent
  //   flag so the first streamed row's contentRO sync-pin lands at the
  //   bottom. There's no scrollTop to write yet.
  //
  // - Non-empty timeline: forceStick() lands scrollTop at the current
  //   target in a single write. The per-thread virtua row-size cache
  //   (replayed via `<Virtualizer cache={pane.cachedVirtuaCache}>` in
  //   the {#key pane.threadId} wrapper) gives virtua the correct
  //   totalSize from frame 0, so the target is right from the first
  //   paint. Any subsequent contentEl growth from svelte-streamdown's
  //   async typesetting (shiki / KaTeX / mermaid /
  //   parseIncompleteMarkdown rebalance) and from virtua's per-row
  //   ResizeObservers refining row heights gets handled invisibly by
  //   the controller's contentRO sync-pin path: each positive delta
  //   re-pins to the new bottom inside the RO callback, before paint.
  //
  // Don't pair `scrollToIndex(last, 'end')` with `markAtBottom()` here
  // — they create two writers (virtua's measurement loop + our
  // sync-pin) targeting slightly different scrollTop values for the
  // same content-grow trigger, and they oscillate. forceStick() alone
  // is the single writer.
  //
  // The trailing rAF `notifyContentMaybeGrew()` is a defensive late-
  // settling re-pin: composer-height RO updates flowing into scrollEl's
  // padding-bottom, virtua's per-row ResizeObservers firing a frame
  // after mount, and the first burst of Streamdown async typesetting
  // can all change geometry one frame after the initial forceStick.
  // Padding-only changes don't re-fire contentRO (W3C ResizeObserver
  // observes content-box) so a paint-time settle that nudges the bottom
  // by a few px would otherwise leave the user "half a tick" above
  // bottom. notifyContentMaybeGrew is escape-aware (bails if the user
  // gestured up between frames) so it can't yank them.
  function restoreToBottom(): void {
    const lastIndex = groupedNodes.length - 1;
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.bottom.entry', {
        threadId: restoredThreadId,
        lastIndex,
        groupedNodesLength: groupedNodes.length,
        hasListRef: listRef !== undefined,
        hasScrollEl: scrollEl !== undefined,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      });
    }
    if (lastIndex < 0) {
      stick.markAtBottom();
      const threadId = snapshotThreadId();
      if (threadId) setThreadScrollSnapshot(threadId, { kind: 'bottom' });
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.bottom.exit', {
          threadId: restoredThreadId,
          branch: 'empty',
        });
      }
      return;
    }
    stick.forceStick();
    saveScrollSnapshot();
    // Capture the thread the rAF was scheduled for so a thread switch
    // between forceStick and the next frame doesn't run the late re-pin
    // against the new thread's geometry. notifyContentMaybeGrew also
    // bails on escape/pause as a second-line defense.
    const expectedThreadId = restoredThreadId;
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.bottom.exit', {
        threadId: restoredThreadId,
        branch: 'forceStick',
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
      });
    }
    requestAnimationFrame(() => {
      const stillSameThread = restoredThreadId === expectedThreadId;
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.bottom.raf', {
          threadId: restoredThreadId,
          expectedThreadId,
          stillSameThread,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        });
      }
      if (!stillSameThread) return;
      stick.notifyContentMaybeGrew();
    });
  }

  async function restoreAnchor(
    threadId: string,
    snap: Extract<ScrollSnapshot, { kind: 'anchor' }>,
    token: number,
  ): Promise<void> {
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.anchor.entry', {
        threadId,
        token,
        itemId: snap.itemId,
        offsetTop: snap.offsetTop,
      });
    }
    const release = stick.pauseAutoScroll();
    try {
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('timeline.restore.anchor.bail', {
            threadId,
            token,
            currentRestoreToken: restoreToken,
            currentPaneThreadId: pane.threadId,
            stage: 'after-tick',
          });
        }
        return;
      }
      if (groupedNodes.length > 0 && !listRef) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('timeline.restore.anchor.bail', {
            threadId,
            token,
            stage: 'no-listref',
            groupedNodesLength: groupedNodes.length,
          });
        }
        return;
      }

      const found = await pane.loadUntilItem(snap.itemId);
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.anchor.loaded', {
          threadId,
          token,
          found,
          itemId: snap.itemId,
        });
      }
      if (token !== restoreToken || pane.threadId !== threadId) return;
      if (!found) {
        restoreToBottom();
        return;
      }
      await tick();
      if (token !== restoreToken || pane.threadId !== threadId || !listRef) return;
      const idx = findTimelineNodeIndex(snap.itemId);
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.anchor.scrollToIndex', {
          threadId,
          token,
          idx,
          offsetTop: snap.offsetTop,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        });
      }
      if (idx < 0) {
        restoreToBottom();
        return;
      }
      stick.stopScroll();
      stick.setEscapedFromLock(true);
      listRef.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Load older
  // ============================================================
  // Capture the user's reading position BEFORE the await, then explicitly
  // re-anchor after the items prepend. Avoids virtua's `shift` mode (which
  // is documented as fragile across async boundaries) — explicit
  // scrollToIndex is deterministic.
  async function handleLoadOlder(): Promise<void> {
    if (!listRef) return;
    const offsetBefore = listRef.getScrollOffset();
    const anchor = captureTimelineAnchor(groupedNodes, listRef, offsetBefore);
    const anchorId = anchor?.itemId ?? null;
    const anchorOffsetTop = anchor?.offsetTop ?? 0;

    const release = stick.pauseAutoScroll();
    // The user is reading older — must not auto-restick from the
    // post-prepend scrollHeight jump. Without this, the controller's
    // content RO would observe the positive delta and sync-pin the
    // user to the new bottom.
    stick.setEscapedFromLock(true);
    const myToken = ++restoreToken;
    try {
      const result = await pane.loadOlder();
      await tick();
      if (myToken !== restoreToken || !listRef || result.status !== 'loaded') return;
      if (!anchorId) return;
      const newIdx = findTimelineNodeIndex(anchorId);
      if (newIdx < 0) return;
      stick.stopScroll();
      listRef.scrollToIndex(newIdx, { align: 'start', offset: -anchorOffsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Scroll-to-item (search hits, plan rows, tray rows)
  // ============================================================

  async function scrollToItem(
    id: string,
    options: { behavior: 'instant' | 'animated'; flash: boolean },
  ): Promise<void> {
    if (!listRef || !id) return;
    const myToken = ++restoreToken;
    const found = await pane.loadUntilItem(id);
    if (myToken !== restoreToken || !listRef) return;
    if (!found) {
      addToast('warning', 'Message is no longer in this thread');
      return;
    }
    await tick();
    if (myToken !== restoreToken || !listRef) return;
    const idx = findTimelineNodeIndex(id);
    if (idx < 0) return;
    const targetNode = groupedNodes[idx];
    const targetItemId = targetNode?.kind === 'leaf' ? targetNode.item.id : id;
    // Programmatic jump elsewhere — cancel any in-flight
    // animateScrollTo + escape so the new position holds. `smooth:
    // true` would route through the browser's native smooth scroll
    // (scrollEl.scrollTo({behavior:'smooth'})), which fires its own
    // scroll events asynchronously and races the controller — drop it.
    if (options.behavior === 'animated' && scrollEl) {
      const targetTop = centeredScrollTopForIndex(idx);
      const result = await stick.animateScrollTo(targetTop);
      await tick();
      if (myToken !== restoreToken || result !== 'completed') return;
    } else {
      stick.stopScroll();
      stick.setEscapedFromLock(true);
      listRef.scrollToIndex(idx, { align: 'center' });
    }
    if (options.flash) targetFlash.flash(targetItemId);
  }

  let lastHandledScrollNonce = 0;
  $effect(() => {
    const req = pane.scrollToItemRequest;
    if (req.nonce === 0) return;
    if (req.nonce === lastHandledScrollNonce) return;
    lastHandledScrollNonce = req.nonce;
    void scrollToItem(req.itemId, {
      behavior: req.behavior,
      flash: req.flash,
    });
  });

  onDestroy(() => {
    if (restoredThreadId) saveScrollSnapshotForThread(restoredThreadId);
    targetFlash.clear();
    stick.detach();
  });
</script>

<!-- Outer wrapper does NOT scroll; it provides the relative containing
     block for the floating scroll-to-bottom chip. The chip MUST sit
     outside the scroll container: `position:absolute; bottom:X` inside
     an `overflow:auto` parent renders in scroll-content space, not
     viewport space, so the chip would otherwise scroll with the
     transcript and ride up off-screen as the user scrolls or as
     auto-follow drives scrollTop downward (browser-confirmed). The
     inner div is the actual scroll container that <Virtualizer
     scrollRef={scrollEl}> reads/writes via, that the controller's
     wheel/scroll/keydown/touch listeners and content ResizeObserver
     bind to. `overflow-anchor: none` disables the browser's
     scroll-anchor adjustment — virtua already owns row-anchor
     preservation via its measurement loop, and the controller owns
     bottom-pinning via the contentRO sync-pin; leaving the browser's
     anchor heuristic ON makes it fight both, producing visible
     scrollTop oscillation as Streamdown's async typesetting (shiki /
     KaTeX / mermaid) grows rows above the viewport. The padding-bottom
     (= composer height + visual breathing room) keeps the last message
     clear of the absolute composer overlay without putting a synthetic
     spacer row inside the virtualized data; it lives on scrollEl
     because contentEl's contentRO observes content-box and
     padding-only changes neither fire the callback (W3C ResizeObserver
     spec — default observation is content-box) nor change
     `entry.contentRect.height`, so a contentEl padding wouldn't
     re-pin via the contentRO seam. ChatView's composer-overlay RO
     calls `notifyContentMaybeGrew()` to handle that case explicitly.
     Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="flex-1 min-h-0 overflow-y-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/25"
    style:overscroll-behavior-y="contain"
    style:overflow-anchor="none"
    style:padding-bottom={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}
    tabindex="-1"
    data-testid="message-timeline-scroll"
    role="log"
    aria-label="Message History"
  >
    {#if pane.showLoadingSpinner}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm" role="status" aria-live="polite">
        <span class="animate-pulse">Loading thread...</span>
      </div>
    {:else if pane.items.length === 0 && !getActiveTurn(pane.threadId) && !pane.loading}
      <div class="flex items-center justify-center h-full text-fg-subtle text-sm">
        No messages yet. Send a message to get started.
      </div>
    {:else if pane.items.length === 0 && getActiveTurn(pane.threadId)}
      <!-- Active turn but no items yet. The working/todo UI lives in the
           ChatView bottom overlay, outside the virtualized history. -->
      <div class="mx-auto w-full max-w-[62rem] px-6 pt-8"></div>
    {:else}
      {#snippet renderNode(node: TimelineNode, depth: number)}
        {#if node.kind === 'leaf'}
          <TimelineLeaf
            {pane}
            item={node.item}
            orphan={node.orphan === true}
            {onImageExpand}
            {userMessageActions}
            codexSubagentReceiverLabels={codexReceiverLabels}
            targetFlash={targetFlash.itemId === node.item.id}
            targetFlashNonce={targetFlash.itemId === node.item.id ? targetFlash.nonce : 0}
          />
        {:else if node.kind === 'group'}
          <SubagentGroup {pane} group={node} {depth} {renderNode} />
        {:else if node.kind === 'wait_group'}
          <WaitGroup {pane} group={node} {onImageExpand} {userMessageActions} />
        {:else}
          <InlineSubagentGroup group={node} {depth} {renderNode} />
        {/if}
      {/snippet}

      <!-- contentEl is the controller's content-RO observation target.
           Virtua's container has `contain: size; height: totalSize+'px'`,
           so contentEl.scrollHeight reflects virtua's totalSize exactly.
           {#key pane.threadId} forces the <Virtualizer> to remount on
           every thread switch so it can re-read the `cache` prop. The
           prop is consumed only at `createVirtualStore(...)` mount time
           (see virtua/svelte/Virtualizer.svelte) — without the {#key},
           a thread switch back would still render the old cache and
           virtua would underestimate `totalSize` until per-row
           ResizeObservers re-measured every visible row. -->
      <div
        bind:this={contentEl}
        style:visibility={hideContentForWarmup ? 'hidden' : 'visible'}
      >
        {#key pane.threadId}
        <Virtualizer
          bind:this={listRef}
          scrollRef={scrollEl}
          data={groupedNodes}
          getKey={(node) => timelineNodeKey(node)}
          itemSize={ESTIMATED_ROW_SIZE}
          bufferSize={BUFFER_SIZE_PX}
          ssrCount={IS_TEST ? 100_000 : undefined}
          cache={pane.cachedVirtuaCache}
          onscroll={handleVirtuaScroll}
          onscrollend={handleVirtuaScrollEnd}
        >
          {#snippet children(node: TimelineNode, index: number)}
            <!-- Outer per-row wrapper. We do NOT set data-item-id here:
                 only TimelineLeaf owns that attribute on its root. Structural
                 rows stay unanchored, and tests rely on the divider rendering
                 BEFORE the [data-item-id] node, not containing it. -->
            <div data-row-index={index} class:mt-4={rowDecorations.toolTextBoundaryIndexes.has(index)}>
              {#if index === 0}
                <!-- Top of timeline. Load-older button (when applicable) and
                     a small top breathing-room spacer ride inside the first
                     row. When user scrolls to the very top, the button is
                     the first thing they see. After load-older completes,
                     the explicit scrollToIndex re-anchors them to where they
                     were reading — the button moves up out of view. -->
                <div class="pt-6 mx-auto w-full max-w-[62rem] px-6">
                  {#if pane.hasMoreHistory}
                    <div class="mb-3 flex justify-center">
                      <Button
                        variant="secondary"
                        size="xs"
                        onclick={handleLoadOlder}
                        loading={pane.loadingOlder}
                        testId="load-older-messages"
                      >
                        {#snippet children()}
                          {pane.loadingOlder ? 'Loading…' : 'Load older messages'}
                        {/snippet}
                      </Button>
                    </div>
                  {/if}
                </div>
              {/if}

              <div class="mx-auto w-full max-w-[62rem] px-6">
                {#if rowDecorations.responseDividerIndexes.has(index)}
                  {@const showResponsePill = rowDecorations.responsePillIndexes.has(index)}
                  <!-- Two visual modes share a fixed wrapper height
                       (`h-[1.625rem]` = 26px = pill chrome: text-[10px]
                       × leading-tight ≈ 12px + py-1 8px + 2× 1px border).
                       Labeled mode renders `line | gap | pill | gap | line`,
                       unlabeled mode renders one continuous full-width line.
                       The pill's `leading-tight` keeps its content inside
                       the fixed wrapper across font-loading variance.
                       Fixed `h-` (not the codebase's usual `min-h-` slot
                       convention) is deliberate: both branches MUST be the
                       exact same height so promoting an intermediate
                       divider to "final" on turn settle never shifts row
                       geometry — satisfies the "no late transcript
                       adornments on completion" rule in
                       `frontend/CLAUDE.md`. Re-derive 1.625rem if the pill
                       classes above change. -->
                  <div data-testid="response-divider" data-final-response={showResponsePill ? 'true' : 'false'}>
                    <div class="my-3 flex h-[1.625rem] items-center gap-3">
                      <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
                      {#if showResponsePill}
                        <span
                          class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[10px] uppercase leading-tight tracking-[0.14em] text-text-secondary"
                        >
                          Response
                        </span>
                        <span class="h-px flex-1 bg-border" aria-hidden="true"></span>
                      {/if}
                    </div>
                  </div>
                {/if}
                <div data-testid="message-timeline-node">
                  {@render renderNode(node, 1)}
                </div>
              </div>
            </div>
          {/snippet}
        </Virtualizer>
        {/key}
      </div>
    {/if}
  </div>

  <!-- Visible when NOT at bottom (intent-or-geometry); the chip is the
       user's escape hatch when they've drifted away. Wiring this to
       `!isSticky` would also pop the chip during sidebar/drawer resize
       leases (pauseDepth > 0) even though the user is geometrically
       glued to the bottom — clearly not desired. Anchored to the outer
       wrapper (which does not scroll), so the chip stays fixed in the
       visible area regardless of transcript scrollTop. -->
  <ScrollToBottomButton visible={!stick.isAtBottom} onClick={() => stick.forceStick()} />
</div>
