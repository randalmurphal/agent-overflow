import { describe, expect, it } from 'vitest';
import {
  findTimelineNodeIndex,
  groupItemsBySubagent,
  isLastRootInTurn,
  isToolTextBoundary,
  MAX_DEPTH,
  nodeContainsItem,
  nodeRole,
  rootTurnIndex,
  timelineNodeItemId,
  timelineNodeKey,
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
    // All three children are completed; latestChildSummary picks the
    // highest-(turnIndex,itemIndex) one — child-3.
    expect(group.latestChildSummary).toBe('final reply');
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

  it('preserves input order for rows with equal timeline coordinates', () => {
    const items = [
      mkItem({ id: 'first-arrived', itemIndex: 1, createdAt: 200 }),
      mkItem({ id: 'second-arrived', itemIndex: 1, createdAt: 100 }),
    ];

    const nodes = groupItemsBySubagent(items);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'first-arrived',
      'second-arrived',
    ]);
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

  it('latestChildSummary prefers an active descendant over a more recent terminal one', () => {
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Agent: explore' }),
      // Earlier-arriving running tool_call — should win the preview
      // because it's still active, even though the terminal sibling
      // below has a higher itemIndex.
      mkItem({
        id: 'running-bash',
        itemIndex: 1,
        parentId: 'parent',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: pwd',
      }),
      mkItem({
        id: 'completed-text',
        itemIndex: 2,
        parentId: 'parent',
        kind: 'assistant_text',
        status: 'completed',
        summary: 'pondering...',
      }),
    ];
    const group = expectGroup(groupItemsBySubagent(items)[0]);
    expect(group.latestChildSummary).toBe('Bash: pwd');
  });

  it('latestChildSummary treats streaming descendants as active too', () => {
    // `streaming` and `running` both signal "subagent is doing work
    // right now" — pickLatestChildSummary biases toward either over
    // a more-recent terminal sibling.
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Agent: think out loud' }),
      mkItem({
        id: 'streaming-text',
        itemIndex: 1,
        parentId: 'parent',
        kind: 'assistant_text',
        status: 'streaming',
        summary: 'thinking through options...',
      }),
      mkItem({
        id: 'completed-tool',
        itemIndex: 2,
        parentId: 'parent',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Read',
        summary: 'Read: foo.ts',
      }),
    ];
    const group = expectGroup(groupItemsBySubagent(items)[0]);
    expect(group.latestChildSummary).toBe('thinking through options...');
  });

  it('latestChildSummary falls back to the most recent terminal descendant when none are active', () => {
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Agent: explore' }),
      mkItem({
        id: 'first',
        itemIndex: 1,
        parentId: 'parent',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Bash',
        summary: 'Bash: ls',
      }),
      mkItem({
        id: 'second',
        itemIndex: 2,
        parentId: 'parent',
        kind: 'tool_call',
        status: 'completed',
        toolName: 'Read',
        summary: 'Read: foo.ts',
      }),
    ];
    const group = expectGroup(groupItemsBySubagent(items)[0]);
    expect(group.latestChildSummary).toBe('Read: foo.ts');
  });

  it('latestChildSummary is empty when no descendant carries a usable summary', () => {
    const items = [
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', summary: 'Agent: idle' }),
      mkItem({
        id: 'silent-child',
        itemIndex: 1,
        parentId: 'parent',
        kind: 'tool_call',
        status: 'running',
        summary: '',
      }),
    ];
    const group = expectGroup(groupItemsBySubagent(items)[0]);
    expect(group.latestChildSummary).toBe('');
  });

  it('latestChildSummary walks into nested subagent groups', () => {
    const items = [
      mkItem({ id: 'root', itemIndex: 0, kind: 'tool_call', summary: 'Agent: outer' }),
      mkItem({
        id: 'inner-parent',
        itemIndex: 1,
        parentId: 'root',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Agent',
        summary: 'Agent: inner',
      }),
      mkItem({
        id: 'inner-child',
        itemIndex: 2,
        parentId: 'inner-parent',
        kind: 'tool_call',
        status: 'running',
        toolName: 'Bash',
        summary: 'Bash: inner work',
      }),
    ];
    const root = expectGroup(groupItemsBySubagent(items)[0]);
    expect(root.latestChildSummary).toBe('Bash: inner work');
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

describe('timelineNodeKey', () => {
  it('returns thread-prefixed l: key for leaves', () => {
    const node: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'i', threadId: 't' }) };
    expect(timelineNodeKey(node)).toBe('l:t:i');
  });

  it('returns thread-prefixed g: key for groups', () => {
    const node: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'p', threadId: 't' }),
      children: [],
      latestChildSummary: '',
      descendantCount: 0,
    };
    expect(timelineNodeKey(node)).toBe('g:t:p');
  });

  it('namespaces by threadId so the same item id in two threads does not collide', () => {
    const a: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'x', threadId: 'A' }) };
    const b: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'x', threadId: 'B' }) };
    expect(timelineNodeKey(a)).not.toBe(timelineNodeKey(b));
  });
});

