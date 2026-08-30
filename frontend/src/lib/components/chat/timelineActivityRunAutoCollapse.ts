// Auto-collapse for settled activity runs. A run that opened as the
// timeline's newest activity keeps rendering open after prose displaces it
// (`openedLive` in the pane's run
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
// Scheduling — structural changes + scroll end, debounced, never during
// reader-visible motion — lives in the shared quiet scheduler
// (timelineQuietWork.ts); this module is its 'quiet' pass, so a release
// can only run while no glide is running or armed. That stand-down can
// only see motion that already exists when the pass runs, so the
// transaction itself also yields: an append landing between the release
// and its bottom restore (structural triggers make gate passes
// inherently append-adjacent) arms the structural spring, and the
// restore hands the trip to it instead of writing a bottom that already
// contains the new row. The transaction is the registry's own — see
// `ThreadActivityRuns.releaseOpenedLive` for its `takeover: 'yield'`.

import type {
  RowUiRegistry,
  TimelineSource,
} from '../../stores/threadPaneRoles';
import type { QuietPass } from './timelineQuietWork';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import {
  renderedItemIdsWithin,
  type ActivityRunNode,
  type TimelineNode,
} from '../../utils/subagentGrouping';
import { activityRunOutOfSight } from '../../utils/activityRunAutoCollapse';

/** What the gate reads: the run registry, plus the reader-deviation peek. */
type AutoCollapseHost = TimelineSource & RowUiRegistry;

export interface TimelineActivityRunAutoCollapseOptions {
  getPane(): AutoCollapseHost;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  /** False while document visibility has made the virtualizer cache stale. */
  geometryReady(): boolean;
}

/**
 * The reader is engaged with this run's inside, so it must not close under
 * them wherever it sits: they pinned its window or scrolled up within it
 * (facts the registry keeps precisely because rows unmount), or they expanded
 * something in it.
 *
 * A failed member deliberately does NOT hold the run open (removed
 * 2026-08-18): the collapsed chip's failure marker (`activityRunSummary`)
 * already keeps the failure visible, so pinning a whole viewport of history
 * open because one command errored punished ordinary operation to restate
 * what the chip says.
 */
function readerEngagedWith(pane: AutoCollapseHost, run: ActivityRunNode): boolean {
  const runs = pane.activityRuns;
  if (runs.windowAnchor(run.runId) !== null) return true;
  if (runs.scrollSnapshot(run.runId)?.escaped) return true;
  // Rendered scope, not `memberItemIds`: identity membership deliberately
  // stops at group parents, but a reader's expansion can sit on any row a
  // group renders — a wait group's children, an opened subagent card's
  // transcript.
  return pane.hasUserExpansionWithin(renderedItemIdsWithin(run.children));
}

export function createTimelineActivityRunAutoCollapse(
  options: TimelineActivityRunAutoCollapseOptions,
): QuietPass {
  function releaseEligibleRuns(): boolean {
    const pane = options.getPane();
    // The common case must cost nothing: no held-open runs, no geometry.
    // The reader-visible-motion stand-down is the scheduler's: this is
    // a 'quiet' pass, so it never runs while a glide is running or
    // armed. A release routes through `preserveViewportBottom`, whose
    // bottom-pinned restore is a direct write — landing it mid-glide
    // (or in the armed gap before the spring's first frame) turns the
    // animation the reader is watching into a snap to the bottom.
    const held = pane.activityRuns.openedLiveRunIds();
    if (held.length === 0) return false;
    // Hidden documents can keep receiving provider/Svelte updates while rAF
    // and ResizeObserver delivery is suspended. The cached viewport is not a
    // proof of invisibility again until the virtualizer publishes a fresh
    // post-resume geometry sample (timelineVisibilityGeometry.ts).
    if (!options.geometryReady()) return false;

    const listRef = options.getListRef();
    if (!listRef) return false;
    // Engine-cached geometry only, same rule as the prune: a clientHeight
    // read here would force layout behind streaming DOM writes. A zero
    // viewport is an unmeasured scroller, and against one every run is
    // "out of sight".
    const viewport = listRef.getViewportSize();
    if (viewport <= 0) return false;
    const scrollTop = Math.max(0, listRef.getScrollOffset());
    const totalSize = listRef.getTotalSize();

    const heldSet = new Set(held);
    const nodes = options.getRevealedNodes();
    const eligible: string[] = [];
    for (let index = 0; index < nodes.length; index += 1) {
      const node = nodes[index];
      if (node.kind !== 'activity_run' || !heldSet.has(node.runId)) continue;
      // The hold is recorded WHILE live, so the tail run is always in the
      // held list — and it is the one run that has not settled into anything
      // releasable yet. Tail-ness, not `live`: releasing a run the reader
      // still sees as newest would be undone by the very next projection
      // pass (`resolveCollapsed` refuses defaults while `atTail`), an inert
      // release that still dropped the recorded hold and bumped the
      // revision. (Its geometry would refuse it too — the last revealed
      // node can never be out of sight below — but this is the semantic
      // reason, not a shortcut.)
      if (node.atTail) continue;
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
    if (eligible.length === 0) return false;

    // Collected first so a pass with nothing to do never pauses the spring:
    // the registry runs the batch inside ONE yield-takeover viewport-bottom
    // hold (`ThreadActivityRuns.releaseOpenedLive` carries the full
    // rationale, incl. the collapse-vs-append race its 'yield' answers).
    pane.activityRuns.releaseOpenedLive(eligible);
    return true;
  }

  return {
    key: 'activity-run-auto-collapse',
    when: 'quiet',
    run: releaseEligibleRuns,
  };
}
