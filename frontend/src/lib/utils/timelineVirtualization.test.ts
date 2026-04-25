import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import type { TimelineNode } from './subagentGrouping';
import {
  buildVirtualLayout,
  computeVirtualWindow,
  timelineNodeKey,
} from './timelineVirtualization';

function item(id: string, overrides: Partial<Item> = {}): Item {
  return {
    id,
    threadId: overrides.threadId ?? 'thread-1',
    kind: overrides.kind ?? 'assistant_text',
    role: overrides.role ?? '',
    summary: overrides.summary ?? '',
    status: overrides.status ?? 'completed',
    payloadId: overrides.payloadId ?? '',
    turnIndex: overrides.turnIndex ?? 0,
    itemIndex: overrides.itemIndex ?? 0,
    parentId: overrides.parentId ?? '',
    meta: overrides.meta ?? '',
    createdAt: overrides.createdAt ?? 0,
    updatedAt: overrides.updatedAt ?? 0,
  };
}

describe('timelineVirtualization', () => {
  it('keys leaves and groups by thread plus item id', () => {
    const leaf: TimelineNode = { kind: 'leaf', item: item('same-id', { threadId: 'thread-a' }) };
    const group: TimelineNode = {
      kind: 'group',
      parent: item('same-id', { threadId: 'thread-b' }),
      children: [],
      descendantCount: 0,
      preview: '',
      truncated: false,
    };

    expect(timelineNodeKey(leaf)).toBe('l:thread-a:same-id');
    expect(timelineNodeKey(group)).toBe('g:thread-b:same-id');
  });

  it('builds offsets from measured row heights without mutating the measurement cache', () => {
    const rows: TimelineNode[] = [
      { kind: 'leaf', item: item('one') },
      { kind: 'leaf', item: item('two') },
      { kind: 'leaf', item: item('three') },
    ];
    const rowHeights = new Map([
      ['l:thread-1:one', 30],
      ['l:thread-1:three', 70],
      ['l:thread-1:stale', 999],
    ]);

    const layout = buildVirtualLayout(rows, rowHeights, 50);

    expect(layout.offsets).toEqual([0, 30, 80, 150]);
    expect(layout.totalHeight).toBe(150);
    expect(layout.rows.map((row) => row.estimatedHeight)).toEqual([50, 50, 50]);
    expect(rowHeights.has('l:thread-1:stale')).toBe(true);
  });

  it('keeps the per-row estimate even when an actual height is measured', () => {
    const rows: TimelineNode[] = [
      { kind: 'leaf', item: item('one') },
      { kind: 'leaf', item: item('two') },
    ];
    const rowHeights = new Map([
      ['l:thread-1:one', 30],
    ]);

    const layout = buildVirtualLayout(rows, rowHeights, (node) => (
      node.kind === 'leaf' && node.item.id === 'one' ? 80 : 120
    ));

    expect(layout.rows.map((row) => row.height)).toEqual([30, 120]);
    expect(layout.rows.map((row) => row.estimatedHeight)).toEqual([80, 120]);
  });

  it('returns an overscanned row window with before and after spacers', () => {
    const rows: TimelineNode[] = [
      { kind: 'leaf', item: item('one') },
      { kind: 'leaf', item: item('two') },
      { kind: 'leaf', item: item('three') },
      { kind: 'leaf', item: item('four') },
    ];
    const layout = buildVirtualLayout(rows, new Map(), 100);

    const window = computeVirtualWindow(layout, 140, 100, 25);

    expect(window.start).toBe(1);
    expect(window.end).toBe(3);
    expect(window.before).toBe(100);
    expect(window.after).toBe(100);
    expect(window.rows.map((row) => row.key)).toEqual(['l:thread-1:two', 'l:thread-1:three']);
  });
});