describe('timelineNodeItemId', () => {
  it('returns the leaf item id', () => {
    const node: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'leaf-1' }) };
    expect(timelineNodeItemId(node)).toBe('leaf-1');
  });

  it('returns the group parent id', () => {
    const node: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'parent-1' }),
      children: [],
      latestChildSummary: '',
      descendantCount: 0,
    };
    expect(timelineNodeItemId(node)).toBe('parent-1');
  });
});

describe('rootTurnIndex', () => {
  it('returns leaf turnIndex', () => {
    const node: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'a', turnIndex: 7 }) };
    expect(rootTurnIndex(node)).toBe(7);
  });

  it('returns group parent turnIndex (children inherit)', () => {
    const node: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'p', turnIndex: 3 }),
      children: [
        { kind: 'leaf', item: mkItem({ id: 'c1', turnIndex: 99 }) },
      ],
      latestChildSummary: '',
      descendantCount: 1,
    };
    expect(rootTurnIndex(node)).toBe(3);
  });
});

describe('isLastRootInTurn', () => {
  function leaf(id: string, turnIndex: number): TimelineNode {
    return { kind: 'leaf', item: mkItem({ id, turnIndex }) };
  }

  it('returns true for the last node in the list', () => {
    const nodes = [leaf('a', 0), leaf('b', 0)];
    expect(isLastRootInTurn(nodes, 1)).toBe(true);
  });

  it('returns true when the next node belongs to a different turn', () => {
    const nodes = [leaf('a', 0), leaf('b', 1), leaf('c', 1)];
    expect(isLastRootInTurn(nodes, 0)).toBe(true);
  });

  it('returns false when the next node is in the same turn', () => {
    const nodes = [leaf('a', 0), leaf('b', 0)];
    expect(isLastRootInTurn(nodes, 0)).toBe(false);
  });

  it('returns false for an out-of-range index', () => {
    const nodes = [leaf('a', 0)];
    expect(isLastRootInTurn(nodes, 99)).toBe(false);
  });
});

describe('nodeContainsItem', () => {
  it('matches leaves by item id', () => {
    const node: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'a' }) };
    expect(nodeContainsItem(node, 'a')).toBe(true);
    expect(nodeContainsItem(node, 'b')).toBe(false);
  });

  it('matches the group parent id', () => {
    const node: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'p' }),
      children: [],
      latestChildSummary: '',
      descendantCount: 0,
    };
    expect(nodeContainsItem(node, 'p')).toBe(true);
  });

  it('walks into nested children of a group', () => {
    const grandchild: TimelineNode = { kind: 'leaf', item: mkItem({ id: 'gc' }) };
    const child: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'c' }),
      children: [grandchild],
      latestChildSummary: '',
      descendantCount: 1,
    };
    const node: TimelineNode = {
      kind: 'group',
      parent: mkItem({ id: 'p' }),
      children: [child],
      latestChildSummary: '',
      descendantCount: 2,
    };
    expect(nodeContainsItem(node, 'gc')).toBe(true);
    expect(nodeContainsItem(node, 'missing')).toBe(false);
  });
});

