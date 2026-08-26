import { describe, expect, it } from 'vitest';
import {
  finalAssistantTextIdsByTurn,
  findTimelineNodeIndex,
  groupItemsBySubagent,
  isToolTextBoundary,
  MAX_DEPTH,
  nodeContainsItem,
  sliceRevealedNodes,
  timelineNodeItemId,
  timelineNodeItemIndex,
  timelineNodeKey,
  visibleTimelineItemIdForItem,
  type SubagentGroupNode,
  type SubagentLiveAggregates,
  type TimelineLeaf,
  type WaitGroupNode,
  type TimelineNode,
} from './subagentGrouping';
import type { SubagentFoldAggregate } from './subagentFold';
import type { Item } from '../types/models';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';

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

  it('keeps a backgrounded Claude launch and a Codex spawn as leaves; only the Claude card exists, at its completion', () => {
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
      mkItem({
        id: 'complete:background-agent',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'background-agent',
        summary: 'Agent: background-agent -> done',
      }),
    ]);

    // A settled Claude background launch: its pre-card launch row at the
    // launch, the card at the completion. A Codex spawn is never a card:
    // its completion may be claimed by a wait group, and the user ruled
    // its `launched` row stays exactly as it was — see `detachedLaunchIDs`
    // in the grouping.
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'background-agent',
      'codex-agent',
      'complete:background-agent',
    ]);
    expectLeaf(nodes[0]);
    expect(expectLeaf(nodes[1]).item.id).toBe('codex-agent');
    const backgroundGroup = expectGroup(nodes[2]);
    expect(backgroundGroup.parent.id).toBe('background-agent');
    expect(backgroundGroup.anchor.id).toBe('complete:background-agent');
    expect(backgroundGroup.children).toEqual([]);
  });

  it('renders a backgrounded Claude agent as an immutable spawn leaf and ONE card at its completion', () => {
    // A backgrounded Claude Agent launches at the top level (parentId
    // empty — the main agent launched it). Its row is the immutable spawn
    // record; its inner transcript (parentId === launch.id) renders INSIDE
    // its card, never interleaved with the main agent's own rows; and the
    // card sits AT the completion sibling (parentId empty, completionOf
    // === launch.id), after the prose the main agent wrote meanwhile —
    // `SubagentGroupNode.anchor`, user ruling 2026-08-23.
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'main-think', itemIndex: 0, kind: 'thinking', summary: 'planning' }),
      mkItem({
        id: 'bg-agent',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent', input: { description: 'investigate' } }),
      }),
      mkItem({ id: 'child-bash', itemIndex: 2, kind: 'tool_call', toolName: 'Bash', parentId: 'bg-agent', summary: 'Bash: ls' }),
      mkItem({
        id: 'child-bash-done',
        itemIndex: 3,
        kind: 'tool_completion',
        toolName: 'Bash',
        parentId: 'bg-agent',
        completionOf: 'child-bash',
        summary: 'Bash: ls',
      }),
      mkItem({ id: 'child-text', itemIndex: 4, kind: 'assistant_text', parentId: 'bg-agent', summary: 'found it' }),
      mkItem({ id: 'main-text', itemIndex: 5, kind: 'assistant_text', summary: 'main agent continues' }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 6,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: investigate',
      }),
    ]);

    // Node ids are the rows the nodes render AT: the spawn leaf answers
    // the launch id, the card answers the completion id. The completion
    // sibling is the card's status source AND its position — the bell is
    // hidden on the strength of the completion rendering (see
    // backgroundCompletionVisibility.test.ts), so the card at the
    // completion point is the transcript's only trace of the agent
    // finishing.
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'main-think',
      'bg-agent',
      'main-text',
      'complete:bg-agent',
    ]);
    expect(expectLeaf(nodes[1]).item.id).toBe('bg-agent');
    const group = expectGroup(nodes[3]);
    expect(group.parent.id).toBe('bg-agent');
    expect(group.anchor.id).toBe('complete:bg-agent');
    expect(group.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'child-bash',
      'child-bash-done',
      'child-text',
    ]);
    expect(group.completion?.id).toBe('complete:bg-agent');
    // A jump to the completion lands on the card; a jump to the LAUNCH
    // lands on the spawn leaf, never on the card turns later (a scroll
    // restore anchored on the spawn row must come back to the spawn row).
    expect(nodeContainsItem(group, 'complete:bg-agent')).toBe(true);
    expect(nodeContainsItem(group, 'bg-agent')).toBe(false);
    expect(findTimelineNodeIndex(nodes, 'complete:bg-agent')).toBe(3);
    expect(findTimelineNodeIndex(nodes, 'bg-agent')).toBe(1);
    // The fold is the header, not a transcript entry: not counted, and
    // never the collapsed preview.
    expect(group.descendantCount).toBe(3);
    expect(group.latestChildSummary).toBe('Bash: ls');
    for (const childId of ['child-bash', 'child-bash-done', 'child-text']) {
      expect(nodes.some((node) => nodeContainsItem(node, childId))).toBe(true);
    }
  });

  it('a running background launch is a spawn leaf with no card; its children wait for the completion', () => {
    // Both launch shapes in one turn. An awaited launch is a card at the
    // launch, children inside. A detached launch is an immutable spawn
    // record: until its completion sibling lands there is no card and its
    // children render nowhere on this surface — the pane and the tray are
    // the live surfaces (user ruling 2026-08-23).
    const nodes = groupItemsBySubagent([
      agentLaunch('fg-agent', 0),
      mkItem({ id: 'fg-child', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'fg-agent', summary: 'Bash: fg' }),
      mkItem({
        id: 'bg-agent',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        summary: 'Agent: bg',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({ id: 'bg-child', itemIndex: 3, kind: 'tool_call', toolName: 'Bash', parentId: 'bg-agent', summary: 'Bash: bg' }),
    ]);

    expect(nodes).toHaveLength(2);
    const fgGroup = expectGroup(nodes[0]);
    expect(fgGroup.parent.id).toBe('fg-agent');
    expect(fgGroup.anchor.id).toBe('fg-agent');
    expect(fgGroup.children.map((child) => expectLeaf(child).item.id)).toEqual(['fg-child']);
    expect(expectLeaf(nodes[1]).item.id).toBe('bg-agent');
    expect(nodes.some((node) => nodeContainsItem(node, 'bg-child'))).toBe(false);
    // The fallback jump target for the unrendered child is the spawn row.
    expect(findTimelineNodeIndex(nodes, 'bg-agent')).toBe(1);
  });

  it('builds a failed background agent\u2019s card at its errored completion', () => {
    // On the wire a failing background subagent terminates via
    // `task_updated{failed}`, which triage maps to an `errored` completion
    // sibling (parentId empty, completionOf === launch.id — verified
    // against 238 real Agent completions). That sibling is the card's status
    // source, so it must reach the node whatever its status, and the failed
    // inner transcript stays inside the same card.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({ id: 'child-bash', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'bg-agent', summary: 'Bash: failing' }),
      mkItem({
        id: 'child-bash-done',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Bash',
        status: 'errored',
        parentId: 'bg-agent',
        completionOf: 'child-bash',
        summary: 'Bash: failing',
      }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 3,
        kind: 'tool_completion',
        toolName: 'Agent',
        status: 'errored',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: investigate',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'bg-agent',
      'complete:bg-agent',
    ]);
    expectLeaf(nodes[0]);
    const group = expectGroup(nodes[1]);
    expect(group.parent.id).toBe('bg-agent');
    const completion = group.completion;
    expect(completion?.id).toBe('complete:bg-agent');
    expect(completion?.status).toBe('errored');
    expect(completion?.completionOf).toBe('bg-agent');
    expect(group.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'child-bash',
      'child-bash-done',
    ]);
  });

  it('nests a foreground launch inside a background launch, with its own grandchild', () => {
    // A bg running Agent launch has a NESTED launch child that is itself
    // foreground — a real Claude shape (an inline agent launched from within
    // a backgrounded one). Depth 2: the nested launch is its own group INSIDE
    // the outer card, and its grandchild sits inside THAT. Nothing reaches
    // the top level.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent', input: { description: 'investigate' } }),
      }),
      mkItem({
        id: 'nested-launch',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: false,
        status: 'completed',
        parentId: 'bg-agent',
        summary: 'Agent: nested',
        meta: toolMeta({ toolName: 'Agent', input: { description: 'nested' } }),
      }),
      mkItem({
        id: 'grandchild-read',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Read',
        status: 'running',
        parentId: 'nested-launch',
        summary: 'Read: file.ts',
      }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 3,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: bg-agent -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['bg-agent', 'complete:bg-agent']);
    const outer = expectGroup(nodes[1]);
    expect(outer.children).toHaveLength(1);
    const nested = expectGroup(outer.children[0]);
    expect(nested.parent.id).toBe('nested-launch');
    expect(nested.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'grandchild-read',
    ]);
    // The outer count rolls the nested card's own descendants up.
    expect(nested.descendantCount).toBe(1);
    expect(outer.descendantCount).toBe(2);
    expect(nodeContainsItem(outer, 'grandchild-read')).toBe(true);
  });

  it('nests a depth-3 mixed subtree (launches and plain tools) under a background anchor', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'depth1-launch',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Task',
        isBackground: false,
        status: 'completed',
        parentId: 'bg-agent',
        summary: 'Task: depth1',
        meta: toolMeta({ toolName: 'Task' }),
      }),
      mkItem({
        id: 'depth2-tool',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Bash',
        status: 'completed',
        parentId: 'depth1-launch',
        summary: 'Bash: depth2',
      }),
      mkItem({
        id: 'depth3-launch',
        itemIndex: 3,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: false,
        status: 'completed',
        parentId: 'depth2-tool',
        summary: 'Agent: depth3',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'depth4-tool',
        itemIndex: 4,
        kind: 'tool_call',
        toolName: 'Read',
        status: 'running',
        parentId: 'depth3-launch',
        summary: 'Read: depth4',
      }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 5,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: bg-agent -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['bg-agent', 'complete:bg-agent']);
    // Only LAUNCHES become cards. `depth2-tool` is an ordinary Bash row, so
    // it stays a leaf and `depth3-launch` — which names it as parent —
    // attaches to the nearest launch ANCESTOR (`depth1-launch`) as a sibling
    // rather than turning a Bash row into a container.
    const outer = expectGroup(nodes[1]);
    const depth1 = expectGroup(outer.children[0]);
    expect(depth1.parent.id).toBe('depth1-launch');
    expect(expectLeaf(depth1.children[0]).item.id).toBe('depth2-tool');
    const depth3 = expectGroup(depth1.children[1]);
    expect(depth3.parent.id).toBe('depth3-launch');
    expect(depth3.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'depth4-tool',
    ]);
    // Three nested cards is exactly MAX_DEPTH; nothing was flattened.
    expect(MAX_DEPTH).toBe(3);
    expect(outer.descendantCount).toBe(4);
    for (const id of ['depth1-launch', 'depth2-tool', 'depth3-launch', 'depth4-tool']) {
      expect(findTimelineNodeIndex(nodes, id)).toBe(1);
    }
  });

  it('keeps a true orphan (missing parent) top-level alongside a nested background subtree', () => {
    // Guards the nesting walk against over-reaching: only rows whose parent
    // is an actual loaded launch move inside a card. A row whose declared
    // parent does not exist in the input stays a flagged top-level leaf.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({ id: 'bg-child', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'bg-agent', summary: 'Bash: bg' }),
      mkItem({ id: 'orphan', itemIndex: 2, parentId: 'missing-parent', summary: 'lost child' }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 3,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: bg-agent -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['bg-agent', 'orphan', 'complete:bg-agent']);
    const orphan = expectLeaf(nodes[1]);
    expect(orphan.orphan).toBe(true);
    expect(expectGroup(nodes[2]).children.map((c) => expectLeaf(c).item.id)).toEqual([
      'bg-child',
    ]);
  });

  it('builds each background launch\u2019s card at its own completion sibling, nested or not', () => {
    // Two completion siblings in the same tree: the OUTER bg launch's has
    // parentId "" (empty), the INNER one carries parentId = the OUTER anchor
    // (the real wire shape for a nested backgrounded launch). Each is where
    // the card of the launch it completes renders — the link is
    // `completionOf`, the position is the sibling's own.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'outer-bg',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        summary: 'Agent: outer',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'inner-bg',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'completed',
        parentId: 'outer-bg',
        summary: 'Agent: inner',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'inner-child',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Bash',
        parentId: 'inner-bg',
        summary: 'Bash: inner work',
      }),
      mkItem({
        id: 'complete:inner-bg',
        itemIndex: 3,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'inner-bg',
        parentId: 'outer-bg',
        summary: 'Agent: inner -> done',
      }),
      mkItem({
        id: 'complete:outer-bg',
        itemIndex: 4,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'outer-bg',
        summary: 'Agent: outer -> done',
      }),
    ]);

    // The outer spawn leaf at the top, the outer card at its completion;
    // inside that card, the inner spawn leaf and then the inner card at
    // ITS completion (which carries the outer launch's parentId).
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'outer-bg',
      'complete:outer-bg',
    ]);
    expectLeaf(nodes[0]);
    const outer = expectGroup(nodes[1]);
    expect(outer.parent.id).toBe('outer-bg');
    expect(outer.completion?.id).toBe('complete:outer-bg');
    expect(outer.children.map((child) => timelineNodeItemId(child))).toEqual([
      'inner-bg',
      'complete:inner-bg',
    ]);
    expectLeaf(outer.children[0]);
    const inner = expectGroup(outer.children[1]);
    expect(inner.parent.id).toBe('inner-bg');
    expect(inner.completion?.id).toBe('complete:inner-bg');
    expect(inner.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'inner-child',
    ]);
    // Every row inside the outer card resolves to the outer card.
    for (const id of ['inner-bg', 'inner-child', 'complete:inner-bg']) {
      expect(findTimelineNodeIndex(nodes, id)).toBe(1);
    }
    expect(findTimelineNodeIndex(nodes, 'outer-bg')).toBe(0);
  });

  it('terminates on malformed cyclic parentId data without swallowing the top-level anchor', () => {
    // Defensive guard for the grouping pass: parentId cycles cannot occur in
    // persisted data (the store enforces acyclic parent chains), but a walk
    // over untrusted input must still terminate and must not let a cycle
    // swallow legitimate rows. Items trapped in a cycle name parents that are
    // not launches, so they degrade to flat top-level leaves (their declared
    // parents exist in the item set, so they are not orphan-flagged) rather
    // than crashing or vanishing.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        summary: 'Agent: investigate',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({ id: 'bg-child', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'bg-agent', summary: 'Bash: bg' }),
      // Mutual cycle: each names the other as parent.
      mkItem({ id: 'cycle-x', itemIndex: 2, kind: 'tool_call', toolName: 'Bash', parentId: 'cycle-y', summary: 'Bash: x' }),
      mkItem({ id: 'cycle-y', itemIndex: 3, kind: 'tool_call', toolName: 'Bash', parentId: 'cycle-x', summary: 'Bash: y' }),
      // Self-referential parentId.
      mkItem({ id: 'self-ref', itemIndex: 4, kind: 'tool_call', toolName: 'Bash', parentId: 'self-ref', summary: 'Bash: self' }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 5,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: bg-agent -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'bg-agent',
      'cycle-x',
      'cycle-y',
      'self-ref',
      'complete:bg-agent',
    ]);
    expect(expectGroup(nodes[4]).children.map((c) => expectLeaf(c).item.id)).toEqual([
      'bg-child',
    ]);
    // Cycle members are not orphan-flagged: their declared parents exist.
    for (const node of nodes.slice(1, 4)) {
      expect(expectLeaf(node).orphan).toBeUndefined();
    }
  });

  it('recognises a Codex spawn when its metadata lives in meta: a leaf whose children stay withheld', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        payloadMeta: toolMeta({ lineCount: 1 }),
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      }),
      mkItem({
        id: 'child-answer',
        itemIndex: 1,
        kind: 'assistant_text',
        parentId: 'codex-agent',
        summary: '0',
      }),
    ]);

    // Recognition is what withholds the child: an unrecognised parent
    // would leave `child-answer` a flat top-level leaf.
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['codex-agent']);
  });

  it('withholds a Codex child prompt echo row from the main timeline', () => {
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

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['codex-agent']);
  });

  it('withholds the whole Codex child transcript from the main timeline', () => {
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

    // The spawn row is the pre-card `launched` leaf (user ruling
    // 2026-08-23); what the child parents to it renders in the agent pane,
    // never on the main timeline.
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['codex-agent']);
  });

  it('withholds a transitive descendant subtree under a Codex spawn', () => {
    // A grandchild whose parent is not itself the spawn anchor resolves to
    // the spawn all the same, and never reaches the top level.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'codex-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'collab_agent',
        isBackground: true,
        summary: 'collab_agent: review',
        meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
      }),
      mkItem({
        id: 'child-tool',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'command_execution',
        parentId: 'codex-agent',
        summary: 'Bash: sleep 20',
      }),
      mkItem({
        id: 'grandchild-tool',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Read',
        status: 'running',
        parentId: 'child-tool',
        summary: 'Read: nested.ts',
      }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['codex-agent']);
    // A leaf claims only itself: a jump to a withheld row goes through
    // `visibleTimelineItemIdForItem`, which maps it to the spawn row.
    expect(nodeContainsItem(nodes[0], 'child-tool')).toBe(false);
    expect(nodeContainsItem(nodes[0], 'grandchild-tool')).toBe(false);
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
    // The standalone wait_agent completion folds in as the header, and counts
    // as contained so a search hit on its id resolves to this wait_group row
    // (findTimelineNodeIndex → its index) instead of a silent scroll no-op.
    expect(group.completion?.id).toBe('complete-wait-1');
    expect(nodeContainsItem(group, 'complete-wait-1')).toBe(true);
    expect(findTimelineNodeIndex(nodes, 'complete-wait-1')).toBe(0);
    expect(nodeContainsItem(group, 'complete-spawn-1')).toBe(true);
  });

  it(
    'folds the wait_agent completion into the carrier so there is no standalone "Finished waiting" leaf or top-level key churn',
    () => {
      // Regression guard for the "Finished waiting" flash → snap the user reported.
      //
      // On a Codex wait completion, Go emits THREE separate provider:item_event
      // upserts, in order (internal/triage codex_background.go resolveSubagentsForWait,
      // tool_lifecycle.go persistSplitToolCompletion):
      //   (a) the wait_agent tool_call CARRIER, now status=completed
      //   (b) a standalone wait_agent tool_completion (id complete:<waitId>,
      //       completionOf=<waitId>) — NO wait_carrier_id
      //   (c) one collab_agent tool_completion per spawned agent, linked to the
      //       carrier (a) by shared wait payload or wait_carrier_id
      // (b) is written/emitted BEFORE (c). Today there is a real frame where the
      // store holds only [a, b]: (b) has no linked children, so the drop rule
      // (groupItemsBySubagent: drop a wait_agent completion only once its carrier
      // HAS children) does not fire, and (b) renders as a TOP-LEVEL leaf —
      // the briefly-visible "Finished waiting" row. When (c) lands, (b) is
      // dropped and (c) nests, so the top-level node set loses (b)'s key. Virtua
      // keys top-level rows by timelineNodeKey (MessageTimeline getKey), so that
      // identity delta drops a row — the visible snap.
      //
      // Note (b) can also be the ONLY completion evidence: a re-wait on
      // already-resolved agents, a partial wait, or an untracked launch yields
      // (b) with terminal agentsStates but ZERO (c). So the fix is to FOLD (b)
      // into the wait_group as its completion (and render its status), not to
      // drop it — gated on the carrier (a) being present so a page boundary that
      // loads (b) without (a) still renders something.
      //
      // Invariant: in the [a, b] frame (b) is folded into the wait_group as its
      // `completion` (NOT a top-level leaf), and the top-level key SET is
      // identical to the settled [a, b, c] frame — no row appears-then-vanishes,
      // so the virtualizer never drops/remounts a top-level row and there is no flash.
      const carrier = mkItem({
        id: 'wait-1', itemIndex: 0, kind: 'tool_call',
        toolName: 'wait_agent', status: 'completed', meta: codexWaitAgentMeta(),
      });
      const waitCompletion = mkItem({
        id: 'complete-wait-1', itemIndex: 1, kind: 'tool_completion',
        toolName: 'wait_agent', completionOf: 'wait-1', summary: 'wait_agent -> done',
        payloadId: 'payload-final', payloadKind: 'tool_call_result',
      });
      const childCompletion = mkItem({
        id: 'complete-spawn-1', itemIndex: 2, kind: 'tool_completion',
        toolName: 'collab_agent', completionOf: 'spawn-1',
        payloadId: 'payload-final', payloadKind: 'tool_call_result',
      });

      // Transient frame: only carrier (a) + standalone completion (b) loaded.
      const transient = groupItemsBySubagent([carrier, waitCompletion]);
      // Exactly one top-level node: the wait carrier, which OWNS (b).
      expect(transient).toHaveLength(1);
      const transientGroup = expectWaitGroup(transient[0]);
      expect(transientGroup.parent.id).toBe('wait-1');
      // (b) is folded in, never a free-standing top-level leaf…
      expect(transient.some((n) => n.kind === 'leaf' && n.item.id === 'complete-wait-1'))
        .toBe(false);
      // …it is the group's completion (the rendered "Finished waiting" header).
      expect(transientGroup.completion?.id).toBe('complete-wait-1');

      // Settled frame: child completion (c) arrives and links to the carrier.
      const settled = groupItemsBySubagent([carrier, waitCompletion, childCompletion]);
      expect(settled).toHaveLength(1);
      const settledGroup = expectWaitGroup(settled[0]);
      expect(settledGroup.children.map((n) => expectLeaf(n).item.id))
        .toEqual(['complete-spawn-1']);
      // (b) stays folded as the header even after (c) nests as a child.
      expect(settledGroup.completion?.id).toBe('complete-wait-1');

      // The anti-flash invariant: the top-level key SET is unchanged between the
      // [a, b] and [a, b, c] frames — no key is inserted then removed, so the
      // virtualizer never drops/remounts a top-level row.
      const transientKeys = new Set(transient.map(timelineNodeKey));
      const settledKeys = new Set(settled.map(timelineNodeKey));
      expect(settledKeys).toEqual(transientKeys);
    },
  );

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

  it('projects a persisted Codex spawn/wait sequence: spawn leaf, and the spawn card under the wait group', () => {
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
    // The spawn's own completion carries `wait_carrier_id`, so the WAIT group
    // claims it — a wait link always wins, or a finished wait would render
    // with nothing under it. That completion is where the spawn's CARD
    // renders (`SubagentGroupNode.anchor`), child transcript inside.
    const waitGroup = expectWaitGroup(nodes[1]);
    expect(waitGroup.parent.id).toBe('wait-review');
    expect(waitGroup.children).toHaveLength(1);
    const spawnCard = expectGroup(waitGroup.children[0]);
    expect(spawnCard.parent.id).toBe('spawn-review');
    expect(spawnCard.anchor.id).toBe('complete-spawn-review');
    expect(spawnCard.completion?.id).toBe('complete-spawn-review');
    expect(spawnCard.children.map((node) => expectLeaf(node).item.id)).toEqual([
      'child-prompt',
      'child-progress',
    ]);
    // Folded completion resolves to this wait_group row (index 1) for
    // search-scroll, not -1.
    expect(nodeContainsItem(waitGroup, 'complete-wait-review')).toBe(true);
    expect(findTimelineNodeIndex(nodes, 'complete-wait-review')).toBe(1);
    expect(expectLeaf(nodes[2]).item.id).toBe('assistant-after-review');
    // The child transcript lives in the card under the wait group (index 1).
    expect(findTimelineNodeIndex(nodes, 'child-prompt')).toBe(1);
    expect(findTimelineNodeIndex(nodes, 'child-progress')).toBe(1);
    expect(findTimelineNodeIndex(nodes, 'complete-spawn-review')).toBe(1);
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

  it('keeps mailbox-delivered agent completions flat after the waited row', () => {
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
      mkItem({
        id: 'complete-spawn-1',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-1',
      }),
    ]);

    expect(nodes).toHaveLength(2);
    const waited = expectWaitGroup(nodes[0]);
    expect(waited.parent.id).toBe('wait-child');
    expect(waited.completion?.id).toBe('complete-wait-child');
    expect(waited.children).toHaveLength(0);
    expect(expectLeaf(nodes[1]).item.id).toBe('complete-spawn-1');
  });

  it('folds a childless timeout wait completion into the wait group as the header', () => {
    // The no-children case (timeout / re-wait / partial / untracked launch):
    // carrier (a) + completion (b), no collab_agent children. (b) is the ONLY
    // status record, so it must surface — but as the group's folded header (the
    // rendered "Finished waiting"), NOT as a separate top-level leaf trailing a
    // still-"Waiting" carrier (the two-row stale display this change removes).
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
    expect(group.children).toEqual([]);
    expect(group.descendantCount).toBe(0);
    // (b) is folded as the header completion, carrying the "stays visible" intent.
    expect(group.completion?.id).toBe('complete-wait-child');
    // It is not a child (descendantCount stays 0). The anchor id stays the
    // carrier, but the folded completion still counts as contained so a search
    // hit on its id resolves to this row instead of a silent scroll no-op —
    // important here because (b) is the wait's only status record.
    expect(nodeContainsItem(group, 'complete-wait-child')).toBe(true);
    expect(findTimelineNodeIndex(nodes, 'complete-wait-child')).toBe(0);
  });

  it('folds each wait completion into its own wait group when several waits coexist', () => {
    // Two independent Codex waits in one window. The per-carrier maps must keep
    // them isolated: each wait_group folds only its own (b) and nests only its
    // own (c), linked by the shared wait payload — no cross-linking by carrier.
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'wait-a', itemIndex: 0, kind: 'tool_call', toolName: 'wait_agent', meta: codexWaitAgentMeta() }),
      mkItem({
        id: 'complete-wait-a',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-a',
        payloadId: 'payload-a',
        payloadKind: 'tool_call_result',
      }),
      mkItem({
        id: 'complete-spawn-a',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-a',
        payloadId: 'payload-a',
        payloadKind: 'tool_call_result',
      }),
      mkItem({ id: 'wait-b', itemIndex: 3, kind: 'tool_call', toolName: 'wait_agent', meta: codexWaitAgentMeta() }),
      mkItem({
        id: 'complete-wait-b',
        itemIndex: 4,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-b',
        payloadId: 'payload-b',
        payloadKind: 'tool_call_result',
      }),
      mkItem({
        id: 'complete-spawn-b',
        itemIndex: 5,
        kind: 'tool_completion',
        toolName: 'collab_agent',
        completionOf: 'spawn-b',
        payloadId: 'payload-b',
        payloadKind: 'tool_call_result',
      }),
    ]);

    expect(nodes).toHaveLength(2);
    const groupA = expectWaitGroup(nodes[0]);
    const groupB = expectWaitGroup(nodes[1]);
    expect(groupA.parent.id).toBe('wait-a');
    expect(groupA.completion?.id).toBe('complete-wait-a');
    expect(groupA.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-spawn-a']);
    expect(groupB.parent.id).toBe('wait-b');
    expect(groupB.completion?.id).toBe('complete-wait-b');
    expect(groupB.children.map((node) => expectLeaf(node).item.id)).toEqual(['complete-spawn-b']);
    // No cross-linking: each completion resolves only to its own group.
    expect(findTimelineNodeIndex(nodes, 'complete-wait-a')).toBe(0);
    expect(findTimelineNodeIndex(nodes, 'complete-wait-b')).toBe(1);
    expect(nodeContainsItem(groupA, 'complete-wait-b')).toBe(false);
    expect(nodeContainsItem(groupB, 'complete-wait-a')).toBe(false);
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

  it('keeps a wait_agent completion as a top-level leaf when its carrier is not loaded', () => {
    // Page-boundary safety branch: the drop/fold is gated on the carrier being
    // loaded. If only the completion is in the window (the carrier paged out
    // above), it must stay a top-level leaf so the finished wait still renders
    // something rather than vanishing — the fold has nowhere to land.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'complete-wait-orphan',
        itemIndex: 5,
        kind: 'tool_completion',
        toolName: 'wait_agent',
        completionOf: 'wait-paged-out',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    expect(expectLeaf(nodes[0]).item.id).toBe('complete-wait-orphan');
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

// Every shape `subagentLaunchInfo` recognises anchors a card, and the tree
// is the same shape whichever one it is. Predicate-level coverage (which
// signals prove a fork, what makes a SendMessage a carrier) lives in
// `subagentLaunch.test.ts`; this block is about the TREE those answers build.
describe('groupItemsBySubagent — launch kinds', () => {
  function forkedSkill(
    id: string,
    itemIndex: number,
    overrides: Partial<Item> = {},
    name = 'code-review',
  ): Item {
    return mkItem({
      id,
      itemIndex,
      kind: 'tool_call',
      toolName: 'Skill',
      summary: `Skill: ${name}`,
      meta: toolMeta({
        toolName: 'Skill',
        input: { skill: name },
        skillFork: { agentId: `agent-${id}`, commandName: name },
      }),
      ...overrides,
    });
  }

  it('renders a forked Skill as a group with its attributed rows inside', () => {
    const nodes = groupItemsBySubagent([
      forkedSkill('skill-1', 0),
      mkItem({
        id: 'fork-tool',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Read',
        parentId: 'skill-1',
        summary: 'Read: thing.ts',
      }),
      mkItem({
        id: 'fork-text',
        itemIndex: 2,
        kind: 'assistant_text',
        parentId: 'skill-1',
        summary: 'three findings',
      }),
      mkItem({ id: 'main-text', itemIndex: 3, kind: 'assistant_text', summary: 'thanks' }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['skill-1', 'main-text']);
    const group = expectGroup(nodes[0]);
    expect(group.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'fork-tool',
      'fork-text',
    ]);
    expect(group.descendantCount).toBe(2);
    expect(group.latestChildSummary).toBe('Read: thing.ts');
  });

  it('detects a fork from an attributed row alone, with no meta stamp', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'skill-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Skill',
        summary: 'Skill: brainstorm',
        meta: toolMeta({ toolName: 'Skill', input: { skill: 'brainstorm' } }),
      }),
      mkItem({
        id: 'fork-text',
        itemIndex: 1,
        kind: 'assistant_text',
        parentId: 'skill-1',
        summary: 'option A',
      }),
    ]);

    expect(nodes).toHaveLength(1);
    expect(expectGroup(nodes[0]).children.map((c) => expectLeaf(c).item.id)).toEqual([
      'fork-text',
    ]);
  });

  it('keeps an INLINE skill a plain leaf', () => {
    // No attributed row, no fork stamp, no descendant decoration: the skill
    // ran in the main context and is an ordinary tool row.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'skill-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Skill',
        status: 'completed',
        summary: 'Skill: init',
        meta: toolMeta({ toolName: 'Skill', input: { skill: 'init' } }),
      }),
      mkItem({ id: 'after', itemIndex: 1, kind: 'assistant_text', summary: 'done' }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['skill-1', 'after']);
  });

  it('renders a SendMessage resume carrier as a group and folds its round-2 completion', () => {
    // claude-wire.md §E6: round 2 of a resumed async agent is carried by the
    // resuming tool_use, and writes its own `complete:<carrierID>` sibling.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'toolu_resume',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'SendMessage',
        isBackground: true,
        status: 'running',
        summary: 'Agent: Frontend transitive suppression fix',
        meta: toolMeta({ task_id: 'a464e54e96a45cd0c', description: 'Frontend fix' }),
      }),
      mkItem({
        id: 'round2-tool',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Bash',
        parentId: 'toolu_resume',
        summary: 'Bash: pnpm test',
      }),
      mkItem({
        id: 'complete:toolu_resume',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'SendMessage',
        isBackground: true,
        completionOf: 'toolu_resume',
        summary: 'Agent: Frontend transitive suppression fix -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'toolu_resume',
      'complete:toolu_resume',
    ]);
    // A resume carrier is a detached Claude launch: spawn leaf, card at
    // its round-2 completion.
    expectLeaf(nodes[0]);
    const group = expectGroup(nodes[1]);
    expect(group.parent.id).toBe('toolu_resume');
    expect(group.children.map((child) => expectLeaf(child).item.id)).toEqual(['round2-tool']);
    expect(group.completion?.id).toBe('complete:toolu_resume');
  });

  it('keeps an ordinary SendMessage a leaf and does not adopt rows under it', () => {
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'send-1',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'SendMessage',
        status: 'completed',
        summary: 'SendMessage: ping',
        meta: toolMeta({ toolName: 'SendMessage' }),
      }),
      mkItem({ id: 'after', itemIndex: 1, kind: 'assistant_text', summary: 'ok' }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['send-1', 'after']);
  });

  it('nests three launch kinds three deep under a background parent', () => {
    // depth 0: backgrounded Agent, depth 1: forked Skill it ran, depth 2: the
    // Agent that fork spawned (claude-wire.md §E9 — "agents a fork spawns are
    // ordinary"), depth 3: that agent's own tool row, at the cap as a leaf.
    const nodes = groupItemsBySubagent([
      mkItem({
        id: 'bg-agent',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        summary: 'Agent: review wave',
        meta: toolMeta({ toolName: 'Agent', input: { subagent_type: 'general-purpose' } }),
      }),
      forkedSkill('skill-1', 1, { parentId: 'bg-agent' }),
      mkItem({
        id: 'depth2-agent',
        itemIndex: 2,
        kind: 'tool_call',
        toolName: 'Agent',
        parentId: 'skill-1',
        summary: 'Agent: angle B',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'depth3-read',
        itemIndex: 3,
        kind: 'tool_call',
        toolName: 'Read',
        parentId: 'depth2-agent',
        summary: 'Read: angle-b.ts',
      }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 4,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: bg-agent -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['bg-agent', 'complete:bg-agent']);
    const outer = expectGroup(nodes[1]);
    const skill = expectGroup(outer.children[0]);
    expect(skill.parent.id).toBe('skill-1');
    const inner = expectGroup(skill.children[0]);
    expect(inner.parent.id).toBe('depth2-agent');
    expect(inner.children.map((child) => expectLeaf(child).item.id)).toEqual(['depth3-read']);
    // Counts roll all the way up through both nested cards.
    expect(inner.descendantCount).toBe(1);
    expect(skill.descendantCount).toBe(2);
    expect(outer.descendantCount).toBe(3);
  });

  // Awaited launches: the depth cap is about nesting, and an awaited card
  // sits at its launch whatever its depth. (A detached Claude launch is a
  // spawn leaf until its completion loads — a different contract, pinned
  // above.)
  function launchChain(depthCount: number, extra: readonly Item[] = []): Item[] {
    const items: Item[] = [];
    for (let depth = 0; depth <= depthCount; depth++) {
      items.push(mkItem({
        id: `l${depth}`,
        itemIndex: depth,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: false,
        ...(depth > 0 ? { parentId: `l${depth - 1}` } : {}),
        meta: toolMeta({ toolName: 'Agent' }),
      }));
    }
    return [...items, ...extra];
  }

  it('flattens the subtree of the launch that sits AT the depth cap', () => {
    const nodes = groupItemsBySubagent(launchChain(5));

    const l1 = expectGroup(expectGroup(nodes[0]).children[0]);
    const l2 = expectGroup(l1.children[0]);
    const l3 = expectGroup(l2.children[0]);
    // l3 is at MAX_DEPTH: it still gets a card, but everything below it
    // flattens into leaf siblings instead of opening further cards.
    expect(MAX_DEPTH).toBe(3);
    expect(l3.children.map((child) => expectLeaf(child).item.id)).toEqual(['l4', 'l5']);
    expect(l3.descendantCount).toBe(2);
  });

  it('keeps a flattened launch completion sibling as a leaf beside it', () => {
    // Below the cap a nested launch renders as a LEAF, so there is no card
    // to build at its completion. The sibling carries the launch's parentId
    // (Go writes it with `ParentID: launch.ParentID`), so it sits in the
    // same bucket and flattens through `enqueue` like any other row — no
    // re-emit path, nothing to forget.
    const nodes = groupItemsBySubagent(launchChain(4, [
      mkItem({
        id: 'complete:l4',
        itemIndex: 5,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'l4',
        parentId: 'l3',
        summary: 'Agent: deep -> done',
      }),
    ]));

    expect(nodes).toHaveLength(1);
    const l3 = expectGroup(
      expectGroup(expectGroup(expectGroup(nodes[0]).children[0]).children[0]).children[0],
    );
    expect(l3.children.map((child) => expectLeaf(child).item.id)).toEqual([
      'l4',
      'complete:l4',
    ]);
    expect(findTimelineNodeIndex(nodes, 'complete:l4')).toBe(0);
  });

  it('keeps a completion sibling as a top-level leaf when its launch is outside the window', () => {
    // Page boundary: the launch scrolled out of the loaded window, so nothing
    // can fold the sibling. It must still render rather than disappear —
    // the same trade `wait_group` makes with an unloaded wait carrier.
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'main-text', itemIndex: 0, kind: 'assistant_text', summary: 'hi' }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: investigate -> done',
      }),
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'main-text',
      'complete:bg-agent',
    ]);
    expect(findTimelineNodeIndex(nodes, 'complete:bg-agent')).toBe(1);
  });

  it('resolves the completion to the card at the completion point, and the launch to its spawn leaf', () => {
    const nodes = groupItemsBySubagent([
      mkItem({ id: 'lead-text', itemIndex: 0, kind: 'assistant_text', summary: 'starting' }),
      mkItem({
        id: 'bg-agent',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        meta: toolMeta({ toolName: 'Agent' }),
      }),
      mkItem({
        id: 'complete:bg-agent',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Agent',
        isBackground: true,
        completionOf: 'bg-agent',
        summary: 'Agent: investigate -> done',
      }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'lead-text',
      'bg-agent',
      'complete:bg-agent',
    ]);
    expectLeaf(nodes[1]);
    const group = expectGroup(nodes[2]);
    expect(group.completion?.id).toBe('complete:bg-agent');
    // The card IS the completion point: a search hit on the completion's
    // own id (Go indexes its summary) lands on the card. The launch id is
    // the spawn leaf's — a scroll anchor taken on the spawn row must come
    // back to the spawn row, not to a card turns later.
    expect(nodeContainsItem(group, 'complete:bg-agent')).toBe(true);
    expect(nodeContainsItem(group, 'bg-agent')).toBe(false);
    expect(findTimelineNodeIndex(nodes, 'complete:bg-agent')).toBe(2);
    expect(findTimelineNodeIndex(nodes, 'bg-agent')).toBe(1);
    expect(timelineNodeItemId(group)).toBe('complete:bg-agent');
  });
});

