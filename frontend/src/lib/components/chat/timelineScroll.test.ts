import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import {
  captureTimelineAnchor,
  centeredScrollTop,
  createAutoLoadOlderGate,
  resolveVisibleTimelineNodeIndex,
  timelineRowElementForIndex,
  type TimelineGeometry,
} from './timelineScroll';

function geometry(indexForOffset: number, offsetForIndex: number): TimelineGeometry {
  return {
    findItemIndex: vi.fn(() => indexForOffset),
    getItemOffset: vi.fn(() => offsetForIndex),
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

  it('tracks auto-load attempts by floor and resets explicitly', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn((offset: number) => {
      expect(offset).toBe(799);
      return 5;
    });
    const base = {
      offset: 799,
      hasMoreHistory: true,
      loadingOlder: false,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex,
    };

    expect(gate.shouldLoad({
      ...base,
      oldestLoadedTurnIndex: 10,
    })).toBe(true);
    expect(gate.attemptedAtFloor).toBe(10);

    expect(gate.shouldLoad({
      ...base,
      oldestLoadedTurnIndex: 10,
    })).toBe(false);

    expect(gate.shouldLoad({
      ...base,
      oldestLoadedTurnIndex: 8,
    })).toBe(true);
    expect(gate.attemptedAtFloor).toBe(8);

    gate.reset();
    expect(gate.attemptedAtFloor).toBeNull();
    expect(gate.shouldLoad({
      ...base,
      oldestLoadedTurnIndex: 8,
    })).toBe(true);
  });

  it.each([
    ['missing history', { hasMoreHistory: false }],
    ['active load', { loadingOlder: true }],
    ['null floor', { oldestLoadedTurnIndex: null }],
    ['unrestored thread', { restoredThreadId: null }],
    ['wrong restored thread', { restoredThreadId: 'thread-2' }],
    ['past offset threshold', { offset: 800 }],
  ])('does not auto-load older items for %s before index lookup', (_, overrides) => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn((_offset: number) => 5);

    expect(gate.shouldLoad({
      offset: 799,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex,
      ...overrides,
    })).toBe(false);
    expect(findFirstVisibleIndex).not.toHaveBeenCalled();
  });

  it('does not auto-load older items past the first visible index threshold', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });

    expect(gate.shouldLoad({
      offset: 799,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex: vi.fn((_offset: number) => 6),
    })).toBe(false);
    expect(gate.attemptedAtFloor).toBeNull();
  });

  it('defers virtualizer index lookup until cheap auto-load gates pass', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn((_offset: number) => 5);

    expect(gate.shouldLoad({
      offset: 800,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex,
    })).toBe(false);
    expect(findFirstVisibleIndex).not.toHaveBeenCalled();
  });

  // Disarm/re-arm state machine. Without these guards the gate's old
  // floor-progress check let the auto-load cascade walk the whole
  // thread: each loadOlder advanced `oldestLoadedTurnIndex`, the
  // floor-progress comparison flipped back to allow, the anchor-
  // restore programmatic scroll fired the gate again, and so on. The
  // new shape requires a real user gesture (or the 350ms cooldown
  // fallback) to re-arm.

  it('disarm() blocks shouldLoad even when geometry would otherwise pass', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn((_offset: number) => 0);
    const base = {
      offset: 100,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex,
    };

    expect(gate.armed).toBe(true);
    expect(gate.shouldLoad(base)).toBe(true);

    gate.disarm();
    expect(gate.armed).toBe(false);

    // Even with a fresh floor (would normally satisfy the
    // floor-progress guard), the gate refuses while disarmed. This is
    // the cascade-prevention behavior.
    expect(gate.shouldLoad({ ...base, oldestLoadedTurnIndex: 8 })).toBe(false);
    expect(gate.shouldLoad({ ...base, oldestLoadedTurnIndex: 5 })).toBe(false);
  });

  it('armOnGesture() re-enables shouldLoad after disarm', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn((_offset: number) => 0);
    const base = {
      offset: 100,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      findFirstVisibleIndex,
    };

    expect(gate.shouldLoad(base)).toBe(true);
    gate.disarm();
    expect(gate.shouldLoad({ ...base, oldestLoadedTurnIndex: 8 })).toBe(false);

    // A real user gesture (wheel / touchmove / keydown wired by
    // MessageTimeline.svelte) re-arms exactly once.
    gate.armOnGesture();
    expect(gate.armed).toBe(true);
    expect(gate.shouldLoad({ ...base, oldestLoadedTurnIndex: 8 })).toBe(true);
  });

  it('disarm cooldown re-arms after the fallback timeout', async () => {
    vi.useFakeTimers();
    try {
      const gate = createAutoLoadOlderGate({
        offsetThreshold: 800,
        indexThreshold: 5,
      });
      gate.disarm();
      expect(gate.armed).toBe(false);

      // Cooldown is AUTO_LOAD_COOLDOWN_MS = 350ms. Anything before
      // that must keep the gate disarmed (gesture-detection is the
      // primary mechanism; the timer is a fallback).
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
      const gate = createAutoLoadOlderGate({
        offsetThreshold: 800,
        indexThreshold: 5,
      });
      gate.disarm();
      expect(gate.armed).toBe(false);

      gate.reset();
      expect(gate.armed).toBe(true);

      // The pending cooldown timer must be cleared so it can't fire
      // later and surprise a freshly-switched thread.
      vi.advanceTimersByTime(1000);
      expect(gate.armed).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('armOnGesture() clears the cooldown so a future disarm gets a fresh window', () => {
    vi.useFakeTimers();
    try {
      const gate = createAutoLoadOlderGate({
        offsetThreshold: 800,
        indexThreshold: 5,
      });
      gate.disarm();
      vi.advanceTimersByTime(100);
      gate.armOnGesture();
      expect(gate.armed).toBe(true);

      // The original cooldown's leftover 250ms must NOT fire and
      // mistakenly "re-arm" the gate after a subsequent disarm.
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