describe('findTimelineNodeIndex', () => {
  it('returns the index of the root that contains the item', () => {
    const nodes: TimelineNode[] = [
      { kind: 'leaf', item: mkItem({ id: 'a' }) },
      {
        kind: 'group',
        parent: mkItem({ id: 'p' }),
        children: [{ kind: 'leaf', item: mkItem({ id: 'gc' }) }],
        latestChildSummary: '',
        descendantCount: 1,
      },
      { kind: 'leaf', item: mkItem({ id: 'b' }) },
    ];
    expect(findTimelineNodeIndex(nodes, 'a')).toBe(0);
    expect(findTimelineNodeIndex(nodes, 'p')).toBe(1);
    expect(findTimelineNodeIndex(nodes, 'gc')).toBe(1); // returns root that contains it
    expect(findTimelineNodeIndex(nodes, 'b')).toBe(2);
  });

  it('returns -1 when no root contains the item', () => {
    const nodes: TimelineNode[] = [{ kind: 'leaf', item: mkItem({ id: 'a' }) }];
    expect(findTimelineNodeIndex(nodes, 'missing')).toBe(-1);
  });
});

describe('nodeRole', () => {
  function leaf(item: Item): TimelineLeaf {
    return { kind: 'leaf', item };
  }
  function group(parentId: string): SubagentGroupNode {
    return {
      kind: 'group',
      parent: mkItem({ id: parentId, kind: 'tool_call' }),
      children: [],
      latestChildSummary: '',
      descendantCount: 0,
    };
  }

  it('classifies tool_call / tool_completion / terminal_interaction as tool', () => {
    expect(nodeRole(leaf(mkItem({ id: 'a', kind: 'tool_call' })))).toBe('tool');
    expect(nodeRole(leaf(mkItem({ id: 'a', kind: 'tool_completion' })))).toBe('tool');
    expect(nodeRole(leaf(mkItem({ id: 'a', kind: 'terminal_interaction' })))).toBe('tool');
  });

  it('classifies SubagentGroup nodes as tool', () => {
    expect(nodeRole(group('parent'))).toBe('tool');
  });

  it('classifies assistant_text / user_text as text', () => {
    expect(nodeRole(leaf(mkItem({ id: 'a', kind: 'assistant_text' })))).toBe('text');
    expect(nodeRole(leaf(mkItem({ id: 'a', kind: 'user_text' })))).toBe('text');
  });

  it('classifies notification / error / compaction / thinking as other', () => {
    for (const kind of ['notification', 'error', 'compaction', 'thinking'] as const) {
      expect(nodeRole(leaf(mkItem({ id: 'a', kind })))).toBe('other');
    }
  });
});

describe('isToolTextBoundary', () => {
  function leaf(id: string, kind: Item['kind']): TimelineLeaf {
    return { kind: 'leaf', item: mkItem({ id, kind }) };
  }
  function group(id: string): SubagentGroupNode {
    return {
      kind: 'group',
      parent: mkItem({ id, kind: 'tool_call' }),
      children: [],
      latestChildSummary: '',
      descendantCount: 0,
    };
  }

  it('returns false at index 0 (no predecessor)', () => {
    expect(isToolTextBoundary([leaf('a', 'tool_call')], 0)).toBe(false);
  });

  it('fires on tool → text', () => {
    const nodes = [leaf('t', 'tool_call'), leaf('msg', 'assistant_text')];
    expect(isToolTextBoundary(nodes, 1)).toBe(true);
  });

  it('fires on text → tool', () => {
    const nodes = [leaf('msg', 'assistant_text'), leaf('t', 'tool_call')];
    expect(isToolTextBoundary(nodes, 1)).toBe(true);
  });

  it('fires on SubagentGroup → text and text → SubagentGroup (group counts as tool)', () => {
    expect(isToolTextBoundary([group('p'), leaf('msg', 'assistant_text')], 1)).toBe(true);
    expect(isToolTextBoundary([leaf('msg', 'assistant_text'), group('p')], 1)).toBe(true);
  });

  it('does not fire on tool → tool or text → text', () => {
    expect(isToolTextBoundary([leaf('a', 'tool_call'), leaf('b', 'tool_call')], 1)).toBe(false);
    expect(isToolTextBoundary([leaf('a', 'assistant_text'), leaf('b', 'assistant_text')], 1)).toBe(false);
  });

  it('does not fire when either side is "other" (notification, compaction, error, thinking)', () => {
    expect(isToolTextBoundary([leaf('n', 'notification'), leaf('t', 'tool_call')], 1)).toBe(false);
    expect(isToolTextBoundary([leaf('t', 'tool_call'), leaf('n', 'notification')], 1)).toBe(false);
    expect(isToolTextBoundary([leaf('c', 'compaction'), leaf('msg', 'assistant_text')], 1)).toBe(false);
  });
});
