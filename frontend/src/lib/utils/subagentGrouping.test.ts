import { describe, expect, it } from 'vitest';
import {
  groupItemsBySubagent,
  MAX_DEPTH,
  MAX_PREVIEW_CHARS,
  type SubagentGroupNode,
  type TimelineLeaf,
  type TimelineNode,
} from './subagentGrouping';
import type { Item } from '../types/models';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: '',
    highlightedContent: '',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

function expectGroup(node: TimelineNode): SubagentGroupNode {
  if (node.kind !== 'group') {
    throw new Error(`expected group node, got ${node.kind} for ${node.item.id}`);
  }
  return node;
}

function expectLeaf(node: TimelineNode): TimelineLeaf {
  if (node.kind !== 'leaf') {
    throw new Error(`expected leaf node, got ${node.kind}`);
  }
  return node;
}

describe('groupItemsBySubagent', () => {
  it('returns an empty array for an empty list', () => {
    expect(groupItemsBySubagent([])).toEqual([]);
  });

  it('returns only leaves when no item has a parentId', () => {
    const items = [
      mkItem({ id: 'a', itemIndex: 0, summary: 'one' }),
      mkItem({ id: 'b', itemIndex: 1, summary: 'two' }),
      mkItem({ id: 'c', itemIndex: 2, summary: 'three' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(3);
    for (const node of nodes) {
      expect(node.kind).toBe('leaf');
    }
    expect(nodes.map((n) => expectLeaf(n).item.id)).toEqual(['a', 'b', 'c']);
  });

  it('nests three children under a single subagent parent', () => {
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Task: refactor' }),
      mkItem({ id: 'child-1', itemIndex: 1, parentId: 'parent', summary: 'reading file' }),
      mkItem({ id: 'child-2', itemIndex: 2, parentId: 'parent', summary: 'editing lines' }),
      mkItem({ id: 'child-3', itemIndex: 3, parentId: 'parent', summary: 'final reply' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(1);

    const group = expectGroup(nodes[0]);
    expect(group.parent.id).toBe('parent');
    expect(group.children).toHaveLength(3);
    expect(group.descendantCount).toBe(3);
    expect(group.children.map((c) => expectLeaf(c).item.id)).toEqual([
      'child-1', 'child-2', 'child-3',
    ]);
    // Preview aggregates text from the children.
    expect(group.preview).toContain('reading file');
    expect(group.preview).toContain('final reply');
    expect(group.truncated).toBe(false);
  });

  it('preserves chronological order within a group', () => {
    // Feed items out of order; the group should still emerge in
    // (turnIndex, itemIndex) order.
    const items = [
      mkItem({ id: 'child-b', itemIndex: 3, parentId: 'parent', summary: 'second' }),
      mkItem({ id: 'parent', itemIndex: 1, kind: 'tool_call', summary: 'task' }),
      mkItem({ id: 'child-a', itemIndex: 2, parentId: 'parent', summary: 'first' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.children.map((c) => expectLeaf(c).item.id)).toEqual(['child-a', 'child-b']);
  });

  it('nests grandchildren inside child groups (parent -> child -> grandchild)', () => {
    const items = [
      mkItem({ id: 'root', itemIndex: 0, kind: 'tool_call', summary: 'outer task' }),
      mkItem({ id: 'mid', itemIndex: 1, parentId: 'root', kind: 'tool_call', summary: 'inner task' }),
      mkItem({ id: 'leaf', itemIndex: 2, parentId: 'mid', summary: 'deep work' }),
      mkItem({ id: 'root-sib', itemIndex: 3, parentId: 'root', summary: 'root sibling' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(1);

    const root = expectGroup(nodes[0]);
    expect(root.descendantCount).toBe(3); // mid + leaf + root-sib

    expect(root.children).toHaveLength(2);
    const mid = expectGroup(root.children[0]);
    expect(mid.parent.id).toBe('mid');
    const leaf = expectLeaf(mid.children[0]);
    expect(leaf.item.id).toBe('leaf');

    const rootSib = expectLeaf(root.children[1]);
    expect(rootSib.item.id).toBe('root-sib');
  });

  it('treats a child whose parent is not visible as an orphan leaf with a warning flag', () => {
    const items = [
      mkItem({ id: 'a', itemIndex: 0, summary: 'hello' }),
      mkItem({
        id: 'orphan',
        itemIndex: 1,
        parentId: 'missing-tool-id',
        summary: 'had a parent once',
      }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(2);
    expect(expectLeaf(nodes[0]).item.id).toBe('a');
    const orphan = expectLeaf(nodes[1]);
    expect(orphan.item.id).toBe('orphan');
    expect(orphan.orphan).toBe(true);
  });

  it(`caps nesting depth at ${MAX_DEPTH} — deeper descendants become flat leaves under the deepest group`, () => {
    // Build a chain 5 levels deep: L0 (root) -> L1 -> L2 -> L3 -> L4.
    // With MAX_DEPTH=3, we expect: L0(group) > L1(group) > L2(group with L3,L4 as flat leaves).
    const items = [
      mkItem({ id: 'L0', itemIndex: 0, kind: 'tool_call', summary: 'depth 0' }),
      mkItem({ id: 'L1', itemIndex: 1, parentId: 'L0', kind: 'tool_call', summary: 'depth 1' }),
      mkItem({ id: 'L2', itemIndex: 2, parentId: 'L1', kind: 'tool_call', summary: 'depth 2' }),
      mkItem({ id: 'L3', itemIndex: 3, parentId: 'L2', kind: 'tool_call', summary: 'depth 3' }),
      mkItem({ id: 'L4', itemIndex: 4, parentId: 'L3', summary: 'depth 4' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(1);

    const l0 = expectGroup(nodes[0]);
    const l1 = expectGroup(l0.children[0]);
    const l2 = expectGroup(l1.children[0]);

    // L2 should be the deepest group; L3 and L4 are rolled up as flat leaves.
    expect(l2.children).toHaveLength(2);
    for (const child of l2.children) {
      expect(child.kind).toBe('leaf');
    }
    const flatIds = l2.children.map((c) => expectLeaf(c).item.id).sort();
    expect(flatIds).toEqual(['L3', 'L4']);
    // Descendant count still honest.
    expect(l2.descendantCount).toBe(2);
  });

  it('bounds the aggregate preview at MAX_PREVIEW_CHARS', () => {
    const bigText = 'x'.repeat(MAX_PREVIEW_CHARS * 4);
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Task' }),
      mkItem({ id: 'child', itemIndex: 1, parentId: 'parent', summary: bigText }),
    ];
    const group = expectGroup(groupItemsBySubagent(items)[0]);
    expect(group.preview.length).toBeLessThanOrEqual(MAX_PREVIEW_CHARS + 10);
    expect(group.truncated).toBe(true);
  });

  it('handles two unrelated subagents in the same turn without interference', () => {
    const items = [
      mkItem({ id: 'p1', itemIndex: 0, kind: 'tool_call', summary: 'task 1' }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'p1', summary: 'child of 1' }),
      mkItem({ id: 'p2', itemIndex: 2, kind: 'tool_call', summary: 'task 2' }),
      mkItem({ id: 'c2', itemIndex: 3, parentId: 'p2', summary: 'child of 2' }),
      mkItem({ id: 'final', itemIndex: 4, summary: 'parent wrap-up' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(3);
    expect(expectGroup(nodes[0]).parent.id).toBe('p1');
    expect(expectGroup(nodes[1]).parent.id).toBe('p2');
    expect(expectLeaf(nodes[2]).item.id).toBe('final');
  });

  it('does not mutate the input items array or individual item objects', () => {
    const items = [
      mkItem({ id: 'p', itemIndex: 0, kind: 'tool_call', summary: 'task' }),
      mkItem({ id: 'c', itemIndex: 1, parentId: 'p', summary: 'child' }),
    ];
    const frozenInput = Object.freeze(items.slice());
    const snapshotJSON = JSON.stringify(items);
    groupItemsBySubagent(frozenInput);
    // Input order unchanged (we sort internally with a copy).
    expect(JSON.stringify(items)).toBe(snapshotJSON);
  });

  it('reports descendantCount for nested groups as the total count of descendants', () => {
    const items = [
      mkItem({ id: 'p', itemIndex: 0, kind: 'tool_call', summary: 'task' }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'p', kind: 'tool_call', summary: 'c1' }),
      mkItem({ id: 'gc1', itemIndex: 2, parentId: 'c1', summary: 'gc1' }),
      mkItem({ id: 'gc2', itemIndex: 3, parentId: 'c1', summary: 'gc2' }),
      mkItem({ id: 'c2', itemIndex: 4, parentId: 'p', summary: 'c2' }),
    ];
    const root = expectGroup(groupItemsBySubagent(items)[0]);
    // c1 + gc1 + gc2 + c2 = 4 descendants
    expect(root.descendantCount).toBe(4);
  });

  it('empty parentId strings are treated as top-level roots', () => {
    const items = [
      mkItem({ id: 'a', itemIndex: 0, parentId: '', summary: 'still root' }),
      mkItem({ id: 'b', itemIndex: 1, summary: 'also root' }),
    ];
    const nodes = groupItemsBySubagent(items);
    expect(nodes).toHaveLength(2);
    for (const node of nodes) expect(node.kind).toBe('leaf');
  });

  // Fast-path: when no item declares a parentId and the input is already
  // in canonical (turnIndex, itemIndex) order, we skip the sort + id-set +
  // grouping walk and wrap each item as a leaf directly. Verify the output
  // matches the slow path for the inputs both can handle.
  describe('fast-path (no subagents, pre-sorted input)', () => {
    it('returns leaves in upsert order for pre-sorted input with no parentIds', () => {
      const items = [
        mkItem({ id: 'a', turnIndex: 0, itemIndex: 0, summary: 'first' }),
        mkItem({ id: 'b', turnIndex: 0, itemIndex: 1, summary: 'second' }),
        mkItem({ id: 'c', turnIndex: 1, itemIndex: 0, summary: 'third' }),
        mkItem({ id: 'd', turnIndex: 1, itemIndex: 1, summary: 'fourth' }),
      ];
      const nodes = groupItemsBySubagent(items);
      expect(nodes).toHaveLength(4);
      expect(nodes.map((n) => expectLeaf(n).item.id)).toEqual(['a', 'b', 'c', 'd']);
      for (const node of nodes) {
        expect(node.kind).toBe('leaf');
        expect(expectLeaf(node).orphan).toBeUndefined();
      }
    });

    it('falls through to slow path when input is out of order (sorts defensively)', () => {
      // If items arrive out of order and none have parentId, the
      // monotonic check fails and we take the slow path, which sorts.
      // This preserves the documented contract that callers may pass
      // unsorted input.
      const items = [
        mkItem({ id: 'late', turnIndex: 2, itemIndex: 0 }),
        mkItem({ id: 'early', turnIndex: 0, itemIndex: 0 }),
        mkItem({ id: 'mid', turnIndex: 1, itemIndex: 0 }),
      ];
      const nodes = groupItemsBySubagent(items);
      expect(nodes.map((n) => expectLeaf(n).item.id)).toEqual(['early', 'mid', 'late']);
    });

    it('fast path does not mutate input when wrapping leaves', () => {
      const items = [
        mkItem({ id: 'a', turnIndex: 0, itemIndex: 0 }),
        mkItem({ id: 'b', turnIndex: 0, itemIndex: 1 }),
      ];
      const snapshot = JSON.stringify(items);
      groupItemsBySubagent(items);
      expect(JSON.stringify(items)).toBe(snapshot);
    });

    it('one parentId anywhere in the list defeats the fast path and triggers grouping', () => {
      // Sanity: even if 99 of 100 items have no parentId, a single
      // subagent entry routes the whole list through the slow path.
      const items = [
        mkItem({ id: 'p', turnIndex: 0, itemIndex: 0, kind: 'tool_call', summary: 'task' }),
        mkItem({ id: 'c', turnIndex: 0, itemIndex: 1, parentId: 'p', summary: 'child' }),
        mkItem({ id: 'x', turnIndex: 0, itemIndex: 2, summary: 'sibling' }),
      ];
      const nodes = groupItemsBySubagent(items);
      expect(nodes).toHaveLength(2);
      const group = nodes[0];
      expect(group.kind).toBe('group');
      if (group.kind === 'group') {
        expect(group.parent.id).toBe('p');
        expect(group.children).toHaveLength(1);
      }
      expect(expectLeaf(nodes[1]).item.id).toBe('x');
    });
  });
});
