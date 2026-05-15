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

export interface TimelineAutoLoadOlderPrecheckState {
  offset: number;
  hasMoreHistory: boolean;
  loadingOlder: boolean;
  oldestLoadedTurnIndex: number | null;
  restoredThreadId: string | null;
  threadId: string | null;
  attemptedAtFloor: number | null;
  offsetThreshold: number;
}

export interface TimelineAutoLoadOlderState extends TimelineAutoLoadOlderPrecheckState {
  firstVisibleIndex: number;
  indexThreshold: number;
}

export interface AutoLoadOlderGateOptions {
  offsetThreshold: number;
  indexThreshold: number;
}

export interface AutoLoadOlderGateState {
  offset: number;
  hasMoreHistory: boolean;
  loadingOlder: boolean;
  oldestLoadedTurnIndex: number | null;
  restoredThreadId: string | null;
  threadId: string | null;
  findFirstVisibleIndex: () => number;
}

export interface AutoLoadOlderGate {
  readonly attemptedAtFloor: number | null;
  reset(): void;
  shouldLoad(state: AutoLoadOlderGateState): boolean;
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

export function shouldInspectAutoLoadOlderIndex(state: TimelineAutoLoadOlderPrecheckState): boolean {
  if (!state.hasMoreHistory) return false;
  if (state.loadingOlder) return false;

  // `pane.loadOlder()` already noops on a null floor, but the progress
  // guard below only works when there is a concrete floor to compare.
  // Without this, malformed backend state (`hasMore=true` with no loaded
  // items) can re-fire the load on every scroll tick.
  if (state.oldestLoadedTurnIndex === null) return false;

  // Restoration must finish first. Loading older rows mid-restore races
  // the anchor capture against an unstable scrollTop.
  if (state.restoredThreadId !== state.threadId) return false;
  if (state.offset >= state.offsetThreshold) return false;

  // Progress guard: if a previous attempt did not advance the floor, do
  // not hammer the same query while the user lingers near the top.
  return state.attemptedAtFloor !== state.oldestLoadedTurnIndex;
}

export function isAutoLoadOlderIndexEligible(
  firstVisibleIndex: number,
  indexThreshold: number,
): boolean {
  return firstVisibleIndex <= indexThreshold;
}

export function shouldAutoLoadOlder(state: TimelineAutoLoadOlderState): boolean {
  return shouldInspectAutoLoadOlderIndex(state)
    && isAutoLoadOlderIndexEligible(state.firstVisibleIndex, state.indexThreshold);
}

export function createAutoLoadOlderGate({
  offsetThreshold,
  indexThreshold,
}: AutoLoadOlderGateOptions): AutoLoadOlderGate {
  let attemptedAtFloor: number | null = null;
  return {
    get attemptedAtFloor() { return attemptedAtFloor; },
    reset(): void {
      attemptedAtFloor = null;
    },
    shouldLoad(state: AutoLoadOlderGateState): boolean {
      if (!shouldInspectAutoLoadOlderIndex({
        offset: state.offset,
        hasMoreHistory: state.hasMoreHistory,
        loadingOlder: state.loadingOlder,
        oldestLoadedTurnIndex: state.oldestLoadedTurnIndex,
        restoredThreadId: state.restoredThreadId,
        threadId: state.threadId,
        attemptedAtFloor,
        offsetThreshold,
      })) return false;

      const firstVisibleIndex = state.findFirstVisibleIndex();
      if (!isAutoLoadOlderIndexEligible(firstVisibleIndex, indexThreshold)) return false;

      attemptedAtFloor = state.oldestLoadedTurnIndex;
      return true;
    },
  };
}
