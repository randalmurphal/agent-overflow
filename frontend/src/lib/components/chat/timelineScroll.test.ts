import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import {
  bottomEdgeGeometry,
  captureTimelineAnchor,
  centeredScrollTop,
  createAutoLoadGate,
  isWithinBottomTriggerZone,
  isWithinTopTriggerZone,
  resolveVisibleTimelineNodeIndex,
  timelineRowElementForIndex,
  type AutoLoadGateState,
  type TimelineGeometry,
} from './timelineScroll';

function geometry(indexForOffset: number, offsetForIndex: number): TimelineGeometry {
  return {
    findItemIndex: vi.fn(() => indexForOffset),
    getItemOffset: vi.fn(() => offsetForIndex),
  };
}

function gateState(overrides: Partial<AutoLoadGateState> = {}): AutoLoadGateState {
  return {
    hasMore: true,
    loading: false,
    floorCursor: { turnIndex: 10, itemIndex: 0 },
    restoredThreadId: 'thread-1',
    threadId: 'thread-1',
    inTriggerZone: () => true,
    ...overrides,
  };
}

describe('timelineScroll', () => {
  it('resolves hidden Codex child rows back to their visible timeline row', () => {
    const spawn = makeItem({
      id: 'codex-agent',
      kind: 'tool_call',
      toolName: 'collab_agent',
      meta: JSON.stringify({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
    });
    const child = makeItem({
      id: 'child-answer',
      itemIndex: 1,
      kind: 'assistant_text',
      parentId: spawn.id,
      summary: 'hidden child',
    });
    const items = [spawn, child];
    const nodes = groupItemsBySubagent(items);

    expect(resolveVisibleTimelineNodeIndex(nodes, items, 'child-answer')).toBe(0);
    expect(resolveVisibleTimelineNodeIndex(nodes, items, 'missing')).toBe(-1);
  });

  it('finds timeline row elements by virtualizer row index', () => {
    const root = document.createElement('div');
    root.innerHTML = '<div data-row-index="3"></div>';

    expect(timelineRowElementForIndex(root, 3)).toBe(root.firstElementChild);
    expect(timelineRowElementForIndex(root, 4)).toBeNull();
    expect(timelineRowElementForIndex(undefined, 3)).toBeNull();
  });

  it('computes centered scrollTop from row and viewport geometry', () => {
    expect(centeredScrollTop(500, 100, 300)).toBe(400);
    expect(centeredScrollTop(500, 400, 300)).toBe(500);
  });

  it('captures an anchor at the current scroll offset', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    const g = geometry(1, 240);

    expect(captureTimelineAnchor(nodes, g, 200)).toEqual({
      itemId: 'b',
      offsetTop: 40,
    });
    expect(g.findItemIndex).toHaveBeenCalledWith(200);
    expect(g.getItemOffset).toHaveBeenCalledWith(1);
  });

  it('optionally clamps anchor capture to the loaded node window', () => {
    const nodes = groupItemsBySubagent([
      makeItem({ id: 'a', summary: 'first' }),
      makeItem({ id: 'b', itemIndex: 1, summary: 'second' }),
    ]);
    const g = geometry(100, 240);

    expect(captureTimelineAnchor(nodes, g, 200)).toBeNull();
    expect(captureTimelineAnchor(nodes, g, 200, { clampIndex: true })).toEqual({
      itemId: 'b',
      offsetTop: 40,
    });
    expect(g.getItemOffset).toHaveBeenLastCalledWith(1);
  });

  it('returns null when virtualizer cannot map the offset to a row', () => {
    const nodes = groupItemsBySubagent([makeItem({ id: 'a', summary: 'first' })]);

    expect(captureTimelineAnchor(nodes, geometry(-1, 0), 200)).toBeNull();
  });

  // ============================================================
  // Trigger-zone geometry (pure helpers)
  // ============================================================

  it('isWithinTopTriggerZone defers the index probe until the offset pre-check passes', () => {
    const thresholds = { offsetThreshold: 800, indexThreshold: 5 };

    const probePastOffset = vi.fn(() => 0);
    expect(isWithinTopTriggerZone(800, thresholds, probePastOffset)).toBe(false);
    expect(probePastOffset).not.toHaveBeenCalled();

    expect(isWithinTopTriggerZone(799, thresholds, () => 5)).toBe(true);
    expect(isWithinTopTriggerZone(799, thresholds, () => 6)).toBe(false);
  });

  it('isWithinBottomTriggerZone mirrors the top zone at the bottom edge', () => {
    const thresholds = { offsetThreshold: 800, indexThreshold: 5 };

    const probePastDistance = vi.fn(() => 0);
    expect(isWithinBottomTriggerZone(800, 100, thresholds, probePastDistance)).toBe(false);
    expect(probePastDistance).not.toHaveBeenCalled();

    // count=100 → last index 99; within 5 of the end means index >= 94.
    expect(isWithinBottomTriggerZone(799, 100, thresholds, () => 94)).toBe(true);
    expect(isWithinBottomTriggerZone(799, 100, thresholds, () => 93)).toBe(false);
    expect(isWithinBottomTriggerZone(799, 100, thresholds, () => 99)).toBe(true);
  });

  it('bottomEdgeGeometry derives distance-from-bottom and the bottom-row probe offset', () => {
    // At max scroll (scrollTop = scrollHeight - clientHeight) distance is 0
    // and the probe lands at the last scrollable pixel.
    expect(bottomEdgeGeometry(1000, 600, 400)).toEqual({
      distanceFromBottom: 0,
      bottomProbeOffset: 999,
    });
    // Mid-scroll: 1000 - 600 - 250 = 150 px from the bottom; probe at the
    // viewport's bottom edge.
    expect(bottomEdgeGeometry(1000, 600, 250)).toEqual({
      distanceFromBottom: 150,
      bottomProbeOffset: 849,
    });
  });

  // ============================================================
  // Auto-load gate — progress guard
  // ============================================================

  it('fires once per floor cursor, blocks until it advances, and resets explicitly', () => {
    const gate = createAutoLoadGate();

    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 10, itemIndex: 0 } }))).toBe(true);
    expect(gate.attemptedAtFloor).toEqual({ turnIndex: 10, itemIndex: 0 });

    // Same floor, no progress since the last attempt → blocked.
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 10, itemIndex: 0 } }))).toBe(false);

    // Floor advanced to an older turn → fires again.
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 8, itemIndex: 0 } }))).toBe(true);
    expect(gate.attemptedAtFloor).toEqual({ turnIndex: 8, itemIndex: 0 });

    gate.reset();
    expect(gate.attemptedAtFloor).toBeNull();
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 8, itemIndex: 0 } }))).toBe(true);
  });

  // Regression: bug-report-20260616T143320Z. A thread whose loaded window is
  // a single 400-item turn (turnIndex 57). Paging older advanced the item
  // cursor (itemIndex 200 → 400) but never the turn index, so the previous
  // turn-index-keyed progress guard latched auto-load off and the user had
  // to use the manual button. The full-cursor compare treats item-level
  // progress within one turn as progress. Fails on the turn-index guard.
  it('treats item-level progress within a single long turn as progress', () => {
    const gate = createAutoLoadGate();

    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 57, itemIndex: 200 } }))).toBe(true);

    // No movement → still blocked.
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 57, itemIndex: 200 } }))).toBe(false);

    // Same turn, deeper item floor (200 more items of turn 57 paged in) →
    // this must fire. A turnIndex-only guard would read 57 === 57 and block.
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 57, itemIndex: 400 } }))).toBe(true);
    expect(gate.attemptedAtFloor).toEqual({ turnIndex: 57, itemIndex: 400 });
  });

  it('stores a clone of the floor cursor, not the live reference', () => {
    // The progress guard must snapshot the cursor at attempt time. If it
    // aliased the pane's live `$state` cursor, a later mutation of that
    // object would silently change `attemptedAtFloor` and break the
    // same-floor block. Pin the clone so a regression to a reference assign
    // is caught (the `toEqual` checks above would not catch it).
    const gate = createAutoLoadGate();
    const liveCursor = { turnIndex: 12, itemIndex: 3 };

    expect(gate.shouldLoad(gateState({ floorCursor: liveCursor }))).toBe(true);
    expect(gate.attemptedAtFloor).not.toBe(liveCursor);
    expect(gate.attemptedAtFloor).toEqual({ turnIndex: 12, itemIndex: 3 });
  });

  // ============================================================
  // Auto-load gate — cheap gates / geometry deferral
  // ============================================================

  it.each([
    ['missing more', { hasMore: false }],
    ['active load', { loading: true }],
    ['null floor', { floorCursor: null }],
    ['unrestored thread', { restoredThreadId: null }],
    ['wrong restored thread', { restoredThreadId: 'thread-2' }],
  ])('does not auto-load for %s before the trigger-zone probe', (_, overrides) => {
    const gate = createAutoLoadGate();
    const inTriggerZone = vi.fn(() => true);

    expect(gate.shouldLoad(gateState({ ...overrides, inTriggerZone }))).toBe(false);
    expect(inTriggerZone).not.toHaveBeenCalled();
    expect(gate.attemptedAtFloor).toBeNull();
  });

  it('does not load when outside the trigger zone, and records no attempt', () => {
    const gate = createAutoLoadGate();

    expect(gate.shouldLoad(gateState({ inTriggerZone: () => false }))).toBe(false);
    expect(gate.attemptedAtFloor).toBeNull();
  });

  it('probes the trigger zone only after the cheap gates pass', () => {
    const gate = createAutoLoadGate();
    const inTriggerZone = vi.fn(() => true);

    // A cheap gate fails → probe never runs.
    expect(gate.shouldLoad(gateState({ loading: true, inTriggerZone }))).toBe(false);
    expect(inTriggerZone).not.toHaveBeenCalled();

    // Cheap gates pass → probe runs exactly once.
    expect(gate.shouldLoad(gateState({ inTriggerZone }))).toBe(true);
    expect(inTriggerZone).toHaveBeenCalledTimes(1);
  });

  // ============================================================
  // Auto-load gate — disarm / re-arm state machine
  // ============================================================
  // Without these guards the floor-progress check let the auto-load cascade
  // walk the whole thread: each load advanced the floor, the progress
  // comparison flipped back to allow, the post-load programmatic scroll
  // fired the gate again, and so on. The shape requires a real user gesture
  // (or the 350ms cooldown fallback) to re-arm.

  it('disarm() blocks shouldLoad even when geometry and progress would pass', () => {
    const gate = createAutoLoadGate();

    expect(gate.armed).toBe(true);
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 10, itemIndex: 0 } }))).toBe(true);

    gate.disarm();
    expect(gate.armed).toBe(false);

    // Even with a fresh floor (would normally satisfy the progress guard),
    // the gate refuses while disarmed. This is the cascade prevention.
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 8, itemIndex: 0 } }))).toBe(false);
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 5, itemIndex: 0 } }))).toBe(false);
  });

  it('armOnGesture() re-enables shouldLoad after disarm', () => {
    const gate = createAutoLoadGate();

    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 10, itemIndex: 0 } }))).toBe(true);
    gate.disarm();
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 8, itemIndex: 0 } }))).toBe(false);

    // A real user gesture (wheel / touchmove / keydown wired by
    // MessageTimeline.svelte) re-arms exactly once.
    gate.armOnGesture();
    expect(gate.armed).toBe(true);
    expect(gate.shouldLoad(gateState({ floorCursor: { turnIndex: 8, itemIndex: 0 } }))).toBe(true);
  });

  it('disarm cooldown re-arms after the fallback timeout', () => {
    vi.useFakeTimers();
    try {
      const gate = createAutoLoadGate();
      gate.disarm();
      expect(gate.armed).toBe(false);

      // Cooldown is AUTO_LOAD_COOLDOWN_MS = 350ms. Anything before that must
      // keep the gate disarmed (gesture detection is the primary mechanism;
      // the timer is a fallback).
      vi.advanceTimersByTime(349);
      expect(gate.armed).toBe(false);

      vi.advanceTimersByTime(2);
      expect(gate.armed).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('reset() re-arms and clears the cooldown timer on thread switch', () => {
    vi.useFakeTimers();
    try {
      const gate = createAutoLoadGate();
      gate.disarm();
      expect(gate.armed).toBe(false);

      gate.reset();
      expect(gate.armed).toBe(true);

      // The pending cooldown timer must be cleared so it can't fire later
      // and surprise a freshly-switched thread.
      vi.advanceTimersByTime(1000);
      expect(gate.armed).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('armOnGesture() clears the cooldown so a future disarm gets a fresh window', () => {
    vi.useFakeTimers();
    try {
      const gate = createAutoLoadGate();
      gate.disarm();
      vi.advanceTimersByTime(100);
      gate.armOnGesture();
      expect(gate.armed).toBe(true);

      // The original cooldown's leftover 250ms must NOT fire and mistakenly
      // "re-arm" the gate after a subsequent disarm.
      gate.disarm();
      vi.advanceTimersByTime(250);
      expect(gate.armed).toBe(false);

      vi.advanceTimersByTime(101);
      expect(gate.armed).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });
});
