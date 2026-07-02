// Load-older / load-newer paging for MessageTimeline: the auto-load
// probes fired from the scroll handler, the explicit "Load older/newer
// messages" buttons, and the "Jump to latest" action. Owns the two
// direction-agnostic auto-load gates (see timelineScroll.ts's
// `createAutoLoadGate`) so each user scroll gesture loads exactly one
// section per direction, not a cascade.

import { tick } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  bottomEdgeGeometry,
  createAutoLoadGate,
  isWithinBottomTriggerZone,
  isWithinTopTriggerZone,
  type AutoLoadZoneThresholds,
} from './timelineScroll';

// Auto-load trigger thresholds, shared by both edges. Older: when the
// user scrolls within AUTO_LOAD_OFFSET_PX of the TOP and the topmost
// rendered row is one of the first AUTO_LOAD_INDEX_THRESHOLD nodes, fire
// `pane.loadOlder()`. Newer: the mirror at the BOTTOM fires
// `pane.loadNewer()`. Either way the next batch slots in before the user
// runs out of buffer. The index gate keeps an idle small-thread render
// from auto-loading just because the whole thing fits in viewport.
const AUTO_LOAD_OFFSET_PX = 800;
const AUTO_LOAD_INDEX_THRESHOLD = 5;
const AUTO_LOAD_ZONE: AutoLoadZoneThresholds = {
  offsetThreshold: AUTO_LOAD_OFFSET_PX,
  indexThreshold: AUTO_LOAD_INDEX_THRESHOLD,
};

export interface TimelinePagingOptions {
  getPane(): ThreadPane;
  stick: UseStickToBottomController;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getScrollEl(): HTMLDivElement | undefined;
  getRevealedNodes(): TimelineNode[];
  getRestoredThreadId(): string | null;
  nextRestoreToken(): number;
  isRestoreTokenCurrent(token: number): boolean;
  saveScrollSnapshot(): void;
}

export interface TimelinePaging {
  maybeAutoLoadOlder(offset: number): boolean;
  maybeAutoLoadNewer(offset: number): boolean;
  handleLoadOlder(): Promise<void>;
  handleLoadNewer(): Promise<void>;
  handleLoadNewerAuto(): Promise<void>;
  jumpToLatest(): Promise<void>;
  /** Re-arm both gates on a real user gesture (wheel/touchmove/keydown). */
  armGatesOnUserGesture(): void;
  /** Reset both gates — thread switch and component teardown. */
  resetGates(): void;
}