describe('visibleTimelineItemIdForItem', () => {
  const claudeAgent = mkItem({
    id: 'agent-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Agent',
    isBackground: true,
    meta: toolMeta({ toolName: 'Agent' }),
  });
  const forkedSkill = mkItem({
    id: 'skill-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'Skill',
    meta: toolMeta({ input: { skill: 'code-review' }, skillFork: { commandName: 'code-review' } }),
  });
  const codexSpawn = mkItem({
    id: 'spawn-1',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'collab_agent',
    meta: toolMeta({ toolName: 'collab_agent', input: { tool: 'spawn_agent' } }),
  });
  const resumeCarrier = mkItem({
    id: 'toolu_resume',
    itemIndex: 0,
    kind: 'tool_call',
    toolName: 'SendMessage',
    isBackground: true,
    meta: toolMeta({ task_id: 'a1' }),
  });

  it.each([
    ['a backgrounded Claude agent', claudeAgent],
    ['a forked skill', forkedSkill],
    ['a Codex spawn', codexSpawn],
    ['a SendMessage resume carrier', resumeCarrier],
  ])('walks a child up to %s', (_label, launch) => {
    const items = [
      launch,
      mkItem({ id: 'child', itemIndex: 1, kind: 'assistant_text', parentId: launch.id }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'child')).toBe(launch.id);
    expect(visibleTimelineItemIdForItem(items, launch.id)).toBe(launch.id);
  });

  it('walks all the way to the OUTERMOST launch, not the immediate one', () => {
    const items = [
      claudeAgent,
      mkItem({
        id: 'skill-1',
        itemIndex: 1,
        kind: 'tool_call',
        toolName: 'Skill',
        parentId: 'agent-1',
        meta: toolMeta({ input: { skill: 'code-review' }, skillFork: { commandName: 'x' } }),
      }),
      mkItem({ id: 'deep', itemIndex: 2, kind: 'assistant_text', parentId: 'skill-1' }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'deep')).toBe('agent-1');
    expect(visibleTimelineItemIdForItem(items, 'skill-1')).toBe('agent-1');
  });

  it('resolves a folded completion sibling to the launch that renders it', () => {
    const items = [
      claudeAgent,
      mkItem({
        id: 'complete:agent-1',
        itemIndex: 1,
        kind: 'tool_completion',
        toolName: 'Agent',
        completionOf: 'agent-1',
      }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'complete:agent-1')).toBe('agent-1');
  });

  it('leaves a row alone when nothing above it is a launch', () => {
    const items = [
      mkItem({ id: 'bash', itemIndex: 0, kind: 'tool_call', toolName: 'Bash' }),
      mkItem({ id: 'under-bash', itemIndex: 1, kind: 'assistant_text', parentId: 'bash' }),
      mkItem({
        id: 'complete:bash',
        itemIndex: 2,
        kind: 'tool_completion',
        toolName: 'Bash',
        completionOf: 'bash',
      }),
      mkItem({ id: 'unknown-parent', itemIndex: 3, parentId: 'gone' }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'under-bash')).toBe('under-bash');
    expect(visibleTimelineItemIdForItem(items, 'complete:bash')).toBe('complete:bash');
    expect(visibleTimelineItemIdForItem(items, 'unknown-parent')).toBe('unknown-parent');
    expect(visibleTimelineItemIdForItem(items, 'missing')).toBe('missing');
  });

  it('terminates on a parentId cycle', () => {
    const items = [
      mkItem({ id: 'x', itemIndex: 0, kind: 'tool_call', toolName: 'Bash', parentId: 'y' }),
      mkItem({ id: 'y', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'x' }),
    ];

    expect(visibleTimelineItemIdForItem(items, 'x')).toBe('x');
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

describe('live fold aggregates (evicted subagent children)', () => {
  // The pane evicts settled subagent child rows from memory and tracks
  // them per launch anchor in a fold registry (utils/subagentFold.ts).
  // The grouping pipeline receives the registry as a lookup so collapsed
  // cards keep honest counts and previews for rows that are not loaded.
  function foldLookup(
    byAnchor: Record<string, SubagentFoldAggregate>,
  ): SubagentLiveAggregates {
    return (anchorId) => byAnchor[anchorId];
  }

  it('adds evicted rows to the count and uses the fold preview when nothing is loaded', () => {
    const nodes = groupItemsBySubagent(
      [agentLaunch('agent-1', 0)],
      foldLookup({
        'agent-1': {
          evictedCount: 3,
          terminalPreview: 'evicted preview',
          terminalTurnIndex: 0,
          terminalItemIndex: 4,
        },
      }),
    );

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(3);
    expect(group.loadedDescendantCount).toBe(0);
    expect(group.latestChildSummary).toBe('evicted preview');
  });

  it('composes loaded children with the fold and resolves the preview by position', () => {
    const fold = foldLookup({
      'agent-1': {
        evictedCount: 2,
        terminalPreview: 'evicted at 5',
        terminalTurnIndex: 0,
        terminalItemIndex: 5,
      },
    });

    // Loaded terminal earlier than the fold → fold preview wins.
    const foldWins = expectGroup(groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      mkItem({ id: 'c1', itemIndex: 2, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'loaded at 2' }),
    ], fold)[0]);
    expect(foldWins.descendantCount).toBe(3);
    expect(foldWins.loadedDescendantCount).toBe(1);
    expect(foldWins.latestChildSummary).toBe('evicted at 5');

    // Loaded terminal later than the fold → loaded preview wins.
    const loadedWins = expectGroup(groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      mkItem({ id: 'c2', itemIndex: 9, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'loaded at 9' }),
    ], fold)[0]);
    expect(loadedWins.latestChildSummary).toBe('loaded at 9');
  });

  it('always prefers an active loaded child over the fold preview', () => {
    const nodes = groupItemsBySubagent(
      [
        agentLaunch('agent-1', 0),
        mkItem({
          id: 'c1',
          itemIndex: 1,
          parentId: 'agent-1',
          kind: 'tool_call',
          toolName: 'Bash',
          status: 'streaming',
          summary: 'streaming now',
        }),
      ],
      foldLookup({
        'agent-1': {
          // Evicted rows are terminal by definition — even a later
          // position must not outrank live work.
          evictedCount: 1,
          terminalPreview: 'evicted later',
          terminalTurnIndex: 5,
          terminalItemIndex: 0,
        },
      }),
    );

    expect(expectGroup(nodes[0]).latestChildSummary).toBe('streaming now');
  });

  it('keeps the decorated count as the ratchet floor over the live total', () => {
    const nodes = groupItemsBySubagent(
      [
        mkItem({
          id: 'agent-1',
          itemIndex: 0,
          kind: 'tool_call',
          toolName: 'Agent',
          meta: toolMeta({ subagentDescendantCount: 10 }),
        }),
        mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', summary: 'loaded' }),
      ],
      foldLookup({
        'agent-1': {
          evictedCount: 2,
          terminalPreview: '',
          terminalTurnIndex: 0,
          terminalItemIndex: 2,
        },
      }),
    );

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(10);
    expect(group.loadedDescendantCount).toBe(1);
  });

  it('composes a nested launch fold into the outer count via the nested descendantCount', () => {
    const nodes = groupItemsBySubagent(
      [
        agentLaunch('root', 0),
        mkItem({
          id: 'nested',
          itemIndex: 1,
          parentId: 'root',
          kind: 'tool_call',
          toolName: 'Task',
          summary: 'Task: inner',
        }),
        mkItem({ id: 'c1', itemIndex: 2, parentId: 'nested', summary: 'inner work' }),
      ],
      foldLookup({
        nested: {
          evictedCount: 3,
          terminalPreview: 'evicted last',
          terminalTurnIndex: 0,
          terminalItemIndex: 5,
        },
      }),
    );

    const root = expectGroup(nodes[0]);
    const nested = expectGroup(root.children[0]);
    expect(nested.descendantCount).toBe(4);
    expect(nested.loadedDescendantCount).toBe(1);
    expect(nested.latestChildSummary).toBe('evicted last');
    // Outer total = nested anchor itself + nested's composed total.
    expect(root.descendantCount).toBe(5);
  });

  it('counts folds of depth-cap flattened launches without inflating loaded rows', () => {
    function nestedLaunch(id: string, itemIndex: number, parentId: string): Item {
      return mkItem({
        id,
        itemIndex,
        parentId,
        kind: 'tool_call',
        toolName: 'Task',
        summary: `Task: ${id}`,
      });
    }
    const nodes = groupItemsBySubagent(
      [
        agentLaunch('root', 0),
        nestedLaunch('a', 1, 'root'),
        nestedLaunch('b', 2, 'a'),
        nestedLaunch('c', 3, 'b'),
        mkItem({ id: 'd', itemIndex: 4, parentId: 'c', summary: 'flattened row' }),
        nestedLaunch('f', 5, 'c'),
      ],
      foldLookup({
        f: {
          evictedCount: 2,
          terminalPreview: 'f evicted',
          terminalTurnIndex: 0,
          terminalItemIndex: 7,
        },
      }),
    );

    const root = expectGroup(nodes[0]);
    const a = expectGroup(root.children[0]);
    const b = expectGroup(a.children[0]);
    // `c` sits at MAX_DEPTH: its subtree renders as flat leaves, so the
    // launch `f` inside it is a leaf, not a fold-aware group node. Its
    // evicted rows still count toward `c`'s total — but NOT toward
    // loadedDescendantCount, or the card would never hydrate on expand.
    const c = expectGroup(b.children[0]);
    expect(c.children.every((child) => child.kind === 'leaf')).toBe(true);
    expect(c.loadedDescendantCount).toBe(2);
    expect(c.descendantCount).toBe(4);
    expect(b.descendantCount).toBe(5);
    expect(root.descendantCount).toBe(7);
  });
});

