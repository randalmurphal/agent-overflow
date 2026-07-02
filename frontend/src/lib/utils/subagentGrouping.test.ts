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
  type SubagentLiveAggregates,
  type TimelineLeaf,
  type WaitGroupNode,
  type TimelineNode,
} from './subagentGrouping';
import type { SubagentFoldAggregate } from './subagentFold';
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

  it('suppresses backgrounded Claude subagent child rows, keeping launch and completion', () => {
    // A backgrounded Claude Agent launches at the top level (parentId
    // empty — the main agent launched it). Its inner transcript carries
    // parentId === launch.id and must NOT leak into the main timeline
    // interleaved with the main agent's own rows. The launch stays a
    // leaf and the completion sibling (parentId empty, completionOf ===
    // launch.id) surfaces the result "on completion".
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

    // Only the main-agent rows, the launch (leaf), and the completion
    // sibling survive as roots — no subagent child leaks between them.
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'main-think',
      'bg-agent',
      'main-text',
      'complete:bg-agent',
    ]);
    for (const childId of ['child-bash', 'child-bash-done', 'child-text']) {
      expect(nodes.some((node) => nodeContainsItem(node, childId))).toBe(false);
    }
  });

  it('nests foreground subagent children while suppressing background ones in the same turn', () => {
    // Both launch types coexisting in one turn: the foreground Agent must
    // still group its child; the background Agent must still suppress its
    // child. Guards the two code paths against future refactors that share
    // the launch predicates.
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
    expect(fgGroup.children.map((child) => expectLeaf(child).item.id)).toEqual(['fg-child']);
    const bgLeaf = expectLeaf(nodes[1]);
    expect(bgLeaf.item.id).toBe('bg-agent');
    expect(nodes.some((node) => nodeContainsItem(node, 'bg-child'))).toBe(false);
  });

  it('surfaces a failed background subagent on its completion item, not as a leaked inner row', () => {
    // The user-facing contract for a backgrounded subagent that fails:
    // the failure shows on the completion item, never as an interleaved
    // inner row. On the wire a failing background subagent terminates via
    // `task_updated{failed}`, which triage maps to an `errored` completion
    // sibling (parentId empty, completionOf === launch.id — verified
    // against 238 real Agent completions). Suppression is structural
    // (parentId-keyed), so it must preserve that completion regardless of
    // status while still dropping the inner transcript, including the
    // child rows that themselves errored.
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

    // Launch leaf + errored completion item survive; the inner (failed)
    // transcript is suppressed.
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'bg-agent',
      'complete:bg-agent',
    ]);
    const completion = expectLeaf(nodes[1]).item;
    expect(completion.status).toBe('errored');
    expect(completion.completionOf).toBe('bg-agent');
    for (const childId of ['child-bash', 'child-bash-done']) {
      expect(nodes.some((node) => nodeContainsItem(node, childId))).toBe(false);
    }
  });

  it('suppresses a grandchild under a non-background launch nested inside a background subagent (transitive suppression)', () => {
    // Regression for the tool-call flash leak: a bg running Agent launch has
    // a NESTED launch child that is itself foreground (not background) — a
    // real Claude shape (an inline agent launched from within a backgrounded
    // one). The old filter only dropped items whose DIRECT parentId matched
    // a suppressed anchor, so the nested launch was dropped (level 1) but
    // its own child fell through to the orphan branch and rendered at the
    // TOP LEVEL. The suppression walk must be transitive over the whole
    // subtree, not one level deep.
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
    ]);

    // Only the bg launch itself survives as a root.
    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['bg-agent']);
    // The grandchild must appear NOWHERE: not as an orphan leaf, not inside
    // a read_group, not nested in any group's children.
    for (const node of nodes) {
      expect(nodeContainsItem(node, 'nested-launch')).toBe(false);
      expect(nodeContainsItem(node, 'grandchild-read')).toBe(false);
    }
    expect(nodes.some((node) => node.kind === 'read_group')).toBe(false);
  });

  it('suppresses a depth-3 mixed subtree (launches and plain tools) under a background anchor', () => {
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
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['bg-agent']);
    for (const id of ['depth1-launch', 'depth2-tool', 'depth3-launch', 'depth4-tool']) {
      expect(nodes.some((node) => nodeContainsItem(node, id))).toBe(false);
    }
  });

  it('keeps a true orphan (missing parent) top-level even alongside a suppressed background subtree', () => {
    // Guards the suppression walk against over-reaching: it must only
    // suppress items reachable by parent->child edges from an actual
    // suppressed anchor, never rows whose declared parent simply does not
    // exist in the input.
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
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual(['bg-agent', 'orphan']);
    const orphan = expectLeaf(nodes[1]);
    expect(orphan.orphan).toBe(true);
    expect(nodes.some((node) => nodeContainsItem(node, 'bg-child'))).toBe(false);
  });

  it('keeps a top-level background completion sibling rendering while suppressing one nested a level deeper', () => {
    // Two completion siblings in the same tree: the OUTER bg launch's
    // completion has parentId "" (empty) and must always render top-level.
    // The INNER bg launch's completion has parentId = the OUTER anchor (the
    // real wire shape for a nested backgrounded launch) — the transitive
    // walk correctly suppresses it along with the rest of the inner subtree.
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

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'outer-bg',
      'complete:outer-bg',
    ]);
    for (const id of ['inner-bg', 'inner-child', 'complete:inner-bg']) {
      expect(nodes.some((node) => nodeContainsItem(node, id))).toBe(false);
    }
  });

  it('terminates on malformed cyclic parentId data without swallowing the top-level anchor', () => {
    // Defensive guard for the suppression pass: parentId cycles cannot occur
    // in persisted data (the store enforces acyclic parent chains), but a
    // grouping walk over untrusted input must still terminate and must not
    // let a cycle swallow legitimate rows. The single forward pass visits
    // each item exactly once, so termination is structural. A top-level
    // anchor has parentId '' and therefore can never enter the suppression
    // set itself. Items trapped in a cycle are unreachable from any
    // suppressed anchor, so they degrade to flat top-level leaves (their
    // declared parents exist in the item set, so they are not
    // orphan-flagged) rather than crashing or vanishing.
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
    ]);

    expect(nodes.map((node) => expectLeaf(node).item.id)).toEqual([
      'bg-agent',
      'cycle-x',
      'cycle-y',
      'self-ref',
    ]);
    expect(nodes.some((node) => nodeContainsItem(node, 'bg-child'))).toBe(false);
    // Cycle members are not orphan-flagged: their declared parents exist.
    for (const node of nodes) {
      expect(expectLeaf(node).orphan).toBeUndefined();
    }
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

  it('suppresses a transitive descendant subtree under a Codex spawn anchor', () => {
    // Same transitive-suppression walk as the background-Claude case: a
    // grandchild whose parent is not itself the spawn anchor must not leak
    // to the top level.
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

    expect(nodes).toHaveLength(1);
    expect(expectLeaf(nodes[0]).item.id).toBe('codex-agent');
    expect(nodes.some((node) => nodeContainsItem(node, 'child-tool'))).toBe(false);
    expect(nodes.some((node) => nodeContainsItem(node, 'grandchild-tool'))).toBe(false);
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
    // Folded completion resolves to this wait_group row (index 1) for
    // search-scroll, not -1.
    expect(nodeContainsItem(waitGroup, 'complete-wait-review')).toBe(true);
    expect(findTimelineNodeIndex(nodes, 'complete-wait-review')).toBe(1);
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
      mkItem({ id: 'c1', itemIndex: 2, parentId: 'agent-1', summary: 'loaded at 2' }),
    ], fold)[0]);
    expect(foldWins.descendantCount).toBe(3);
    expect(foldWins.loadedDescendantCount).toBe(1);
    expect(foldWins.latestChildSummary).toBe('evicted at 5');

    // Loaded terminal later than the fold → loaded preview wins.
    const loadedWins = expectGroup(groupItemsBySubagent([
      agentLaunch('agent-1', 0),
      mkItem({ id: 'c2', itemIndex: 9, parentId: 'agent-1', summary: 'loaded at 9' }),
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
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', summary: 'first step' }),
      mkItem({ id: 'c2', itemIndex: 2, parentId: 'agent-1', summary: 'second step' }),
      mkItem({ id: 'c3', itemIndex: 3, parentId: 'agent-1', summary: 'third step' }),
    ]);

    const group = expectGroup(nodes[0]);
    expect(group.descendantCount).toBe(3);
    expect(group.loadedDescendantCount).toBe(3);
    expect(group.latestChildSummary).toBe('third step');
  });

  it('keeps the decorated count when loaded children trail it', () => {
    const nodes = groupItemsBySubagent([
      decoratedLaunch('agent-1', 0, { subagentDescendantCount: 5 }),
      mkItem({ id: 'c1', itemIndex: 1, parentId: 'agent-1', summary: 'only loaded row' }),
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
