// Per-thread scroll-snapshot save/restore session for MessageTimeline.
// Owns the restore-session bookkeeping (`restoredThreadId`,
// `scrollSnapshotThreadId`/`scrollSnapshotSwitchGeneration`,
// `restoreToken`) that both the switch-edge `$effect.pre` and the
// restore `$effect` in MessageTimeline.svelte read and write, plus the
// snapshot save/restore/scroll-to-item flows that consume it. Modules
// 2-4 (timelineSizePriors, timelinePaging, timelineWindowAnchor) read
// the session through `restoredThreadId`/`nextRestoreToken`/
// `isRestoreTokenCurrent` rather than owning their own copy.
//
// `pane` can be swapped at runtime (see options.getPane), so nothing
// here may capture a `ThreadPane` reference at construction time.
//
// The discussion surface (components/discussion/ChannelView.svelte)
// hand-mirrors a simplified form of this module's arm-restore-snap →
// initial-load → forceStick({reason:'restore'}) choreography — no
// snapshot save/restore or scroll-to-item, just the switch-edge consent
// arming and the post-load restore stick. If that sequence's contract
// changes here, check ChannelView's mount effect too.

import { tick } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { addToast } from '../../stores/toast.svelte';
import {
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
  type ScrollSnapshot,
} from '../../utils/threadScrollSnapshots';
import { revealActivityRunItem } from '../../utils/activityRunWindow';
import { captureTimelineAnchor } from './timelineScroll';
import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';

export interface TimelineRestoreOptions {
  getPane(): ThreadPane;
  stick: UseStickToBottomController;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getScrollEl(): HTMLDivElement | undefined;
  getRevealedNodes(): TimelineNode[];
  getGroupedNodes(): TimelineNode[];
  findTimelineNodeIndex(itemId: string): number;
  /**
   * Wired to module 2's `maybePersistSizePriorsInterim` — the RATE-BOUND
   * capture. This one rides the scroll cadence, which fires per frame.
   */
  persistSizePriors(): void;
  /**
   * Wired to module 2's `maybePersistSizePriors` — the exact capture, for
   * the final edges that must not be refused by that rate bound.
   */
  persistSizePriorsExact(): void;
  armWarmupWithReset(): void;
  /** Wired to module 3's `resetGates` (both auto-load gates' `.reset()`). */
  resetAutoLoadGates(): void;
  /** Wired to module 4's `clearTimelineWindowPruneShift`. */
  clearTimelineWindowPruneShift(): void;
}

export interface TimelineRestore {
  /** Reactive — read in the restore `$effect` and the listRef-bind trace. */
  readonly restoredThreadId: string | null;
  nextRestoreToken(): number;
  isRestoreTokenCurrent(token: number): boolean;
  invalidateRestore(): void;
  saveScrollSnapshot(): void;
  handleSwitchEdgePre(nextThreadId: string | null, nextSwitchGeneration: number): void;
  maybeRestoreAfterFlush(): void;
  scrollToItem(id: string): Promise<void>;
  saveSnapshotOnDestroy(): void;
}

