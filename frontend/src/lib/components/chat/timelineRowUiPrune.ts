// Offscreen row-UI-state pruning for MessageTimeline. Row expansion
// handles keep loaded payload bytes and Svelte effect roots so rows can
// survive normal windowing remounts; this module bounds that retention
// to a buffer around the visible range plus the tail. Scheduling
// (structural changes + scroll end, debounced, never per scroll frame)
// lives in the shared quiet scheduler (timelineQuietWork.ts); this pass
// runs on the 'always' rung because it mutates no reader-visible
// geometry and must keep bounding memory mid-stream.

import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { QuietPass } from './timelineQuietWork';
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
}

export function createTimelineRowUiPrune(
  options: TimelineRowUiPruneOptions,
): QuietPass {
  let lastRowUiPruneSignature = '';

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
    // Every signature input is a scalar, so a no-op prune (same window,
    // same structure, same active rows) bails without walking the node
    // tree OR the item list and before the retention collection
    // allocates anything. The pass NEVER proves a no-op by surveying
    // items: the store bumps `rowUiRetentionRevision` at write time for
    // the writes that can move the answer.
    const signature = timelineRowUiPruneSignature({
      threadId: pane.threadId,
      timelineRevision: pane.timelineRevision,
      // A run's mount window decides which of its children are retained, and
      // it moves without touching structure: relocating a window mounts a
      // different set of rows at the same node count, same range, same items.
      // Without this the pass would bail as a no-op and keep retaining the
      // window the user just left. One scalar, no tree walk.
      activityRunRevision: pane.activityRuns.revision,
      revealTurnIndex: pane.revealBoundary?.turnIndex ?? '',
      revealItemIndex: pane.revealBoundary?.itemIndex ?? '',
      nodesLength: revealedNodes.length,
      range,
      rowUiRetentionRevision: pane.rowUiRetentionRevision,
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
        isGroupExpanded: pane.isSubagentGroupExpanded,
        resolveItem: pane.getItemById,
      },
    );
    pane.pruneRowUiState(retention);
  }

  return {
    key: 'row-ui-prune',
    when: 'always',
    run: () => {
      pruneOffscreenRowUiState();
      // Retention drops dispose offscreen state only — nothing the
      // reader can see moves, so the mutation slot stays available.
      return false;
    },
  };
}
