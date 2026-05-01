import { describe, expect, it } from 'vitest';
import {
  findTimelineNodeIndex,
  groupItemsBySubagent,
  isToolTextBoundary,
  nodeContainsItem,
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

function toolMeta(values: Record<string, unknown>): string {
  return JSON.stringify(values);
}

function inlineAgent(id: string, itemIndex: number, assistantMessageID = 'assistant-1'): Item {
  return mkItem({
    id,
    itemIndex,
    kind: 'tool_call',
    toolName: 'Agent',
    summary: 'Agent: investigate',
    meta: toolMeta({
      toolName: 'Agent',
      assistant_message_id: assistantMessageID,
      is_inline_subagent: true,
      inline_subagent_group_id: assistantMessageID,
      input: { description: 'investigate' },
    }),
  });
}

function expectGroup(node: TimelineNode): SubagentGroupNode {
  if (node.kind !== 'group') {
    throw new Error(`expected group node, got ${node.kind}`);
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
  it('keeps ordinary rows as leaves', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'a', itemIndex: 0, summary: 'one' }),
      mkItem({ id: 'b', itemIndex: 1, summary: 'two' }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['a', 'b']);
  });

  it('renders a Claude inline Agent as a stable group before children arrive', () => {
    const nodes = groupItemsBySubagent([inlineAgent('agent-1', 0)]);

    const group = expectGroup(nodes[0]);
    expect(group.parent.id).toBe('agent-1');
    expect(group.memberCount).toBe(1);
    expect(group.children).toEqual([]);
    expect(group.descendantCount).toBe(0);
  });

  it('groups sibling inline Agents from the same assistant message under the first agent row', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      inlineAgent('agent-2', 1, 'assistant-1'),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.parent.id).toBe('agent-1');
    expect(group.memberCount).toBe(2);
    expect(group.children).toHaveLength(1);
    expect(expectGroup(group.children[0]).parent.id).toBe('agent-2');
    expect(nodeContainsItem(group, 'agent-2')).toBe(true);
  });

  it('preserves chronological order between sibling inline Agents and first-agent children', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      inlineAgent('agent-2', 1, 'assistant-1'),
      mkItem({
        id: 'child-of-agent-1',
        itemIndex: 2,
        parentId: 'agent-1',
        kind: 'assistant_text',
        summary: 'child after agent two',
      }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.children.map((node) => timelineNodeItemId(node))).toEqual([
      'agent-2',
      'child-of-agent-1',
    ]);
  });

  it('nests children only below inline Agent launch rows', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0),
      mkItem({
        id: 'child',
        itemIndex: 1,
        parentId: 'agent-1',
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: file.ts',
      }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['child']);
    expect(group.descendantCount).toBe(1);
    expect(group.latestChildSummary).toBe('Read: file.ts');
  });

  it('does not use generic parentId nesting for non-agent rows', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', toolName: 'Bash' }),
      mkItem({ id: 'child', itemIndex: 1, parentId: 'parent', summary: 'stdout' }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['parent', 'child']);
  });

  it('keeps backgrounded Claude agents and Codex spawn_agent rows flat', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'background-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'codex-agent',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'spawn_agent',
        meta: toolMeta({ toolName: 'spawn_agent' }),
      }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'background-agent',
      'codex-agent',
    ]);
  });

  it('keeps foreground Agent-named rows flat without the inline marker', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'agent-like',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('agent-like');
  });

  it('keeps marked non-Agent rows flat', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'marked-read',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Read',
        meta: toolMeta({ toolName: 'Read', is_inline_subagent: true }),
      }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('marked-read');
  });

  it('does not duplicate a nested inline Agent as a root', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      {
        ...inlineAgent('agent-2', 1, 'assistant-2'),
        parentId: 'agent-1',
      },
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.children).toHaveLength(1);
    expect(expectGroup(group.children[0]).parent.id).toBe('agent-2');
    expect(findTimelineNodeIndex(nodes, 'agent-2')).toBe(0);
  });

  it('does not reorder inline Agents across intervening non-agent rows', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      mkItem({ id: 'bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash' }),
      inlineAgent('agent-2', 2, 'assistant-1'),
    ]);

    expect(nodes).toHaveLength(3);
    expect(expectGroup(nodes[0]).parent.id).toBe('agent-1');
    expect(expectLeaf(nodes[1]).item.id).toBe('bash');
    expect(expectGroup(nodes[2]).parent.id).toBe('agent-2');
  });

  it('surfaces missing parents as orphan leaves instead of dropping them', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'orphan', itemIndex: 0, parentId: 'missing', summary: 'lost child' }),
    ]);

    const orphan = expectLeaf(nodes[0]);
    expect(orphan.item.id).toBe('orphan');
    expect(orphan.orphan).toBe(true);
  });

  it('preserves stable keys and lookup semantics for grouped children', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0),
      mkItem({ id: 'child', itemIndex: 1, parentId: 'agent-1', summary: 'work' }),
      mkItem({ id: 'after', itemIndex: 2, summary: 'after' }),
    ]);

    expect(timelineNodeKey(nodes[0])).toBe('g:thread-1:inline:assistant-1:agent-1');
    expect(timelineNodeItemId(nodes[0])).toBe('agent-1');
    expect(findTimelineNodeIndex(nodes, 'child')).toBe(0);
    expect(findTimelineNodeIndex(nodes, 'after')).toBe(1);
  });

  it('detects tool/text spacing boundaries after grouping', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'user', itemIndex: 0, kind: 'user_text', role: 'user' }),
      inlineAgent('agent-1', 1),
      mkItem({ id: 'response', itemIndex: 2, kind: 'assistant_text', role: 'assistant' }),
    ]);

    expect(isToolTextBoundary(nodes, 0)).toBe(false);
    expect(isToolTextBoundary(nodes, 1)).toBe(true);
    expect(isToolTextBoundary(nodes, 2)).toBe(true);
  });
});
