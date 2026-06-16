// Diagnostic UI-render-trace helper for MessageTimeline. Pulled out of
// the component so the orchestrator stays focused on layout and scroll
// integration. Production builds short-circuit at the
// `isUiRenderTraceEnabled()` check, so this module is dev-only payload.

import type { VirtualizerHandle } from 'virtua/svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { TimelineNode } from '../../utils/subagentGrouping';
import {
  isUiRenderTraceEnabled,
  recordUiTrace,
  scheduleDomUiTrace,
} from '../../utils/uiRenderTrace';

const MAX_TRACE_NODES = 120;
const MAX_TRACE_DOM_ROWS = 64;

export function recordTimelineRenderTrace(
  pane: ThreadPane,
  groupedNodes: readonly TimelineNode[],
  scrollEl: HTMLElement | undefined,
  listRef: VirtualizerHandle | undefined,
): void {
  if (!isUiRenderTraceEnabled()) return;
  recordUiTrace('timeline.state', {
    threadId: pane.threadId,
    itemCount: pane.items.length,
    timelineRevision: pane.timelineRevision,
    groupedNodeCount: groupedNodes.length,
    // Per-node thread ids are redundant with the top-level `threadId`
    // and omitting them shaves ~60 bytes per node (`itemThreadId` is a
    // 38-char UUID repeated for every leaf). Cross-thread inline rows
    // are the only case where the per-node thread id would differ — we
    // accept losing that distinction in the trace to keep the
    // post-shrink retention budget intact. `orphan` is omitted when
    // false (the common case) for the same reason.
    nodes: groupedNodes.slice(0, MAX_TRACE_NODES).map((node) => {
      if (node.kind === 'leaf') {
        const out: Record<string, unknown> = {
          kind: 'leaf',
          itemId: node.item.id,
          itemKind: node.item.kind,
          turnIndex: node.item.turnIndex,
        };
        if (node.orphan === true) out.orphan = true;
        return out;
      }
      if (node.kind === 'group') {
        return {
          kind: 'group',
          parentId: node.parent.id,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'wait_group') {
        return {
          kind: 'wait_group',
          parentId: node.parent.id,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'read_group') {
        return {
          kind: 'read_group',
          groupKey: node.groupKey,
          memberCount: node.members.length,
          turnIndex: node.members[0]?.turnIndex ?? 0,
        };
      }
      const _exhaustive: never = node;
      return _exhaustive;
    }),
    // The items array used to live here. It dominated trace file bytes
    // (single timeline.state snapshot averaged ~63 KB on a 228-item
    // thread, burning ~57% of the 10 MB rotation cap on data that
    // barely changes between consecutive emissions). The DOM trace
    // (timeline.dom, scheduled below) captures rendered row identity and
    // scroll geometry; the kind/turn information is in `nodes` above.
  });
  scheduleDomUiTrace('timeline', 'timeline.dom', () => ({
    threadId: pane.threadId,
    rowCount: scrollEl?.querySelectorAll('[data-item-id]').length ?? 0,
    rows: Array.from(scrollEl?.querySelectorAll<HTMLElement>('[data-item-id]') ?? [])
      .slice(0, MAX_TRACE_DOM_ROWS)
      .map((el) => ({
        itemId: el.dataset.itemId ?? '',
      })),
    scrollOffset: listRef ? Math.round(listRef.getScrollOffset()) : 0,
    scrollSize: listRef ? Math.round(listRef.getScrollSize()) : 0,
    viewportSize: listRef ? Math.round(listRef.getViewportSize()) : 0,
  }));
}

export function startTimelineRowResizeTrace(root: Element): () => void {
  if (!isUiRenderTraceEnabled()) return () => {};

  const ROW_SELECTOR = '[data-row-index]';
  const tracked = new Map<Element, { rowIndex: string; height: number | null }>();
  const dirtyRows = new Set<Element>();
  let measureFrame: number | null = null;

  const measureRowHeight = (el: Element): number =>
    Math.round((el as HTMLElement).getBoundingClientRect().height);

  const measureTrackedRows = () => {
    measureFrame = null;
    const rowsToMeasure = Array.from(dirtyRows);
    dirtyRows.clear();
    for (const el of rowsToMeasure) {
      const t = tracked.get(el);
      if (!t) continue;
      if (!el.isConnected) {
        tracked.delete(el);
        continue;
      }
      const newHeight = measureRowHeight(el);
      if (t.height === null) {
        t.height = newHeight;
        continue;
      }
      if (newHeight === t.height) continue;
      const prevHeight = t.height;
      t.height = newHeight;
      const itemEl = el.querySelector<HTMLElement>('[data-item-id]');
      recordUiTrace('timeline.row.resize', {
        rowIndex: t.rowIndex,
        itemId: itemEl?.dataset.itemId ?? '',
        prevHeight,
        newHeight,
        delta: newHeight - prevHeight,
      });
    }
  };

  const scheduleMeasure = () => {
    if (measureFrame !== null) return;
    measureFrame = requestAnimationFrame(measureTrackedRows);
  };

  const markDirty = (el: Element | null) => {
    if (!el || !tracked.has(el)) return;
    dirtyRows.add(el);
    scheduleMeasure();
  };

  const rowForMutationTarget = (target: Node): Element | null => {
    if (target instanceof Element) {
      if (target.matches(ROW_SELECTOR)) return target;
      return target.closest(ROW_SELECTOR);
    }
    return target.parentElement?.closest(ROW_SELECTOR) ?? null;
  };

  const trackElement = (el: Element) => {
    if (tracked.has(el)) return;
    tracked.set(el, {
      rowIndex: (el as HTMLElement).dataset.rowIndex ?? '',
      height: null,
    });
    markDirty(el);
  };

  const untrackElement = (el: Element) => {
    tracked.delete(el);
    dirtyRows.delete(el);
  };

  root.querySelectorAll(ROW_SELECTOR).forEach(trackElement);

  const mo = new MutationObserver((mutations) => {
    for (const m of mutations) {
      if (m.type === 'childList') {
        markDirty(rowForMutationTarget(m.target));
      }
      m.addedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(ROW_SELECTOR)) {
          trackElement(n);
        }
        n.querySelectorAll?.(ROW_SELECTOR).forEach((el) => {
          trackElement(el);
        });
      });
      m.removedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(ROW_SELECTOR)) untrackElement(n);
        n.querySelectorAll?.(ROW_SELECTOR).forEach(untrackElement);
      });
      if (m.type === 'attributes' || m.type === 'characterData') {
        markDirty(rowForMutationTarget(m.target));
      }
    }
  });
  mo.observe(root, {
    attributes: true,
    attributeFilter: ['class', 'style'],
    characterData: true,
    childList: true,
    subtree: true,
  });

  return () => {
    mo.disconnect();
    if (measureFrame !== null) cancelAnimationFrame(measureFrame);
    tracked.clear();
  };
}