describe('subagent anchor decoration fallback', () => {
  // History windows load launch anchors without their child rows; the
  // store stamps `subagentDescendantCount` / `subagentLatestChildSummary`
  // on the anchor's meta (internal/store/subagent_items.go) so the
  // collapsed card renders identically before the transcript hydrates.
  function decoratedLaunch(
    id: string,
    itemIndex: number,
    decoration: Record<string, unknown>,
  ): Item {
    return mkItem({
      id,
      itemIndex,
      kind: 'tool_call',
      toolName: 'Agent',
      summary: 'Agent: investigate',
      meta: toolMeta({
        toolName: 'Agent',
        input: { description: 'investigate' },
        ...decoration,
      }),
    });
  }

  it('renders collapsed aggregates from anchor meta when no child rows are loaded', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, {
        subagentDescendantCount: 7,
        subagentLatestChildSummary: '  running\n   go test ./...  ',
      }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.children).toHaveLength(0);
    expect(group.descendantCount).toBe(7);
    expect(group.loadedDescendantCount).toBe(0);
    // Decorated summaries normalize exactly like loaded-child previews.
    expect(group.latestChildSummary).toBe('running go test ./...');
  });

  it('prefers loaded children for the preview and keeps the count monotonic', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, {
        subagentDescendantCount: 2,
        subagentLatestChildSummary: 'stale decorated preview',
      }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'first step' }),
      mkItem({ id: 'c2', itemIndex: 2, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'second step' }),
      mkItem({ id: 'c3', itemIndex: 3, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'third step' }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(3);
    expect(group.loadedDescendantCount).toBe(3);
    expect(group.latestChildSummary).toBe('third step');
  });

  it('keeps prose and thinking out of the collapsed activity preview', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, { subagentDescendantCount: 3 }),
      mkItem({
        id: 'read',
        itemIndex: 1,
        parentId: 'agent-1',
        kind: 'tool_call',
        toolName: 'Read',
        summary: 'Read parse_system.go',
      }),
      mkItem({
        id: 'thinking',
        itemIndex: 2,
        parentId: 'agent-1',
        kind: 'thinking',
        summary: 'I should inspect one more path',
      }),
      mkItem({
        id: 'prose',
        itemIndex: 3,
        parentId: 'agent-1',
        kind: 'assistant_text',
        summary: 'The review is complete',
      }),
    ]);

    expect(expectGroup(nodes[0]).latestChildSummary).toBe('Read parse_system.go');
  });

  it('keeps the decorated count when loaded children trail it', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, { subagentDescendantCount: 5 }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'only loaded row' }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(5);
    expect(group.loadedDescendantCount).toBe(1);
    expect(group.latestChildSummary).toBe('only loaded row');
  });

  it('falls back to the decorated preview when loaded children carry no text', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, {
        subagentDescendantCount: 1,
        subagentLatestChildSummary: 'decorated preview',
      }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', summary: '' }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.latestChildSummary).toBe('decorated preview');
  });

  // The candidate walk decides "does this row contribute text?" without
  // building the preview, and normalizes only the winner. These two pin
  // the halves of that split: the gate must reject exactly what
  // normalization would have emptied, and the winner must still come out
  // normalized rather than raw.
  it('skips a whitespace-only child summary as a preview candidate', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, { subagentDescendantCount: 2 }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: 'real text' }),
      mkItem({ id: 'c2', itemIndex: 2, parentId: 'agent-1', kind: 'tool_call', toolName: 'Bash', summary: '  \n\t ' }),
    ]);

    // c2 is the later row, so a gate that let it through would win and
    // render an empty preview.
    expect(expectGroup(nodes[0]).latestChildSummary).toBe('real text');
  });

  // Both winner branches, because they are separate returns: an active
  // descendant and a terminal one must come out of the walk equally
  // normalized.
  for (const status of ['completed', 'streaming'] as const) {
    it(`normalizes the winning ${status} child summary, not only the decoration`, () => {
      const nodes = groupItemsBySubagent([
        decoratedLaunch('agent-1', 0, { subagentDescendantCount: 1 }),
        mkItem({
          id: 'c1',
          itemIndex: 1,
          parentId: 'agent-1',
          kind: 'tool_call',
          toolName: 'Bash',
          status,
          summary: `\n  ran\n\n  tests ${'x'.repeat(400)}`,
        }),
      ]);

      const summary = expectGroup(nodes[0]).latestChildSummary;
      expect(summary.startsWith('ran tests ')).toBe(true);
      expect(summary.endsWith('...')).toBe(true);
      expect(summary.length).toBeLessThanOrEqual(163);
    });
  }

  it('truncates an oversized decorated preview like child previews', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, {
        subagentDescendantCount: 1,
        subagentLatestChildSummary: 'x'.repeat(400),
      }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.latestChildSummary.endsWith('...')).toBe(true);
    expect(group.latestChildSummary.length).toBeLessThanOrEqual(163);
  });

  it('ignores malformed decorated counts', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, {
        subagentDescendantCount: -3,
        subagentLatestChildSummary: 42,
      }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(0);
    expect(group.loadedDescendantCount).toBe(0);
    expect(group.latestChildSummary).toBe('');
  });
});

