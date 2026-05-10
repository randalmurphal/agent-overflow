import { describe, expect, it } from 'vitest';
import {
  finalAssistantTextIdsByTurn,
  findTimelineNodeIndex,
  groupItemsBySubagent,
  isToolTextBoundary,
  nodeContainsItem,
  timelineNodeItemId,
  timelineNodeKey,
  visibleTimelineItemIdForItem,
  type InlineSubagentGroupNode,
  type SubagentGroupNode,
  type TimelineLeaf,
  type WaitGroupNode,
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

function codexWaitAgentMeta(): string {
  return toolMeta({ toolName: 'wait_agent', input: { tool: 'wait_agent' } });
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

function expectInlineGroup(node: TimelineNode): InlineSubagentGroupNode {
  if (node.kind !== 'inline_subagent_group') {
    throw new Error(`expected inline_subagent_group node, got ${node.kind}`);
  }
  return node;
}

function expectLeaf(node: TimelineNode): TimelineLeaf {
  if (node.kind !== 'leaf') {
    throw new Error(`expected leaf node, got ${node.kind}`);
  }
  return node;
}

function expectWaitGroup(node: TimelineNode): WaitGroupNode {
  if (node.kind !== 'wait_group') {
    throw new Error(`expected wait_group node, got ${node.kind}`);
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

  it('renders a Claude inline Agent in a stable wrapper before children arrive', () => {
    const nodes = groupItemsBySubagent([inlineAgent('agent-1', 0)]);

    const wrapper = expectInlineGroup(nodes[0]);
    expect(wrapper.groupKey).toBe('inline:assistant-1:agent-1');
    expect(wrapper.memberCount).toBe(1);
    expect(wrapper.members).toHaveLength(1);
    expect(wrapper.members[0].parent.id).toBe('agent-1');
    expect(wrapper.members[0].children).toEqual([]);
    expect(wrapper.descendantCount).toBe(0);
  });

  it('groups sibling inline Agents from the same assistant message as peer cards in one wrapper', () => {
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      inlineAgent('agent-2', 1, 'assistant-1'),
    ]);

    expect(nodes).toHaveLength(1);
    const wrapper = expectInlineGroup(nodes[0]);
    expect(wrapper.groupKey).toBe('inline:assistant-1:agent-1');
    expect(wrapper.memberCount).toBe(2);
    expect(wrapper.members.map((member) => member.parent.id)).toEqual(['agent-1', 'agent-2']);
    expect(nodeContainsItem(wrapper, 'agent-2')).toBe(true);
  });

  it('keeps first-agent children inside the first agent card instead of hoisting them', () => {
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

    const wrapper = expectInlineGroup(nodes[0]);
    expect(wrapper.members.map((member) => member.parent.id)).toEqual(['agent-1', 'agent-2']);
    expect(wrapper.members[0].children.map((node) => timelineNodeItemId(node))).toEqual([
      'child-of-agent-1',
    ]);
    expect(wrapper.members[1].children).toEqual([]);
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

    const wrapper = expectInlineGroup(nodes[0]);
    const group = wrapper.members[0];
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['child']);
    expect(group.descendantCount).toBe(1);
    expect(wrapper.descendantCount).toBe(1);
    expect(group.latestChildSummary).toBe('Read: file.ts');
  });

  it('does not use generic parentId nesting for non-agent rows', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'parent', itemIndex: 0, kind: 'tool_call', toolName: 'Bash' }),
      mkItem({ id: 'child', itemIndex: 1, parentId: 'parent', summary: 'stdout' }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['parent', 'child']);
  });

  it('keeps backgrounded Claude agents and Codex spawn rows flat', () => {
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
        toolName: 'collab_agent',
        isBackground: true,
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('background-agent');
    expect(expectLeaf(nodes[1]).item.id).toBe('codex-agent');
  });

  it('keeps Codex spawn rows flat when spawn metadata lives in meta', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        payloadMeta: toolMeta({ lineCount: 1 }),
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('codex-agent');
  });

  it('suppresses Codex child prompt echo rows from the main timeline', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        isBackground: true,
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent', prompt: 'Inspect the parser' } }),
      }),
      mkItem({
        id: 'child-prompt',
        itemIndex: 1,
        kind: 'user_text',
        role: 'user',
        parentId: 'codex-agent',
        summary: 'Inspect the parser',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    expect(expectLeaf(nodes[0]).item.id).toBe('codex-agent');
  });

  it('suppresses Codex child transcript rows from the main timeline', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        isBackground: true,
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent', prompt: 'Repeatable prompt' } }),
      }),
      mkItem({
        id: 'initial-prompt-echo',
        itemIndex: 1,
        kind: 'user_text',
        role: 'user',
        parentId: 'codex-agent',
        summary: 'Repeatable prompt',
      }),
      mkItem({
        id: 'later-repeated-message',
        itemIndex: 2,
        kind: 'user_text',
        role: 'user',
        parentId: 'codex-agent',
        summary: 'Repeatable prompt',
      }),
      mkItem({
        id: 'child-tool',
        itemIndex: 3,
        kind: 'tool_call',
        toolName: 'command_execution',
        parentId: 'codex-agent',
        summary: 'Bash: sleep 20',
      }),
      mkItem({
        id: 'child-answer',
        itemIndex: 4,
        kind: 'assistant_text',
        role: 'assistant',
        parentId: 'codex-agent',
        summary: '0',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    expect(expectLeaf(nodes[0]).item.id).toBe('codex-agent');
  });

  it('maps hidden Codex child transcript rows back to their visible spawn row', () => {
    const items = [
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      }),
      mkItem({
        id: 'child-answer',
        itemIndex: 1,
        kind: 'assistant_text',
        parentId: 'codex-agent',
        summary: '0',
      }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'child-answer')).toBe('codex-agent');
    expect(visibleTimelineItemIdForItem(items, 'codex-agent')).toBe('codex-agent');
  });

  it('nests Codex subagent completions under the wait carrier when they share the wait payload', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'wait-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'wait_agent',
        meta: codexWaitAgentMeta(),
      }),
      mkItem({
        id: 'complete-wait-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-1',
        payloadId: 'payload-final',
        payloadKind: 'tool_call_result',
      }),
      mkItem({
        id: 'complete-spawn-1',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-1',
        payloadId: 'payload-final',
        payloadKind: 'tool_call_result',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('wait-1');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual([
      'complete-wait-1',
      'complete-spawn-1',
    ]);
    expect(nodeContainsItem(group, 'complete-spawn-1')).toBe(true);
  });

  it('nests terminal command completions under the terminal wait carrier for the same process', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'waited-pid-1',
        itemIndex: 0,
        kind: 'terminal_interaction',
        meta: JSON.stringify({ process_id: 'pid-1' }),
      }),
      mkItem({
        id: 'complete-cmd-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'command_execution',
        completionOf: 'cmd-1',
        meta: JSON.stringify({ process_id: 'pid-1' }),
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('waited-pid-1');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-cmd-1']);
  });

  it('nests legacy camelCase terminal command completions by process', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'waited-pid-1',
        itemIndex: 0,
        kind: 'terminal_interaction',
        meta: JSON.stringify({ processId: 'pid-1' }),
      }),
      mkItem({
        id: 'complete-cmd-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'command_execution',
        completionOf: 'cmd-1',
        meta: JSON.stringify({ processId: 'pid-1' }),
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('waited-pid-1');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-cmd-1']);
  });

  it('nests target completions under an explicit wait_carrier_id', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'wait-child',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'wait_agent',
        meta: codexWaitAgentMeta(),
      }),
      mkItem({
        id: 'complete-spawn-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-1',
        meta: JSON.stringify({ wait_carrier_id: 'wait-child' }),
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('wait-child');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-spawn-1']);
  });

  it('nests target completions under a legacy camelCase wait carrier id', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'wait-child',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'wait_agent',
        meta: codexWaitAgentMeta(),
      }),
      mkItem({
        id: 'complete-spawn-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-1',
        meta: JSON.stringify({ waitCarrierID: 'wait-child' }),
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('wait-child');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-spawn-1']);
  });

  it('nests a timeout wait completion under the neutral wait carrier', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'wait-child',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'wait_agent',
        status: 'completed',
        meta: codexWaitAgentMeta(),
      }),
      mkItem({
        id: 'complete-wait-child',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-child',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('wait-child');
    expect(group.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-wait-child']);
  });

  it('keeps a non-Codex wait_agent-named tool flat', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'foreign-wait',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'wait_agent',
        meta: toolMeta({ toolName: 'wait_agent', input: { query: 'not Codex wait_agent' } }),
      }),
      mkItem({
        id: 'foreign-completion',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'foreign-wait',
      }),
    ]);

    expect(nodes).toHaveLength(2);
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['foreign-wait', 'foreign-completion']);
  });

  it('does not treat non-empty terminal interactions as wait carriers', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'interacted-pid-1',
        itemIndex: 0,
        kind: 'terminal_interaction',
        status: 'completed',
        meta: JSON.stringify({ process_id: 'pid-1', has_stdin: true }),
      }),
      mkItem({
        id: 'complete-cmd-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'command_execution',
        completionOf: 'cmd-1',
        meta: JSON.stringify({ process_id: 'pid-1' }),
      }),
    ]);

    expect(nodes).toHaveLength(2);
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['interacted-pid-1', 'complete-cmd-1']);
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
    const wrapper = expectInlineGroup(nodes[0]);
    const group = wrapper.members[0];
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
    expect(expectInlineGroup(nodes[0]).members[0].parent.id).toBe('agent-1');
    expect(expectLeaf(nodes[1]).item.id).toBe('bash');
    const second = expectInlineGroup(nodes[2]);
    expect(second.members[0].parent.id).toBe('agent-2');
    expect(second.groupKey).toBe('inline:assistant-1:agent-2');
  });

  it('keeps non-contiguous inline wrapper keys stable when older history loads', () => {
    const partial = groupItemsBySubagent([
      mkItem({ id: 'bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash' }),
      inlineAgent('agent-2', 2, 'assistant-1'),
    ]);
    const partialWrapper = expectInlineGroup(partial[1]);

    const complete = groupItemsBySubagent([
      inlineAgent('agent-1', 0, 'assistant-1'),
      mkItem({ id: 'bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash' }),
      inlineAgent('agent-2', 2, 'assistant-1'),
    ]);
    const completeWrapper = expectInlineGroup(complete[2]);

    expect(partialWrapper.groupKey).toBe('inline:assistant-1:agent-2');
    expect(completeWrapper.groupKey).toBe(partialWrapper.groupKey);
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

    expect(timelineNodeKey(nodes[0])).toBe('ig:thread-1:inline:assistant-1:agent-1');
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

describe('finalAssistantTextIdsByTurn', () => {
  it('picks the last assistant_text per settled turn', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'u0', kind: 'user_text', role: 'user' }),
      mkItem({ id: 't0:0', itemIndex: 1, kind: 'assistant_text', summary: 'mid' }),
      mkItem({ id: 't0:1', itemIndex: 2, kind: 'assistant_text', summary: 'final-of-0' }),
      mkItem({ id: 'u1', turnIndex: 1, kind: 'user_text', role: 'user' }),
      mkItem({ id: 't1:0', turnIndex: 1, itemIndex: 1, kind: 'assistant_text', summary: 'final-of-1' }),
    ]);

    const ids = finalAssistantTextIdsByTurn(nodes, null);
    expect(ids).toEqual(new Set(['t0:1', 't1:0']));
  });

  it('omits the in-flight turn so the closing message of an unfinished turn is not labelled', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 't0:0', kind: 'assistant_text', summary: 'final-of-0' }),
      mkItem({ id: 't1:0', turnIndex: 1, kind: 'assistant_text', summary: 'streaming' }),
    ]);

    const ids = finalAssistantTextIdsByTurn(nodes, 1);
    expect(ids).toEqual(new Set(['t0:0']));
  });

  it('returns the empty set when no leaves are assistant_text', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'u0', kind: 'user_text', role: 'user' }),
      mkItem({ id: 'tool:0', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', summary: 'ls' }),
    ]);

    expect(finalAssistantTextIdsByTurn(nodes, null).size).toBe(0);
  });

  it('skips assistant_text nested inside subagent groups (chat row contract)', () => {
    // The "Response" pill divider can only sit before a top-level leaf
    // (chat row contract), so a turn whose only trailing assistant_text
    // lives inside a subagent group must not appear here.
    const nodes = groupItemsBySubagent([
      inlineAgent('agent-0', 0),
      mkItem({
        id: 'inside',
        itemIndex: 1,
        parentId: 'agent-0',
        kind: 'assistant_text',
        summary: 'subagent text',
      }),
    ]);

    expect(finalAssistantTextIdsByTurn(nodes, null).size).toBe(0);
  });
});
