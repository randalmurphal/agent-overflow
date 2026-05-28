import { describe, expect, it } from 'vitest';
import {
  finalAssistantTextIdsByTurn,
  findTimelineNodeIndex,
  groupItemsBySubagent,
  isToolTextBoundary,
  nodeContainsItem,
  sliceRevealedNodes,
  timelineNodeItemId,
  timelineNodeItemIndex,
  timelineNodeKey,
  visibleTimelineItemIdForItem,
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

function agentLaunch(id: string, itemIndex: number, toolName: 'Agent' | 'Task' = 'Agent'): Item {
  return mkItem({
    id,
    itemIndex,
    kind: 'tool_call',
    toolName,
    summary: `${toolName}: investigate`,
    meta: toolMeta({
      toolName,
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

  it('renders a Claude foreground Agent as a group before children arrive', () => {
    const nodes = groupItemsBySubagent([agentLaunch('agent-1', 0)]);

    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.parent.id).toBe('agent-1');
    expect(group.children).toEqual([]);
    expect(group.descendantCount).toBe(0);
  });

  it('renders a foreground Task as a group', () => {
    const nodes = groupItemsBySubagent([agentLaunch('task-1', 0, 'Task')]);

    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.parent.id).toBe('task-1');
    expect(group.children).toEqual([]);
  });

  it('does not group a tool_completion with Agent toolName', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'comp', itemIndex: 0, kind: 'tool_completion', toolName: 'Agent', completionOf: 'agent-1' }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('comp');
  });

  it('renders sibling foreground Agents as independent group cards', () => {
    const nodes = groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      agentLaunch('agent-2', 1),
    ]);

    expect(nodes).toHaveLength(2);
    expect(expectGroup(nodes[0]).parent.id).toBe('agent-1');
    expect(expectGroup(nodes[1]).parent.id).toBe('agent-2');
    expect(nodeContainsItem(nodes[1], 'agent-2')).toBe(true);
  });

  it('keeps children inside the correct agent card', () => {
    const nodes = groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      agentLaunch('agent-2', 1),
      mkItem({
        id: 'child-of-agent-1',
        itemIndex: 2,
        parentId: 'agent-1',
        kind: 'assistant_text',
        summary: 'child after agent two',
      }),
    ]);

    expect(nodes).toHaveLength(2);
    const group1 = expectGroup(nodes[0]);
    const group2 = expectGroup(nodes[1]);
    expect(group1.children.map((node) => timelineNodeItemId(node))).toEqual([
      'child-of-agent-1',
    ]);
    expect(group2.children).toEqual([]);
  });

  it('nests children only below Agent launch rows', () => {
    const nodes = groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      mkItem({
        id: 'child',
        itemIndex: 1,
        parentId: 'agent-1',
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read: file.ts',
      }),
    ]);

    expect(nodes).toHaveLength(1);
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
      'complete-spawn-1',
    ]);
    expect(group.descendantCount).toBe(1);
    expect(nodeContainsItem(group, 'complete-wait-1')).toBe(false);
    expect(nodeContainsItem(group, 'complete-spawn-1')).toBe(true);
  });

  it('keeps terminal command completions as siblings after the terminal wait carrier', () => {
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

    expect(nodes).toHaveLength(2);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('waited-pid-1');
    expect(group.children).toEqual([]);
    expect(expectLeaf(nodes[1]).item.id).toBe('complete-cmd-1');
  });

  it('keeps legacy camelCase terminal command completions flat by process', () => {
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

    expect(nodes).toHaveLength(2);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('waited-pid-1');
    expect(group.children).toEqual([]);
    expect(expectLeaf(nodes[1]).item.id).toBe('complete-cmd-1');
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

  it('projects a persisted Codex spawn/wait sequence with hidden child transcript rows', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'spawn-review',
        itemIndex: 25,
        kind: 'tool_call',
        toolName: 'collab_agent',
        isBackground: true,
        summary: 'collab_agent: review',
        meta: toolMeta({
          toolName: 'collab_agent',
          input: {
            tool: 'spawn_agent',
            receiverThreadIds: ['child-review'],
            newAgentNickname: 'Chandrasekhar',
          },
        }),
      }),
      mkItem({
        id: 'child-prompt',
        itemIndex: 26,
        kind: 'user_text',
        role: 'user',
        parentId: 'spawn-review',
        summary: 'Review the timeline code',
      }),
      mkItem({
        id: 'child-progress',
        itemIndex: 27,
        kind: 'assistant_text',
        parentId: 'spawn-review',
        summary: 'I will inspect the live path.',
      }),
      mkItem({
        id: 'wait-review',
        itemIndex: 33,
        kind: 'tool_call',
        toolName: 'wait_agent',
        summary: 'wait_agent',
        meta: toolMeta({
          input: {
            tool: 'wait_agent',
            receiverThreadIds: ['child-review'],
            agentsStates: {
              'child-review': {
                status: 'completed',
                message: 'Recommended | frontend/src/lib/components/chat/MessageTimeline.svelte:223 | retry layout',
              },
            },
          },
        }),
      }),
      mkItem({
        id: 'complete-wait-review',
        itemIndex: 68,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        summary: 'wait_agent',
        meta: toolMeta({
          input: {
            tool: 'wait_agent',
            receiverThreadIds: ['child-review'],
            agentsStates: {
              'child-review': {
                status: 'completed',
                message: 'Recommended | frontend/src/lib/components/chat/MessageTimeline.svelte:223 | retry layout',
              },
            },
          },
        }),
      }),
      mkItem({
        id: 'complete-spawn-review',
        itemIndex: 69,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-review',
        payloadId: 'payload-wait-review',
        payloadKind: 'tool_call_result',
        summary: 'collab_agent: review -> done',
        meta: toolMeta({ wait_carrier_id: 'wait-review', item_status: 'completed' }),
      }),
      mkItem({
        id: 'assistant-after-review',
        itemIndex: 70,
        kind: 'assistant_text',
        summary: 'The review caught one edge I agree with.',
      }),
    ]);

    expect(nodes).toHaveLength(3);
    expect(expectLeaf(nodes[0]).item.id).toBe('spawn-review');
    const waitGroup = expectWaitGroup(nodes[1]);
    expect(waitGroup.parent.id).toBe('wait-review');
    expect(waitGroup.children.map((node) => expectLeaf(node).item.id)).toEqual([
      'complete-spawn-review',
    ]);
    expect(nodeContainsItem(waitGroup, 'complete-wait-review')).toBe(false);
    expect(expectLeaf(nodes[2]).item.id).toBe('assistant-after-review');
    expect(nodes.some((node) => nodeContainsItem(node, 'child-prompt'))).toBe(false);
    expect(nodes.some((node) => nodeContainsItem(node, 'child-progress'))).toBe(false);
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

  it('keeps a timeout wait completion visible after the neutral wait carrier', () => {
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

    expect(nodes).toHaveLength(2);
    const group = expectWaitGroup(nodes[0]);
    expect(group.parent.id).toBe('wait-child');
    expect(group.children).toEqual([]);
    expect(group.descendantCount).toBe(0);
    expect(nodeContainsItem(group, 'complete-wait-child')).toBe(false);
    expect(expectLeaf(nodes[1]).item.id).toBe('complete-wait-child');
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

  it('renders foreground Agent-named rows as groups even without inline marker meta', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'agent-like',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
    ]);

    expect(expectGroup(nodes[0]).parent.id).toBe('agent-like');
  });

  it('keeps non-Agent/Task tool rows flat regardless of meta', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'marked-read',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Read',
        meta: toolMeta({ toolName: 'Read' }),
      }),
    ]);

    expect(expectLeaf(nodes[0]).item.id).toBe('marked-read');
  });

  it('does not duplicate a nested Agent as a root', () => {
    const nodes = groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      {
        ...agentLaunch('agent-2', 1),
        parentId: 'agent-1',
      },
    ]);

    expect(nodes).toHaveLength(1);
    const group = expectGroup(nodes[0]);
    expect(group.children).toHaveLength(1);
    expect(expectGroup(group.children[0]).parent.id).toBe('agent-2');
    expect(findTimelineNodeIndex(nodes, 'agent-2')).toBe(0);
  });

  it('preserves order of Agents with intervening non-agent rows', () => {
    const nodes = groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      mkItem({ id: 'bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash' }),
      agentLaunch('agent-2', 2),
    ]);

    expect(nodes).toHaveLength(3);
    expect(expectGroup(nodes[0]).parent.id).toBe('agent-1');
    expect(expectLeaf(nodes[1]).item.id).toBe('bash');
    expect(expectGroup(nodes[2]).parent.id).toBe('agent-2');
  });

  it('keeps group keys stable when older unrelated rows load', () => {
    const partial = groupItemsBySubagent([
      agentLaunch('agent-2', 2),
    ]);
    const partialGroup = expectGroup(partial[0]);

    const complete = groupItemsBySubagent([
      mkItem({ id: 'bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash' }),
      agentLaunch('agent-2', 2),
    ]);
    const completeGroup = expectGroup(complete[1]);

    expect(partialGroup.groupKey).toBe('agent-2');
    expect(completeGroup.groupKey).toBe(partialGroup.groupKey);
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
      agentLaunch('agent-1', 0),
      mkItem({ id: 'child', itemIndex: 1, parentId: 'agent-1', summary: 'work' }),
      mkItem({ id: 'after', itemIndex: 2, summary: 'after' }),
    ]);

    expect(timelineNodeKey(nodes[0])).toBe('g:thread-1:agent-1');
    expect(timelineNodeItemId(nodes[0])).toBe('agent-1');
    expect(findTimelineNodeIndex(nodes, 'child')).toBe(0);
    expect(findTimelineNodeIndex(nodes, 'after')).toBe(1);
  });

  it('detects tool/text spacing boundaries after grouping', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'user', itemIndex: 0, kind: 'user_text', role: 'user' }),
      agentLaunch('agent-1', 1),
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
      agentLaunch('agent-0', 0),
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

