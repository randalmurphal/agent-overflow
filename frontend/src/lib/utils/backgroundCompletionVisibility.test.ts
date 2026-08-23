// Pipeline tripwire for the one invariant two independent rules rest on:
//
//   A settled background task renders its completion EXACTLY ONCE, at the
//   point in the transcript where it completed, and its bell renders not
//   at all.
//
// `filterRedundantNotifications` hides the bell on the strength of the
// completed lifecycle sibling existing — it assumes that sibling renders
// in place. `groupItemsBySubagent` builds the launch's card AT that
// sibling (`SubagentGroupNode.anchor`), folding it in as the status
// source. Each rule was correct alone and each had its own unit test;
// what neither test saw was an earlier grouping pass folding the sibling
// onto a card at the LAUNCH and dropping it from the node array, which
// left the main transcript with no trace of an agent finishing (live
// regression 2026-08-22: "THERE IS NEVER THE COMPLETION IN THE MAIN
// TIMELINE"). This file runs the two rules in their production order,
// over the shapes the backend actually writes, and counts rows.

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

/** The shapes Go writes for one background task (`writeBackgroundCompletionSibling`, `writeBell`). */
function backgroundTask(opts: {
  toolName: 'Agent' | 'Bash';
  launchId: string;
  taskId: string;
  at: number;
  parentId?: string;
  watch?: boolean;
  withBell: boolean;
}): Item[] {
  const meta = JSON.stringify({
    task_id: opts.taskId,
    toolName: opts.toolName,
    input: { description: `${opts.launchId} work` },
    ...(opts.watch ? { watch_task: true } : {}),
  });
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
      itemIndex: opts.at + 10,
      kind: 'tool_completion',
      toolName: opts.toolName,
      isBackground: true,
      completionOf: opts.launchId,
      summary: `${opts.toolName}: ${opts.launchId} -> done`,
      meta,
      ...(opts.parentId ? { parentId: opts.parentId } : {}),
    }),
  ];
  if (opts.withBell) {
    rows.push(
      mkItem({
        id: `task-notification:${opts.taskId}`,
        itemIndex: opts.at + 11,
        kind: 'notification',
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
      ...backgroundTask({ toolName: 'Agent', launchId: 'agent-1', taskId: 'T1', at: 1, withBell: true }),
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
      ...backgroundTask({ toolName: 'Bash', launchId: 'bash-1', taskId: 'T2', at: 0, withBell: true }),
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
    // Q11: nested completions do not notify, so there is no bell to hide;
    // the completion row is still a row, inside the launching agent's body.
    const nodes = project([
      ...backgroundTask({ toolName: 'Agent', launchId: 'outer', taskId: 'T3', at: 0, withBell: true }),
      ...backgroundTask({ toolName: 'Agent', launchId: 'inner', taskId: 'T4', at: 1, parentId: 'outer', withBell: false }),
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

  it('a watch task keeps its bells: they are the history, not a redundant ping', () => {
    const nodes = project([
      ...backgroundTask({ toolName: 'Bash', launchId: 'monitor-1', taskId: 'T5', at: 0, watch: true, withBell: true }),
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
