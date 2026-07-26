// Offscreen row-UI-state pruning for MessageTimeline. Row expansion
// handles keep loaded payload bytes and Svelte effect roots so rows can
// survive normal windowing remounts; this module bounds that retention
// to a buffer around the visible range plus the tail, on a prune cadence
// (structural changes + scroll end) rather than per scroll frame.

import { tick } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import { ACTIVITY_RUN_WINDOW_ROWS_DEFAULT } from '../../utils/activityRunGrouping';
import {
  collectTimelineRowUiRetention,
  timelineRowUiPruneSignature,
} from './timelineRowUiRetention';

// Expansion handles keep loaded payload bytes and Svelte effect roots so
// rows can survive normal windowing remounts. Keep that cache near the
// viewport and tail only; old offscreen rows remount collapsed instead of
// retaining detached DOM through stale component contexts.
const ROW_UI_RETAIN_NODE_BUFFER = 96;
const ROW_UI_RETAIN_TAIL_NODE_COUNT = 64;

export interface TimelineRowUiPruneOptions {
  getPane(): ThreadPane;
  getListRef(): TimelineVirtualizerHandle | undefined;
  getRevealedNodes(): TimelineNode[];
  isTest: boolean;
}

export interface TimelineRowUiPrune {
  /** Schedules a debounced (one-tick) prune pass. Called from the
   * component's structural/scroll-attach trigger effects and from
   * `handleTimelineScrollEnd`. */
  schedule(): void;
  /** Bumps the schedule token so any in-flight scheduled prune from a
   * torn-down instance no-ops. Called from `onDestroy`. */
  invalidate(): void;
}

export function createTimelineRowUiPrune(
  options: TimelineRowUiPruneOptions,
): TimelineRowUiPrune {
  let lastRowUiPruneSignature = '';
  let rowUiPruneToken = 0;

  function clampTimelineIndex(index: number): number {
    const revealedNodes = options.getRevealedNodes();
    if (revealedNodes.length === 0) return -1;
    if (!Number.isFinite(index)) return 0;
    return Math.max(0, Math.min(revealedNodes.length - 1, Math.floor(index)));
  }

  function currentVisibleTimelineRange(): { first: number; last: number } | null {
    const listRef = options.getListRef();
    if (!listRef || options.getRevealedNodes().length === 0) return null;
    // The engine's cached geometry only — a clientHeight read here would
    // force layout, and the prune used to run behind scroll frames
    // interleaved with streaming DOM writes. A zero viewport means the
    // scroller hasn't measured yet; pruning against it would treat
    // every row as offscreen and drop leased expansion state that's
    // about to be visible.
    const viewport = listRef.getViewportSize();
    if (viewport <= 0) return null;
    const offset = Math.max(0, listRef.getScrollOffset());
    const first = clampTimelineIndex(listRef.findItemIndex(offset));
    const last = clampTimelineIndex(listRef.findItemIndex(offset + viewport));
    if (first < 0 || last < 0) return null;
    return first <= last ? { first, last } : { first: last, last: first };
  }

  function pruneOffscreenRowUiState(): void {
    const range = currentVisibleTimelineRange();
    if (!range) return;

    const pane = options.getPane();
    const revealedNodes = options.getRevealedNodes();
    // Every signature input is available without walking the node tree,
    // so a no-op prune (same window, same structure, same active rows)
    // bails before the retention collection allocates anything.
    const signature = timelineRowUiPruneSignature({
      threadId: pane.threadId,
      timelineRevision: pane.timelineRevision,
      revealTurnIndex: pane.revealBoundary?.turnIndex ?? '',
      revealItemIndex: pane.revealBoundary?.itemIndex ?? '',
      nodesLength: revealedNodes.length,
      range,
      items: pane.items,
    });
    if (signature === lastRowUiPruneSignature) return;
    lastRowUiPruneSignature = signature;

    const retention = collectTimelineRowUiRetention(
      revealedNodes,
      pane.items,
      range,
      {
        nodeBuffer: ROW_UI_RETAIN_NODE_BUFFER,
        tailNodeCount: ROW_UI_RETAIN_TAIL_NODE_COUNT,
        runTailNodeCount: ACTIVITY_RUN_WINDOW_ROWS_DEFAULT,
        isGroupExpanded: pane.isSubagentGroupExpanded,
      },
    );
    pane.pruneRowUiState(retention);
  }

  function schedule(): void {
    if (options.isTest) return;
    const token = ++rowUiPruneToken;
    void tick().then(() => {
      if (token !== rowUiPruneToken) return;
      pruneOffscreenRowUiState();
    });
  }

  function invalidate(): void {
    rowUiPruneToken += 1;
  }

  return {
    schedule,
    invalidate,
  };
}
