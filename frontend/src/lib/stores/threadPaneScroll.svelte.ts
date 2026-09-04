// stores/threadPaneScroll.svelte.ts
//
// OWNS the pane's edge onto its scroll surface: the registered
// `PaneScrollController` slot, the scroll-to-item INTENT the timeline reads,
// and every decision about arming the structural-append spring and the
// warm-up gate — including the gates each arm shares (restore-in-progress,
// discussion surface, empty pane) and the superseded-nudge token.
//
// MUST NOT write scroll offsets. Nothing here touches `scrollTop`, the DOM,
// or the virtualizer: it talks only to the minimal `PaneScrollController`
// surface a mounted timeline registers, and publishes an intent for
// everything that needs virtualizer index resolution. It also owns no
// timeline data — the item count arrives through a dep, and the live-content
// stamp stays on the pane because it is read imperatively by the controller.

import { tick, untrack } from 'svelte';
import type { Thread } from '../types/models';
import {
  threadUsesDiscussionSurface,
  type PaneScrollController,
  type ScrollToItemRequest,
} from './threadPaneShared';

export interface ThreadPaneScrollOptions {
  getThread(): Thread | null;
  /** The pane's `loading` flag — a switch+load settle is a restore, not an append. */
  getLoading(): boolean;
  /** Bumped by switch/clear; captured across the nudge's awaits. */
  getSwitchGeneration(): number;
  /** Rows currently in the loaded window (the warm-up gate's empty-pane test). */
  getItemCount(): number;
  /** Stamp the pane's live-content latch. */
  stampLiveContent(): void;
}

export interface ThreadPaneScroll {
  /**
   * Registered scroll controller for this pane. Read by surfaces that
   * need to suspend auto-follow during a gesture. Call
   * `pause = pane.scrollController?.pauseAutoScroll()`
   * on pointerdown and `pause?.()` on pointerup/cancel — the lease is
   * idempotent so a stray double-release is safe.
   */
  readonly controller: PaneScrollController | null;
  /** MessageTimeline calls this on mount. */
  attach(controller: PaneScrollController): void;
  detach(controller: PaneScrollController): void;
  readonly scrollToItemRequest: ScrollToItemRequest;
  requestScrollToItem(itemID: string): void;
  armStructuralSpring(): boolean;
  armInitialSliceWarmup(): boolean;
  armLiveContentAppendSpring(): void;
}

