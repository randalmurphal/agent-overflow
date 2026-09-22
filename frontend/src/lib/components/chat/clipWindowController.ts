import type { PaneScrollController } from '../../stores/threadPaneShared';
import type { UseStickToBottomController } from '../../utils/scroll/index.svelte';
import type { TimelineVirtualizerHandle } from '../../utils/virtual/types';

/** Register the clip's actual viewport with the data window that can prune it. */
export function createClipWindowController<T>(options: {
  stick: UseStickToBottomController;
  list: TimelineVirtualizerHandle;
  nodes(): readonly T[];
  itemIds(node: T): Iterable<string>;
}): PaneScrollController {
  const { stick, list } = options;
  function visibleTimelineItemIds(): ReadonlySet<string> | null {
    if (stick.isAtBottom && !stick.escapedFromLock) return null;
    const nodes = options.nodes();
    if (!nodes.length) return null;
    const offset = list.getScrollOffset();
    const first = Math.max(0, Math.min(nodes.length - 1, list.findItemIndex(offset)));
    const last = Math.max(first, Math.min(nodes.length - 1,
      list.findItemIndex(offset + Math.max(0, list.getViewportSize() - 1))));
    const ids = new Set<string>();
    for (let index = first; index <= last; index++) {
      for (const id of options.itemIds(nodes[index])) ids.add(id);
    }
    return ids;
  }
  return {
    pauseAutoScroll: stick.pauseAutoScroll,
    autoScrollInFlight: stick.autoScrollInFlight,
    observe: stick.observe,
    markStructuralContentPending: stick.markStructuralContentPending,
    armWarmup: stick.armWarmup,
    preserveScrollAnchor: stick.preserveScrollAnchor,
    visibleTimelineItemIds,
    canPreserveTimelineWindow: keeps => {
      const ids = visibleTimelineItemIds();
      return ids === null || [...ids].every(keeps);
    },
  };
}