// ── Corrupt parent links at the depth cap ─────────────────────────────────
//
// `childrenByParent` is built from provider-supplied `parentId`s and keyed by
// item id, neither of which this pass owns. The depth-cap flatten walks it as
// a BFS, and written as "keep going until you run out" that is, for corrupt
// links, not a wrong render but a synchronous loop that never returns: one
// core pegged, no paint, no error, nothing in any log.
//
// Diagnostics are asserted against the real capture pipeline (dedupe ->
// serialize -> batch -> RPC), so the claim under test is that the report
// reaches `ui-trace/frontend-errors.jsonl`, not that a spy was called.

describe('subagentGrouping depth-cap flattening survives corrupt parent links', () => {
  const diagnostics = installDiagnosticsCapture();

  function launch(id: string, itemIndex: number, parentId?: string): Item {
    return mkItem({ id, itemIndex, parentId, kind: 'tool_call', toolName: 'Task', summary: id });
  }

  /** A launch chain exactly deep enough that the last group flattens its subtree. */
  function chainToCap(): Item[] {
    expect(MAX_DEPTH).toBe(3);
    return [
      launch('root', 0),
      launch('a', 1, 'root'),
      launch('b', 2, 'a'),
      launch('c', 3, 'b'), // sits AT the cap: its subtree flattens
    ];
  }

  function flattenedIds(items: Item[]): string[] {
    let node = groupItemsBySubagent(items)[0];
    for (let depth = 0; depth < MAX_DEPTH; depth += 1) {
      if (node.kind !== 'group') throw new Error(`depth ${depth} is ${node.kind}`);
      node = node.children[0];
    }
    if (node.kind !== 'group') throw new Error(`cap node is ${node.kind}`);
    return node.children.map((child) => {
      if (child.kind !== 'leaf') throw new Error('flattened subtree must be leaves');
      return child.item.id;
    });
  }

  it('de-duplicates the cap node\'s OWN children, not only their descendants', async () => {
    // `d` is emitted twice — the shape a transport-gap replay produces
    // (`eventsTransportGap.ts` refreshes and re-upserts) — so the initial
    // bucket itself carries a duplicate. Seeding the visited set FROM that
    // bucket (rather than pushing it through the same path) deduped nothing
    // and reported nothing: the run rendered two leaves with the same id,
    // which is a duplicate key in the row `{#each}` downstream, and that
    // throws.
    const items = [
      ...chainToCap(),
      launch('d', 4, 'c'),
      launch('d', 5, 'c'), // duplicate id
      mkItem({ id: 'e', itemIndex: 6, parentId: 'd', summary: 'grandchild' }),
    ];

    expect(flattenedIds(items)).toEqual(['d', 'e']);

    const records = await diagnostics.all();
    expect(records).toHaveLength(1);
    expect(records[0].message).toContain('subagentGrouping');
    // Constant message; the launch id and the skip count ride in the detail,
    // or every corrupt launch would mint its own dedupe signature.
    expect(records[0].message).not.toContain('launch c');
    expect(records[0].detail).toContain('launch c');
    // Console fallback: a remote session cannot persist the record at all.
    expect(diagnostics.warnings().join('\n')).toContain('launch c');
  });

  it('terminates on a genuine parentId cycle instead of allocating until dead', async () => {
    // A real cycle in the links: c -> d, d -> e, e -> d. Provider items are
    // keyed by id, so the second `d` is a distinct item carrying an id already
    // in the walk — which is exactly how a cycle reaches this code. With no
    // visited set at all (how this walk was originally written) the queue
    // grows forever and the tab dies with nothing logged.
    const items = [
      ...chainToCap(),
      launch('d', 4, 'c'),
      launch('e', 5, 'd'),
      launch('d', 6, 'e'), // closes the loop back onto `d`
    ];

    expect(flattenedIds(items)).toEqual(['d', 'e']);
    expect((await diagnostics.messages())).toHaveLength(1);
  });

  it('says nothing for a well-formed subtree', async () => {
    const items = [
      ...chainToCap(),
      launch('d', 4, 'c'),
      mkItem({ id: 'e', itemIndex: 6, parentId: 'd', summary: 'grandchild' }),
    ];

    expect(flattenedIds(items)).toEqual(['d', 'e']);
    expect(await diagnostics.messages()).toEqual([]);
  });

  it('flattens a wide bucket without a spread-apply', async () => {
    // The other half of the rewrite: `queue.push(...grand)` is bounded by the
    // engine's argument limit. This width is well under it — the point is
    // that a wide bucket flattens in one pass, in order, with nothing
    // reported.
    const wide = [
      ...chainToCap(),
      ...Array.from({ length: 5_000 }, (_, i) =>
        mkItem({ id: `w${i}`, itemIndex: 4 + i, parentId: 'c', summary: `w${i}` })),
    ];

    const ids = flattenedIds(wide);
    expect(ids).toHaveLength(5_000);
    expect(ids[0]).toBe('w0');
    expect(ids[4_999]).toBe('w4999');
    expect(await diagnostics.messages()).toEqual([]);
  });
});