export function createThreadPaneScroll(
  options: ThreadPaneScrollOptions,
): ThreadPaneScroll {
  /**
   * Nonce bumped when the pane wants the active MessageTimeline to scroll
   * to a specific item. Scroll side effects are DOM operations that
   * shouldn't live on the store, so the store publishes an intent and
   * the timeline reads it reactively. Consumers compare the most
   * recently observed nonce against `scrollToItemRequest.nonce` and
   * react when it changes. `itemId` is the target id; an empty string
   * means "no outstanding request". `behavior` and `flash` let the
   * owner of the actual scroll container decide how visible the jump
   * should be without exposing DOM methods through the pane.
   */
  let scrollToItemRequest: ScrollToItemRequest = $state({
    itemId: '',
    nonce: 0,
  });

  /**
   * Live registration slot for the timeline's sticky-bottom controller.
   * MessageTimeline registers its controller on mount so external surfaces
   * (inspector panels, resizable panes) can acquire a `pauseAutoScroll()` lease while a
   * gesture is in flight, preventing auto-follow from yanking the view
   * mid-drag. The factory only knows about the minimal surface
   * (`PaneScrollController`) — it never depends on the virtualizer or the DOM
   * controller's full type, so the contract stays cheap to honour.
   */
  // `raw`, and load-bearing: a plain `$state` PROXIES the controller on
  // assignment, so the object the pane hands back is never `===` the one that
  // registered. Every identity check against it silently fails —
  // `detach`'s "is this still mine" guard never matched, so the
  // slot was never cleared and a torn-down controller (and through it the whole
  // detached timeline subtree) stayed reachable from the pane for as long as the
  // pane lived. A controller is a handle, not reactive data: nothing reads
  // through it, and every consumer re-reads the slot itself.
  let scrollController: PaneScrollController | null = $state.raw(null);

  // One flush and one frame/timeout pair per pane. Bursts replace the
  // pending intent instead of allocating an async chain for every append.
  let structuralNudgeToken = 0;
  let nudgeController: PaneScrollController | null = null;
  let nudgeGeneration = 0;
  let nudgeFlushPending = false;
  let nudgeFrame: number | null = null;
  let nudgeTimer: ReturnType<typeof setTimeout> | null = null;
  const HIDDEN_FRAME_FALLBACK_MS = 32;

  function cancelNudge(): void {
    structuralNudgeToken += 1;
    if (nudgeFrame !== null) cancelAnimationFrame(nudgeFrame);
    if (nudgeTimer !== null) clearTimeout(nudgeTimer);
    nudgeFrame = null;
    nudgeTimer = null;
    nudgeController = null;
  }

  function scheduleNudge(controller: PaneScrollController): void {
    nudgeController = controller;
    nudgeGeneration = options.getSwitchGeneration();
    if (nudgeFlushPending) return;
    nudgeFlushPending = true;
    void tick().then(() => {
      nudgeFlushPending = false;
      const expectedController = nudgeController;
      if (!expectedController) return;
      const token = structuralNudgeToken;
      const generation = nudgeGeneration;
      const settle = (): void => {
        if (token !== structuralNudgeToken) return;
        cancelNudge();
        if (generation !== options.getSwitchGeneration()) return;
        if (scrollController !== expectedController || options.getLoading()) return;
        if (threadUsesDiscussionSurface(options.getThread())) return;
        expectedController.observe('live-content');
      };
      // Hidden WebViews suspend rAF while wire batches keep arriving.
      // Either boundary releases both handles; teardown cancels them too.
      if (typeof requestAnimationFrame === 'function') {
        nudgeFrame = requestAnimationFrame(settle);
        nudgeTimer = setTimeout(settle, HIDDEN_FRAME_FALLBACK_MS);
      } else {
        nudgeTimer = setTimeout(settle, 0);
      }
    });
  }

  /**
   * Arm the structural-append spring and schedule its follow-up nudge.
   * Returns whether the gates passed and the controller was armed. The
   * pane data layer is the sole owner of this decision; the call sites
   * are `armLiveContentAppendSpring` below (wire appends to the loaded
   * tail via `applyProviderItemUpserts`, and `recomputeRevealPass`
   * releasing withheld rows) plus the composer's optimistic user-send,
   * which arms WITHOUT the live-content stamp (the send stays a
   * one-shot; see `lastLiveContentAt`). Scroll writes still belong to
   * the controller — the pane only talks to the registered
   * `PaneScrollController` surface, the same seam the
   * `scrollToItemRequest` intent publishes through when a scroll needs
   * virtualizer index resolution.
   *
   * The arm runs synchronously with the data change — strictly before the
   * Svelte flush in which the virtualizer measures the new/released rows
   * and delivers their geometry — so the growth itself is spring-eligible,
   * not just the remeasure that follows it. An effect-based arm loses that
   * ordering race (bug-report-20260702T193212Z).
   *
   * The nudge (observe('live-content') after flush + one frame) re-checks
   * the bottom once the DOM has settled. A thinking row tail-pins its
   * clipped body internally, so its visible movement often does not grow
   * the outer timeline row; when the next top-level row mounts, contentRO
   * timing alone can miss the first bottom target, especially with
   * Streamdown's async markdown layout still growing the row.
   * 'live-content' honors spring mode / the just-armed structural window
   * and is escape-aware, so a user scrolled away is never yanked.
   *
   * Gates, shared by every caller:
   * - `loading`: the whole switch+load settle is a restore, not an
   *   in-turn append (bug-report-20260622T041049Z class); the warm gate
   *   independently pins the post-restore settle.
   * - discussion surface: those panes swap the chat timeline for
   *   ChannelView, which attaches ITS OWN controller here; timeline item
   *   changes render nothing, and arming would open a 250ms spring
   *   window on unrelated channel-message growth.
   */
  function armStructuralSpring(): boolean {
    cancelNudge();
    const controller = scrollController;
    if (!controller) return false;
    if (options.getLoading()) return false;
    if (threadUsesDiscussionSurface(options.getThread())) return false;
    controller.markStructuralContentPending();
    scheduleNudge(controller);
    return true;
  }

  /**
   * Re-close the warm-up gate for the rows an initial slice just merged
   * into an empty pane. See `PaneScrollController.armWarmup` for why the
   * switch-edge arm alone does not survive the fetch, and
   * `armStructuralSpring` above for the same synchronous-with-the-data
   * ordering contract.
   *
   * Returns whether the gate was re-armed (the cold-load trace records
   * it — a fetch that mounted rows without re-arming is this defect
   * regressing).
   *
   * Two gates:
   * - Nothing mounted (`items` still empty — a genuinely empty thread):
   *   there is no cascade to hide, and holding the gate closed would
   *   sync-pin the first streamed tokens instead of gliding them and
   *   leave the pane behind an empty 2.5s failsafe. Empty panes stay
   *   visible, exactly as the placeholder→materialized path already
   *   treats them.
   * - Discussion surface: those panes register ChannelView's controller,
   *   which owns an unrelated scroll surface — the same reason
   *   `armStructuralSpring` stands down.
   */
  function armInitialSliceWarmup(): boolean {
    if (options.getItemCount() === 0) return false;
    if (threadUsesDiscussionSurface(options.getThread())) return false;
    const controller = scrollController;
    if (!controller) return false;
    controller.armWarmup();
    return true;
  }

  /**
   * A wire append to the loaded tail (or a reveal-gate release mounting
   * withheld rows) IS live content advancing: arm the structural spring
   * AND stamp live content, sharing the arm's restore gates.
   *
   * Neither signal picks the animation — growth while pinned always
   * glides (see `utils/scroll/resolver.ts#springGateIsOpen`). They tell
   * the controller more content is expected imminently, which keeps the
   * spring sentinel alive across delivery gaps instead of cancelling on
   * each arrival, and lets the viewport-change path distinguish an
   * append from idle composer geometry. The 250ms one-shot covers only
   * the append's first growth delivery; the stamp opens the rolling
   * liveness window a background-task completion needs while its
   * payload preview / markdown / highlight spans settle after turn end.
   */
  function armLiveContentAppendSpring(): void {
    if (armStructuralSpring()) options.stampLiveContent();
  }

  return {
    get controller() {
      return scrollController;
    },
    attach(controller: PaneScrollController): void {
      if (untrack(() => scrollController) !== controller) cancelNudge();
      scrollController = controller;
    },
    detach(controller: PaneScrollController): void {
      // Only clear if the registered controller matches — protects
      // against a stale teardown disposing a freshly remounted pane's
      // controller during fast thread switches. Depends on the slot being
      // `$state.raw`; see its declaration.
      if (scrollController === controller) {
        cancelNudge();
        scrollController = null;
      }
    },
    get scrollToItemRequest() {
      return scrollToItemRequest;
    },
    /**
     * Publish a scroll-to-item intent for the MessageTimeline to pick
     * up. Consumers call this instead of reaching into the timeline
     * directly — keeps DOM operations inside the component that owns
     * the scroll container, and lets the pane mediate window loading
     * if the target isn't visible yet. The timeline handler is
     * responsible for awaiting `loadUntilItem` before scrolling.
     */
    requestScrollToItem(itemID: string): void {
      if (!itemID) return;
      scrollToItemRequest = {
        itemId: itemID,
        nonce: scrollToItemRequest.nonce + 1,
      };
    },
    armStructuralSpring,
    armInitialSliceWarmup,
    armLiveContentAppendSpring,
  };
}
