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
  summarizeItemsForTrace,
} from '../../utils/uiRenderTrace';

const MAX_TRACE_NODES = 120;
const MAX_TRACE_DOM_ROWS = 160;
const TRACE_TEXT_PREVIEW_CHARS = 120;

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
    nodes: groupedNodes.slice(0, MAX_TRACE_NODES).map((node) => {
      if (node.kind === 'leaf') {
        return {
          kind: 'leaf',
          itemId: node.item.id,
          itemThreadId: node.item.threadId,
          itemKind: node.item.kind,
          turnIndex: node.item.turnIndex,
          orphan: node.orphan === true,
        };
      }
      if (node.kind === 'group') {
        return {
          kind: 'group',
          parentId: node.parent.id,
          parentThreadId: node.parent.threadId,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'wait_group') {
        return {
          kind: 'wait_group',
          parentId: node.parent.id,
          parentThreadId: node.parent.threadId,
          childCount: node.children.length,
          turnIndex: node.parent.turnIndex,
        };
      }
      if (node.kind === 'read_group') {
        return {
          kind: 'read_group',
          groupKey: node.groupKey,
          threadId: node.threadId,
          memberCount: node.members.length,
          turnIndex: node.members[0]?.turnIndex ?? 0,
        };
      }
      return {
        kind: 'inline_subagent_group',
        groupKey: node.groupKey,
        threadId: node.threadId,
        memberCount: node.memberCount,
        childCount: node.members.length,
        turnIndex: node.members[0]?.parent.turnIndex ?? 0,
      };
    }),
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

export function startTimelineRowResizeTrace(root: Element): () => void {
  if (!isUiRenderTraceEnabled()) return () => {};

  const ROW_SELECTOR = '[data-row-index]';
  const tracked = new Map<Element, { rowIndex: string; height: number }>();

  const ro = new ResizeObserver((entries) => {
    for (const entry of entries) {
      const t = tracked.get(entry.target);
      if (!t) continue;
      const newHeight = Math.round(entry.contentRect.height);
      if (newHeight === t.height) continue;
      const prevHeight = t.height;
      t.height = newHeight;
      // Skip the initial 0/-1 -> N first measurement (no real "change").
      if (prevHeight < 0) continue;
      const targetEl = entry.target as HTMLElement;
      const itemId = targetEl.querySelector<HTMLElement>('[data-item-id]')?.dataset.itemId ?? '';
      const textPreview = (targetEl.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 100);
      recordUiTrace('timeline.row.resize', {
        rowIndex: t.rowIndex,
        itemId,
        prevHeight,
        newHeight,
        delta: newHeight - prevHeight,
        contentTags: fingerprintTimelineRow(targetEl),
        outerHTMLLen: targetEl.outerHTML.length,
        childCount: targetEl.querySelectorAll('*').length,
        descendants: inspectTimelineRowDescendants(targetEl),
        textPreview,
      });
    }
  });

  const trackElement = (el: Element) => {
    if (tracked.has(el)) return;
    tracked.set(el, {
      rowIndex: (el as HTMLElement).dataset.rowIndex ?? '',
      height: -1,
    });
    ro.observe(el);
  };

  const untrackElement = (el: Element) => {
    if (!tracked.delete(el)) return;
    ro.unobserve(el);
  };

  root.querySelectorAll(ROW_SELECTOR).forEach(trackElement);

  const mo = new MutationObserver((mutations) => {
    for (const m of mutations) {
      m.addedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(ROW_SELECTOR)) trackElement(n);
        n.querySelectorAll?.(ROW_SELECTOR).forEach(trackElement);
      });
      m.removedNodes.forEach((n) => {
        if (!(n instanceof Element)) return;
        if (n.matches(ROW_SELECTOR)) untrackElement(n);
        n.querySelectorAll?.(ROW_SELECTOR).forEach(untrackElement);
      });
    }
  });
  mo.observe(root, { childList: true, subtree: true });

  return () => {
    mo.disconnect();
    ro.disconnect();
    tracked.clear();
  };
}

function fingerprintTimelineRow(el: Element): string {
  const tags: string[] = [];
  if (el.querySelector('pre.shiki, pre[class*="shiki"]')) tags.push('shiki');
  if (el.querySelector('[class*="skeleton"], [class*="animate-pulse"]')) tags.push('skeleton');
  if (el.querySelector('[data-mermaid-source], svg.mermaid, .mermaid svg')) tags.push('mermaid');
  if (el.querySelector('.katex, [class*="katex"]')) tags.push('katex');
  if (el.querySelector('[data-streamdown-code]')) tags.push('sd-code');
  if (el.querySelector('img')) tags.push('img');
  if (el.querySelector('[data-testid="approval-card"]')) tags.push('approval');
  if (el.querySelector('[data-testid="todo-list"]')) tags.push('todo');
  if (el.querySelector('[data-testid*="working"]')) tags.push('working');
  return tags.join(',');
}

function inspectTimelineRowDescendants(el: Element): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const pre = el.querySelector('pre');
  if (pre) {
    out.preClass = (pre.className?.toString() ?? '').slice(0, 120);
    out.preChildCount = pre.children.length;
    out.preTextLen = (pre.textContent ?? '').length;
  }
  const sdCode = el.querySelector('[data-streamdown-code]');
  if (sdCode) out.sdCodeId = sdCode.getAttribute('data-streamdown-code') ?? '';
  const mermaid = el.querySelector('[data-mermaid-source]');
  if (mermaid) {
    out.mermaidHasSvg = mermaid.querySelector('svg') !== null;
    out.mermaidChildCount = mermaid.children.length;
  }
  const katex = el.querySelector('.katex, [class*="katex"]');
  if (katex) out.katexRendered = katex.querySelector('.katex-mathml') !== null;
  const img = el.querySelector('img');
  if (img) {
    out.imgComplete = (img as HTMLImageElement).complete;
    out.imgNaturalH = (img as HTMLImageElement).naturalHeight;
  }
  return out;
}