export function createTimelinePaging(options: TimelinePagingOptions): TimelinePaging {
  const autoLoadOlderGate = createAutoLoadGate();
  const autoLoadNewerGate = createAutoLoadGate();

  // Auto-load-older trigger. Fires `pane.loadOlder()` when the user is
  // reading near the top of the loaded window, so older items page in
  // before they hit a wall. The "Load older messages" button at the top of
  // the timeline is the explicit fallback when auto-load is bypassed (no
  // progress, fast-skip past the threshold, etc.). Returns whether it fired.
  function maybeAutoLoadOlder(offset: number): boolean {
    // Cheap pre-check before building the gate-state object + zone closure
    // on every scroll frame. `shouldLoad`'s own `!hasMore` check remains the
    // authoritative gate; this just keeps the allocation off the hot path.
    const pane = options.getPane();
    const listRef = options.getListRef();
    if (!listRef || !pane.hasMoreHistory) return false;
    if (
      !autoLoadOlderGate.shouldLoad({
        hasMore: pane.hasMoreHistory,
        loading: pane.loadingOlder,
        floorCursor: pane.oldestLoadedCursor,
        restoredThreadId: options.getRestoredThreadId(),
        threadId: pane.threadId,
        inTriggerZone: () =>
          isWithinTopTriggerZone(offset, AUTO_LOAD_ZONE, () => listRef.findItemIndex(offset)),
      })
    )
      return false;
    void handleLoadOlder();
    return true;
  }

  // Auto-load-newer trigger — mirror of older at the bottom edge. Fires
  // `pane.loadNewer()` when the user scrolls within the prefetch zone of
  // the loaded window's bottom while more recent history is unloaded
  // (`hasMoreNewer`, set after an older-paging prune dropped the tail, or
  // after a search jump into the middle). Returns whether it fired.
  function maybeAutoLoadNewer(offset: number): boolean {
    // Cheap pre-check (mirror of maybeAutoLoadOlder): `hasMoreNewer` is false
    // for most of a thread's life, so this keeps the gate-state object + zone
    // closure off the per-frame path until the user has actually paged away
    // from the tail. `shouldLoad`'s `!hasMore` check stays authoritative.
    const pane = options.getPane();
    const listRef = options.getListRef();
    const viewport = options.getScrollEl();
    if (!listRef || !viewport || !pane.hasMoreNewer) return false;
    if (
      !autoLoadNewerGate.shouldLoad({
        hasMore: pane.hasMoreNewer,
        loading: pane.loadingNewer,
        floorCursor: pane.newestLoadedCursor,
        restoredThreadId: options.getRestoredThreadId(),
        threadId: pane.threadId,
        inTriggerZone: () => {
          const edge = bottomEdgeGeometry(viewport.scrollHeight, viewport.clientHeight, offset);
          return isWithinBottomTriggerZone(
            edge.distanceFromBottom,
            options.getRevealedNodes().length,
            AUTO_LOAD_ZONE,
            () => listRef.findItemIndex(edge.bottomProbeOffset),
          );
        },
      })
    )
      return false;
    void handleLoadNewerAuto();
    return true;
  }

  // The prepend holds the reading position via the engine's head-splice
  // handling: pane.loadOlder sets pane.pendingTimelineShiftAtHead for the
  // head-grow flush, so the engine re-bases its size store and reports the
  // scrollTop compensation in one step. There is deliberately NO explicit
  // re-anchor here — a
  // re-anchor captured before the await would yank the user back if they
  // kept scrolling while the page loaded. The pause lease keeps the
  // prepend's scrollHeight growth from re-sticking; `escaped` marks the
  // user as reading older.
  async function handleLoadOlder(): Promise<void> {
    const listRef = options.getListRef();
    if (!listRef) return;
    const release = options.stick.pauseAutoScroll();
    options.stick.setEscapedFromLock(true);
    const pane = options.getPane();
    const switchGenAtStart = pane.switchGeneration;
    try {
      await pane.loadOlder();
      await tick();
      options.saveScrollSnapshot();
    } finally {
      release();
      // Disarm so the prepend's shift compensation (a programmatic scrollTop
      // write, not a user gesture) can't re-fire the gate into a cascade. A
      // real wheel/touch/keydown re-arms; the 350ms cooldown is the
      // fallback. Guard on switchGeneration so a thread switch mid-load
      // (which already reset the new pane's gate) is not disarmed.
      if (options.getPane().switchGeneration === switchGenAtStart) autoLoadOlderGate.disarm();
    }
  }

  // Manual "Load newer messages" button. Jumps to the end of the freshly
  // loaded page (align:'end') so the click visibly reveals newer content.
  async function handleLoadNewer(): Promise<void> {
    const listRef = options.getListRef();
    if (!listRef) return;
    const release = options.stick.pauseAutoScroll();
    const myToken = options.nextRestoreToken();
    const pane = options.getPane();
    const switchGenAtStart = pane.switchGeneration;
    try {
      const result = await pane.loadNewer();
      await tick();
      const currentListRef = options.getListRef();
      if (
        !options.isRestoreTokenCurrent(myToken)
        || !currentListRef
        || result.status !== 'loaded'
      )
        return;
      const lastIndex = options.getRevealedNodes().length - 1;
      if (lastIndex < 0) return;
      // Explicit navigation into the middle of history (more-newer may
      // remain below): escape bottom follow, then jump.
      options.stick.setEscapedFromLock(true);
      currentListRef.scrollToIndex(lastIndex, { align: 'end' });
      options.saveScrollSnapshot();
    } finally {
      release();
      // The scrollToIndex(end) above can land in the bottom trigger zone;
      // disarm so that programmatic scroll can't auto-fire another load.
      // Guard on switchGeneration (a thread switch mid-load already reset the
      // new pane's gate).
      if (options.getPane().switchGeneration === switchGenAtStart) autoLoadNewerGate.disarm();
    }
  }

  // Auto-load-newer path. Unlike the manual button it must NOT scroll:
  // newer rows append below the viewport (tail-grow, no shift) so the
  // reading position is unchanged, and loadNewer's head-prune holds position
  // via the engine's head-splice handling. The pause lease guards the
  // transient scrollHeight growth from a restick.
  async function handleLoadNewerAuto(): Promise<void> {
    const listRef = options.getListRef();
    if (!listRef) return;
    const release = options.stick.pauseAutoScroll();
    const pane = options.getPane();
    const switchGenAtStart = pane.switchGeneration;
    try {
      await pane.loadNewer();
      await tick();
      options.saveScrollSnapshot();
    } finally {
      release();
      // Disarm so the head-prune's shift compensation (programmatic, not a
      // user gesture) can't re-fire the gate into a cascade. Guard on
      // switchGeneration so a thread switch mid-load (which already reset the
      // new pane's gate) is not disarmed. See handleLoadOlder.
      if (options.getPane().switchGeneration === switchGenAtStart) autoLoadNewerGate.disarm();
    }
  }

  async function jumpToLatest(): Promise<void> {
    const myToken = options.nextRestoreToken();
    const pane = options.getPane();
    const loaded = pane.hasMoreNewer ? await pane.loadRecentTail() : true;
    await tick();
    if (!options.isRestoreTokenCurrent(myToken) || !loaded) return;
    options.stick.forceStick({ reason: 'user' });
    options.saveScrollSnapshot();
  }

  function armGatesOnUserGesture(): void {
    autoLoadOlderGate.armOnGesture();
    autoLoadNewerGate.armOnGesture();
  }

  function resetGates(): void {
    autoLoadOlderGate.reset();
    autoLoadNewerGate.reset();
  }

  return {
    maybeAutoLoadOlder,
    maybeAutoLoadNewer,
    handleLoadOlder,
    handleLoadNewer,
    handleLoadNewerAuto,
    jumpToLatest,
    armGatesOnUserGesture,
    resetGates,
  };
}
