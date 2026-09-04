<script lang="ts">
  import { onDestroy, setContext } from 'svelte';
  import type {
    PaneScrollController,
    ThreadPane,
  } from '../../stores/thread.svelte';
  import { createUseStickToBottomController } from '../../utils/scroll/index.svelte';
  import { createContentGeometryNotifier } from '../../utils/scroll/contentGeometryNotifier';
  import { getSettings } from '../../stores/settings.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import {
    createUserMessageOverflowCoordinator,
    USER_MESSAGE_OVERFLOW_COORDINATOR_CONTEXT,
  } from './userMessageOverflowMeasurement';
  import { isLiveContentActive, LIVE_CONTENT_ACTIVE_HOLD_MS } from '../../utils/liveContentActivity';
  import {
    CHAT_MARKDOWN_PRESENCE_CONTEXT,
    CHAT_MARKDOWN_SETTLED_CONTEXT,
  } from './markdownSettledContext';
  import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
  import TimelineVirtualizer from '../virtual/TimelineVirtualizer.svelte';
  import {
    timelineNodeItemIndex,
    timelineNodeKey,
    timelineNodeTurnIndex,
    type TimelineNode,
  } from '../../utils/subagentGrouping';
  import { getActiveTurn } from '../../stores/threadStatuses.svelte';
  import Button from '../primitives/Button.svelte';
  import MessageNavRail from './MessageNavRail.svelte';
  import {
    NAV_RAIL_ROW_LEFT_PADDING_PX,
    NAV_RAIL_ROW_MAX_WIDTH_PX,
    NAV_RAIL_ROW_RIGHT_PADDING_PX,
  } from './messageNavRail';
  import ReadGroupRow from './ReadGroupRow.svelte';
  import OverlayScrollbar from '../shared/OverlayScrollbar.svelte';
  import ScrollToBottomButton from './ScrollToBottomButton.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import ActivityRun from './ActivityRun.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import WaitGroup from './WaitGroup.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import type { UserMessageActions } from './userMessageActions';
  import { resolveVisibleTimelineNodeIndex } from './timelineScroll';
  import { observeScrollSurfaceContentWidth } from './scrollSurfaceWidth';
  import { createTimelineRestore } from './timelineRestore.svelte';
  import { createTimelineSizePriors } from './timelineSizePriors.svelte';
  import { createTimelinePaging } from './timelinePaging';
  import { createTimelineWindowAnchor } from './timelineWindowAnchor.svelte';
  import { createTimelineRowProjection } from './timelineRowProjection.svelte';
  import { responsePillLabel } from './timelineRows';
  import { createTimelineDiagnostics } from './timelineDiagnostics';
  import { createTimelineQuietWork } from './timelineQuietWork';
  import { createTimelineRowUiPrune } from './timelineRowUiPrune';
  import { createTimelineJump, JUMP_FLASH_DURATION_MS } from './timelineJump.svelte';
  import { createTimelineActivityRunAutoCollapse } from './timelineActivityRunAutoCollapse';
  import { createTimelineVisibilityGeometry } from './timelineVisibilityGeometry';
  import { coldLoadPriors, coldLoadWarmEdge } from '../../utils/coldLoadTrace';

  // Extra buffer rendered above + below the viewport so fast scrolls
  // (trackpad fling, scrollbar drag) don't outrun the rendered window —
  // that was the source of the "text disappears under the composer then
  // reappears" flicker. Mechanically the window recenters on every
  // scroll event (any landing point mounts same-frame), so the buffer
  // only bridges the compositor-vs-main-thread gap: 1-2 frames of
  // scroll velocity in the steady state, plus main-thread stall bursts
  // (streaming markdown parse, highlight ingest — tens of ms) at fling
  // speed. It also lets row-size estimate corrections and async row
  // content (images, first-visit mermaid/katex/spans) settle offscreen
  // instead of at the visible edge.
  //
  // NOT a memory dial. The 1800 -> 1200 trim (2026-07-21) was tried as
  // a tile-memory cut and measured ~nothing (86.4 -> 89.9MB renderer
  // cc/tile_memory, within noise): parked-at-bottom panes only fill
  // the above-side buffer, and layer overheads dominated — buffered
  // rows cost DOM, not raster; tiles only exist near each pane's
  // viewport. Shrinking below ~800px starts exposing the stall x
  // fling blanking scenarios above for low-single-digit-MB DOM
  // savings; 1200 ≈ a viewport of stall insurance. Keep in sync with
  // TimelineVirtualizer's DEFAULT_BUFFER_PX.
  const BUFFER_SIZE_PX = 1200;
  // Visual breathing room between the last message and the composer
  // overlay; combined with the --composer-height variable from ChatView.
  const BOTTOM_PAD_PX = 16;
  const TIMELINE_TOP_SPACER_PX = 24;
  const TIMELINE_LOAD_OLDER_HEADER_PX = 60;
  // Soft fade at the top of the scroll viewport: content dissolves under
  // the chat header instead of meeting a hard gap. Implemented as a
  // gradient OVERLAY (surface color -> transparent, painted over the
  // content), NOT a mask on the scroller. The two are pixel-identical
  // here because the backdrop the old mask revealed is the flat
  // app-shell surface shown through ChatView's transparent timeline
  // (verified live 2026-07-21, max channel delta 2/255) — but their
  // paint cost is wildly different. Prior layer experiments measured a
  // mask on the scroller as a full viewport-sized texture (~4.5MB in
  // the renderer plus a GPU-process mirror per streaming pane), and the
  // mask re-applies on every streaming repaint. The overlay bounds that
  // work to a 32px strip. Like the
  // mask, it is paint-only: no effect on scrollHeight/clientHeight/
  // scrollTop and no content-RO traffic, so it stays entirely clear of
  // the scroll controller.
  //
  // The overlay stops SCROLLBAR_SAFE_PX short of the right edge so the
  // scrollbar never gets tinted at the top (the old mask kept an
  // always-opaque strip there for the same reason). Overshooting just
  // leaves a thin unfaded margin on the right, where the centered
  // content never reaches anyway. Tune either number to taste.
  const TOP_FADE_PX = 32;
  const SCROLLBAR_SAFE_PX = 16;
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
    pendingCutAfter = null,
  }: {
    pane: ThreadPane;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
    /**
     * Display position of a revert that is actually in flight. Every row
     * strictly AFTER it is about to be destroyed, and renders dimmed for
     * the duration — an honest pending state, since nothing truncates
     * until the backend's own event lands. Null whenever no destruction
     * is running (an open editor destroys nothing yet).
     *
     * A separate prop rather than a field on `userMessageActions`: this
     * is consumed by the row WRAPPER, one level above the components
     * that read those actions, and it applies to every row kind.
     */
    pendingCutAfter?: { turnIndex: number; itemIndex: number } | null;
  } = $props();

  /**
   * Strictly after the pending cut in DISPLAY order. Positional, not
   * per-turn: Claude's item-granular cut keeps the anchor turn's rows
   * that precede the anchor, so those are not doomed. The anchor is a
   * user message, so no activity run ever straddles it and comparing a
   * node by its first item's position is exact.
   */
  function isPendingCutRow(node: TimelineNode): boolean {
    if (!pendingCutAfter) return false;
    const turnIndex = timelineNodeTurnIndex(node);
    if (turnIndex !== pendingCutAfter.turnIndex) return turnIndex > pendingCutAfter.turnIndex;
    return timelineNodeItemIndex(node) > pendingCutAfter.itemIndex;
  }

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
  // geometry arrives engine-sourced through the virtualizer's geometry
  // subscription, post-flush and before the paint that displays the
  // change.
  let contentEl: HTMLDivElement | undefined = $state(undefined);
  const scrollbarGeometry = createContentGeometryNotifier();
  const userMessageOverflow = createUserMessageOverflowCoordinator();
  setContext(USER_MESSAGE_OVERFLOW_COORDINATOR_CONTEXT, userMessageOverflow);

  // Width is not the only input to line wrapping. Root font scale and the UI
  // typeface are live settings, and a newly loaded webfont can change metrics
  // again after the setting write. Queue one pre-paint batch for either edge;
  // row-local observers are intentionally absent because inserting a toggle
  // from a descendant RO callback changes its already-delivered row ancestor.
  $effect(() => {
    const settings = getSettings();
    void settings.fontSize;
    void settings.sansFont;
    userMessageOverflow.requestAll();
  });

  $effect(() => {
    // FontFaceSet is available in WebView2, but not in every supported test
    // or remote-browser DOM. Settings changes still remeasure above; only the
    // later font-load correction depends on this optional platform API.
    const fonts = document.fonts;
    if (!fonts) return;
    const remeasure = (): void => userMessageOverflow.requestAll();
    fonts.addEventListener('loadingdone', remeasure);
    fonts.addEventListener('loadingerror', remeasure);
    return () => {
      fonts.removeEventListener('loadingdone', remeasure);
      fonts.removeEventListener('loadingerror', remeasure);
    };
  });
  // Imperative handle into the virtualizer. Set once it mounts.
  let listRef: TimelineVirtualizerHandle | undefined = $state(undefined);
  const visibilityGeometry = createTimelineVisibilityGeometry();
  let scrollSurfaceContentWidth = $state(0);
  // Message-nav rail instance — its viewport sync (in-view ticks +
  // position marker) is driven imperatively from the scroll callbacks
  // below, never by a listener of its own.
  let navRail: MessageNavRail | undefined = $state(undefined);

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

  // Is timeline content still arriving? LIVENESS ONLY — it does not pick
  // spring vs sync-pin (growth while pinned at the bottom always glides;
  // see utils/scroll/resolver.ts springGateIsOpen). It keeps the spring's
  // post-arrival sentinel alive across inter-chunk gaps and lets an
  // activity-rail composer resize ride an in-flight glide.
  //
  // The pane stamps `lastLiveContentAt` on prose/reasoning reveals,
  // direct text patches, new text-like provider rows, visible-field
  // updates to already mounted rows (tool output previews,
  // running→completed result chrome), and gated wire appends / reveal
  // releases (armLiveContentAppendSpring). The 500ms hold is pure
  // tuning; see utils/liveContentActivity.ts.
  function liveContentActiveNow(): boolean {
    return isLiveContentActive(
      performance.now(),
      pane.lastLiveContentAt,
      LIVE_CONTENT_ACTIVE_HOLD_MS,
    );
  }

  const stick = createUseStickToBottomController({
    liveContentActive: liveContentActiveNow,
    quietContextSignal: () => anyMarkdownSettledSinceArm || mountedMarkdownCount === 0,
    // The virtualizer is the content-geometry source (its spacer height
    // IS the content height) — the controller creates no contentEl RO;
    // samples arrive through the geometry subscription below, which is
    // taken after `attach` so the first one cannot be delivered (and
    // lost) while this controller is still detached.
    externalContentGeometry: true,
    onContentGeometryProcessed: scrollbarGeometry.notify,
    // ... and learns every controller write the moment it lands, so the
    // offset its compensations are computed from never trails a glide
    // by a frame (see VirtualEngine.noteScrollOffset).
    onScrollTopWritten: (top) => listRef?.noteScrollTopWritten(top),
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
  // panes re-pin to the bottom. The re-pin is system-initiated, so it
  // goes through `requestBottom` as a 'yield' — a glide mid-flight
  // across the pane move keeps owning the trip instead of being snapped
  // over. The scrollToIndex write goes through the controller chokepoint
  // (applyScrollTarget), so it is tagged programmatic and preserves
  // intent.
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
    const list = listRef;
    if (!list) return;
    list.revalidate();
    if (shouldStickToBottom) {
      stick.requestBottom({
        takeover: 'yield',
        write: () => {
          list.scrollToIndex(lastIndex, { align: 'end' });
          stick.markAtBottom();
        },
      });
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
    // The rate-bounded variant: this reaches the snapshot path, which
    // fires per scroll frame. The exact capture is the settle edge below,
    // plus the final edges (unmount here, switch-away through the
    // controller adapter).
    persistSizePriors: () => sizePriors.maybePersistSizePriorsInterim(),
    persistSizePriorsExact: () => sizePriors.maybePersistSizePriors(),
    armWarmupWithReset,
    resetAutoLoadGates: () => paging.resetGates(),
  });

  // Explicit-jump session (nav-rail clicks, jump-to-first) + the landing
  // flash overlay. Routes through the restore session's scrollToItem, so
  // load-until-item, run reveal, and escape semantics stay in one place.
  // Row column shell, shared by every row-aligned surface (rows, the
  // load-older/newer chips, and the landing flash). Its left padding is
  // derived from the nav rail's hit width plus an 8px dead gutter, so an
  // invisible jump target can never overlap selectable transcript text.
  // The 62rem outer cap and asymmetric padding preserve the previous 920px
  // wide-pane content bounds exactly while giving narrow panes 8px more
  // clearance on each side. One style definition keeps every copy aligned.
  // Compact has no rail (a desktop affordance, per the phone design), so
  // the left gutter it reserves goes too and the row is symmetric.
  const ROW_SHELL_CLASSES = 'mx-auto w-full';
  let compact = $derived(isCompactLayout());
  let rowShellStyle = $derived([
    `max-width: ${NAV_RAIL_ROW_MAX_WIDTH_PX}px`,
    `padding-left: ${compact ? NAV_RAIL_ROW_RIGHT_PADDING_PX : NAV_RAIL_ROW_LEFT_PADDING_PX}px`,
    `padding-right: ${NAV_RAIL_ROW_RIGHT_PADDING_PX}px`,
  ].join('; '));

  const jump = createTimelineJump({
    getPane: () => pane,
    getListRef: () => listRef,
    scrollToItem: (id) => restore.scrollToItem(id),
    findTimelineNodeIndex,
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

  // Deferred structural work — the recent-window prune retry, the
  // activity-run auto-collapse releases, and the row-UI prune — runs on
  // ONE cadence owned by the quiet scheduler: structural triggers +
  // scroll end, debounced, with geometry-mutating passes gated on
  // "no glide running or armed" and spread one per callback. See
  // timelineQuietWork.ts and docs/architecture/scroll-arbitration-plan.md.
  const quietWork = createTimelineQuietWork({
    isTest: IS_TEST,
    autoScrollInFlight: () => stick.autoScrollInFlight(),
    passes: [
      // The settle-deferred recent-window prune. The store records the
      // debt (`hasDeferredRecentWindowPrune`); running it here — never
      // at wire settle — is what keeps the head-drop's flush (the most
      // expensive in the app) out of the reveal drain's glide.
      {
        key: 'recent-window-prune',
        when: 'quiet',
        run: () => {
          if (!pane.hasDeferredRecentWindowPrune) return false;
          const revision = pane.timelineRevision;
          pane.retryDeferredRecentWindowPrune();
          return pane.timelineRevision !== revision;
        },
      },
      createTimelineActivityRunAutoCollapse({
        getPane: () => pane,
        getListRef: () => listRef,
        getRevealedNodes: () => revealedNodes,
        geometryReady: visibilityGeometry.ready,
      }),
      createTimelineRowUiPrune({
        getPane: () => pane,
        getListRef: () => listRef,
        getRevealedNodes: () => revealedNodes,
      }),
    ],
  });

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
    // The reset form, not the bare controller call: the incoming rows'
    // markdown has not typeset yet, so the settled-since-arm latch must
    // start this cycle false or the shortened quiet window would open on
    // a stale settle.
    armWarmup: armWarmupWithReset,
    autoScrollInFlight: () => stick.autoScrollInFlight(),
    canPreserveTimelineWindow: windowAnchor.canPreserveTimelineWindow,
    preserveViewportBottom: windowAnchor.preserveViewportBottom,
    stickToLatest: () => {
      void paging.jumpToLatest();
    },
    // The EXACT capture: a switch-away is a final edge, so it must not be
    // refused by the scroll cadence's rate bound.
    persistSizePriors: () => sizePriors.maybePersistSizePriors(),
  };

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

  // Content geometry: the virtualizer IS the source (see the template
  // comment on contentEl), and this is the ONLY consumer of it here.
  //
  // Subscription (the virtualizer has no geometry prop), declared AFTER
  // the attach effect above so it runs after it: a prop-delivered sample
  // that lands before `stick.attach` has an element is dropped by the
  // controller and then suppressed forever by the virtualizer's
  // field-by-field dedupe, because the tuple never changes again — a
  // populated first mount then sat at scrollTop=0 claiming the bottom.
  // The subscription replays this instance's current sample on
  // subscribe, so ordering is a contract instead of a race. The
  // `{#key pane.threadId}` remount makes each virtualizer a fresh
  // source; the teardown unsubscribes the old instance before the new
  // one is subscribed, and instance identity is what lets an identical
  // tuple from the new virtualizer through.
  $effect(() => {
    const list = listRef;
    if (!list) return;
    return list.subscribeContentGeometry((sample) => {
      stick.deliverContentGeometry(sample);
      // A hidden document invalidates the collapse gate's cached viewport.
      // This existing post-flush geometry delivery is the first trustworthy
      // point after resume; schedule exactly once when it clears the barrier.
      if (visibilityGeometry.noteGeometrySample()) quietWork.schedule();
      // Streaming growth and remeasure shift row offsets without a
      // scroll event; keep the rail's marker + in-view fill honest.
      // rAF-coalesced inside the rail, so bursts cost one recompute
      // per frame.
      navRail?.scheduleViewportSync();
    });
  });

  $effect(() => visibilityGeometry.install());

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
      // This observer targets the scroll surface, above every virtual row.
      // Resolve descendant user-message overflow here before row observers
      // process the same width reflow. A child observer that inserted its
      // toggle after the row delivery changed an already-delivered ancestor,
      // which Chromium reported as an undelivered ResizeObserver loop.
      userMessageOverflow.measureAll();
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

  // Quiet-work cadence: structural timeline changes (revision / reveal /
  // thread switch) plus scroll END — never per scroll frame. Each pass
  // recomputes its work fresh on every run, so transitions that ride no
  // structural bump (a lone settle flip) are picked up by the next
  // trigger; until then held state is only over-retained, never
  // prematurely dropped.
  $effect(() => {
    pane.threadId;
    pane.timelineRevision;
    pane.revealBoundary;
    // A run's mount window decides which of its children are retained, and it
    // moves without touching any of the above: relocating a window mounts a
    // different set of rows at the same node count and the same item list.
    // Without this the pass the signature is ready to accept is never
    // scheduled, so the window the reader left stays retained until an
    // unrelated outer scroll. Each bump is one deliberate action (a toggle, a
    // chunk, a jump), never a streaming delta. (New items also push settled
    // runs further from the tail, and an auto-collapse release bumps this
    // same revision — the follow-up pass then finds no held runs and costs
    // one Map scan.)
    pane.activityRuns.revision;
    // A turn settling with no drain behind it (tool-only turns) flips the
    // deferred-prune flag with no structural change or scroll write in
    // tow — the flag itself must be a trigger or the prune waits for the
    // next turn's churn.
    pane.hasDeferredRecentWindowPrune;
    quietWork.schedule();
  });

  $effect(() => {
    listRef;
    scrollEl;
    quietWork.schedule();
  });


  // ============================================================
  // Virtualizer scroll callbacks → snapshot persist
  // ============================================================
  // The native scroll listener bound by the controller drives intent.
  // Virtualizer's callbacks here are only for snapshot persistence so
  // back-button / thread-switch returns to the same place.

  function handleTimelineScroll(offset: number): void {
    restore.saveScrollSnapshot();
    // Both are O(1)-ish on this 60Hz path: noteScroll is a subtraction,
    // and the rail sync is rAF-coalesced binary-search math.
    jump.noteScroll(offset);
    navRail?.scheduleViewportSync();
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
    navRail?.scheduleViewportSync();
    // Scroll end is also how tail growth with no structural bump reaches
    // the quiet passes: the bottom pin's scrollTop writes end in a
    // synthesized scrollend once streaming goes quiet. (The scheduler's
    // own recheck timer covers the sentinel outliving that scrollend.)
    quietWork.schedule();
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

  let lastJumpEdge = '';
  $effect.pre(() => {
    restore.handleSwitchEdgePre(pane.scrollStateKey, pane.switchGeneration);
    // MessageTimeline survives a thread switch (only the virtualizer is
    // keyed), so a landing flash — or its pending settle watch — armed
    // on the previous thread must die here, not at unmount.
    const edge = `${pane.threadId ?? ''}#${pane.switchGeneration}`;
    if (edge !== lastJumpEdge) {
      lastJumpEdge = edge;
      jump.invalidate();
    }
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
    void restore.scrollToItem(req.itemId);
  });

  onDestroy(() => {
    jump.invalidate();
    quietWork.invalidate();
    restore.invalidateRestore();
    restore.saveSnapshotOnDestroy();
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
     The top-fade OVERLAY (sibling after the scroller) fades the first
     TOP_FADE_PX of content as it rises under the header (replacing the
     old hard top padding), stopping SCROLLBAR_SAFE_PX short of the
     right edge so the scrollbar never tints. It's a paint-only effect,
     so like the padding-bottom above it never changes
     scrollHeight/clientHeight/scrollTop and stays clear of the
     controller. See the TOP_FADE_PX comment for why it is not a mask
     on this element.
     The scroller draws NO native bar (`pane-scroll-surface` suppresses it;
     the styled `::-webkit-scrollbar` in app.css is a classic,
     space-consuming bar): the <OverlayScrollbar> sibling after the top
     fade is the scrollbar, consuming zero layout width in every state.
     That retires the `scrollbar-gutter: stable both-edges` reservation
     that used to hold the centered column still across the bar's
     appearance — with no bar there is nothing to reserve against, and
     the centered column cannot shift when content first overflows
     mid-stream. Intent is stated, not inferred: the overlay bar's
     gestures report through onUserScrollStart/onUserScrollEnd (the
     intent machine's geometric gutter hit test reads offsetWidth −
     clientWidth, which is now always 0 — dead on this surface by
     design). See the contract notes in OverlayScrollbar.svelte.
     Layout shape mirrors discussion/ChannelView.svelte
     (`relative flex h-full flex-col` + `flex-1 min-h-0 overflow-y-auto`)
     so the two surfaces stay in lockstep. -->
<div class="relative flex h-full flex-col">
  <div
    bind:this={scrollEl}
    class="pane-scroll-surface flex-1 min-h-0 overflow-y-auto focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-accent/25"
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
      <div class={`${ROW_SHELL_CLASSES} pt-8`} style={rowShellStyle}></div>
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
            {renderNode}
          />
        {:else if node.kind === 'read_group'}
          <ReadGroupRow {pane} group={node} />
        {:else if node.kind === 'activity_run'}
          <ActivityRun
            {pane}
            run={node}
            {depth}
            live={node.live}
            atTail={node.atTail}
            {renderNode}
          />
        {/if}
      {/snippet}

      <!-- The outer wrapper is the warm-up gate's hide target. contentEl is
           the virtualizer's stable mounted-row plane and the controller's
           registered geometry target (geometry itself is
           engine-sourced: the virtualizer's container has `contain:
           size; height: totalSize+'px'`, so the samples it publishes
           through the geometry subscription above report the content
           height the controller would otherwise have had to
           re-observe). {#key pane.threadId} forces the
           <TimelineVirtualizer> to remount on every thread switch so its
           engine resets with the timeline. `estimate` replays the
           previous visit's measured sizes when the priors validity key
           still matches (width + structure + expansion), so a revisited
           thread mounts at its final height instead of re-running the
           estimate→measure cascade — the thread-switch flicker. On any
           mismatch the estimate degrades per-row to the kind table /
           flat default. See utils/virtual/priors.ts and
           docs/architecture/frontend-scroll.md. -->
      <!-- Warm-up clears to no inline value, never `visible`: an explicit
           `visibility: visible` would override the inherited hidden that
           the compact screen swap keys on and paint the timeline over the
           thread list (found on-device 2026-09-04). -->
      <div style:visibility={hideContentForWarmup ? 'hidden' : undefined}>
        {#key pane.threadId}
        <TimelineVirtualizer
          bind:this={listRef}
          bind:renderPlane={contentEl}
          scrollRef={scrollEl}
          data={revealedNodes}
          estimate={sizePriors.rowEstimate}
          getKey={(node) => timelineNodeKey(node)}
          bufferSize={BUFFER_SIZE_PX}
          renderAll={IS_TEST}
          headerSize={pane.hasMoreHistory ? TIMELINE_LOAD_OLDER_HEADER_PX : TIMELINE_TOP_SPACER_PX}
          onscroll={handleTimelineScroll}
          onscrollend={handleTimelineScrollEnd}
          onCompensation={stick.applyEngineCompensation}
          applyScrollTarget={stick.applyScrollTarget}
          trackReadingAnchor={() => !stick.isAtBottom || stick.escapedFromLock}
        >
          {#snippet header()}
            <!-- The timeline header has stable identity and exact geometry.
                 Keeping it outside transcript rows prevents a prepend from
                 removing 60px from the retained former-first row. -->
            <div class={`h-full pt-6 ${ROW_SHELL_CLASSES}`} style={rowShellStyle}>
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
          {/snippet}
          {#snippet children(node: TimelineNode, index: number)}
            <!-- Outer per-row wrapper. We do NOT set data-item-id here:
                 only TimelineLeaf owns that attribute on its root. Structural
                 rows stay unanchored, and tests rely on the divider rendering
                 BEFORE the [data-item-id] node, not containing it.

                 The rail is NOT drawn here. Every row that sits on it is
                 wrapped into an `activity_run` by the last projection pass,
                 and the run draws one continuous border for the whole block
                 (ActivityRun.svelte) — which is also what makes the rail
                 clickable as a single collapse control. Rows that reach this
                 wrapper directly (prose, user messages, errors,
                 notifications, proposed plans) are exactly the ones that
                 never had a rail. -->
            <!-- `chat-row-pending-cut` is a pure class toggle on a wrapper
                 that already exists: dimming must not change any row's DOM
                 shape or write per-row state, or a virtualizer remeasure
                 lands in the middle of a destructive RPC. -->
            <div
              data-row-index={index}
              class="transition-opacity duration-200"
              class:mt-4={rows.rowDecorations.toolTextBoundaryIndexes.has(index)}
              class:chat-row-pending-cut={isPendingCutRow(node)}
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
                <!-- Row column: see rowShellStyle for the rail clearance
                     contract shared by every row-aligned surface. -->
                <div class={ROW_SHELL_CLASSES} style={rowShellStyle}>
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
                            {responsePillLabel(node)}{#if responseDuration}{' '}<span class="normal-case tabular-nums tracking-normal">{responseDuration}</span>{/if}
                          </span>
                          <span class="timeline-hairline flex-1" aria-hidden="true"></span>
                        {/if}
                      </div>
                    </div>
                  {/if}
                <div data-testid="message-timeline-node">
                  {@render renderNode(node, 1)}
                </div>
              </div>
            </div>
            </div>
          {/snippet}
        </TimelineVirtualizer>
        {#if pane.hasMoreNewer}
          <div
            class={`${ROW_SHELL_CLASSES} flex justify-center pb-6 pt-2`}
            style={rowShellStyle}
          >
            <div class="flex items-center gap-2 rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/80 px-2 py-1.5 shadow-sheet">
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

  <!-- Top-fade overlay (see the TOP_FADE_PX comment for why this is an
       overlay and not a mask on the scroller). `.scroll-top-fade` owns the
       one-pixel overdraw across the independently snapped compositor edge.
       It sits after the scroller in source order so it paints above content,
       before the jump-to-latest chip so the chip stays on top. -->
  <div
    aria-hidden="true"
    class="scroll-top-fade"
    style:right={`${SCROLLBAR_SAFE_PX}px`}
    style:--scroll-top-fade-depth={`${TOP_FADE_PX}px`}
    data-testid="message-timeline-top-fade"
  ></div>

  <!-- The pane's scrollbar. A sibling of the scroller (nothing that starts
       on the strip reaches the scroller on its own), consuming zero layout
       width so overflow transitions can never re-wrap the transcript.
       Gestures state intent through the callback pair, mirroring what the
       native-gutter hit test used to arm: a drag/track-click/strip-wheel
       escapes bottom-follow (which also bails any in-flight spring), and
       releasing at the bottom re-sticks exactly like the jump chip. Owner
       writes — the follow spring pinning per streamed chunk, and every
       preserveViewportBottom transaction's shrink-clamp + restore (those
       run under a pause lease, so isSticky is false exactly then) — keep
       the bar faded via positionOwnerDriven. -->
  <OverlayScrollbar
    target={scrollEl}
    contentGeometry={scrollbarGeometry}
    ariaLabel="Scroll message history"
    placement="inset-y-0 right-0.5 w-1.5"
    ownerDrivenPosition={() => stick.positionOwnerDriven}
    onUserScrollStart={() => stick.setEscapedFromLock(true)}
    onUserScrollEnd={(atBottom) => {
      if (atBottom) stick.forceStick();
    }}
  />

  <!-- Landing flash for explicit jumps (nav rail, jump-to-first): an
       instant teleport needs a "you landed here" cue. Overlay on the
       non-scrolling wrapper, matching the row column's centering, so no
       row gains a transition the timeline kill rule would have to
       carve out. Positioned once at landing; a real scroll cancels it
       (jump.noteScroll). Keyed on nonce so a repeat jump restarts the
       animation. Opacity-only, so it stays visible (just static) under
       prefers-reduced-motion. -->
  {#if jump.flash}
    {#key jump.flash.nonce}
      <div
        aria-hidden="true"
        class="pointer-events-none absolute inset-x-0"
        style:top={`${jump.flash.top}px`}
        style:height={`${jump.flash.height}px`}
        data-testid="timeline-jump-flash"
      >
        <div class={`${ROW_SHELL_CLASSES} h-full`} style={rowShellStyle}>
          <div
            class="nav-jump-flash h-full w-full rounded-lg bg-accent/10"
            style:animation-duration={`${JUMP_FLASH_DURATION_MS}ms`}
          ></div>
        </div>
      </div>
    {/key}
  {/if}

  <!-- Message navigation rail: tick pills in the left row padding, one
       per user message. Sibling of the scroller for the same C24 reason
       as the chip below; rowShellStyle derives its left edge from the
       rail's hit width plus a real dead gutter. -->
  {#if !compact}
    <MessageNavRail
      bind:this={navRail}
      {pane}
      nodes={revealedNodes}
      getListRef={() => listRef}
      onJumpToItem={(id) => {
        void jump.jumpToItem(id);
      }}
    />
  {/if}

  <!-- Visible when the user has escaped or is no longer near the bottom.
       Wiring this to `!isSticky` would also pop the chip during sidebar/
       drawer resize leases (pauseDepth > 0) even though the user is
       geometrically glued to the bottom. Anchored to the outer wrapper
       (which does not scroll), so the chip stays fixed in the visible
       area regardless of transcript scrollTop. -->
  <ScrollToBottomButton visible={!stick.isAtBottom || pane.hasMoreNewer} onClick={() => { void paging.jumpToLatest(); }} />
</div>

<style>
  /* Honest pending state for a revert that is actually running: the rows
     the backend is about to destroy recede, but stay readable and stay
     interactive — nothing has been truncated yet, and pretending
     otherwise would be the optimistic lie this flow exists to avoid. */
  .chat-row-pending-cut {
    opacity: 0.42;
  }

  /* Landing-flash fade: hold briefly so the eye finds the landing, then
     dissolve. Duration comes inline from JUMP_FLASH_DURATION_MS so the
     removal timer and the animation cannot drift. */
  .nav-jump-flash {
    animation-name: nav-jump-flash-fade;
    animation-timing-function: ease-out;
    animation-fill-mode: forwards;
  }
  @keyframes nav-jump-flash-fade {
    0% {
      opacity: 1;
    }
    35% {
      opacity: 1;
    }
    100% {
      opacity: 0;
    }
  }
</style>
