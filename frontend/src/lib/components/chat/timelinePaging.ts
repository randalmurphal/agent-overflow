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
  type AutoLoadGate,
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
        // Identity, not a thread id: restore bookkeeping is keyed by the
        // pane's scroll-state key (see timelineRestore).
        threadId: pane.scrollStateKey,
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
        // Identity, not a thread id: restore bookkeeping is keyed by the
        // pane's scroll-state key (see timelineRestore).
        threadId: pane.scrollStateKey,
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

  // Shared shape behind all three load handlers below: pause auto-scroll
  // for the duration of the load, run the caller's load body, then disarm
  // the gate — UNLESS a thread switch happened mid-load (that switch
  // already reset the new pane's gate, so disarming here would stomp on
  // the wrong pane's state). Disarming matters because each load's own
  // follow-up write — the older prepend's head-splice compensation, the
  // newer-manual scrollToIndex, or the newer-auto head-prune's
  // compensation — is a PROGRAMMATIC scrollTop write, not a user gesture,
  // and must not re-fire the gate into a load cascade; a real
  // wheel/touch/keydown re-arms it, and the gate's own 350ms cooldown is
  // the fallback for devices where gesture detection misses an event.
  async function withGuardedDisarm(
    gate: AutoLoadGate,
    body: () => Promise<void>,
  ): Promise<void> {
    const release = options.stick.pauseAutoScroll();
    const switchGenAtStart = options.getPane().switchGeneration;
    try {
      await body();
    } finally {
      release();
      if (options.getPane().switchGeneration === switchGenAtStart) gate.disarm();
    }
  }

  // The engine's head mutation holds the reading position through the
  // estimated prepend. The virtualizer keeps the current retained DOM row
  // stationary while newly mounted estimates measure. There is no explicit
  // pre-request restore here. Scroll input keeps the virtualizer's candidate
  // current while the request is in flight, so a user who keeps moving is
  // never pulled back to the request's starting position. The pause lease
  // keeps scrollHeight growth from re-sticking; `escaped` marks the user as
  // reading older.
  async function handleLoadOlder(): Promise<void> {
    if (!options.getListRef()) return;
    const pane = options.getPane();
    await withGuardedDisarm(autoLoadOlderGate, async () => {
      options.stick.setEscapedFromLock(true);
      await pane.loadOlder();
      await tick();
      options.saveScrollSnapshot();
    });
  }

  // Manual "Load newer messages" button. Jumps to the end of the freshly
  // loaded page (align:'end') so the click visibly reveals newer content.
  async function handleLoadNewer(): Promise<void> {
    if (!options.getListRef()) return;
    await withGuardedDisarm(autoLoadNewerGate, async () => {
      const myToken = options.nextRestoreToken();
      const pane = options.getPane();
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
      // scrollToIndex(end) below can land in the bottom trigger zone;
      // withGuardedDisarm's disarm keeps that programmatic scroll from
      // auto-firing another load.
      currentListRef.scrollToIndex(lastIndex, { align: 'end' });
      options.saveScrollSnapshot();
    });
  }

  // Auto-load-newer path. Unlike the manual button it must NOT scroll:
  // newer rows append below the viewport (tail-grow, no shift) so the
  // reading position is unchanged, and loadNewer's head-prune holds position
  // via the engine's head-splice handling. The pause lease guards the
  // transient scrollHeight growth from a restick.
  async function handleLoadNewerAuto(): Promise<void> {
    if (!options.getListRef()) return;
    const pane = options.getPane();
    await withGuardedDisarm(autoLoadNewerGate, async () => {
      await pane.loadNewer();
      await tick();
      options.saveScrollSnapshot();
    });
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
