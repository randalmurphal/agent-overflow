// Per-thread scroll-snapshot save/restore session for MessageTimeline.
// Owns the restore-session bookkeeping (`restoredThreadId`, the
// switch-edge state machine's `RestoreEdge`, the navigation and hold
// tokens) that both the switch-edge `$effect.pre` and the restore
// `$effect` in MessageTimeline.svelte read and write, plus the snapshot
// save/restore/scroll-to-item flows that consume it. Modules
// 2-4 (timelineSizePriors, timelinePaging, timelineWindowAnchor) read
// the session through `restoredThreadId` and the token methods rather
// than owning their own copy.
//
// `pane` can be swapped at runtime (see options.getPane), so nothing
// here may capture a `ThreadPane` reference at construction time.
//
// The discussion surface (components/discussion/ChannelView.svelte)
// hand-mirrors a simplified form of this module's arm-restore-snap →
// initial-load → forceStick({reason:'restore'}) choreography — no
// snapshot save/restore or scroll-to-item, just the switch-edge consent
// arming and the post-load restore stick. If that sequence's contract
// changes here, check ChannelView's `beginInitialLoad` too (its ONE
// entry: the channel edge and the error banner's Retry both go through
// it, because a retry that armed nothing spent a consent the failed
// attempt had already consumed).

import { tick } from 'svelte';
import type {
  PaneSession,
  TimelineSource,
  TimelineWindow,
  ScrollHost,
} from '../../stores/threadPaneRoles';
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
import { captureTimelineAnchor, type ResolvedTimelineNode } from './timelineScroll';
import { isUiRenderTraceEnabled, recordUiTrace } from '../../utils/uiRenderTrace';
import { reportFrontendDiagnostic } from '../../utils/frontendErrorCapture';
import type { LoadUntilItemResult } from '../../stores/threadPaneShared';

export interface TimelineRestoreOptions {
  getPane(): PaneSession & TimelineSource & TimelineWindow & ScrollHost;
  stick: UseStickToBottomController;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getScrollEl(): HTMLDivElement | undefined;
  getRevealedNodes(): TimelineNode[];
  getGroupedNodes(): TimelineNode[];
  windowVerified(): boolean;
  /** The node of `nodes` that shows `itemId` (`resolveVisibleTimelineNode`). */
  resolveTimelineNode(itemId: string, nodes: readonly TimelineNode[]): ResolvedTimelineNode | null;
  /**
   * Wired to module 2's `maybePersistSizePriorsInterim` — the RATE-BOUND
   * capture. This one rides the scroll cadence, which fires per frame.
   */
  persistSizePriors(): void;
  /**
   * Wired to module 2's `persistSizePriorsFinal`, the final-edge capture,
   * refused by neither the rate bound nor the total-size gate.
   */
  persistSizePriorsExact(): void;
  armWarmupWithReset(): void;
  /** Wired to module 3's `resetGates` (both auto-load gates' `.reset()`). */
  resetAutoLoadGates(): void;
}

/**
 * What the switch-edge `$effect.pre` last observed on this surface. The
 * classification has to be EXHAUSTIVE over transitions, which is why it
 * is a union rather than the nullable thread id + `-1` generation
 * sentinel it replaced: that shape could not tell a FIRST MOUNT of an
 * ALREADY-POPULATED pane (a page refresh with a restored pane layout, or
 * an existing thread opened in a new pane) apart from a
 * placeholder→materialized transition. Both read as "no previous thread
 * id", so the first mount took the optimistic branch (`skipWarmup()` +
 * `markAtBottom()`) and never armed the restore snap — the later
 * `forceStick({reason:'restore'})` was then correctly refused by the
 * controller's consent gate, leaving a tail-seeded transcript rendered at
 * scrollTop=0 under a sticky-bottom claim (production incident
 * 2026-08-29).
 *
 * `unseen` is the pre-first-edge state and is never re-entered, so it is
 * never equal to anything — the first edge always acts.
 */
type RestoreEdge =
  | { kind: 'unseen' }
  | { kind: 'placeholder'; generation: number }
  | { kind: 'thread'; key: string; generation: number };

function isSameRestoreEdge(a: RestoreEdge, b: RestoreEdge): boolean {
  if (a.kind === 'placeholder' && b.kind === 'placeholder') {
    return a.generation === b.generation;
  }
  if (a.kind === 'thread' && b.kind === 'thread') {
    return a.key === b.key && a.generation === b.generation;
  }
  return false;
}

