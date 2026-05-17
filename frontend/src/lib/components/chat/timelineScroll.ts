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

interface TimelineAutoLoadOlderPrecheckState {
  offset: number;
  hasMoreHistory: boolean;
  loadingOlder: boolean;
  oldestLoadedTurnIndex: number | null;
  restoredThreadId: string | null;
  threadId: string | null;
  attemptedAtFloor: number | null;
  offsetThreshold: number;
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
  findFirstVisibleIndex: (offset: number) => number;
}

export interface AutoLoadOlderGate {
  readonly attemptedAtFloor: number | null;
  readonly armed: boolean;
  reset(): void;
  shouldLoad(state: AutoLoadOlderGateState): boolean;
  /**
   * Suspends auto-load until a real user gesture arrives (or the
   * fallback cooldown elapses). Called after every `pane.loadOlder()`
   * completes — without this, the anchor-restore programmatic scroll
   * that follows the prepend re-fires the gate on the next tick and
   * walks the entire history in a cascade.
   */
  disarm(): void;
  /**
   * Re-arms the gate. Wired to wheel / touchmove / keydown listeners
   * on the scroll surface so a real user gesture re-enables auto-load
   * exactly once per user action. Programmatic scrolls
   * (`listRef.scrollToIndex`, anchor restore) don't fire these events
   * so they cannot re-arm.
   */
  armOnGesture(): void;
}

/**
 * Fallback cooldown that re-arms the gate even when no user gesture
 * is detected. Belt-and-braces against edge devices (e.g., touch
 * momentum-scroll that doesn't fire continuous `touchmove`) — the
 * primary mechanism is gesture detection. 350ms is the smallest
 * window that doesn't make the next intentional scroll feel laggy.
 */
const AUTO_LOAD_COOLDOWN_MS = 350;

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

function shouldInspectAutoLoadOlderIndex(state: TimelineAutoLoadOlderPrecheckState): boolean {
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

function isAutoLoadOlderIndexEligible(
  firstVisibleIndex: number,
  indexThreshold: number,
): boolean {
  return firstVisibleIndex <= indexThreshold;
}

export function createAutoLoadOlderGate({
  offsetThreshold,
  indexThreshold,
}: AutoLoadOlderGateOptions): AutoLoadOlderGate {
  let attemptedAtFloor: number | null = null;
  let armed = true;
  let cooldownTimer: ReturnType<typeof setTimeout> | null = null;

  function clearCooldown(): void {
    if (cooldownTimer !== null) {
      clearTimeout(cooldownTimer);
      cooldownTimer = null;
    }
  }

  return {
    get attemptedAtFloor() { return attemptedAtFloor; },
    get armed() { return armed; },
    reset(): void {
      // Thread switch: clear all state. The new thread should be free
      // to auto-load older from frame 0 if its initial slice triggers
      // the geometry conditions.
      attemptedAtFloor = null;
      armed = true;
      clearCooldown();
    },
    disarm(): void {
      armed = false;
      clearCooldown();
      cooldownTimer = setTimeout(() => {
        // Fallback: if no user gesture arrives within the cooldown
        // window, re-arm anyway. Defends against edge devices where
        // gesture detection misses an event (touch momentum scroll
        // with sparse `touchmove`).
        armed = true;
        cooldownTimer = null;
      }, AUTO_LOAD_COOLDOWN_MS);
    },
    armOnGesture(): void {
      armed = true;
      clearCooldown();
    },
    shouldLoad(state: AutoLoadOlderGateState): boolean {
      // Gesture-armed: after each load, `disarm()` flips this false.
      // Only a real user gesture (wheel/touchmove/keydown wired by
      // the timeline) or the cooldown fallback can flip it back true.
      // Programmatic scrolls don't fire those events, so the
      // anchor-restore after a load cannot cascade.
      if (!armed) return false;

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

      const firstVisibleIndex = state.findFirstVisibleIndex(state.offset);
      if (!isAutoLoadOlderIndexEligible(firstVisibleIndex, indexThreshold)) return false;

      attemptedAtFloor = state.oldestLoadedTurnIndex;
      return true;
    },
  };
}
