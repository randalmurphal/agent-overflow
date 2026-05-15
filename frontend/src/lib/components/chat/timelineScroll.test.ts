import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import {
  captureTimelineAnchor,
  centeredScrollTop,
  createAutoLoadOlderGate,
  isAutoLoadOlderIndexEligible,
  resolveVisibleTimelineNodeIndex,
  shouldAutoLoadOlder,
  shouldInspectAutoLoadOlderIndex,
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

  it('auto-loads older items when restored near the top of the loaded window', () => {
    expect(shouldAutoLoadOlder({
      offset: 799,
      firstVisibleIndex: 5,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      attemptedAtFloor: null,
      offsetThreshold: 800,
      indexThreshold: 5,
    })).toBe(true);
  });

  it('auto-loads older items again after the loaded floor advances', () => {
    expect(shouldAutoLoadOlder({
      offset: 799,
      firstVisibleIndex: 5,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 8,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      attemptedAtFloor: 10,
      offsetThreshold: 800,
      indexThreshold: 5,
    })).toBe(true);
  });

  it.each([
    ['missing history', { hasMoreHistory: false }],
    ['active load', { loadingOlder: true }],
    ['null floor', { oldestLoadedTurnIndex: null }],
    ['unrestored thread', { restoredThreadId: null }],
    ['wrong restored thread', { restoredThreadId: 'thread-2' }],
    ['past offset threshold', { offset: 800 }],
    ['past index threshold', { firstVisibleIndex: 6 }],
    ['already attempted same floor', { attemptedAtFloor: 10 }],
  ])('does not auto-load older items for %s', (_, overrides) => {
    expect(shouldAutoLoadOlder({
      offset: 799,
      firstVisibleIndex: 5,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      attemptedAtFloor: null,
      offsetThreshold: 800,
      indexThreshold: 5,
      ...overrides,
    })).toBe(false);
  });

  it('keeps the virtualizer index lookup behind the cheap auto-load gates', () => {
    expect(shouldInspectAutoLoadOlderIndex({
      offset: 799,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      attemptedAtFloor: null,
      offsetThreshold: 800,
    })).toBe(true);

    expect(shouldInspectAutoLoadOlderIndex({
      offset: 800,
      hasMoreHistory: true,
      loadingOlder: false,
      oldestLoadedTurnIndex: 10,
      restoredThreadId: 'thread-1',
      threadId: 'thread-1',
      attemptedAtFloor: null,
      offsetThreshold: 800,
    })).toBe(false);
  });

  it('checks first visible row eligibility separately from cheap auto-load gates', () => {
    expect(isAutoLoadOlderIndexEligible(5, 5)).toBe(true);
    expect(isAutoLoadOlderIndexEligible(6, 5)).toBe(false);
  });

  it('tracks auto-load attempts by floor and resets explicitly', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn(() => 5);
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

  it('defers virtualizer index lookup until cheap auto-load gates pass', () => {
    const gate = createAutoLoadOlderGate({
      offsetThreshold: 800,
      indexThreshold: 5,
    });
    const findFirstVisibleIndex = vi.fn(() => 5);

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
});
