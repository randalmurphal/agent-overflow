// Diagnostic UI-render-trace helper for MessageTimeline. Pulled out of
// the component so the orchestrator stays focused on layout and scroll
// integration. Production builds short-circuit at the
// `isUiRenderTraceEnabled()` check, so this module is dev-only payload.

import type { VListHandle } from 'virtua/svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  isUiRenderTraceEnabled,
  recordUiTrace,
  scheduleDomUiTrace,
  summarizeItemsForTrace,
} from '../../utils/uiRenderTrace';

const MAX_TRACE_NODES = 120;
const MAX_TRACE_DOM_ROWS = 160;
const TRACE_TEXT_PREVIEW_CHARS = 120;

export function recordTimelineRenderTrace(
  pane: ThreadPane,
  groupedNodes: readonly TimelineNode[],
  scrollEl: HTMLElement | undefined,
  listRef: VListHandle | undefined,
): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace('timeline.state', {
    threadId: pane.threadId,
    itemCount: pane.items.length,
    timelineRevision: pane.timelineRevision,
    groupedNodeCount: groupedNodes.length,
    nodes: groupedNodes.slice(0, MAX_TRACE_NODES).map((node) => (
      node.kind === 'leaf'
        ? {
            kind: 'leaf',
            itemId: node.item.id,
            itemThreadId: node.item.threadId,
            itemKind: node.item.kind,
            turnIndex: node.item.turnIndex,
            orphan: node.orphan === true,
          }
        : {
            kind: 'group',
            parentId: node.parent.id,
            parentThreadId: node.parent.threadId,
            childCount: node.children.length,
            turnIndex: node.parent.turnIndex,
          }
    )),
    items: summarizeItemsForTrace(pane.items),
  });
  scheduleDomUiTrace('timeline', 'timeline.dom', () => ({
    threadId: pane.threadId,
    rowCount: scrollEl?.querySelectorAll('[data-item-id]').length ?? 0,
    rows: Array.from(scrollEl?.querySelectorAll<HTMLElement>('[data-item-id]') ?? [])
      .slice(0, MAX_TRACE_DOM_ROWS)
      .map((el) => ({
        itemId: el.dataset.itemId ?? '',
        textPreview: (el.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, TRACE_TEXT_PREVIEW_CHARS),
      })),
    scrollOffset: listRef ? Math.round(listRef.getScrollOffset()) : 0,
    scrollSize: listRef ? Math.round(listRef.getScrollSize()) : 0,
    viewportSize: listRef ? Math.round(listRef.getViewportSize()) : 0,
  }));
}
