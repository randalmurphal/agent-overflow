// Pipeline tripwire for the invariant two independent rules rest on:
//
//   Every stop of a background task renders EXACTLY ONCE, at the point in
//   the transcript where it happened, and no bell repeats it.
//
// `filterRedundantNotifications` hides a command's bell on the strength of
// the completed lifecycle sibling existing, which assumes that sibling
// renders in place. An agent rings no bell: each of its stops, parked or
// ending, is a sibling of its own. `groupItemsBySubagent` builds the
// launch's card AT each sibling (`SubagentGroupNode.anchor`), folding it
// in as the status source, so a grouping pass that folded a sibling onto
// a card at the LAUNCH would leave the transcript with no trace of the
// stop. This file runs the two rules in their production order, over the
// shapes the backend writes, and counts rows.

import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { filterRedundantNotifications } from './notificationFilter';
import {
  findTimelineNodeIndex,
  groupItemsBySubagent,
  nodeContainsItem,
  timelineNodeItemId,
  type TimelineNode,
} from './subagentGrouping';

function mkItem(overrides: Partial<Item> & { id: string; itemIndex: number }): Item {
  return {
    rev: 0,
    threadId: 'thread-1',
    turnIndex: 0,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: '',
    createdAt: overrides.itemIndex,
    updatedAt: overrides.itemIndex,
    ...overrides,
  };
}

/** The shapes Go writes for one background task: its ending sibling, and
 * for a top-level command or a watch task its bell. An agent's stops ring
 * no bell (internal/triage/agent_stops.go). */
function backgroundTask(opts: {
  toolName: 'Agent' | 'Bash';
  launchId: string;
  taskId: string;
  at: number;
  completedAt?: number;
  parentId?: string;
  watch?: boolean;
}): Item[] {
  const meta = JSON.stringify({
    task_id: opts.taskId,
    toolName: opts.toolName,
    input: { description: `${opts.launchId} work` },
    ...(opts.watch ? { watch_task: true } : {}),
  });
  const completedAt = opts.completedAt ?? opts.at + 10;
  const rows: Item[] = [
    mkItem({
      id: opts.launchId,
      itemIndex: opts.at,
      kind: 'tool_call',
      toolName: opts.toolName,
      isBackground: true,
      status: 'completed',
      summary: `${opts.toolName}: ${opts.launchId}`,
      meta,
      ...(opts.parentId ? { parentId: opts.parentId } : {}),
    }),
    mkItem({
      id: `complete:${opts.launchId}`,
      itemIndex: completedAt,
      kind: 'tool_completion',
      toolName: opts.toolName,
      isBackground: true,
      completionOf: opts.launchId,
      summary: `${opts.toolName}: ${opts.launchId} -> done`,
      meta,
      ...(opts.parentId ? { parentId: opts.parentId } : {}),
    }),
  ];
  if (opts.toolName === 'Bash' && (!opts.parentId || opts.watch)) {
    rows.push(
      mkItem({
        id: `task-notification:${opts.taskId}`,
        itemIndex: completedAt + 1,
        kind: 'notification',
        toolName: opts.toolName,
        summary: `Background task ${opts.taskId} completed`,
        meta,
      }),
    );
  }
  return rows;
}

function project(items: Item[]): TimelineNode[] {
  const sorted = [...items].sort((a, b) => a.itemIndex - b.itemIndex);
  return groupItemsBySubagent(filterRedundantNotifications(sorted));
}

/** How many top-level nodes carry (or contain) `itemId`. */
function rowsCarrying(nodes: TimelineNode[], itemId: string): number {
  return nodes.filter((node) => nodeContainsItem(node, itemId)).length;
}

function bellsIn(nodes: TimelineNode[]): string[] {
  return nodes
    .filter((node) => node.kind === 'leaf' && node.item.kind === 'notification')
    .map((node) => timelineNodeItemId(node));
}

