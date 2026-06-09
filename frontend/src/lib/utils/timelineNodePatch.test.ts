import { describe, it, expect } from 'vitest';
import { patchTimelineNodeItemRefs } from './timelineNodePatch';
import type { Item } from '../types/models';
import type {
  TimelineNode,
  TimelineLeaf,
  SubagentGroupNode,
  WaitGroupNode,
  ReadGroupNode,
} from './subagentGrouping';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 't1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  } as Item;
}

function leaf(item: Item, orphan?: boolean): TimelineLeaf {
  return { kind: 'leaf', item, orphan };
}

describe('patchTimelineNodeItemRefs', () => {
  it('returns same array reference when nothing changed', () => {
    const a = mkItem({ id: 'a', summary: 'hello' });
    const b = mkItem({ id: 'b', summary: 'world' });
    const nodes: TimelineNode[] = [leaf(a), leaf(b)];
    const lookup = (id: string) => (id === 'a' ? a : b);
    const result = patchTimelineNodeItemRefs(nodes, lookup);
    expect(result).toBe(nodes);
  });

  it('returns same array reference for empty input', () => {
    const nodes: TimelineNode[] = [];
    const result = patchTimelineNodeItemRefs(nodes, () => undefined);
    expect(result).toBe(nodes);
  });

  it('patches a leaf whose item reference changed', () => {
    const a = mkItem({ id: 'a', summary: 'hello' });
    const b = mkItem({ id: 'b', summary: 'world' });
    const nodes: TimelineNode[] = [leaf(a), leaf(b)];

    const aUpdated = mkItem({ id: 'a', summary: 'hello updated' });
    const lookup = (id: string) => (id === 'a' ? aUpdated : b);
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    expect(result).not.toBe(nodes);
    expect(result[0]).not.toBe(nodes[0]);
    expect((result[0] as TimelineLeaf).item).toBe(aUpdated);
    expect(result[1]).toBe(nodes[1]);
  });

  it('preserves orphan flag on patched leaves', () => {
    const a = mkItem({ id: 'a', summary: 'orphan' });
    const nodes: TimelineNode[] = [leaf(a, true)];

    const aUpdated = mkItem({ id: 'a', summary: 'orphan updated' });
    const result = patchTimelineNodeItemRefs(nodes, () => aUpdated);

    expect((result[0] as TimelineLeaf).orphan).toBe(true);
  });

  it('patches SubagentGroupNode with changed child and recomputes latestChildSummary', () => {
    const parent = mkItem({ id: 'p', summary: 'parent' });
    const child = mkItem({ id: 'c', summary: 'child', updatedAt: 1 });
    const group: SubagentGroupNode = {
      kind: 'group',
      parent,
      groupKey: 'g1',
      children: [leaf(child)],
      descendantCount: 1,
      latestChildSummary: 'child',
    };
    const nodes: TimelineNode[] = [group];

    const childUpdated = mkItem({ id: 'c', summary: 'child updated', updatedAt: 2 });
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'c') return childUpdated;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    expect(result).not.toBe(nodes);
    const patchedGroup = result[0] as SubagentGroupNode;
    expect(patchedGroup).not.toBe(group);
    expect(patchedGroup.parent).toBe(parent);
    expect((patchedGroup.children[0] as TimelineLeaf).item).toBe(childUpdated);
    expect(patchedGroup.groupKey).toBe('g1');
    expect(patchedGroup.descendantCount).toBe(1);
    expect(patchedGroup.latestChildSummary).toBe('child updated');
  });

  it('patches SubagentGroupNode with changed parent', () => {
    const parent = mkItem({ id: 'p', summary: 'parent' });
    const child = mkItem({ id: 'c', summary: 'child' });
    const group: SubagentGroupNode = {
      kind: 'group',
      parent,
      groupKey: 'g1',
      children: [leaf(child)],
      descendantCount: 1,
      latestChildSummary: 'child',
    };
    const nodes: TimelineNode[] = [group];

    const parentUpdated = mkItem({ id: 'p', summary: 'parent updated' });
    const lookup = (id: string) => {
      if (id === 'p') return parentUpdated;
      if (id === 'c') return child;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    const patchedGroup = result[0] as SubagentGroupNode;
    expect(patchedGroup.parent).toBe(parentUpdated);
    expect(patchedGroup.children).toBe(group.children);
  });

  it('returns same SubagentGroupNode when nothing changed', () => {
    const parent = mkItem({ id: 'p', summary: 'parent' });
    const child = mkItem({ id: 'c', summary: 'child' });
    const group: SubagentGroupNode = {
      kind: 'group',
      parent,
      groupKey: 'g1',
      children: [leaf(child)],
      descendantCount: 1,
      latestChildSummary: 'child',
    };
    const nodes: TimelineNode[] = [group];
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'c') return child;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);
    expect(result).toBe(nodes);
    expect(result[0]).toBe(group);
  });

  it('patches WaitGroupNode with changed child', () => {
    const parent = mkItem({ id: 'p', summary: 'wait' });
    const child = mkItem({ id: 'c', summary: 'child' });
    const group: WaitGroupNode = {
      kind: 'wait_group',
      parent,
      groupKey: 'wg1',
      children: [leaf(child)],
      descendantCount: 1,
    };
    const nodes: TimelineNode[] = [group];

    const childUpdated = mkItem({ id: 'c', summary: 'child updated' });
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'c') return childUpdated;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    const patched = result[0] as WaitGroupNode;
    expect(patched).not.toBe(group);
    expect(patched.parent).toBe(parent);
    expect(patched.children[0].item).toBe(childUpdated);
    // The group has no folded completion; patching must not fabricate one.
    expect(patched.completion).toBeUndefined();
  });

  it('returns same WaitGroupNode when nothing changed', () => {
    const parent = mkItem({ id: 'p', summary: 'wait' });
    const child = mkItem({ id: 'c', summary: 'child' });
    const group: WaitGroupNode = {
      kind: 'wait_group',
      parent,
      groupKey: 'wg1',
      children: [leaf(child)],
      descendantCount: 1,
    };
    const nodes: TimelineNode[] = [group];
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'c') return child;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);
    expect(result).toBe(nodes);
    expect(result[0]).toBe(group);
  });

  it('patches the WaitGroupNode completion (folded header) when it changes', () => {
    const parent = mkItem({ id: 'p', summary: 'wait' });
    const child = mkItem({ id: 'c', summary: 'child' });
    const completion = mkItem({ id: 'cw', summary: 'wait running' });
    const group: WaitGroupNode = {
      kind: 'wait_group',
      parent,
      groupKey: 'wg1',
      completion,
      children: [leaf(child)],
      descendantCount: 1,
    };
    const nodes: TimelineNode[] = [group];

    const completionUpdated = mkItem({ id: 'cw', summary: 'wait finished' });
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'c') return child;
      if (id === 'cw') return completionUpdated;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    const patched = result[0] as WaitGroupNode;
    expect(patched).not.toBe(group);
    expect(patched.parent).toBe(parent);
    expect(patched.children[0].item).toBe(child);
    // The folded completion ref is swapped so the header's status refreshes.
    expect(patched.completion).toBe(completionUpdated);
  });

  it('returns the same WaitGroupNode when the completion is unchanged', () => {
    const parent = mkItem({ id: 'p', summary: 'wait' });
    const completion = mkItem({ id: 'cw', summary: 'wait finished' });
    const group: WaitGroupNode = {
      kind: 'wait_group',
      parent,
      groupKey: 'wg1',
      completion,
      children: [],
      descendantCount: 0,
    };
    const nodes: TimelineNode[] = [group];
    const lookup = (id: string) => {
      if (id === 'p') return parent;
      if (id === 'cw') return completion;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);
    expect(result).toBe(nodes);
    expect(result[0]).toBe(group);
  });

  it('patches ReadGroupNode with changed member', () => {
    const m1 = mkItem({ id: 'm1', summary: 'read1' });
    const m2 = mkItem({ id: 'm2', summary: 'read2' });
    const group: ReadGroupNode = {
      kind: 'read_group',
      groupKey: 'rg1',
      threadId: 't1',
      members: [m1, m2],
    };
    const nodes: TimelineNode[] = [group];

    const m1Updated = mkItem({ id: 'm1', summary: 'read1 updated' });
    const lookup = (id: string) => {
      if (id === 'm1') return m1Updated;
      if (id === 'm2') return m2;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);

    const patched = result[0] as ReadGroupNode;
    expect(patched).not.toBe(group);
    expect(patched.members[0]).toBe(m1Updated);
    expect(patched.members[1]).toBe(m2);
  });

  it('returns same ReadGroupNode when nothing changed', () => {
    const m1 = mkItem({ id: 'm1', summary: 'read1' });
    const m2 = mkItem({ id: 'm2', summary: 'read2' });
    const group: ReadGroupNode = {
      kind: 'read_group',
      groupKey: 'rg1',
      threadId: 't1',
      members: [m1, m2],
    };
    const nodes: TimelineNode[] = [group];
    const lookup = (id: string) => {
      if (id === 'm1') return m1;
      if (id === 'm2') return m2;
      return undefined;
    };
    const result = patchTimelineNodeItemRefs(nodes, lookup);
    expect(result).toBe(nodes);
    expect(result[0]).toBe(group);
  });

  it('treats getItem returning undefined as unchanged', () => {
    const a = mkItem({ id: 'a', summary: 'hello' });
    const nodes: TimelineNode[] = [leaf(a)];
    const result = patchTimelineNodeItemRefs(nodes, () => undefined);
    expect(result).toBe(nodes);
  });
});
