import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import InlineSubagentGroupTestHarness from './InlineSubagentGroupTestHarness.svelte';
import type { Item } from '../../types/models';
import type { InlineSubagentGroupNode, SubagentGroupNode } from '../../utils/subagentGrouping';

function mkItem(overrides: Partial<Item> & { id: string }): Item {
  const createdAt = overrides.createdAt ?? 0;
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    toolName: 'Agent',
    summary: 'Agent: launching',
    createdAt,
    updatedAt: overrides.updatedAt ?? createdAt,
    ...overrides,
  };
}

function mkMember(id: string, overrides: Partial<Item> = {}): SubagentGroupNode {
  return {
    kind: 'group',
    parent: mkItem({ id, ...overrides }),
    groupKey: `inline:assistant-1:${id}`,
    children: [],
    descendantCount: 0,
    latestChildSummary: '',
  };
}

function mkWrapper(members: SubagentGroupNode[], descendantCount = 0): InlineSubagentGroupNode {
  return {
    kind: 'inline_subagent_group',
    groupKey: 'inline:assistant-1:agent-1',
    threadId: 'thread-1',
    members,
    memberCount: members.length,
    descendantCount,
  };
}

describe('<InlineSubagentGroup>', () => {
  it('renders a non-collapsible wrapper header and passes members through at the same depth', () => {
    const group = mkWrapper([
      mkMember('agent-1', { itemIndex: 0, summary: 'Agent: one' }),
      mkMember('agent-2', { itemIndex: 1, summary: 'Agent: two' }),
    ], 3);

    const { getByTestId, getAllByTestId, queryByTestId } = render(InlineSubagentGroupTestHarness, {
      props: { group, startDepth: 1 },
    });

    expect(getByTestId('inline-subagent-group-label').textContent).toContain('Running Agents');
    expect(getByTestId('inline-subagent-group-meta').textContent?.trim()).toBe('2 agents · 2 running · 3 entries');
    expect(queryByTestId('inline-subagent-group-toggle')).toBeNull();

    const members = getAllByTestId('inline-subagent-member');
    expect(members).toHaveLength(2);
    expect(members.map((node) => node.getAttribute('data-parent-id'))).toEqual(['agent-1', 'agent-2']);
    expect(members.map((node) => node.getAttribute('data-depth'))).toEqual(['1', '1']);
  });

  it('keeps the stable wrapper chrome-less for a completed one-agent group', () => {
    const group = mkWrapper([
      mkMember('agent-1', { status: 'completed' }),
    ]);

    const { getByTestId, queryByTestId } = render(InlineSubagentGroupTestHarness, {
      props: { group },
    });

    expect(queryByTestId('inline-subagent-group-header')).toBeNull();
    expect(queryByTestId('inline-subagent-group-toggle')).toBeNull();
    expect(getByTestId('inline-subagent-member')).toHaveAttribute('data-parent-id', 'agent-1');
    expect(getByTestId('inline-subagent-group')).toHaveAttribute('data-agent-count', '1');
    expect(getByTestId('inline-subagent-group')).toHaveAttribute('data-running-count', '0');
  });
});