/** How an explicit jump ended; see `scrollToItem`. */
type JumpOutcome = 'issued' | 'withheld' | 'missing' | 'failed' | 'superseded' | 'cancelled' | 'unresolved';

/** Lookups a jump repeats while window cuts keep superseding them. */
const JUMP_LOOKUP_ATTEMPTS = 3;

export interface TimelineRestore {
  /** Reactive — read in the restore `$effect` and the listRef-bind trace. */
  readonly restoredThreadId: string | null;
  /** Start a navigation: ends every older one. */
  beginNavigation(): number;
  /**
   * Whether the navigation still owns the viewport. When it does, ends
   * every hold, so call it last, right before the navigation's write.
   */
  claimNavigation(token: number): boolean;
  /** Start a viewport hold: ends older holds, never a navigation. */
  beginHold(): number;
  isHoldCurrent(token: number): boolean;
  /** Reader input on the scroller: an explicit jump still loading yields to it. */
  noteReaderGesture(): void;
  /** Teardown: ends every navigation and hold. */
  invalidateRestore(): void;
  saveScrollSnapshot(): void;
  handleSwitchEdgePre(nextThreadId: string | null, nextSwitchGeneration: number): void;
  maybeRestoreAfterFlush(): void;
  scrollToItem(id: string): Promise<boolean>;
  saveSnapshotOnDestroy(): void;
}

