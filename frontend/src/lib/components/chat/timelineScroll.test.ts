import { describe, expect, it, vi } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import { groupItemsBySubagent } from '../../utils/subagentGrouping';
import {
  captureTimelineAnchor,
  centeredScrollTop,
  resolveVisibleTimelineNodeIndex,
  shouldAutoLoadOlder,
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
});
