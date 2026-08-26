import { compareCursors, type TimelineCursorLike } from '../../stores/threadItems';
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

/**
 * A node held by its BOTTOM edge instead of its top.
 *
 * Which edge is held decides which way a height change moves the page. Holding
 * a node above the change keeps the timeline growing downward, which is right
 * when rows are being removed out from under a reader who is not looking at
 * them (the window prune). Holding a node at the viewport's bottom sends the
 * change upward instead, which is right when the reader ASKED for it: a run
 * they expanded should reveal itself above the rows they are already reading,
 * not shove those rows down the page.
 */
export interface TimelineTailAnchor {
  itemId: string;
  /** Node bottom minus viewport bottom — 0 when the two are flush. */
  offsetBottom: number;
}

export interface TimelineGeometry {
  findItemIndex(offset: number): number;
  getItemOffset(index: number): number;
}

/** Adds what a bottom-edge anchor needs on top of `TimelineGeometry`. */
export interface TimelineTailGeometry extends TimelineGeometry {
  getViewportSize(): number;
  sizeAt(index: number): number;
}

/**
 * Lazy geometry predicate. Returns true when the viewport sits inside the
 * prefetch zone at the loading edge — near the top for an older-gate, near
 * the bottom for a newer-gate. The gate calls this LAST, only after the
 * cheap state gates pass, so the per-scroll-frame engine index lookup it
 * performs stays off the hot path while the gate is disarmed, mid-load, or
 * already-attempted at the current floor.
 */
export type AutoLoadTriggerZone = () => boolean;

export interface AutoLoadGateState {
  hasMore: boolean;
  loading: boolean;
  /**
   * Floor cursor in the load direction: the oldest loaded item for an
   * older-gate, the newest loaded for a newer-gate. The progress guard
   * compares the FULL cursor (turnIndex AND itemIndex). Paging within a
   * single huge turn advances itemIndex but never turnIndex, so a guard
   * keyed on turnIndex alone reads "no progress" and latches auto-load off
   * — the common "one giant turn" Codex/Claude thread. Comparing the whole
   * cursor keeps paging alive through such turns.
   */
  floorCursor: TimelineCursorLike | null;
  restoredThreadId: string | null;
  threadId: string | null;
  inTriggerZone: AutoLoadTriggerZone;
}

export interface AutoLoadGate {
  readonly attemptedAtFloor: TimelineCursorLike | null;
  readonly armed: boolean;
  reset(): void;
  shouldLoad(state: AutoLoadGateState): boolean;
  /**
   * Suspends auto-load until a real user gesture arrives (or the fallback
   * cooldown elapses). Called after every load completes — without this,
   * the anchor-restore programmatic scroll that follows an older prepend,
   * or the anchor-preserving prune that follows a newer append, re-fires
   * the gate on the next tick and walks the history in a cascade.
   */
  disarm(): void;
  /**
   * Re-arms the gate. Wired to wheel / touchmove / keydown listeners on the
   * scroll surface so a real user gesture re-enables auto-load exactly once
   * per user action. Programmatic scrolls (`listRef.scrollToIndex`, anchor
   * restore) don't fire these events so they cannot re-arm.
   */
  armOnGesture(): void;
}

