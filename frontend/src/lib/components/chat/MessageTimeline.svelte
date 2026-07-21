<script lang="ts">
  import { onDestroy, setContext } from 'svelte';
  import type {
    PaneScrollController,
    ThreadPane,
  } from '../../stores/thread.svelte';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { latchedSpringMode, SPRING_MODE_HOLD_MS } from '../../utils/springAnimationLatch';
  import {
    CHAT_MARKDOWN_PRESENCE_CONTEXT,
    CHAT_MARKDOWN_SETTLED_CONTEXT,
  } from './markdownSettledContext';
  import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
  import TimelineVirtualizer from '../virtual/TimelineVirtualizer.svelte';
  import { timelineNodeKey, type TimelineNode } from '../../utils/subagentGrouping';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import ReadGroupRow from './ReadGroupRow.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import WaitGroup from './WaitGroup.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import type { UserMessageActions } from './userMessageActions';
  import { resolveVisibleTimelineNodeIndex } from './timelineScroll';
  import { createTimelineTargetFlash } from './timelineTargetFlash.svelte';
  import { observeScrollSurfaceContentWidth } from './scrollSurfaceWidth';
  import { createTimelineRestore } from './timelineRestore.svelte';
  import { createTimelineSizePriors } from './timelineSizePriors.svelte';
  import { createTimelinePaging } from './timelinePaging';
  import { createTimelineWindowAnchor } from './timelineWindowAnchor.svelte';
  import { createTimelineRowProjection } from './timelineRowProjection.svelte';
  import { createTimelineDiagnostics } from './timelineDiagnostics';
  import { createTimelineRowUiPrune } from './timelineRowUiPrune';
  import { coldLoadPriors, coldLoadWarmEdge } from '../../utils/coldLoadTrace';

  // Extra buffer rendered above + below the viewport. Sized for two viewports
  // worth of rows on each side so fast scrolls (trackpad fling, scrollbar
  // drag) don't outrun the rendered window — that was the source of the
  // "text disappears under the composer then reappears" flicker. Each
  // ~56px row × 1800px = ~32 extra rows per side. Trade a few MB of mounted
  // DOM/component state for the smoother scroll. Revisit only if it's
  // ever measured to hurt mount-time on first-open.
  const BUFFER_SIZE_PX = 1800;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 16;
  // Soft fade at the top of the scroll viewport: content dissolves under
  // the chat header instead of meeting a hard gap. Paint-only mask, so
  // (unlike padding or a spacer) it never changes
  // scrollHeight/clientHeight/scrollTop and never fires the content
  // ResizeObserver — it stays entirely clear of the scroll controller.
  //
  // Two mask layers composited as a union: layer 1 is the fade over the
  // content column; layer 2 is a solid strip that keeps the right
  // SCROLLBAR_SAFE_PX fully opaque so the SCROLLBAR itself never fades at
  // the top (a single full-width gradient would fade the scrollbar too,
  // since it's part of this element's paint). SCROLLBAR_SAFE_PX is an
  // approximate scrollbar width — overshooting just leaves a thin unfaded
  // margin on the right, where the centered content never reaches anyway.
  // Tune either number to taste.
  const TOP_FADE_PX = 32;
  const SCROLLBAR_SAFE_PX = 16;
  const TOP_FADE_MASK =
    `linear-gradient(to bottom, transparent 0, #000 ${TOP_FADE_PX}px) ` +
    `left top / calc(100% - ${SCROLLBAR_SAFE_PX}px) 100% no-repeat, ` +
    `linear-gradient(#000, #000) right top / ${SCROLLBAR_SAFE_PX}px 100% no-repeat`;
  const TARGET_FLASH_MS = 900;
  // happy-dom returns 0 for clientHeight/clientWidth, which makes the
  // windowing engine mount zero rows. In happy-dom test runs we mount
  // everything via the virtualizer's renderAll seam so test assertions
  // can find the rendered DOM. The real-Chromium `browser` vitest
  // project also runs with MODE==='test' but has real geometry and MUST
  // keep real windowing (streaming outcome tests count row unmounts; a
  // flat render-all then unmount wave would poison those counters), so
  // the gate keys on happy-dom's window marker, not MODE alone.
  // Production (vite dev/build) always sees `false`, keeping real
  // windowing.
  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  let {
    pane,
    onImageExpand,
    userMessageActions,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
  } = $props();

  // Inner scroll container. We own scrolling here; <TimelineVirtualizer>
  // renders its measured rows inside `contentEl` and reads scroll input
  // via scrollRef. The element is wrapped in a non-scrolling `relative
  // flex h-full flex-col` shell that anchors the floating
  // <ScrollToBottomButton> outside the scroll content (see template
  // comment for why).
  let scrollEl: HTMLDivElement | undefined = $state(undefined);
  // Wrapper around <TimelineVirtualizer>: the warm-up visibility gate's
  // hide target, and the controller's registered content element. The
  // controller does NOT observe it (`externalContentGeometry`) — content
  // geometry arrives engine-sourced through the virtualizer's
  // `onContentGeometry` samples, post-flush and before the paint that
  // displays the change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  // Imperative handle into the virtualizer. Set once it mounts.
  let listRef: TimelineVirtualizerHandle | undefined = $state(undefined);
  let scrollSurfaceContentWidth = $state(0);

  const targetFlash = createTimelineTargetFlash(TARGET_FLASH_MS);

  // Node-derivation pipeline (structural grouping, the reveal gate, rail
  // classification, response-pill duration) lives in
  // timelineRowProjection.svelte.ts. `revealedNodes` gets a local alias
  // because it's referenced throughout this component (factory option
  // closures below, the template, scroll-snapshot capture) — the alias is a
  // plain property read, so it carries the SAME array reference reactively
  // without wrapping or copying it; that identity flows into the
  // virtualizer and index-based decorations downstream.
  const rows = createTimelineRowProjection({ getPane: () => pane });
  let revealedNodes = $derived(rows.revealedNodes);

  // Animation mode is keyed on whether LIVE timeline content advanced
  // recently (`pane.lastLiveContentAt`), NOT on whether a provider turn
  // is active. Streaming chunks come in fast enough that the contentRO
  // sync-pin would land them invisibly; the spring chase gives the user
  // the familiar "viewport follows the text as it streams in" UX. Keying
  // on content rather than turn lifecycle is what makes the spring follow
  // a turn that ends mid-stream and the word-by-word drain tail that
  // reveals for seconds after the wire turn closes; it also keeps late
  // Streamdown typesetting on settled content sync-pinned, because
  // typesetting grows row height but never advances content (it never
  // stamps `lastLiveContentAt`). The controller's warm gate (quiescence-
  // based, with a 2.5s failsafe) independently defends against the
  // e00723f regression where mount-time row remeasurement + Streamdown
  // typesetting would spring-chase a thread restore visibly.
  // Warm-gate aggregation: rising-edge-once boolean that flips true the
  // first time ANY ChatMarkdown rendered inside the timeline tree fires
  // `onsettled` since the last `armWarmup()` call. Reset to false by
  // `armWarmupWithReset()` below. The controller reads this via
  // `quietContextSignal` to shorten the warm-gate quiet window from
  // QUIET_MS (100ms) to SETTLED_QUIET_MS (16ms) once we have first-hand
  // evidence that the visible async-typesetting cascade (shiki / katex
  // / mermaid) finished — the conservative 100ms wait exists to defend
  // against a late typesetting wave landing an RO event after the gate
  // lifts, and that defense is no longer needed once we know
  // typesetting is done.
  let anyMarkdownSettledSinceArm = $state(false);
  // Live count of ChatMarkdown instances mounted in this timeline tree
  // (registered via CHAT_MARKDOWN_PRESENCE_CONTEXT). Zero means the
  // mounted window has nothing to typeset, so the quiet signal reports
  // settled-by-absence — without this, a thread whose visible tail has
  // no markdown rows (all tool output / terminals / images) never flips
  // the settled boolean and the warm gate holds hidden content until
  // the 2.5s failsafe.
  //
  // Deliberately NOT $state: registrations mutate it from child mount /
  // teardown effects (an unsafe-mutation context for reactive state),
  // and its only reader is the controller's imperative
  // quietContextSignal getter — no template or $derived tracks it.
  let mountedMarkdownCount = 0;

  // Spring while live content advanced within SPRING_MODE_HOLD_MS, else
  // sync-pin. The pane stamps `lastLiveContentAt` on prose/reasoning
  // reveals, direct text patches, new text-like provider rows,
  // visible-field updates to already mounted rows (tool output previews,
  // running→completed result chrome), and gated wire appends / reveal
  // releases (armLiveContentAppendSpring), so during a stream the latch
  // reads 'spring' continuously and falls to 'instant'
  // ~SPRING_MODE_HOLD_MS after the last advance — and a post-turn append
  // (background-task completion sibling) gets the same window as the
  // identical rows arriving mid-stream.
  // The 500ms hold is pure tuning; see springAnimationLatch.ts.
  function animationModeForScroll(): 'spring' | 'instant' {
    return latchedSpringMode(performance.now(), pane.lastLiveContentAt, SPRING_MODE_HOLD_MS);
  }

  const stick = createUseStickToBottomController({
    animationMode: animationModeForScroll,
    quietContextSignal: () => anyMarkdownSettledSinceArm || mountedMarkdownCount === 0,
    // The virtualizer is the content-geometry source (its spacer height
    // IS the content height) — the controller creates no contentEl RO;
    // samples arrive through `onContentGeometry` below.
    externalContentGeometry: true,
  });

  function markMarkdownSettled(): void {
    if (anyMarkdownSettledSinceArm) return;
    anyMarkdownSettledSinceArm = true;
    stick.notifyQuietContextSignalChanged();
  }
  setContext(CHAT_MARKDOWN_SETTLED_CONTEXT, markMarkdownSettled);

  // Presence registration (see mountedMarkdownCount above). The 0↔1
  // transitions can flip the composed quietContextSignal in either
  // direction — a first markdown mounting after the quiet timer armed
  // must DISARM it (typesetting is now possible; the settled-by-absence
  // license is withdrawn), which the controller handles on notify.
  function registerMarkdownPresence(): () => void {
    mountedMarkdownCount += 1;
    if (mountedMarkdownCount === 1 && !anyMarkdownSettledSinceArm) {
      stick.notifyQuietContextSignalChanged();
    }
    return () => {
      mountedMarkdownCount -= 1;
      if (mountedMarkdownCount === 0 && !anyMarkdownSettledSinceArm) {
        stick.notifyQuietContextSignalChanged();
      }
    };
  }
  setContext(CHAT_MARKDOWN_PRESENCE_CONTEXT, registerMarkdownPresence);

  function armWarmupWithReset(): void {
    anyMarkdownSettledSinceArm = false;
    stick.armWarmup();
  }

  // Pane-move reconciliation ('host-layout' observations from PaneHost):
  // insertBefore can leave the scroller with transiently bad geometry, so
  // the virtualizer re-reads viewport + offset (revalidate) and sticky
  // panes re-pin to the bottom. The scrollToIndex write goes through the
  // controller chokepoint (applyScrollTarget), so it is tagged
  // programmatic and preserves intent.
  function notifyHostLayoutSettled(): void {
    const lastIndex = revealedNodes.length - 1;
    const shouldStickToBottom = !stick.escapedFromLock;
    if (lastIndex < 0) {
      if (shouldStickToBottom) {
        stick.markAtBottom();
      } else {
        stick.observe('content');
      }
      return;
    }
    if (!listRef) return;
    listRef.revalidate();
    if (shouldStickToBottom) {
      listRef.scrollToIndex(lastIndex, { align: 'end' });
      stick.markAtBottom();
    }
  }

  // ============================================================
  // Extracted timeline sessions
  // ============================================================
  // (`findTimelineNodeIndex`, referenced below, is declared with the
  // rest of the Helpers section further down — `function` declarations
  // are hoisted, so the forward reference is safe.)
  // Restore session (timelineRestore.svelte.ts), row-size priors
  // (timelineSizePriors.svelte.ts), load-older/newer paging
  // (timelinePaging.ts), and the timeline-window prune anchor
  // transaction (timelineWindowAnchor.svelte.ts) cross-reference each
  // other's methods. Every cross-reference below is arrow-wrapped so it
  // isn't evaluated until the wrapping closure is actually CALLED — by
  // then every factory in this block has finished constructing, so
  // construction order doesn't matter (TDZ-protection; mirrors
  // thread.svelte.ts's `getScrollController: () => scrollController`
  // pattern for the same reason).
  const sizePriors = createTimelineSizePriors({
    getPane: () => pane,
    getListRef: () => listRef,
    getRevealedNodes: () => revealedNodes,
    getScrollSurfaceContentWidth: () => scrollSurfaceContentWidth,
    getRestoredThreadId: () => restore.restoredThreadId,
  });

  const paging = createTimelinePaging({
    getPane: () => pane,
    stick,
    getListRef: () => listRef,
    getScrollEl: () => scrollEl,
    getRevealedNodes: () => revealedNodes,
    getRestoredThreadId: () => restore.restoredThreadId,
    nextRestoreToken: () => restore.nextRestoreToken(),
    isRestoreTokenCurrent: (token) => restore.isRestoreTokenCurrent(token),
    saveScrollSnapshot: () => restore.saveScrollSnapshot(),
  });

  const windowAnchor = createTimelineWindowAnchor({
    getPane: () => pane,
    stick,
    getListRef: () => listRef,
    getScrollEl: () => scrollEl,
    getRevealedNodes: () => revealedNodes,
    findTimelineNodeIndex,
    saveScrollSnapshot: () => restore.saveScrollSnapshot(),
    nextRestoreToken: () => restore.nextRestoreToken(),
    isRestoreTokenCurrent: (token) => restore.isRestoreTokenCurrent(token),
  });

  const restore = createTimelineRestore({
    getPane: () => pane,
    stick,
    getListRef: () => listRef,
    getScrollEl: () => scrollEl,
    getRevealedNodes: () => revealedNodes,
    getGroupedNodes: () => rows.groupedNodes,
    findTimelineNodeIndex,
    persistSizePriors: () => sizePriors.maybePersistSizePriors(),
    armWarmupWithReset,
    resetAutoLoadGates: () => paging.resetGates(),
    clearTimelineWindowPruneShift: () => windowAnchor.clearTimelineWindowPruneShift(),
    targetFlash,
  });

  const diag = createTimelineDiagnostics({
    getPane: () => pane,
    stick,
    getScrollEl: () => scrollEl,
    getContentEl: () => contentEl,
    getListRef: () => listRef,
    getRevealedNodes: () => revealedNodes,
    getScrollSurfaceContentWidth: () => scrollSurfaceContentWidth,
    getRestoredThreadId: () => restore.restoredThreadId,
  });

  const prune = createTimelineRowUiPrune({
    getPane: () => pane,
    getListRef: () => listRef,
    getRevealedNodes: () => revealedNodes,
    isTest: IS_TEST,
  });

  // Depends on windowAnchor (module 4), so declared here rather than
  // alongside the rest of the node-derivation pipeline above.
  let virtualizerShiftAtHead = $derived(
    pane.pendingTimelineShiftAtHead || windowAnchor.pruneShiftAtHead,
  );

  // Explicit adapter, not the raw controller: 'host-layout' observations
  // route through the revalidate + re-pin handler above instead of the
  // controller's plain content path, and the timeline-window anchor
  // transaction only exists at this layer.
  const paneScrollController: PaneScrollController = {
    pauseAutoScroll: () => stick.pauseAutoScroll(),
    observe: (kind) => {
      if (kind === 'host-layout') {
        notifyHostLayoutSettled();
        return;
      }
      stick.observe(kind);
    },
    preserveScrollAnchor: (anchor, action) =>
      stick.preserveScrollAnchor(anchor, action),
    markStructuralContentPending: () => stick.markStructuralContentPending(),
    preserveTimelineWindowAnchor: windowAnchor.preserveTimelineWindowAnchor,
  };

  $effect(() => {
    if (!pane.hasDeferredRecentWindowPrune) return;
    if (!stick.isSticky) return;
    pane.retryDeferredRecentWindowPrune();
  });

  // Hide contentEl while the virtualizer and async row content settle.
  // Priors-miss mounts start from kind-table/flat estimates; the per-row
  // ResizeObserver then corrects actual heights. The controller keeps
  // scrollTop pinned, but rows can still shift between paints. The
  // warmup gate reveals content after QUIET_MS=100ms of contentRO
  // silence or the FAILSAFE_MS=2500ms ceiling.
  const WARMUP_HIDE_THRESHOLD = 5;
  let hideContentForWarmup = $derived(!stick.isWarm && pane.items.length > WARMUP_HIDE_THRESHOLD);

  // Publish the controller on the pane so external surfaces (sidebar
  // resizers, resizable drawers) can acquire a `pauseAutoScroll()` lease
  // during gestures. The effect's return function detaches symmetrically
  // when the `pane` reference changes and on component teardown, so a stale
  // pointer to a torn-down controller can't leak. (A thread switch remounts
  // only the inner {#key pane.threadId} virtualizer — this component and its
  // scrollEl persist — so detach here is driven by a `pane` prop swap, not a
  // remount.)
  $effect(() => {
    pane.attachScrollController(paneScrollController);
    return () => pane.detachScrollController(paneScrollController);
  });

  // Bind the controller to the actual DOM elements. The content RO and
  // wheel/scroll/keydown/touch listeners all start here. Re-runs if
  // either ref changes (thread switch / HMR).
  $effect(() => {
    if (!scrollEl || !contentEl) return;
    const surface = scrollEl;
    stick.attach(surface, contentEl);

    // Re-arm both auto-load gates on real user gestures so each user
    // scroll loads exactly one section per direction, not a cascade.
    // `handleLoadOlder` / `handleLoadNewerAuto` call `disarm()` after each
    // load; the anchor-restore (older) or anchor-preserving prune (newer)
    // that follows is a programmatic scroll which does NOT fire these
    // listeners, so the gate stays disarmed until the user actually moves.
    // The 350ms cooldown in the gate itself is a fallback for devices
    // where gesture detection misses an event.
    const onUserGesture = (): void => {
      paging.armGatesOnUserGesture();
    };
    surface.addEventListener('wheel', onUserGesture, { passive: true });
    surface.addEventListener('touchmove', onUserGesture, { passive: true });
    surface.addEventListener('keydown', onUserGesture);
    return () => {
      surface.removeEventListener('wheel', onUserGesture);
      surface.removeEventListener('touchmove', onUserGesture);
      surface.removeEventListener('keydown', onUserGesture);
    };
  });

  // The scroll surface's CONTENT-box width, sourced ONLY from the async
  // ResizeObserver inside observeScrollSurfaceContentWidth. It feeds the
  // size-priors width dimension (timelineSizePriors.svelte.ts's capture
  // and its lazy-once validity check). This effect
  // must depend on `scrollEl` alone: it never reads
  // `scrollSurfaceContentWidth` and never seeds it from a synchronous layout
  // query. Either would re-subscribe the effect to the width state, so any
  // write — including the surface RO's own async delivery — re-triggers it;
  // the re-run disconnects and re-creates the observer, and a fresh
  // observe() always schedules an initial delivery (per spec). With a
  // border-box gBCR seed disagreeing with that
  // content-box delivery, the two values never converge, so the effect re-fires
  // forever at literal idle — re-rendering every visible row each time. That is
  // the width-oscillation feedback loop documented on
  // observeScrollSurfaceContentWidth (idle CPU/heap-churn incident 2026-06-26;
  // the CPU trace caught this exact effect re-running ~33k times in 30s of
  // idle). The RO's first delivery — content-box, before paint — sets the
  // initial width; until then it stays 0, which the priors validity check
  // reads as "layout hasn't reported yet" and optimistically trusts the
  // entry's captured width (see buildRowEstimate in
  // timelineSizePriors.svelte.ts).
  $effect(() => {
    const surface = scrollEl;
    if (!surface) {
      scrollSurfaceContentWidth = 0;
      return;
    }
    return observeScrollSurfaceContentWidth(surface, (width) => {
      scrollSurfaceContentWidth = width;
    });
  });

  // Structural-append spring arming, the live-content latch stamp for
  // appends, and the post-flush 'live-content' nudge are owned entirely
  // by the pane data layer (thread.svelte.ts
  // `armLiveContentAppendSpring`): `applyProviderItemUpserts` arms for
  // wire appends and `recomputeRevealPass` arms when the reveal gate
  // releases withheld rows. Both run synchronously with the data
  // change, so they cannot lose the ordering race an effect here had
  // against the virtualizer's same-flush geometry delivery
  // (bug-report-20260702T193212Z), and neither is keyed on an active
  // turn, so post-turn appends (interrupt echo, force-closed tool rows,
  // background-task completion siblings) arm and stamp too.
  // Sidebar/host layout nudges keep using the instant
  // 'content'/'host-layout' paths; ChatView composer geometry observes as
  // 'composer-geometry' so activity-rail changes during streaming can
  // continue the spring.

  // Diagnostic UI render trace, memory/geometry probes, and the
  // trace-flag gated row-resize / margin-divergence / reasoning-tail-jump
  // oracles all live in timelineDiagnostics.ts; production builds
  // short-circuit inside each helper, so these thin effects' only
  // steady-state cost is the reactive dep tracking their called method
  // performs.
  $effect(() => {
    diag.recordRenderTrace();
  });

  $effect(() => diag.memoryDiagnosticsSnapshotInstall());

  $effect(() => diag.geometryProbeInstall());

  $effect(() => {
    diag.recordListRefBindTrace();
  });

  $effect(() => diag.rowResizeTraceInstall());

  $effect(() => diag.marginDivergenceTraceInstall());

  $effect(() => diag.reasoningTailJumpTraceInstall());

  // ============================================================
  // Helpers
  // ============================================================
  // Pure helpers live alongside the TimelineNode type in
  // subagentGrouping.ts; the local thin wrapper below adapts them to the
  // current groupedNodes array so the template doesn't have to thread
  // `groupedNodes` into every call site.

  function findTimelineNodeIndex(itemId: string): number {
    return resolveVisibleTimelineNodeIndex(revealedNodes, pane.items, itemId);
  }

  // Prune cadence: structural timeline changes (revision / reveal /
  // thread switch) plus scroll END — never per scroll frame. Retention
  // is recomputed fresh on every run, so active-row transitions that
  // ride no structural bump (a lone settle flip) are picked up by the
  // next trigger; until then the stale entry is only over-retained,
  // never prematurely pruned.
  $effect(() => {
    pane.threadId;
    pane.timelineRevision;
    pane.revealBoundary;
    prune.schedule();
  });

  $effect(() => {
    listRef;
    scrollEl;
    prune.schedule();
  });


  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleTimelineScroll(offset: number): void {
    restore.saveScrollSnapshot();
    // Older and newer zones are geometrically exclusive in a normal window
    // (you can't be near both edges at once), but a degenerate window that
    // fits in the viewport could satisfy both; firing one direction per
    // frame avoids two concurrent loads racing the shared pagingGeneration.
    if (paging.maybeAutoLoadOlder(offset)) return;
    paging.maybeAutoLoadNewer(offset);
    // No prune here: this fires every scroll frame (60Hz under the
    // spring), and pruning is a memory bound, not a render input.
    // Scroll-end + structural effects cover it.
  }

  function handleTimelineScrollEnd(): void {
    restore.saveScrollSnapshot();
    prune.schedule();
  }

  $effect.pre(() => {
    sizePriors.resolveRowEstimateOnThreadEdge(pane.threadId);
  });

  $effect(() => {
    sizePriors.captureOnWarmRisingEdge(stick.isWarm);
    // Dev-trace only (see coldLoadTrace.ts): stamp the priors replay
    // summary, then close the cold-load session `thread.svelte.ts`
    // opened for this pane's current switch, once the warm gate's
    // rising edge matches this threadId. Order matters — the stamp must
    // land before the edge emits the record.
    coldLoadPriors(pane.paneId, sizePriors.replayStats());
    coldLoadWarmEdge(pane.paneId, pane.threadId ?? '', stick.isWarm, stick.warmReason);
  });

  $effect.pre(() => {
    restore.handleSwitchEdgePre(pane.threadId, pane.switchGeneration);
  });

  $effect(() => {
    restore.maybeRestoreAfterFlush();
  });

  let lastHandledScrollNonce = 0;
  $effect(() => {
    const req = pane.scrollToItemRequest;
    if (req.nonce === 0) return;
    if (req.nonce === lastHandledScrollNonce) return;
    lastHandledScrollNonce = req.nonce;
    void restore.scrollToItem(req.itemId, { flash: req.flash });
  });

  onDestroy(() => {
    prune.invalidate();
    restore.invalidateRestore();
    restore.saveSnapshotOnDestroy();
    targetFlash.clear();
    paging.resetGates();
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
     inner div is the actual scroll container that <TimelineVirtualizer
     scrollRef={scrollEl}> reads scroll input from and that the
     controller's wheel/scroll/keydown/touch listeners bind to.
     `overflow-anchor: none` disables the browser's
     scroll-anchor adjustment — the engine already owns row-anchor
     preservation via its compensation path, and the controller owns
     bottom-pinning via the contentRO sync-pin; leaving the browser's
     anchor heuristic ON makes it fight both, producing visible
     scrollTop oscillation as Streamdown's async typesetting (shiki /
     KaTeX / mermaid) grows rows above the viewport. The padding-bottom
     (= composer height + visual breathing room) keeps the last message
     clear of the absolute composer overlay without putting a synthetic
     spacer row inside the virtualized data; it lives on scrollEl
     because the content-geometry pipeline reports the engine's
     totalSize — padding changes don't move it, so they could never
     re-pin through that seam. ChatView's composer-overlay RO
     calls `observe('composer-geometry')` to handle that case explicitly.
     The top `mask` fades the first TOP_FADE_PX of content as it rises
     under the header (replacing the old hard top padding), while a solid
     mask layer over the right SCROLLBAR_SAFE_PX keeps the scrollbar from
     fading with it. It's a paint-only effect, so like the padding-bottom
     above it never changes scrollHeight/clientHeight/scrollTop and stays
     clear of the controller.
     `scrollbar-gutter: stable both-edges` keeps the centered
     `mx-auto max-w-[62rem]` rows aligned with ChatView's composer overlay.
     The styled `::-webkit-scrollbar` (app.css) is a classic, space-consuming
     bar, not an overlay, so without a reserved gutter the centered column
     jumps ~5px left the moment the bar appears. `both-edges` is required,
     not single-edge `stable`: WebKitGTK reserves the gutter only while the
     bar is actually present, so a single-edge gutter still shifts the column
     on the idle→scrolling transition. Symmetric reservation holds the center
     in both states, and idle reserves zero (no always-visible bar, no idle
     offset). Verified in WebKitGTK 6.0 2.52.3; see
     docs/architecture/frontend-scroll.md.
     ChannelView.svelte is left-aligned, so the bar only reflows its right
     edge and it needs no gutter — that is why this directive is chat-only.
     Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="flex-1 min-h-0 overflow-y-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/25"
    style:overscroll-behavior-y="contain"
    style:overflow-anchor="none"
    style:scrollbar-gutter="stable both-edges"
    style:padding-bottom={`calc(var(--composer-height, 0px) + ${BOTTOM_PAD_PX}px)`}
    style:mask={TOP_FADE_MASK}
    style:-webkit-mask={TOP_FADE_MASK}
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
            codexSubagentReceiverLabels={rows.codexReceiverLabels}
            targetFlash={targetFlash.itemId === node.item.id}
            targetFlashNonce={targetFlash.itemId === node.item.id ? targetFlash.nonce : 0}
          />
        {:else if node.kind === 'group'}
          <SubagentGroup {pane} group={node} {depth} {renderNode} />
        {:else if node.kind === 'wait_group'}
          <WaitGroup
            {pane}
            group={node}
            {onImageExpand}
            {userMessageActions}
            codexSubagentReceiverLabels={rows.codexReceiverLabels}
          />
        {:else if node.kind === 'read_group'}
          <ReadGroupRow {pane} group={node} />
        {/if}
      {/snippet}

      <!-- contentEl is the warm-up gate's hide target and the
           controller's registered content element (geometry itself is
           engine-sourced: the virtualizer's container has `contain:
           size; height: totalSize+'px'`, so its `onContentGeometry`
           samples report the content height the controller would
           otherwise have had to re-observe). {#key pane.threadId} forces the
           <TimelineVirtualizer> to remount on every thread switch so its
           engine resets with the timeline. `estimate` replays the
           previous visit's measured sizes when the priors validity key
           still matches (width + structure + expansion), so a revisited
           thread mounts at its final height instead of re-running the
           estimate→measure cascade — the thread-switch flicker. On any
           mismatch the estimate degrades per-row to the kind table /
           flat default. See utils/virtual/priors.ts and
           docs/architecture/frontend-scroll.md. -->
      <!-- will-change-transform: the scroll controller composites the spring's
           sub-pixel remainder onto this element as a translateY (glide
           residue). Keeping the layer promotion permanent avoids the
           subpixel-AA repaint blink that layer creation/destruction would
           cause at every chase start/end. -->
      <div
        bind:this={contentEl}
        class="will-change-transform"
        style:visibility={hideContentForWarmup ? 'hidden' : 'visible'}
      >
        {#key pane.threadId}
        <TimelineVirtualizer
          bind:this={listRef}
          scrollRef={scrollEl}
          data={revealedNodes}
          estimate={sizePriors.rowEstimate}
          shift={virtualizerShiftAtHead}
          getKey={(node) => timelineNodeKey(node)}
          bufferSize={BUFFER_SIZE_PX}
          renderAll={IS_TEST}
          onscroll={handleTimelineScroll}
          onscrollend={handleTimelineScrollEnd}
          onCompensation={stick.applyEngineCompensation}
          applyScrollTarget={stick.applyScrollTarget}
          onContentGeometry={stick.deliverContentGeometry}
        >
          {#snippet children(node: TimelineNode, index: number)}
            {@const currentLeafItem = rows.currentTimelineLeafItem(node)}
            {@const isRail = rows.timelineNodeHasRail(node, currentLeafItem)}
            <!-- Outer per-row wrapper. We do NOT set data-item-id here:
                 only TimelineLeaf owns that attribute on its root. Structural
                 rows stay unanchored, and tests rely on the divider rendering
                 BEFORE the [data-item-id] node, not containing it.

                 `isRail` decides whether this row participates in the
                 continuous left-border under consecutive tool / think rows.
                 Leaf rows of those kinds get the rail; subagent / wait
                 group containers also opt in so the rail stays continuous
                 through nested cards and the agent card's chev/ico/label
                 gutter aligns with adjacent tool rows. Everything else
                 (assistant_text, user_text, notifications, api errors)
                 renders flat and breaks the line. -->
            <div
              data-row-index={index}
              class:mt-4={rows.rowDecorations.toolTextBoundaryIndexes.has(index)}
            >
              <!-- Style-load-bearing: app.css sets `display: flow-root` on
                   [data-row-geometry-content] to establish a BFC that CONTAINS
                   each row's trailing bottom margin in its own content box
                   (UserMessage's `.group mb-5`, the error card `mb-4`,
                   notification / retry `mb-1.5`, …). Without it those margins
                   collapse out through this all-plain wrapper chain and are
                   trapped only by the `contain: layout style` VirtualRow
                   wrapper, so the measured row total (margin included) and the
                   row's own content box (margin excluded) disagree; the
                   trapped margin re-measures inconsistently during streaming
                   reflow → totalSize oscillates → scrollTop clamp →
                   `spring.oscillationSnap` = the settle flicker. Keyed to the
                   attribute (not a class) so a refactor here can't drop it
                   silently; it is display-only. Coupling +
                   behavioral regression test + full analysis: see the rule's
                   comment in app.css and
                   docs/architecture/settle-flicker-analysis.md. -->
              <div data-row-geometry-content>
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
                          onclick={paging.handleLoadOlder}
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
                  {#if rows.rowDecorations.responseDividerIndexes.has(index)}
                    {@const showResponsePill = rows.rowDecorations.responsePillIndexes.has(index)}
                    {@const responseDuration = rows.responsePillDuration(node)}
                    <!-- Two visual modes share a fixed wrapper height
                         (`h-[1.625rem]` = 26px = pill chrome: text-[0.625rem]
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
                         `frontend/AGENTS.md`. Re-derive 1.625rem if the pill
                         classes above change. -->
                    <div data-testid="response-divider" data-final-response={showResponsePill ? 'true' : 'false'}>
                      <div class="my-3 flex h-[1.625rem] items-center gap-3">
                        <span class="timeline-hairline flex-1" aria-hidden="true"></span>
                        {#if showResponsePill}
                          <span
                            class="rounded-full border border-border bg-surface-1 px-2.5 py-1 text-[0.625rem] uppercase leading-tight tracking-[0.14em] text-text-secondary"
                          >
                            Response{#if responseDuration}{' '}<span class="normal-case tabular-nums tracking-normal">{responseDuration}</span>{/if}
                          </span>
                          <span class="timeline-hairline flex-1" aria-hidden="true"></span>
                        {/if}
                      </div>
                    </div>
                  {/if}
                  <!-- Rail offsets: ml-[14px] places the border-l 14px
                     inside the row column (under the chev gutter);
                     pl-[18px] shifts content past the icon + label
                     gutters so the row body lines up with the body
                     column described in
                     docs/specs/tool-call-ui-redesign/README.md
                     §Row geometry. -->
                <div
                  data-testid="message-timeline-node"
                  data-rail={isRail ? 'true' : 'false'}
                  class={isRail ? 'border-l border-border-subtle ml-[14px] pl-[18px]' : ''}
                >
                  {@render renderNode(node, 1)}
                </div>
              </div>
            </div>
            </div>
          {/snippet}
        </TimelineVirtualizer>
        {#if pane.hasMoreNewer}
          <div class="mx-auto flex w-full max-w-[62rem] justify-center px-6 pb-6 pt-2">
            <div class="flex items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/80 px-2 py-1.5 shadow-sm">
              <Button
                variant="secondary"
                size="xs"
                onclick={paging.handleLoadNewer}
                loading={pane.loadingNewer}
                testId="load-newer-messages"
              >
                {#snippet children()}
                  {pane.loadingNewer ? 'Loading…' : 'Load newer messages'}
                {/snippet}
              </Button>
              <Button
                variant="ghost"
                size="xs"
                onclick={() => { void paging.jumpToLatest(); }}
                loading={pane.loadingNewer}
                testId="jump-to-latest-messages"
              >
                {#snippet children()}Jump to latest{/snippet}
              </Button>
            </div>
          </div>
        {/if}
        {/key}
      </div>
    {/if}
  </div>

  <!-- Visible when the user has escaped or is no longer near the bottom.
       Wiring this to `!isSticky` would also pop the chip during sidebar/
       drawer resize leases (pauseDepth > 0) even though the user is
       geometrically glued to the bottom. Anchored to the outer wrapper
       (which does not scroll), so the chip stays fixed in the visible
       area regardless of transcript scrollTop. -->
  <ScrollToBottomButton visible={!stick.isAtBottom || pane.hasMoreNewer} onClick={() => { void paging.jumpToLatest(); }} />
</div>