export function createTimelineRestore(options: TimelineRestoreOptions): TimelineRestore {
  let restoredThreadId: string | null = $state(null);
  // Tracks which thread we last persisted into the snapshot store via
  // the thread-switch effect — separate from `restoredThreadId` so a
  // thread switch can dispose the previous snapshot before the next
  // restore completes.
  let scrollSnapshotThreadId: string | null = $state(null);
  // Last observed `pane.switchGeneration`. Paired with
  // `scrollSnapshotThreadId` so the restore-effect.pre reset path fires
  // on same-thread re-switch (a forced reload calls
  // `pane.switchThread(currentThread)` to reload items in place; the
  // thread id doesn't change but the generation counter bumps). Without
  // this discriminator, revert leaves `restoredThreadId === threadId`
  // and the restore $effect early-returns — the viewport sticks at
  // scrollTop=0 with the "Load older messages" banner visible. The
  // sentinel `-1` makes the first effect run a no-op for the
  // `if (scrollSnapshotThreadId)` branch (since scrollSnapshotThreadId
  // is null on mount, no restoredThreadId reset is needed).
  let scrollSnapshotSwitchGeneration = -1;
  // Token bumped on every external "interrupt" — thread switch, user
  // scroll, programmatic scrollToItem — so async restore work can detect
  // staleness and bail.
  let restoreToken = 0;

  function snapshotThreadId(): string | null {
    return options.getPane().threadId || null;
  }

  function saveScrollSnapshot(): void {
    const threadId = snapshotThreadId();
    if (!threadId) return;
    saveScrollSnapshotForThread(threadId);
    // Refresh the size priors on the same triggers as the scroll position
    // snapshot — restore, scroll, load-older settle. Size-gated, so it only
    // re-slices when the cascade actually grew the surface, and rate-bounded
    // on top of that because the bottom-pin re-pin fires one of these per
    // frame while the tail streams.
    //
    // Which means this is NOT what makes the outgoing thread's priors fresh
    // at switch time: a capture landing inside the interim cooldown is
    // skipped, so the last one before a switch can be. The final edges
    // capture exactly instead — `switchThread` asks the timeline directly
    // through the controller adapter, and `saveSnapshotOnDestroy` below
    // covers unmount.
    options.persistSizePriors();
  }

  function saveScrollSnapshotForThread(threadId: string): void {
    // The `restoredThreadId !== threadId` guard already covers the
    // "ignore scroll events fired before restoration" case — once
    // restoration runs (which happens as soon as the timeline has
    // items, including the cache-hit fast path), saves are allowed.
    // No separate loading check is needed.
    const listRef = options.getListRef();
    if (!listRef || restoredThreadId !== threadId) return;
    if (options.stick.isAtBottom) {
      setThreadScrollSnapshot(threadId, { kind: 'bottom' });
      return;
    }
    const offset = listRef.getScrollOffset();
    // Negative when the anchor row's top has scrolled above the viewport
    // top by `-offsetTop` pixels. Restoration recreates exactly this
    // relationship via scrollToIndex({ align:'start', offset: -offsetTop }).
    const anchor = captureTimelineAnchor(options.getRevealedNodes(), listRef, offset, {
      clampIndex: true,
    });
    if (!anchor) return;
    setThreadScrollSnapshot(threadId, { kind: 'anchor', ...anchor });
  }

  // Reset restoration tracking on thread change BEFORE the new thread's
  // effects run, AND suspend auto-follow until restoreToBottom (or
  // restoreAnchor) takes over. Setting escapedFromLockState=true synchronously here
  // freezes the controller until the new thread's restoration runs.
  //
  // We do NOT call saveScrollSnapshotForThread for the outgoing thread
  // here. By the time this effect runs, switchThread has already mutated
  // pane.items to the incoming thread's cached items — so
  // listRef.findItemIndex would return an index in the WRONG thread's
  // array, and the saved anchor would carry the incoming thread's item
  // id under the outgoing thread's snapshot key. The continuous
  // scroll-event-driven saves (handleTimelineScroll, handleTimelineScrollEnd)
  // already keep the outgoing thread's snapshot fresh — the most recent
  // user scroll IS the snapshot.
  function handleSwitchEdgePre(nextThreadId: string | null, nextSwitchGeneration: number): void {
    // Same-thread re-switch (forced in-place reload) keeps
    // `pane.threadId` constant but bumps `pane.switchGeneration`. We
    // need the reset path to run in that case too — otherwise
    // `restoredThreadId` stays equal to `threadId`, the restore
    // $effect early-returns, and the viewport sticks at scrollTop=0.
    const threadIdChanged = scrollSnapshotThreadId !== nextThreadId;
    const switchGenerationChanged = scrollSnapshotSwitchGeneration !== nextSwitchGeneration;
    if (threadIdChanged || switchGenerationChanged) {
      if (isUiRenderTraceEnabled()) {
        const scrollEl = options.getScrollEl();
        const pane = options.getPane();
        recordUiTrace('timeline.restore.effectPre', {
          oldThreadId: scrollSnapshotThreadId,
          newThreadId: nextThreadId,
          oldSwitchGeneration: scrollSnapshotSwitchGeneration,
          newSwitchGeneration: nextSwitchGeneration,
          sameThreadReswitch: !threadIdChanged && switchGenerationChanged,
          scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
          scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
          clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
          paneItems: pane.items.length,
          paneLoading: pane.loading,
        });
      }
      if (scrollSnapshotThreadId) {
        restoredThreadId = null;
        restoreToken += 1;
      }
      options.resetAutoLoadGates();
      options.clearTimelineWindowPruneShift();
      if (nextThreadId && scrollSnapshotThreadId) {
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
        options.armWarmupWithReset();
        // Sets the defensive escape (freezing auto-follow against the
        // outgoing thread's geometry) and arms the one-shot restore-snap
        // consent for the upcoming `restoreToBottom() →
        // stick.forceStick({reason: 'restore'})`. Any outer-scroll intent
        // between this point and the restore $effect (extremely rare;
        // both run inside the same flush) re-clears the arm, causing the
        // restore to NO-OP and preserving the user's intent — the
        // load-bearing distinguisher between "the user has explicitly
        // escaped" and "this $effect.pre just defensively set escape=true
        // while preparing the new thread for restore." See
        // utils/scroll/intent.ts § Restore-snap consent.
        options.stick.armRestoreSnap();
      } else if (nextThreadId) {
        // Placeholder → materialized transition: the timeline was empty
        // so there is no measurement cascade to hide. Skip the warm-up
        // gate so the optimistic user message renders immediately.
        options.stick.skipWarmup();
        options.stick.markAtBottom();
      } else {
        // Draft / placeholder transition (pane.threadId === null when a
        // draft placeholder is active or the pane has no thread): the
        // restore $effect short-circuits on `!threadId`, so the
        // defensive escape would never be cleared and the
        // scroll-to-bottom chip would appear over the empty "No messages
        // yet" placeholder. There is no content to anchor against, no
        // measurement cascade to hide, and no restore to gate — flip the
        // controller directly back to sticky-bottom.
        options.stick.markAtBottom();
      }
    }
    scrollSnapshotThreadId = nextThreadId;
    scrollSnapshotSwitchGeneration = nextSwitchGeneration;
  }

  function maybeRestoreAfterFlush(): void {
    const pane = options.getPane();
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
    const groupedNodes = options.getGroupedNodes();
    const hasTimelineRows = groupedNodes.length > 0;
    const listRef = options.getListRef();
    if (hasTimelineRows && !listRef) return;
    restoredThreadId = threadId;
    // Branch synchronously on snapshot kind. The bottom branch only
    // needs scrollEl (forceStick) — running it inline keeps the
    // controller's pauseAutoScroll lease and the `await tick()`
    // microtask boundary out of the critical "switch in → land at
    // bottom" path, so the incoming thread's first paint already sits
    // at the bottom. (Under virtua this also defused a deferred
    // scroller-attach race reading the outgoing thread's carry-over
    // scrollTop; the bespoke virtualizer attaches synchronously, but
    // the paint-ordering reason stands.)
    const snap = getThreadScrollSnapshot(threadId);
    if (isUiRenderTraceEnabled()) {
      const scrollEl = options.getScrollEl();
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
  }

  // Bottom restore. Two cases:
  //
  // - Empty timeline (no rows yet): just flip the controller's intent
  //   flag so the first streamed row's contentRO sync-pin lands at the
  //   bottom. There's no scrollTop to write yet.
  //
  // - Non-empty timeline: forceStick() lands scrollTop at the current
  //   target in a single write. Any subsequent contentEl growth from
  //   svelte-streamdown's
  //   async typesetting (shiki / KaTeX / mermaid /
  //   parseIncompleteMarkdown rebalance) and from the virtualizer's
  //   per-row measurements refining row heights gets handled invisibly
  //   by the controller's contentRO sync-pin path: each positive delta
  //   re-pins to the new bottom inside the RO callback, before paint.
  //
  // Don't pair `scrollToIndex(last, 'end')` with `markAtBottom()` here
  // — they create two writers (the index-scroll convergence + our
  // sync-pin) targeting slightly different scrollTop values for the
  // same content-grow trigger, and they oscillate. forceStick() alone
  // is the single writer.
  //
  // The trailing rAF `observe('content')` is a defensive late-settling
  // re-pin: composer-height RO updates flowing into scrollEl's
  // padding-bottom, per-row measurements firing a frame
  // after mount, and the first burst of Streamdown async typesetting
  // can all change geometry one frame after the initial forceStick.
  // Padding-only changes don't re-fire contentRO (W3C ResizeObserver
  // observes content-box) so a paint-time settle that nudges the bottom
  // by a few px would otherwise leave the user "half a tick" above
  // bottom. The content observation is escape-aware (bails if the user
  // gestured up between frames) so it can't yank them.
  function restoreToBottom(): void {
    // Last RENDERED row (the virtualizer's `data` is `revealedNodes`). If a
    // stream event set the reveal gate between switch and restore, the true
    // last index is the revealed one — scrolling to a withheld index would
    // land out of the engine's range.
    const revealedNodes = options.getRevealedNodes();
    const lastIndex = revealedNodes.length - 1;
    const scrollEl = options.getScrollEl();
    if (isUiRenderTraceEnabled()) {
      recordUiTrace('timeline.restore.bottom.entry', {
        threadId: restoredThreadId,
        lastIndex,
        groupedNodesLength: revealedNodes.length,
        hasListRef: options.getListRef() !== undefined,
        hasScrollEl: scrollEl !== undefined,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
      });
    }
    if (lastIndex < 0) {
      options.stick.markAtBottom();
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
    // reason:'restore' so the controller's consent gate filters this
    // call. The matching `armRestoreSnap()` runs from `$effect.pre`
    // above; if anything cleared the consent between then and now
    // (outer-scroll intent, selection, or programmatic escape), this
    // NO-OPs and the user's scroll position is preserved. This is what defends
    // against the seq-509 stale-restore bug — a `restoreToBottom()`
    // mistakenly firing without a real thread switch can no longer
    // slam the user to the bottom and wipe their escape.
    options.stick.forceStick({ reason: 'restore' });
    saveScrollSnapshot();
    // Capture the thread the rAF was scheduled for so a thread switch
    // between forceStick and the next frame doesn't run the late re-pin
    // against the new thread's geometry. The content observation also
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
      options.stick.observe('content');
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
    const release = options.stick.pauseAutoScroll();
    try {
      await tick();
      const pane = options.getPane();
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
      const groupedNodes = options.getGroupedNodes();
      if (groupedNodes.length > 0 && !options.getListRef()) {
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
      if (token !== restoreToken || options.getPane().threadId !== threadId) return;
      if (!found) {
        restoreToBottom();
        return;
      }
      await tick();
      const listRef = options.getListRef();
      if (token !== restoreToken || options.getPane().threadId !== threadId || !listRef) return;
      const idx = options.findTimelineNodeIndex(snap.itemId);
      const scrollEl = options.getScrollEl();
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
      // The anchor restore is a mid-thread position: escape bottom
      // follow (as any explicit navigation does), then jump. The write
      // itself is chokepoint-tagged via applyScrollTarget.
      options.stick.setEscapedFromLock(true);
      listRef?.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      saveScrollSnapshot();
    } finally {
      release();
    }
  }

  // ============================================================
  // Scroll-to-item (search hits, plan rows, tray rows)
  // ============================================================

  async function scrollToItem(id: string): Promise<void> {
    const listRef = options.getListRef();
    if (!listRef || !id) return;
    const myToken = ++restoreToken;
    const pane = options.getPane();
    const found = await pane.loadUntilItem(id);
    if (myToken !== restoreToken || !options.getListRef()) return;
    if (!found) {
      addToast('warning', 'Message is no longer in this thread');
      return;
    }
    await tick();
    if (myToken !== restoreToken || !options.getListRef()) return;
    let idx = options.findTimelineNodeIndex(id);
    if (idx < 0) return;
    let targetNode = options.getRevealedNodes()[idx];
    if (targetNode?.kind === 'activity_run') {
      // The row is the RUN, and the target may be collapsed into its chip or
      // outside its mount window — so the run is pointed at the item before
      // the outer scroll, and the outer scroll then measures the height that
      // produced. Its own row consumes the focus request once mounted, which
      // is what makes the order here safe: the run need not be on screen yet.
      revealActivityRunItem(pane.activityRuns, targetNode, id);
      await tick();
      if (myToken !== restoreToken || !options.getListRef()) return;
      // Expanding a chip re-measures every row after it, so the index is
      // re-resolved rather than reused.
      idx = options.findTimelineNodeIndex(id);
      if (idx < 0) return;
      targetNode = options.getRevealedNodes()[idx];
    }
    // Explicit navigation: escape bottom follow, then jump (the write is
    // chokepoint-tagged via applyScrollTarget).
    options.stick.setEscapedFromLock(true);
    options.getListRef()?.scrollToIndex(idx, { align: 'center' });
  }

  function nextRestoreToken(): number {
    return ++restoreToken;
  }

  function isRestoreTokenCurrent(token: number): boolean {
    return token === restoreToken;
  }

  function invalidateRestore(): void {
    restoreToken += 1;
  }

  function saveSnapshotOnDestroy(): void {
    if (!restoredThreadId) return;
    saveScrollSnapshotForThread(restoredThreadId);
    // Unmount is a final edge — the pane is closing or being replaced, and
    // nothing after this will capture. Exact, for the same reason the
    // switch-away edge is: the rate bound exists to thin a per-frame
    // cadence, and there is no cadence left here to thin.
    options.persistSizePriorsExact();
  }

  return {
    get restoredThreadId() {
      return restoredThreadId;
    },
    nextRestoreToken,
    isRestoreTokenCurrent,
    invalidateRestore,
    saveScrollSnapshot,
    handleSwitchEdgePre,
    maybeRestoreAfterFlush,
    scrollToItem,
    saveSnapshotOnDestroy,
  };
}
