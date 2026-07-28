// Auto-collapse for settled activity runs. A run that opened because it was
// live keeps rendering open after it settles (`openedLive` in the pane's run
// registry); this gate releases that hold once doing so is invisible and
// unwanted by nobody — off-screen, well behind the tail, nothing the reader
// engaged with inside. Released, the run takes the thread's defaults, which
// is an instant collapse when those say collapsed.
//
// Instant-while-hidden is the whole design. The fold animation this replaced
// collapsed the run in front of the reader, and no easing made losing a
// viewport of height under your eyes acceptable; scheduling the same change
// for when it cannot be seen needs no easing at all. The geometric half is
// pure (utils/activityRunAutoCollapse.ts); this module owns the reader half,
// because it reads the pane's registries.
//
// Same cadence and shape as timelineRowUiPrune: structural changes + scroll
// end, debounced one tick, never per scroll frame. Growth that bumps nothing
// structural (prose streaming below a settled run) still reaches the gate
// through scroll end — the pin writes scrollTop while content grows, and the
// virtualizer synthesizes scrollend after the writes go quiet. A pass that
// lands while the growth's glide is still running stands down
// (`autoScrollInFlight`) and lets that scrollend re-run it.

import { tick } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import { withViewportBottomHeld } from '../../stores/threadPaneShared';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import {
  renderedItemIdsWithin,
  type ActivityRunNode,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { activityRunOutOfSight } from '../../utils/activityRunAutoCollapse';
import { activityRunHasFailure } from '../../utils/activityRunSummary';

export interface TimelineActivityRunAutoCollapseOptions {
  getPane(): ThreadPane;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  isTest: boolean;
}

export interface TimelineActivityRunAutoCollapse {
  /** Schedules a debounced (one-tick) gate pass. Called from the same
   * trigger sites as the row-UI prune. */
  schedule(): void;
  /** Bumps the schedule token so an in-flight pass from a torn-down
   * instance no-ops. Called from `onDestroy`. */
  invalidate(): void;
}

/**
 * The reader is engaged with this run's inside, so it must not close under
 * them wherever it sits: they pinned its window or scrolled up within it
 * (facts the registry keeps precisely because rows unmount), they expanded
 * something in it, or it holds a failure they have not addressed. Failure is
 * permanent-until-answered by design (the Buildkite rule): an errored run
 * the reader never saw must still be open when they scroll back to it.
 */
function readerEngagedWith(pane: ThreadPane, run: ActivityRunNode): boolean {
  const runs = pane.activityRuns;
  if (runs.windowAnchor(run.runId) !== null) return true;
  if (runs.scrollSnapshot(run.runId)?.escaped) return true;
  // Rendered scope, not `memberItemIds`: identity membership deliberately
  // stops at group parents, but a reader's expansion can sit on any row a
  // group renders — a wait group's children, an opened subagent card's
  // transcript. Failure keeps the membership scope on purpose; it matches
  // what the collapsed chip summarizes (`activityRunSummary`).
  if (pane.hasUserExpansionWithin(renderedItemIdsWithin(run.children))) return true;
  return activityRunHasFailure(run.memberItemIds, pane.getItemById);
}

export function createTimelineActivityRunAutoCollapse(
  options: TimelineActivityRunAutoCollapseOptions,
): TimelineActivityRunAutoCollapse {
  let token = 0;

  function releaseEligibleRuns(): void {
    const pane = options.getPane();
    // The common case must cost nothing: no held-open runs, no geometry.
    const held = pane.activityRuns.openedLiveRunIds();
    if (held.length === 0) return;

    const listRef = options.getListRef();
    if (!listRef) return;
    // Never during reader-visible motion. A release routes through
    // `preserveViewportBottom`, whose bottom-pinned restore is a direct
    // write — landing it mid-glide (or in the armed gap before the
    // spring's first frame) turns the animation the reader is watching
    // into a snap to the bottom. Deferring loses nothing: the settle that
    // ends the glide synthesizes the scrollend that re-triggers this gate
    // in quiet.
    if (pane.scrollController?.autoScrollInFlight()) return;
    // Engine-cached geometry only, same rule as the prune: a clientHeight
    // read here would force layout behind streaming DOM writes. A zero
    // viewport is an unmeasured scroller, and against one every run is
    // "out of sight".
    const viewport = listRef.getViewportSize();
    if (viewport <= 0) return;
    const scrollTop = Math.max(0, listRef.getScrollOffset());
    const totalSize = listRef.getTotalSize();

    const heldSet = new Set(held);
    const nodes = options.getRevealedNodes();
    const eligible: string[] = [];
    for (let index = 0; index < nodes.length; index += 1) {
      const node = nodes[index];
      if (node.kind !== 'activity_run' || !heldSet.has(node.runId)) continue;
      // The hold is recorded WHILE live, so the live run is always in the
      // held list — and it is the one run that has not settled into anything
      // releasable yet. (Its geometry would refuse it too; this is the
      // semantic reason, not a shortcut.)
      if (node.live) continue;
      const top = listRef.getItemOffset(index);
      const bottom =
        index + 1 < nodes.length ? listRef.getItemOffset(index + 1) : totalSize;
      if (!activityRunOutOfSight({ top, bottom, scrollTop, viewport, totalSize })) {
        continue;
      }
      if (readerEngagedWith(pane, node)) continue;
      eligible.push(node.runId);
    }
    // A held id with no revealed run node is simply not visited. Steady-state
    // the two views agree — the held list and `revealedNodes` come out of the
    // same projection pass, which sweeps unclaimed entries — but reading
    // `revealedNodes` above can recompute a projection gone dirty since the
    // held list was captured, sweeping a run already in `heldSet`. Leaving it
    // alone is right: the registry dropped it, and `releaseOpenedLive`
    // no-ops on unknown ids. Nothing here may create entries.
    if (eligible.length === 0) return;

    // One anchored transaction around the whole batch — the rule every
    // mutator of run collapse state follows (chat/AGENTS.md). The runs are
    // off-screen, so for a pinned reader the browser's own clamp would
    // already cancel the shrink; the transaction is what makes zero motion
    // DETERMINISTIC for the rest: a reader parked mid-list below a released
    // run gets their anchor row put back after the flush instead of trusting
    // the engine to compensate an estimate-driven height change above them.
    // Collected first so a pass with nothing to do never pauses the spring.
    withViewportBottomHeld(pane.scrollController, () => {
      for (const runId of eligible) pane.activityRuns.releaseOpenedLive(runId);
    });
  }

  function schedule(): void {
    if (options.isTest) return;
    const current = ++token;
    void tick().then(() => {
      if (current !== token) return;
      releaseEligibleRuns();
    });
  }

  function invalidate(): void {
    token += 1;
  }

  return {
    schedule,
    invalidate,
  };
}
