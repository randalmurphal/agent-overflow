import { describe, expect, it } from 'vitest';
import { makeItem } from '../../../test/helpers/chat';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import type {
  ReadGroupNode,
  SubagentGroupNode,
  TimelineNode,
  WaitGroupNode,
} from '../../utils/subagentGrouping';
import { createTimelineRowProjection } from './timelineRowProjection.svelte';

// timelineNodeHasRail never reads `pane` — the getPane stub only backs
// `currentTimelineLeafItem`'s getItemById lookup (used to resolve leaf
// fixtures below) so it satisfies the factory's option contract.
const fakePane = { getItemById: () => undefined } as unknown as ThreadPane;
const rows = createTimelineRowProjection({ getPane: () => fakePane });

function leaf(overrides: Partial<Item> = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

const group: SubagentGroupNode = {
  kind: 'group',
  parent: makeItem({ id: 'parent' }),
  groupKey: 'group:parent',
  children: [],
  descendantCount: 0,
  loadedDescendantCount: 0,
  latestChildSummary: '',
};

const waitGroup: WaitGroupNode = {
  kind: 'wait_group',
  parent: makeItem({ id: 'wait-parent' }),
  groupKey: 'wait:wait-parent',
  children: [],
  descendantCount: 0,
};

const readGroup: ReadGroupNode = {
  kind: 'read_group',
  groupKey: 'read:first',
  threadId: 'thread-1',
  members: [],
};

describe('timelineNodeHasRail', () => {
  it.each<[string, TimelineNode, boolean]>([
    ['tool_call leaf', leaf({ kind: 'tool_call' }), true],
    ['tool_completion leaf', leaf({ kind: 'tool_completion' }), true],
    ['thinking leaf', leaf({ kind: 'thinking' }), true],
    ['proposed_plan payload is rail-exempt', leaf({ kind: 'tool_call', payloadKind: 'proposed_plan' }), false],
    ['assistant_text leaf has no rail', leaf({ kind: 'assistant_text' }), false],
    ['group node', group, true],
    ['wait_group node', waitGroup, true],
    ['read_group node', readGroup, true],
  ])('%s -> %s', (_label, node, expected) => {
    const leafItem = rows.currentTimelineLeafItem(node);
    expect(rows.timelineNodeHasRail(node, leafItem)).toBe(expected);
  });
});