describe('sliceRevealedNodes', () => {
  // think(0) | Agent group(1) {child(2)} | text(3) — a top-level sequence
  // with a subagent group in the middle.
  function buildTimeline(): TimelineNode[] {
    const launch = agentLaunch('agent-1', 1);
    return groupItemsBySubagent([
      mkItem({ id: 'think-0', itemIndex: 0, kind: 'thinking', summary: 'reasoning' }),
      launch,
      mkItem({
        id: 'child-2',
        itemIndex: 2,
        kind: 'assistant_text',
        parentId: 'agent-1',
        summary: 'subagent step',
      }),
      mkItem({ id: 'text-3', itemIndex: 3, kind: 'assistant_text', summary: 'final answer' }),
    ]);
  }

  it('returns the same array reference when the boundary is null (no gate)', () => {
    const nodes = buildTimeline();
    expect(sliceRevealedNodes(nodes, null)).toBe(nodes);
  });

  it('withholds the trailing run after a mid-sequence boundary', () => {
    const nodes = buildTimeline();
    // Boundary at the Agent launch (the group parent, position 0:1) reveals
    // the thinking row and the whole subagent group, withholds text-3.
    const revealed = sliceRevealedNodes(nodes, { turnIndex: 0, itemIndex: 1 });
    expect(revealed.map(timelineNodeItemId)).toEqual(['think-0', 'agent-1']);
    // The group is returned whole — its child rides inside, never gated out.
    const group = expectGroup(revealed[1]);
    expect(group.children.map((c) => timelineNodeItemId(c))).toEqual(['child-2']);
  });

  it('a streaming thinking row withholds the subsequent subagent group', () => {
    const nodes = buildTimeline();
    const revealed = sliceRevealedNodes(nodes, { turnIndex: 0, itemIndex: 0 });
    expect(revealed.map(timelineNodeItemId)).toEqual(['think-0']);
  });

  it('returns the same reference when the boundary is at or past the last node', () => {
    const nodes = buildTimeline();
    expect(sliceRevealedNodes(nodes, { turnIndex: 0, itemIndex: 3 })).toBe(nodes);
    expect(sliceRevealedNodes(nodes, { turnIndex: 9, itemIndex: 0 })).toBe(nodes);
  });

  it('compares turnIndex before itemIndex across turns', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'a', turnIndex: 0, itemIndex: 5, summary: 'older turn tail' }),
      mkItem({ id: 'b', turnIndex: 1, itemIndex: 0, kind: 'thinking', summary: 'new turn' }),
      mkItem({ id: 'c', turnIndex: 1, itemIndex: 1, kind: 'tool_call', summary: 'tool' }),
    ]);
    // Boundary in turn 1 keeps the whole earlier turn even though its
    // itemIndex (5) is numerically larger than the boundary's (0).
    const revealed = sliceRevealedNodes(nodes, { turnIndex: 1, itemIndex: 0 });
    expect(revealed.map(timelineNodeItemId)).toEqual(['a', 'b']);
  });

  it('timelineNodeItemIndex reads the root item index for groups', () => {
    const nodes = buildTimeline();
    expect(timelineNodeItemIndex(nodes[0])).toBe(0); // think leaf
    expect(timelineNodeItemIndex(nodes[1])).toBe(1); // Agent group → parent
  });
});
