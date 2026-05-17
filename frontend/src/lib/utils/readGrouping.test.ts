import { describe, expect, it } from 'vitest';
import { groupConsecutiveReads } from './readGrouping';
import type { ReadGroupNode, TimelineLeaf, TimelineNode } from './subagentGrouping';
import type { Item } from '../types/models';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

function read(id: string, summary: string, overrides: Partial<Item> = {}): TimelineLeaf {
  return {
    kind: 'leaf',
    item: mkItem({
      id,
      kind: 'tool_call',
      toolName: 'Read',
      summary,
      ...overrides,
    }),
  };
}

function leaf(id: string, partial: Partial<Item>): TimelineLeaf {
  return { kind: 'leaf', item: mkItem({ id, ...partial }) };
}

function asReadGroup(node: TimelineNode): ReadGroupNode {
  if (node.kind !== 'read_group') throw new Error(`expected read_group, got ${node.kind}`);
  return node;
}

describe('groupConsecutiveReads', () => {
  it('folds two-or-more adjacent Read leaves into a single read_group node', () => {
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go'),
      read('r2', 'Read: b.go'),
      read('r3', 'Read: c.go'),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out).toHaveLength(1);
    const group = asReadGroup(out[0]);
    expect(group.members.map((m) => m.id)).toEqual(['r1', 'r2', 'r3']);
    expect(group.groupKey).toBe('reads:r1');
    expect(group.threadId).toBe('thread-1');
  });

  it('leaves a single Read leaf alone — the dropdown chrome only collapses on a real run', () => {
    // MIN_GROUP_SIZE is 2: a single Read keeps its full
    // GenericToolCallRow shell (chevron / preview / EditorLink icon /
    // expansion body) so isolated reads stay first-class rows.
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go'),
      leaf('a1', { kind: 'assistant_text', summary: 'hi' }),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out).toEqual(nodes);
  });

  it('emits multiple read_groups when separated by a non-Read node', () => {
    // The grouping is "consecutive" — a single non-Read in between
    // splits the run. Without the split, the row would have to claim
    // ownership of the assistant text in the middle, which would
    // misrepresent the timeline.
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go'),
      read('r2', 'Read: b.go'),
      leaf('a1', { kind: 'assistant_text', summary: 'looking…' }),
      read('r3', 'Read: c.go'),
      read('r4', 'Read: d.go'),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out.map((n) => n.kind)).toEqual(['read_group', 'leaf', 'read_group']);
    expect(asReadGroup(out[0]).members.map((m) => m.id)).toEqual(['r1', 'r2']);
    expect(asReadGroup(out[2]).members.map((m) => m.id)).toEqual(['r3', 'r4']);
    expect(asReadGroup(out[0]).groupKey).not.toBe(asReadGroup(out[2]).groupKey);
  });

  it('does not pull a single Read between non-Read nodes into a group', () => {
    const nodes: TimelineNode[] = [
      leaf('a1', { kind: 'assistant_text', summary: 'a' }),
      read('r1', 'Read: a.go'),
      leaf('a2', { kind: 'assistant_text', summary: 'b' }),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out.map((n) => n.kind)).toEqual(['leaf', 'leaf', 'leaf']);
  });

  it('ignores non-leaf node kinds when scanning for runs', () => {
    // A SubagentGroupNode in the middle splits the run — its visible
    // chrome belongs to a subagent transcript and the compact reads
    // row would otherwise pretend to own activity that lives inside
    // an entirely different container.
    const group: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'p1', kind: 'tool_call', toolName: 'Agent', summary: 'Agent: x' }),
      groupKey: 'g:p1',
      children: [],
      descendantCount: 0,
      latestChildSummary: '',
    };
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go'),
      read('r2', 'Read: b.go'),
      group,
      read('r3', 'Read: c.go'),
      read('r4', 'Read: d.go'),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out.map((n) => n.kind)).toEqual(['read_group', 'group', 'read_group']);
  });

  it('skips backgrounded Read launches (defense against an unexpected tray flow)', () => {
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go'),
      read('r2', 'Read: b.go', { isBackground: true }),
      read('r3', 'Read: c.go'),
    ];
    const out = groupConsecutiveReads(nodes);
    // The backgrounded read breaks the run; the two surrounding leaves
    // are no longer adjacent reads.
    expect(out.map((n) => n.kind)).toEqual(['leaf', 'leaf', 'leaf']);
  });

  it('groups regardless of completion status', () => {
    // The user explicitly opted out of distinguishing failure state
    // here ("don't care about read failures") — the row only shows
    // file names, so status is irrelevant to grouping.
    const nodes: TimelineNode[] = [
      read('r1', 'Read: a.go', { status: 'completed' }),
      read('r2', 'Read: b.go', { status: 'errored' }),
      read('r3', 'Read: c.go', { status: 'running' }),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out).toHaveLength(1);
    expect(asReadGroup(out[0]).members.map((m) => m.id)).toEqual(['r1', 'r2', 'r3']);
  });

  it('returns the input unchanged when there is nothing to group', () => {
    const nodes: TimelineNode[] = [
      leaf('a1', { kind: 'assistant_text', summary: 'a' }),
      leaf('a2', { kind: 'user_text', summary: 'b' }),
    ];
    const out = groupConsecutiveReads(nodes);
    expect(out).toEqual(nodes);
  });

  it('handles an empty input without allocating a fresh array', () => {
    const nodes: TimelineNode[] = [];
    expect(groupConsecutiveReads(nodes)).toBe(nodes);
  });
});