export function createTimelineRestore(options: TimelineRestoreOptions): TimelineRestore {
  let restoredThreadId: string | null = $state(null);
  // The last edge the switch-edge `$effect.pre` observed — the identity
  // (thread key or placeholder) AND `pane.switchGeneration`, because a
  // forced in-place reload calls `pane.switchThread(currentThread)` to
  // replace items without changing the thread id; only the generation
  // moves. Without that discriminator, revert leaves
  // `restoredThreadId === threadId`, the restore $effect early-returns,
  // and the viewport sticks at scrollTop=0 with the "Load older
  // messages" banner visible.
  //
  // Deliberately NOT `$state`: nothing outside `handleSwitchEdgePre`
  // reads it, and its only reader was the caller's own `$effect.pre`,
  // which the write then re-ran for nothing.
  let edge: RestoreEdge = { kind: 'unseen' };
  // Two lifetimes, so housekeeping cannot cancel what the reader asked
  // for. A navigation (switch edge, anchor restore, scrollToItem, jump to
  // latest, manual load newer) takes `navigationToken`, and its async work
  // resumes only while that is current. A viewport hold
  // (`preserveViewportBottom`) takes `holdToken`. A newer hold ends an
  // older one; a hold never ends a navigation. A navigation ends every
  // hold when it claims the viewport for its write, so a hold that started
  // during its awaits cannot move the viewport after it lands. The switch
  // edge and teardown end both.
  let navigationToken = 0;
  let holdToken = 0;
  // Bumped by reader input on the scroller. An explicit jump that is still
  // loading its row yields to it rather than yank the reader afterwards.
  let readerGestureEpoch = 0;

  function endAll(): void {
    navigationToken += 1;
    holdToken += 1;
  }

  // The restore/snapshot IDENTITY for this timeline surface. The base
  // pane's scrollStateKey IS its thread id; a scoped facade
  // (agentScopeView) answers a per-scope key so its snapshots and
  // restore bookkeeping never collide with the main timeline's for the
  // same thread. Every identity compare in this module uses the key —
  // pane.threadId appears only where a REAL thread id is required
  // (loadUntilItem, trace payloads' pane fields).
  function snapshotThreadId(): string | null {
    return options.getPane().scrollStateKey || null;
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
    // A scroll event inside the restore window is not the reader's
    // position: it is the switch's own motion. The anchor path sets
    // `restoredThreadId` before its awaits, so the guard above no longer
    // covers it, and saving here would overwrite the incoming thread's
    // snapshot with wherever the transaction happens to be mid-flight.
    if (options.stick.restorePending) return;
    if (options.stick.isAtBottom && !options.getPane().hasMoreNewer) {
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
    const previous = edge;
    const next: RestoreEdge =
      nextThreadId === null
        ? { kind: 'placeholder', generation: nextSwitchGeneration }
        : { kind: 'thread', key: nextThreadId, generation: nextSwitchGeneration };
    edge = next;
    if (isSameRestoreEdge(previous, next)) return;

    if (isUiRenderTraceEnabled()) {
      const scrollEl = options.getScrollEl();
      const pane = options.getPane();
      recordUiTrace('timeline.restore.effectPre', {
        oldThreadId: previous.kind === 'thread' ? previous.key : null,
        newThreadId: nextThreadId,
        // `-1` for the pre-first-edge state, matching the sentinel the
        // nullable form used to carry, so trace consumers keep reading
        // one shape.
        oldSwitchGeneration: previous.kind === 'unseen' ? -1 : previous.generation,
        newSwitchGeneration: nextSwitchGeneration,
        sameThreadReswitch:
          previous.kind === 'thread' && next.kind === 'thread' && previous.key === next.key,
        // The transition itself — the thing the branch below keys on.
        edgeTransition: `${previous.kind}->${next.kind}`,
        scrollTop: scrollEl ? Math.round(scrollEl.scrollTop) : null,
        scrollHeight: scrollEl ? Math.round(scrollEl.scrollHeight) : null,
        clientHeight: scrollEl ? Math.round(scrollEl.clientHeight) : null,
        paneItems: pane.items.length,
        paneLoading: pane.loading,
      });
    }

    // A thread we had already observed owns a restore session: end it
    // before the next one starts, so a switch can dispose the previous
    // restore before the incoming one completes. Nothing to end from
    // `unseen` (restoredThreadId is still null) or from a placeholder
    // (it never restored).
    if (previous.kind === 'thread') {
      restoredThreadId = null;
      endAll();
    }
    options.resetAutoLoadGates();

    if (next.kind === 'placeholder') {
      // → draft / placeholder (pane.threadId === null when a draft
      // placeholder is active or the pane has no thread): the restore
      // $effect short-circuits on `!threadId`, so the defensive escape
      // would never be cleared and the scroll-to-bottom chip would
      // appear over the empty-thread greeting. There is no
      // content to anchor against, no measurement cascade to hide, and
      // no restore to gate — flip the controller directly back to
      // sticky-bottom.
      options.stick.markAtBottom();
      return;
    }

    if (previous.kind === 'placeholder') {
      // Placeholder → materialized transition: the timeline was empty
      // so there is no measurement cascade to hide. Skip the warm-up
      // gate so the optimistic user message renders immediately.
      options.stick.skipWarmup();
      options.stick.markAtBottom();
      return;
    }

    // `unseen` → thread (first mount of a pane that may already be
    // populated) and thread → thread (a switch, or the same key with a
    // new generation: the forced in-place reload) take the same, full
    // restore choreography. A first mount is NOT a placeholder
    // materialization: the pane can carry a restored layout's thread
    // with its whole cached window, which mounts an estimate→measure
    // cascade and needs a real restore.
    //
    // Re-arm the warm-up gate BEFORE the DOM update flushes. The restore
    // $effect calls forceStick() (which also arms the gate), but that
    // runs AFTER DOM update — so without this $effect.pre reset, the
    // first paint of the new thread would inherit the outgoing thread's
    // settled `isWarm=true`, making hideContentForWarmup=false during
    // the new thread's measurement cascade. attach() can't carry this
    // load: scrollEl/contentEl don't change across switches
    // (MessageTimeline isn't keyed on threadId), so the attach $effect
    // early-returns. This was the flaky-fix bug: cache-miss switches off
    // a long-settled prior thread reproduced the visible "lands wrong,
    // jumps to correct" sequence; cache-miss switches off an unsettled
    // prior thread (warm=false coincidentally) hid the cascade and
    // looked fine. On a first mount the warm gate settles by
    // markdown-absence for an empty pane, so arming it here cannot wedge
    // one.
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
  }

  function maybeRestoreAfterFlush(): void {
    const pane = options.getPane();
    const threadId = pane.scrollStateKey;
    const itemsLength = pane.items.length;
    const loading = pane.loading;
    if (!threadId) return;
    if (restoredThreadId === threadId) return;
    if (!options.windowVerified()) return;
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
    void restoreAnchor(threadId, snap, beginNavigation());
  }

  // Bottom restore. Two cases:
  //
  // - Empty timeline (no rows yet): just flip the controller's intent
  //   flag so the first streamed row's contentRO sync-pin lands at the
  //   bottom. There's no scrollTop to write yet.
  //
  // - Non-empty timeline: forceStick() lands scrollTop at the current
  //   target in a single write. Any subsequent contentEl growth from
  //   markdown async typesetting (shiki / KaTeX / mermaid /
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
      if (token !== navigationToken || pane.scrollStateKey !== threadId) {
        if (isUiRenderTraceEnabled()) {
          recordUiTrace('timeline.restore.anchor.bail', {
            threadId,
            token,
            currentRestoreToken: navigationToken,
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

      const loaded = await pane.loadUntilItem(snap.itemId);
      if (isUiRenderTraceEnabled()) {
        recordUiTrace('timeline.restore.anchor.loaded', {
          threadId,
          token,
          loaded,
          itemId: snap.itemId,
        });
      }
      if (token !== navigationToken || options.getPane().scrollStateKey !== threadId) return;
      if (loaded === 'superseded') return;
      if (loaded !== 'loaded') {
        // The snapshot's row is gone (or its load failed and said so):
        // the bottom is the only position left to restore to.
        restoreToBottom();
        return;
      }
      await tick();
      const listRef = options.getListRef();
      if (token !== navigationToken || options.getPane().scrollStateKey !== threadId || !listRef) return;
      const idx = options.resolveTimelineNode(snap.itemId, options.getRevealedNodes())?.index ?? -1;
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
      if (!claimNavigation(token)) return;
      // The anchor restore is a mid-thread position: escape bottom
      // follow (as any explicit navigation does), then jump. The write
      // itself is chokepoint-tagged via applyScrollTarget.
      options.stick.markEscaped();
      listRef?.scrollToIndex(idx, { align: 'start', offset: -snap.offsetTop });
      saveScrollSnapshot();
    } finally {
      release();
      // Every bail path above must consume the consent: left armed, the
      // chip stays hidden and the controller keeps refusing non-restore
      // placements. The token guard is because a newer restore may
      // already have armed its own consent, which this one must not clear.
      if (token === navigationToken) options.stick.clearRestoreConsent();
    }
  }

  // ============================================================
  // Scroll-to-item (search hits, nav rail)
  // ============================================================

  /**
   * Returns whether the jump issued its scroll. The nav rail's landing
   * flash keys on that answer, so a jump that did not issue cannot flash
   * and cannot cancel a newer jump's flash.
   *
   * Every call ends in one outcome, traced as `timeline.jump`:
   * - `issued` / `withheld`: the write went out, to the row or, for a row
   *   the reveal gate still withholds, to the last revealed row.
   * - `missing`: the row is gone from the thread (warning toast).
   * - `failed`: the window load failed and has already reported it.
   * - `superseded`: a newer navigation or a switch owns the viewport.
   * - `cancelled`: the reader moved the scroller while the row loaded.
   * - `unresolved`: the row loaded and nothing renders it (warning toast
   *   and a diagnostic: a defect, never an expected state).
   */
  async function scrollToItem(id: string): Promise<boolean> {
    if (!id) return false;
    const token = beginNavigation();
    const pane = options.getPane();
    const key = pane.scrollStateKey;
    const generation = pane.switchGeneration;
    const gestures = readerGestureEpoch;
    // This navigation replaces the switch's restore, whose consent would
    // otherwise stay armed past a restore this cut short.
    options.stick.clearRestoreConsent();
    const interruption = (): JumpOutcome | null => {
      const current = options.getPane();
      if (token !== navigationToken || current.scrollStateKey !== key
        || current.switchGeneration !== generation) return 'superseded';
      return gestures === readerGestureEpoch ? null : 'cancelled';
    };

    // A window cut or a reload of pending pages can supersede the lookup
    // while this navigation still owns the viewport; only those retry.
    let loaded: LoadUntilItemResult = 'superseded';
    for (let attempt = 0; attempt < JUMP_LOOKUP_ATTEMPTS && loaded === 'superseded'; attempt++) {
      loaded = await pane.loadUntilItem(id);
      const interrupted = interruption();
      if (interrupted) return finishJump(id, interrupted);
    }
    if (loaded === 'missing') {
      addToast('warning', 'Message is no longer in this thread');
      return finishJump(id, 'missing');
    }
    if (loaded === 'failed') return finishJump(id, 'failed');
    if (loaded === 'superseded') return finishJump(id, 'unresolved', 'lookup-superseded');

    await tick();
    let interrupted = interruption();
    if (interrupted) return finishJump(id, interrupted);
    let target = locateJumpTarget(id);
    const node = target && !target.withheld ? options.getRevealedNodes()[target.index] : undefined;
    if (target && node?.kind === 'activity_run') {
      // The row is the RUN, and the target may be collapsed into its chip or
      // outside its mount window, so the run is pointed at the item before
      // the outer scroll, which then measures the height that produced. Its
      // own row consumes the focus request once mounted, which is what makes
      // the order here safe: the run need not be on screen yet.
      if (!revealActivityRunItem(pane.activityRuns, node, target.itemId)) {
        // The projected run does not hold the row the resolver found in it.
        // The outer jump still lands on the run.
        console.warn('Timeline jump could not reveal an activity run member');
        reportFrontendDiagnostic('Timeline jump could not reveal an activity run member', 'stage=run-reveal');
      }
      await tick();
      interrupted = interruption();
      if (interrupted) return finishJump(id, interrupted);
      // Expanding a chip re-measures every row after it, so the index is
      // re-resolved rather than reused.
      target = locateJumpTarget(id);
    }
    const listRef = options.getListRef();
    if (!target || !listRef) return finishJump(id, 'unresolved', target ? 'no-list' : 'no-node');
    if (!claimNavigation(token)) return finishJump(id, 'superseded');
    // Explicit navigation: escape bottom follow, then jump (the write is
    // chokepoint-tagged via applyScrollTarget).
    options.stick.markEscaped();
    listRef.scrollToIndex(target.index, { align: 'center' });
    return finishJump(id, target.withheld ? 'withheld' : 'issued', '', target);
  }

  /**
   * The revealed node to land on. A row the reveal gate still withholds
   * lands on the last revealed node, the frontier it reveals below.
   */
  function locateJumpTarget(id: string): (ResolvedTimelineNode & { withheld: boolean }) | null {
    const revealed = options.getRevealedNodes();
    const shown = options.resolveTimelineNode(id, revealed);
    if (shown) return { ...shown, withheld: false };
    if (revealed.length === 0 || !options.resolveTimelineNode(id, options.getGroupedNodes())) return null;
    return { index: revealed.length - 1, itemId: id, withheld: true };
  }

  function finishJump(
    id: string,
    outcome: JumpOutcome,
    stage = '',
    target?: ResolvedTimelineNode,
  ): boolean {
    if (outcome === 'unresolved') {
      addToast('warning', 'Could not show that message');
      console.warn(`Timeline jump could not resolve a loaded row (stage=${stage})`);
      reportFrontendDiagnostic('Timeline jump could not resolve a loaded row', `stage=${stage}`);
    }
    if (isUiRenderTraceEnabled()) {
      const pane = options.getPane();
      recordUiTrace('timeline.jump', {
        paneId: pane.paneId,
        threadId: pane.threadId,
        itemId: id,
        outcome,
        stage: stage || null,
        index: target?.index ?? null,
        landedItemId: target?.itemId ?? null,
      });
    }
    return outcome === 'issued' || outcome === 'withheld';
  }

  function beginNavigation(): number {
    return ++navigationToken;
  }

  function claimNavigation(token: number): boolean {
    if (token !== navigationToken) return false;
    holdToken += 1;
    return true;
  }

  function beginHold(): number {
    return ++holdToken;
  }

  function isHoldCurrent(token: number): boolean {
    return token === holdToken;
  }

  function noteReaderGesture(): void {
    readerGestureEpoch += 1;
  }

  function invalidateRestore(): void {
    endAll();
  }

  function saveSnapshotOnDestroy(): void {
    if (!restoredThreadId) return;
    saveScrollSnapshotForThread(restoredThreadId);
    // Unmount is a final edge: the pane is closing or being replaced, and
    // nothing after this will capture. Final, for the same reason the
    // switch-away edge is: the rate bound and size gate exist to thin a
    // per-frame cadence, and there is no cadence left here to thin.
    options.persistSizePriorsExact();
  }

  return {
    get restoredThreadId() {
      return restoredThreadId;
    },
    beginNavigation,
    claimNavigation,
    beginHold,
    isHoldCurrent,
    noteReaderGesture,
    invalidateRestore,
    saveScrollSnapshot,
    handleSwitchEdgePre,
    maybeRestoreAfterFlush,
    scrollToItem,
    saveSnapshotOnDestroy,
  };
}
