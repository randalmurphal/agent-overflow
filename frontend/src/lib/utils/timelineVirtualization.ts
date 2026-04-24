import type { TimelineNode } from './subagentGrouping';

export type VirtualLayout = {
  rows: Array<{ node: TimelineNode; index: number; key: string; height: number }>;
  offsets: number[];
  totalHeight: number;
};

export type VirtualWindow = {
  start: number;
  end: number;
  before: number;
  after: number;
  rows: Array<{ node: TimelineNode; index: number; key: string }>;
};

export function timelineNodeKey(node: TimelineNode): string {
  return node.kind === 'group'
    ? `g:${node.parent.threadId}:${node.parent.id}`
    : `l:${node.item.threadId}:${node.item.id}`;
}

export function buildVirtualLayout(
  nodes: TimelineNode[],
  rowHeights: Map<string, number>,
  estimatedRowHeight: number,
): VirtualLayout {
  const rows = nodes.map((node, index) => {
    const key = timelineNodeKey(node);
    return {
      node,
      index,
      key,
      height: rowHeights.get(key) ?? estimatedRowHeight,
    };
  });
  const activeKeys = new Set(rows.map((row) => row.key));
  for (const key of rowHeights.keys()) {
    if (!activeKeys.has(key)) {
      rowHeights.delete(key);
    }
  }

  const offsets = new Array<number>(rows.length + 1);
  offsets[0] = 0;
  for (let index = 0; index < rows.length; index += 1) {
    offsets[index + 1] = offsets[index] + rows[index].height;
  }
  return { rows, offsets, totalHeight: offsets[rows.length] };
}

export function computeVirtualWindow(
  layout: VirtualLayout,
  scrollTop: number,
  height: number,
  overscanPx: number,
): VirtualWindow {
  if (layout.rows.length === 0) {
    return { start: 0, end: 0, before: 0, after: 0, rows: [] };
  }

  const visibleTop = Math.max(0, scrollTop - overscanPx);
  const visibleBottom = scrollTop + Math.max(height, 1) + overscanPx;
  const start = Math.max(0, upperBound(layout.offsets, visibleTop) - 1);
  const end = Math.min(layout.rows.length, Math.max(start + 1, upperBound(layout.offsets, visibleBottom)));

  return {
    start,
    end,
    before: layout.offsets[start],
    after: layout.totalHeight - layout.offsets[end],
    rows: layout.rows.slice(start, end),
  };
}

function upperBound(values: number[], target: number): number {
  let low = 0;
  let high = values.length;
  while (low < high) {
    const mid = Math.floor((low + high) / 2);
    if (values[mid] <= target) {
      low = mid + 1;
    } else {
      high = mid;
    }
  }
  return low;
}