// Leaf nodes are cached per Item object (see `leafNode` in the module):
// two passes over the same Items hand back the same node objects, and a
// replaced Item — the store's per-write behavior — mints a fresh leaf.
describe('groupItemsBySubagent — leaf reuse across passes', () => {
  it('reuses leaf nodes for reference-identical Items (fast path)', () => {
    const a = mkItem({ id: 'a1', kind: 'assistant_text' });
    const b = mkItem({ id: 'b1', kind: 'tool_call', toolName: 'Bash' });
    const first = groupItemsBySubagent([a, b]);
    const second = groupItemsBySubagent([a, b]);
    expect(second[0]).toBe(first[0]);
    expect(second[1]).toBe(first[1]);
  });

  it('mints a fresh leaf for a replaced Item and keeps neighbors', () => {
    const a = mkItem({ id: 'a1', kind: 'assistant_text' });
    const b = mkItem({ id: 'b1', kind: 'tool_call', toolName: 'Bash' });
    const first = groupItemsBySubagent([a, b]);
    const bReplaced = mkItem({ id: 'b1', kind: 'tool_call', toolName: 'Bash', summary: 'grew' });
    const second = groupItemsBySubagent([a, bReplaced]);
    expect(second[0]).toBe(first[0]);
    expect(second[1]).not.toBe(first[1]);
    expect((second[1] as { item: Item }).item).toBe(bReplaced);
  });
});
