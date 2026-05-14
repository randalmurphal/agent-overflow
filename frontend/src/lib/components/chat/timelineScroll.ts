import type { Item } from '../../types/models';
import {
  findTimelineNodeIndex,
  timelineNodeItemId,
  visibleTimelineItemIdForItem,
  type TimelineNode,
} from '../../utils/subagentGrouping';

export interface TimelineAnchor {
  itemId: string;
  offsetTop: number;
}

export interface TimelineGeometry {
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
}

export interface TimelineAutoLoadOlderState {
  offset: number;
  firstVisibleIndex: number;
  hasMoreHistory: boolean;
  loadingOlder: boolean;
  oldestLoadedTurnIndex: number | null;
  restoredThreadId: string | null;
  threadId: string | null;
  attemptedAtFloor: number | null;
  offsetThreshold: number;
  indexThreshold: number;
}

export function resolveVisibleTimelineNodeIndex(
  nodes: TimelineNode[],
  items: readonly Item[],
  itemId: string,
): number {
  const direct = findTimelineNodeIndex(nodes, itemId);
  if (direct >= 0) return direct;
  const visibleItemId = visibleTimelineItemIdForItem(items, itemId);
  return visibleItemId === itemId ? -1 : findTimelineNodeIndex(nodes, visibleItemId);
}

export function timelineRowElementForIndex(
  root: ParentNode | undefined | null,
  index: number,
): HTMLElement | null {
  return root?.querySelector(`[data-row-index="${index}"]`) ?? null;
}

export function centeredScrollTop(
  rowTop: number,
  rowHeight: number,
  viewportHeight: number,
): number {
  return rowTop - Math.max(0, (viewportHeight - rowHeight) / 2);
}

export function captureTimelineAnchor(
  nodes: readonly TimelineNode[],
  geometry: TimelineGeometry,
  offset: number,
  opts: { clampIndex?: boolean } = {},
): TimelineAnchor | null {
  const rawIdx = geometry.findItemIndex(offset);
  if (rawIdx < 0) return null;
  const idx = opts.clampIndex ? Math.min(rawIdx, nodes.length - 1) : rawIdx;
  const node = nodes[idx];
  if (!node) return null;
  return {
    itemId: timelineNodeItemId(node),
    offsetTop: geometry.getItemOffset(idx) - offset,
  };
}

export function shouldAutoLoadOlder(state: TimelineAutoLoadOlderState): boolean {
  if (!state.hasMoreHistory) return false;
  if (state.loadingOlder) return false;
  if (state.oldestLoadedTurnIndex === null) return false;
  if (state.restoredThreadId !== state.threadId) return false;
  if (state.offset >= state.offsetThreshold) return false;
  if (state.firstVisibleIndex > state.indexThreshold) return false;
  return state.attemptedAtFloor !== state.oldestLoadedTurnIndex;
}