export interface AutoLoadZoneThresholds {
  offsetThreshold: number;
  indexThreshold: number;
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

/**
 * The node the viewport's bottom edge is resting on, and how far past that edge
 * its own bottom sits.
 *
 * `bottom - 1` rather than `bottom`, because an offset exactly on a node
 * boundary belongs to the node BELOW it — the one the reader cannot see yet.
 * Anchoring to that one would hold the wrong row still by exactly one row.
 */
export function captureTimelineTailAnchor(
  nodes: readonly TimelineNode[],
  geometry: TimelineTailGeometry,
  offset: number,
): TimelineTailAnchor | null {
  const bottom = offset + geometry.getViewportSize();
  const rawIdx = geometry.findItemIndex(Math.max(0, bottom - 1));
  if (rawIdx < 0) return null;
  const idx = Math.min(rawIdx, nodes.length - 1);
  const node = nodes[idx];
  if (!node) return null;
  return {
    itemId: timelineNodeItemId(node),
    offsetBottom: geometry.getItemOffset(idx) + geometry.sizeAt(idx) - bottom,
  };
}

/**
 * Precondition for BOTH auto-load zones: the scroller is long enough that
 * the top zone and the bottom zone are disjoint. The top zone is
 * `offset < offsetThreshold` and the bottom zone is
 * `range - offset < offsetThreshold`, so with `range < 2 × offsetThreshold`
 * every offset sits in at least one zone — and a timeline whose items
 * collapse into a few tall nodes (a single giant activity run: 700 items,
 * ~3 nodes, ~150px of outer range) sits in BOTH at once, forever. The
 * per-zone index gates cannot save it: they count NODES, and the run is one
 * node. In that state any scroll event with an armed gate fired
 * `loadOlder`, which escaped bottom-follow and head-pruned the live tail
 * out of the window (soak incident 2026-08-25: follow died mid-stream,
 * then the two zones ping-ponged loadOlder/loadNewer under the reader).
 * Auto-paging stands down until the geometry can express reader intent;
 * the manual chips and jump-to-latest remain the affordance.
 */
export function autoLoadZonesDisjoint(
  scrollRange: number,
  thresholds: AutoLoadZoneThresholds,
): boolean {
  return scrollRange >= thresholds.offsetThreshold * 2;
}

/**
 * Older edge: viewport within `offsetThreshold` px of the top AND the
 * topmost rendered row within the first `indexThreshold` nodes. The
 * `firstVisibleIndex` thunk is invoked only after the cheap offset
 * pre-check passes, keeping the engine lookup off the hot path.
 */
export function isWithinTopTriggerZone(
  offset: number,
  thresholds: AutoLoadZoneThresholds,
  firstVisibleIndex: () => number,
): boolean {
  if (offset >= thresholds.offsetThreshold) return false;
  return firstVisibleIndex() <= thresholds.indexThreshold;
}

/**
 * Newer edge: viewport within `offsetThreshold` px of the bottom AND the
 * bottommost rendered row within the last `indexThreshold` nodes. Mirror of
 * `isWithinTopTriggerZone`; `lastVisibleIndex` is a thunk for the same
 * deferral reason.
 */
export function isWithinBottomTriggerZone(
  distanceFromBottom: number,
  nodeCount: number,
  thresholds: AutoLoadZoneThresholds,
  lastVisibleIndex: () => number,
): boolean {
  if (distanceFromBottom >= thresholds.offsetThreshold) return false;
  return lastVisibleIndex() >= nodeCount - 1 - thresholds.indexThreshold;
}

export interface BottomEdgeGeometry {
  distanceFromBottom: number;
  bottomProbeOffset: number;
}

/**
 * Derives the two bottom-edge quantities the newer trigger needs from raw
 * scroll geometry: how far the viewport is from the scrollable bottom, and
 * the offset to probe for the bottommost rendered row. Pulled out as a pure
 * function so the (sign-error-prone) arithmetic is unit-testable without a
 * live DOM — happy-dom reports zero geometry. `bottomProbeOffset` lands in
 * the composer padding past the last row at max scroll, which clamps the
 * engine lookup to the final index (intended).
 */
export function bottomEdgeGeometry(
  scrollHeight: number,
  clientHeight: number,
  offset: number,
): BottomEdgeGeometry {
  return {
    distanceFromBottom: scrollHeight - clientHeight - offset,
    bottomProbeOffset: offset + clientHeight - 1,
  };
}

/**
 * Direction-agnostic auto-load gate. The older and newer triggers share
 * one arming/cooldown/progress state machine; only the floor cursor
 * (oldest vs newest) and the `inTriggerZone` geometry differ, and both are
 * supplied per `shouldLoad` call.
 */
export function createAutoLoadGate(): AutoLoadGate {
  let attemptedAtFloor: TimelineCursorLike | null = null;
  let armed = true;
  let cooldownTimer: ReturnType<typeof setTimeout> | null = null;

  function clearCooldown(): void {
    if (cooldownTimer !== null) {
      clearTimeout(cooldownTimer);
      cooldownTimer = null;
    }
  }

  // First attempt (null) always counts as progress; afterwards the floor
  // cursor must have moved since the last attempt — compared on the FULL
  // cursor so item-level progress within one turn re-enables the load.
  function madeProgressSince(floorCursor: TimelineCursorLike): boolean {
    return attemptedAtFloor === null
      || compareCursors(attemptedAtFloor, floorCursor) !== 0;
  }

  return {
    get attemptedAtFloor() { return attemptedAtFloor; },
    get armed() { return armed; },
    reset(): void {
      // Thread switch: clear all state. The new thread should be free to
      // auto-load from frame 0 if its initial slice triggers the geometry.
      attemptedAtFloor = null;
      armed = true;
      clearCooldown();
    },
    disarm(): void {
      armed = false;
      clearCooldown();
      cooldownTimer = setTimeout(() => {
        // Fallback: if no user gesture arrives within the cooldown window,
        // re-arm anyway. Defends against edge devices where gesture
        // detection misses an event (touch momentum scroll with sparse
        // `touchmove`).
        armed = true;
        cooldownTimer = null;
      }, AUTO_LOAD_COOLDOWN_MS);
    },
    armOnGesture(): void {
      armed = true;
      clearCooldown();
    },
    shouldLoad(state: AutoLoadGateState): boolean {
      // Gesture-armed: after each load, `disarm()` flips this false. Only a
      // real user gesture (wheel/touchmove/keydown wired by the timeline) or
      // the cooldown fallback flips it back true. Programmatic scrolls don't
      // fire those events, so a post-load re-anchor cannot cascade.
      if (!armed) return false;
      if (!state.hasMore) return false;
      if (state.loading) return false;
      // pane.loadOlder/loadNewer already noop on a null floor, but the
      // progress guard needs a concrete cursor to compare. Without this,
      // malformed backend state (`hasMore=true` with no loaded items)
      // re-fires the load on every scroll tick.
      if (state.floorCursor === null) return false;
      // Restoration must finish first. Loading mid-restore races the anchor
      // capture against an unstable scrollTop.
      if (state.restoredThreadId !== state.threadId) return false;
      // Progress guard: don't hammer the same query while the user lingers
      // at the edge if the previous attempt didn't move the floor cursor.
      if (!madeProgressSince(state.floorCursor)) return false;
      // Expensive engine geometry — deferred until the cheap gates pass.
      if (!state.inTriggerZone()) return false;
      attemptedAtFloor = {
        turnIndex: state.floorCursor.turnIndex,
        itemIndex: state.floorCursor.itemIndex,
      };
      return true;
    },
  };
}
