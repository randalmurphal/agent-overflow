import { describe, expect, it } from 'vitest';
import { filterRedundantNotifications } from './notificationFilter';
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

function withTaskId(taskId: string): string {
  return JSON.stringify({ task_id: taskId });
}

function ids(items: readonly Item[]): string[] {
  return items.map((it) => it.id);
}

describe('filterRedundantNotifications', () => {
  it('returns the input array reference when there are no notifications (hot path)', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'completed', meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 1, kind: 'tool_completion', meta: withTaskId('T1') }),
    ];
    expect(filterRedundantNotifications(items)).toBe(items);
  });

  it('returns the input array reference when there are no completed lifecycle rows', () => {
    const items = [
      mkItem({ id: 'plan', kind: 'notification', meta: JSON.stringify({ kind: 'plan_update' }) }),
    ];
    expect(filterRedundantNotifications(items)).toBe(items);
  });

  it('hides a notification whose task_id matches a tool_completion later in the array', () => {
    // Stash-drain order: notification persisted first, completion sibling
    // written next.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T1') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 2, kind: 'tool_completion', meta: withTaskId('T1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'completion']);
  });

  it('hides a notification whose task_id matches a tool_completion earlier in the array', () => {
    // TaskOutput-drain order: completion was persisted earlier, the bell
    // arrives later.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 1, kind: 'tool_completion', meta: withTaskId('T1') }),
      mkItem({ id: 'notif', itemIndex: 2, kind: 'notification', meta: withTaskId('T1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'completion']);
  });

  it('hides a notification matching a foreground tool_call that already completed', () => {
    const items = [
      mkItem({ id: 'fg', kind: 'tool_call', status: 'completed', meta: withTaskId('T2') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['fg']);
  });

  it('hides multi-subagent bells separated from their Task tool_calls by a still-running sibling', () => {
    // Screenshot scenario: 3 inline subagents, 2 done and 1 running. The
    // trailing notifications for A1 and A3 must drop even though A2's
    // running tool_call sits between them and their matching completed
    // Task tool_calls.
    const items = [
      mkItem({ id: 'A1', kind: 'tool_call', status: 'completed', meta: withTaskId('A1') }),
      mkItem({ id: 'A2', itemIndex: 1, kind: 'tool_call', status: 'running', meta: withTaskId('A2') }),
      mkItem({ id: 'A3', itemIndex: 2, kind: 'tool_call', status: 'completed', meta: withTaskId('A3') }),
      mkItem({ id: 'notif-A1', itemIndex: 3, kind: 'notification', meta: withTaskId('A1') }),
      mkItem({ id: 'notif-A3', itemIndex: 4, kind: 'notification', meta: withTaskId('A3') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['A1', 'A2', 'A3']);
  });

  it('preserves a notification that carries no task_id (plan_update / hook / warning)', () => {
    const items = [
      mkItem({ id: 'plan', kind: 'notification', meta: JSON.stringify({ kind: 'plan_update', plan: [] }) }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['plan']);
  });

  it('preserves a notification when the only completed lifecycle row carries a different task_id', () => {
    const items = [
      mkItem({ id: 'completion-other', kind: 'tool_completion', meta: withTaskId('T1') }),
      mkItem({ id: 'notif-T2', itemIndex: 1, kind: 'notification', meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['completion-other', 'notif-T2']);
  });

  it('preserves a notification while the matching tool_call is still running', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T3') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', meta: withTaskId('T3') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('preserves a notification when the matching tool_call ended in errored', () => {
    // Pins the narrow "completed" rule: a failed tool_call does NOT
    // suppress the bell. The user still wants the explicit failure ping.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'errored', meta: withTaskId('T4') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', meta: withTaskId('T4') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('preserves a notification when the matching tool_call was killed by the user', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'killed', meta: withTaskId('T5') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', meta: withTaskId('T5') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('hides a notification persisted under a Task subagent (parentId set) before grouping', () => {
    // The filter walks the flat list before subagentGrouping runs, so a
    // notification with a parentId is dropped exactly the same way as a
    // top-level one. Locks the architectural claim that we never have to
    // recurse into SubagentGroupNode.children to filter.
    const items = [
      mkItem({ id: 'parent', kind: 'tool_call', status: 'completed', meta: withTaskId('SUB1') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', parentId: 'parent', meta: withTaskId('SUB1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['parent']);
  });

  it('hides every notification that shares a completed task_id (no per-task cap)', () => {
    // task_notification IDs are normally unique per task_id so this
    // wouldn't happen in production, but the contract is "set membership"
    // — locking it lets a future refactor swap the inner short-circuit
    // without silently breaking the second row.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'completed', meta: withTaskId('T6') }),
      mkItem({ id: 'notif-1', itemIndex: 1, kind: 'notification', meta: withTaskId('T6') }),
      mkItem({ id: 'notif-2', itemIndex: 2, kind: 'notification', meta: withTaskId('T6') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch']);
  });
});
