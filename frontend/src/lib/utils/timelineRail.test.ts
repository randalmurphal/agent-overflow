import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import type {
  ActivityRunNode,
  ReadGroupNode,
  SubagentGroupNode,
  TimelineNode,
  WaitGroupNode,
} from './subagentGrouping';
import { timelineNodeHasRail } from './timelineRail';

function leaf(overrides: Partial<Item> = {}): TimelineNode {
  return { kind: 'leaf', item: makeItem(overrides) };
}

const groupParent = makeItem({ id: 'parent' });
const group: SubagentGroupNode = {
  kind: 'group',
  parent: groupParent,
  anchor: groupParent,
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

const activityRun: ActivityRunNode = {
  kind: 'activity_run',
  runId: 'r1',
  threadId: 'thread-1',
  children: [leaf({ kind: 'tool_call' })],
  collapsed: false,
  live: false,
  atTail: false,
  mountedFrom: 0,
  mountedRows: 1,
  membershipEpoch: 1,
  memberItemIds: ['i1'],
  summaryItemIds: ['i1'],
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
    // The run DRAWS the rail; it does not sit on one. Answering true here
    // would make the run a member of itself on the next projection pass.
    ['activity_run draws its own rail', activityRun, false],
  ])('%s -> %s', (_label, node, expected) => {
    const leafItem = node.kind === 'leaf' ? node.item : null;
    expect(timelineNodeHasRail(node, leafItem)).toBe(expected);
  });
});
