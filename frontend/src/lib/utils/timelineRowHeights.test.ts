import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { TimelineNode } from './subagentGrouping';
import { estimateTimelineNodeHeight, timelineNodeHeightSignature } from './timelineRowHeights';

describe('timelineRowHeights', () => {
  it('invalidates same-length content changes', () => {
    const first: TimelineNode = {
      kind: 'leaf',
      item: makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        summary: 'Wait A',
        meta: JSON.stringify({ input: { receiverThreadIds: ['a'] } }),
      }),
    };
    const second: TimelineNode = {
      kind: 'leaf',
      item: makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        summary: 'Wait B',
        meta: JSON.stringify({ input: { receiverThreadIds: ['b', 'c'] } }),
      }),
    };

    expect(timelineNodeHeightSignature(first, 0, [first], false, null, new Map()))
      .not.toBe(timelineNodeHeightSignature(second, 0, [second], false, null, new Map()));
  });

  it('uses tighter estimates for terminal interaction markers', () => {
    expect(estimateTimelineNodeHeight({
      kind: 'leaf',
      item: makeItem({ id: 'waited', kind: 'terminal_interaction' }),
    })).toBe(32);
  });
});