describe('background completion visibility (filter + grouping, production order)', () => {
  it('a top-level background agent: one completion row at the completion point, zero bells', () => {
    const nodes = project([
      mkItem({ id: 'lead', itemIndex: 0, summary: 'launching' }),
      ...backgroundTask({ toolName: 'Agent', launchId: 'agent-1', taskId: 'T1', at: 1 }),
      mkItem({ id: 'child', itemIndex: 2, kind: 'tool_call', toolName: 'Bash', parentId: 'agent-1', summary: 'Bash: ls' }),
      mkItem({ id: 'main-prose', itemIndex: 5, summary: 'main keeps going' }),
    ]);

    expect(bellsIn(nodes)).toEqual([]);
    expect(rowsCarrying(nodes, 'complete:agent-1')).toBe(1);
    // At its own position — after the prose the main agent wrote while
    // the task ran — as the agent's card, with the transcript under it.
    // The launch row stays where it was, as the immutable spawn record.
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'lead',
      'agent-1',
      'main-prose',
      'complete:agent-1',
    ]);
    expect(nodes[1].kind).toBe('leaf');
    const card = nodes[3];
    if (card.kind !== 'group') throw new Error('expected the completion point to be the card');
    expect(card.parent.id).toBe('agent-1');
    expect(card.children.map((child) => timelineNodeItemId(child))).toEqual(['child']);
    expect(findTimelineNodeIndex(nodes, 'complete:agent-1')).toBe(3);
  });

  it('a top-level background command: same contract', () => {
    const nodes = project([
      ...backgroundTask({ toolName: 'Bash', launchId: 'bash-1', taskId: 'T2', at: 0 }),
      mkItem({ id: 'main-prose', itemIndex: 5, summary: 'meanwhile' }),
    ]);

    expect(bellsIn(nodes)).toEqual([]);
    expect(rowsCarrying(nodes, 'complete:bash-1')).toBe(1);
    // A Bash launch is not a subagent launch: both rows are plain leaves.
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'bash-1',
      'main-prose',
      'complete:bash-1',
    ]);
    expect(nodes.every((node) => node.kind === 'leaf')).toBe(true);
  });

  it('a nested background agent: its completion row sits inside the parent card, no bell is written', () => {
    // An agent's stops ring no bell at any depth, so there is none to hide;
    // the completion row is still a row, inside the launching agent's body.
    const nodes = project([
      ...backgroundTask({ toolName: 'Agent', launchId: 'outer', taskId: 'T3', at: 0, completedAt: 12 }),
      ...backgroundTask({ toolName: 'Agent', launchId: 'inner', taskId: 'T4', at: 1, parentId: 'outer' }),
      mkItem({ id: 'outer-prose', itemIndex: 3, parentId: 'outer', summary: 'outer continues' }),
    ]);

    expect(bellsIn(nodes)).toEqual([]);
    expect(rowsCarrying(nodes, 'complete:inner')).toBe(1);
    expect(rowsCarrying(nodes, 'complete:outer')).toBe(1);
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['outer', 'complete:outer']);
    expect(nodes[0].kind).toBe('leaf');
    const outer = nodes[1];
    if (outer.kind !== 'group') throw new Error('expected the outer completion point to be a card');
    expect(outer.children.map((child) => timelineNodeItemId(child))).toEqual([
      'inner',
      'outer-prose',
      'complete:inner',
    ]);
    expect(outer.children[0].kind).toBe('leaf');
    expect(outer.children[2].kind).toBe('group');
  });

  it('a parked agent: a card at every stop with its own run, and an older build’s agent bell stays', () => {
    const meta = JSON.stringify({ task_id: 'T7', toolName: 'Agent', input: { description: 'gate watcher' } });
    const stop = (id: string, itemIndex: number, status: 'parked' | 'completed') => mkItem({
      id, itemIndex, kind: 'tool_completion', toolName: 'Agent', isBackground: true, completionOf: 'agent', status, meta,
    });
    const nodes = project([
      mkItem({ id: 'agent', itemIndex: 0, kind: 'tool_call', toolName: 'Agent', isBackground: true, status: 'running', meta }),
      mkItem({ id: 'run-1-tool', itemIndex: 1, kind: 'tool_call', toolName: 'Bash', parentId: 'agent' }),
      stop('complete:agent:parked:u1', 2, 'parked'),
      mkItem({ id: 'legacy-bell', itemIndex: 3, kind: 'notification', toolName: 'Agent', summary: 'Agent reported', meta }),
      mkItem({ id: 'wake-1', itemIndex: 4, kind: 'user_text', role: 'user', parentId: 'agent' }),
      mkItem({ id: 'run-2-tool', itemIndex: 5, kind: 'tool_call', toolName: 'Bash', parentId: 'agent' }),
      mkItem({ id: 'main-prose', itemIndex: 6, summary: 'meanwhile' }),
      stop('complete:agent:parked:u2', 7, 'parked'),
      mkItem({ id: 'wake-2', itemIndex: 8, kind: 'user_text', role: 'user', parentId: 'agent' }),
      mkItem({ id: 'run-3-tool', itemIndex: 9, kind: 'tool_call', toolName: 'Bash', parentId: 'agent' }),
      stop('complete:agent', 10, 'completed'),
    ]);

    expect(bellsIn(nodes)).toEqual(['legacy-bell']);
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual([
      'agent',
      'complete:agent:parked:u1',
      'legacy-bell',
      'main-prose',
      'complete:agent:parked:u2',
      'complete:agent',
    ]);
    const runs = [nodes[1], nodes[4], nodes[5]].map((card) => {
      if (card.kind !== 'group') throw new Error('expected every stop to be a card');
      return card.children.map((child) => timelineNodeItemId(child));
    });
    expect(runs).toEqual([['run-1-tool'], ['wake-1', 'run-2-tool'], ['wake-2', 'run-3-tool']]);
  });

  it('a nested parked agent: each of its stops is a card inside the parent card', () => {
    const outerMeta = JSON.stringify({ task_id: 'T8', toolName: 'Agent', input: { description: 'outer' } });
    const innerMeta = JSON.stringify({ task_id: 'T9', toolName: 'Agent', input: { description: 'inner' } });
    const agentRow = (id: string, itemIndex: number, overrides: Partial<Item>) => mkItem({
      id, itemIndex, toolName: 'Agent', isBackground: true, ...overrides,
    });
    const nodes = project([
      agentRow('outer', 0, { kind: 'tool_call', status: 'running', meta: outerMeta }),
      agentRow('inner', 1, { kind: 'tool_call', status: 'running', parentId: 'outer', meta: innerMeta }),
      mkItem({ id: 'inner-run-1', itemIndex: 2, kind: 'tool_call', toolName: 'Bash', parentId: 'inner' }),
      agentRow('complete:inner:parked:u1', 3, {
        kind: 'tool_completion', status: 'parked', parentId: 'outer', completionOf: 'inner', meta: innerMeta,
      }),
      mkItem({ id: 'outer-prose', itemIndex: 4, parentId: 'outer', summary: 'outer continues' }),
      mkItem({ id: 'inner-wake', itemIndex: 5, kind: 'user_text', role: 'user', parentId: 'inner' }),
      mkItem({ id: 'inner-run-2', itemIndex: 6, kind: 'tool_call', toolName: 'Bash', parentId: 'inner' }),
      agentRow('complete:inner', 7, { kind: 'tool_completion', parentId: 'outer', completionOf: 'inner', meta: innerMeta }),
      agentRow('complete:outer', 8, { kind: 'tool_completion', completionOf: 'outer', meta: outerMeta }),
    ]);

    expect(bellsIn(nodes)).toEqual([]);
    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['outer', 'complete:outer']);
    const outer = nodes[1];
    if (outer.kind !== 'group') throw new Error('expected the outer completion point to be a card');
    expect(outer.children.map((child) => timelineNodeItemId(child))).toEqual([
      'inner',
      'complete:inner:parked:u1',
      'outer-prose',
      'complete:inner',
    ]);
    const innerRuns = [outer.children[1], outer.children[3]].map((card) => {
      if (card.kind !== 'group') throw new Error('expected every nested stop to be a card');
      return card.children.map((child) => timelineNodeItemId(child));
    });
    expect(innerRuns).toEqual([['inner-run-1'], ['inner-wake', 'inner-run-2']]);
  });

  it('a watch task keeps its bells: they are the history, not a redundant ping', () => {
    const nodes = project([
      ...backgroundTask({ toolName: 'Bash', launchId: 'monitor-1', taskId: 'T5', at: 0, watch: true }),
    ]);

    expect(bellsIn(nodes)).toEqual(['task-notification:T5']);
    expect(rowsCarrying(nodes, 'complete:monitor-1')).toBe(1);
  });

  it('a still-running background agent is a bell-less spawn leaf with no completion row', () => {
    // The other half of the contract: nothing renders a completion that
    // has not happened — and nothing renders a card either (the spawn row
    // is immutable; the live transcript is the pane's).
    const nodes = project([
      mkItem({
        id: 'agent-live',
        itemIndex: 0,
        kind: 'tool_call',
        toolName: 'Agent',
        isBackground: true,
        status: 'running',
        summary: 'Agent: live',
        meta: JSON.stringify({ task_id: 'T6', toolName: 'Agent', input: {} }),
      }),
      mkItem({ id: 'main-prose', itemIndex: 1, summary: 'waiting' }),
    ]);

    expect(nodes.map((node) => timelineNodeItemId(node))).toEqual(['agent-live', 'main-prose']);
    expect(nodes.every((node) => node.kind === 'leaf')).toBe(true);
    expect(nodes.some((node) => node.kind === 'leaf' && node.item.kind === 'tool_completion')).toBe(false);
  });
});
