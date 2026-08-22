import { describe, expect, it } from 'vitest';
import { filterRedundantNotifications } from './notificationFilter';
import type { Item } from '../types/models';

const BELL = 'Background command "sleep 1" completed (exit code 0)';

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

/** A completion sibling that ABSORBED the bell: caption stamped on its
 * first write (`meta.notification_summary`). */
function withCaption(taskId: string, summary = BELL): string {
  return JSON.stringify({ task_id: taskId, notification_summary: summary });
}

function withWatchTask(taskId: string): string {
  return JSON.stringify({ task_id: taskId, watch_task: true });
}

function ids(items: readonly Item[]): string[] {
  return items.map((it) => it.id);
}

describe('filterRedundantNotifications', () => {
  it('returns the input array reference when there are no notifications (hot path)', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'completed', meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 1, kind: 'tool_completion', meta: withCaption('T1') }),
    ];
    expect(filterRedundantNotifications(items)).toBe(items);
  });

  it('returns the input array reference when there are no completed lifecycle rows', () => {
    const items = [
      mkItem({ id: 'plan', kind: 'notification', meta: JSON.stringify({ kind: 'plan_update' }) }),
    ];
    expect(filterRedundantNotifications(items)).toBe(items);
  });

  it('hides a notification whose completion sibling carries its caption (stash-drain order)', () => {
    // Notification persisted after the sibling; the sibling absorbed the
    // bell text as `notification_summary` on its first write.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T1') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 2, kind: 'tool_completion', meta: withCaption('T1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'completion']);
  });

  it('hides a notification whose captioned sibling sits earlier in the array', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T1') }),
      mkItem({ id: 'completion', itemIndex: 1, kind: 'tool_completion', meta: withCaption('T1') }),
      mkItem({ id: 'notif', itemIndex: 2, kind: 'notification', summary: BELL, meta: withTaskId('T1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'completion']);
  });

  it('hides a notification whose sibling exists without a caption (TaskOutput-first order)', () => {
    // The sibling was created by a TaskOutput drain BEFORE the bell
    // arrived, so the caption missed its one first-write chance. This is
    // the NORMAL agentic wait order (bg Bash + blocking TaskOutput), and
    // the bell's text is the CLI's formulaic restatement of what the
    // sibling already shows — existence of the completed sibling hides
    // it (user ruling 2026-08-22; the row stays in SQLite).
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withTaskId('T1') }),
      mkItem({
        id: 'completion',
        itemIndex: 1,
        kind: 'tool_completion',
        summary: 'sleep 1 → completed',
        meta: withTaskId('T1'),
      }),
      mkItem({ id: 'notif', itemIndex: 2, kind: 'notification', summary: BELL, meta: withTaskId('T1') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'completion']);
  });

  it('hides a notification whose text equals the completion row summary', () => {
    const items = [
      mkItem({
        id: 'completion',
        kind: 'tool_completion',
        summary: `  ${BELL}  `,
        meta: withTaskId('T2'),
      }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['completion']);
  });

  it('hides against a completed foreground tool_call with equal text', () => {
    const items = [
      mkItem({ id: 'fg', kind: 'tool_call', status: 'completed', summary: BELL, meta: withTaskId('T2') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['fg']);
  });

  it('hides a notification against a completed tool_call with different text', () => {
    // Legacy history and the foreground case: a completed lifecycle row
    // with the same task_id hides the bell regardless of caption or
    // summary text.
    const items = [
      mkItem({ id: 'fg', kind: 'tool_call', status: 'completed', summary: 'Bash: sleep 30', meta: withTaskId('T2') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['fg']);
  });

  it('hides multi-subagent bells separated from their captioned completions by a running sibling', () => {
    // Screenshot scenario: 3 subagents, 2 done and 1 running. The
    // trailing notifications for A1 and A3 must drop even though A2's
    // running tool_call sits between them and their captioned siblings.
    const items = [
      mkItem({ id: 'A1', kind: 'tool_completion', meta: withCaption('A1', 'done A1') }),
      mkItem({ id: 'A2', itemIndex: 1, kind: 'tool_call', status: 'running', meta: withTaskId('A2') }),
      mkItem({ id: 'A3', itemIndex: 2, kind: 'tool_completion', meta: withCaption('A3', 'done A3') }),
      mkItem({ id: 'notif-A1', itemIndex: 3, kind: 'notification', summary: 'done A1', meta: withTaskId('A1') }),
      mkItem({ id: 'notif-A3', itemIndex: 4, kind: 'notification', summary: 'done A3', meta: withTaskId('A3') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['A1', 'A2', 'A3']);
  });

  it('preserves a notification that carries no task_id (plan_update / hook / warning)', () => {
    const items = [
      mkItem({ id: 'completion', kind: 'tool_completion', meta: withCaption('T1') }),
      mkItem({
        id: 'plan',
        itemIndex: 1,
        kind: 'notification',
        meta: JSON.stringify({ kind: 'plan_update', plan: [] }),
      }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['completion', 'plan']);
  });

  it('preserves a notification when the only captioned row carries a different task_id', () => {
    const items = [
      mkItem({ id: 'completion-other', kind: 'tool_completion', meta: withCaption('T1') }),
      mkItem({ id: 'notif-T2', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['completion-other', 'notif-T2']);
  });

  it('preserves a notification while the matching tool_call is still running', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', summary: BELL, meta: withTaskId('T3') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T3') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('preserves a notification when the matching tool_call ended in errored', () => {
    // Pins the narrow "completed" rule: a failed tool_call does NOT
    // suppress the bell. The user still wants the explicit failure ping.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'errored', summary: BELL, meta: withTaskId('T4') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T4') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('preserves a notification when the matching tool_call was killed by the user', () => {
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'killed', summary: BELL, meta: withTaskId('T5') }),
      mkItem({ id: 'notif', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T5') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['launch', 'notif']);
  });

  it('hides an absorbed notification persisted under a Task subagent (parentId set) before grouping', () => {
    // The filter walks the flat list before subagentGrouping runs, so a
    // notification with a parentId is dropped exactly the same way as a
    // top-level one. Locks the architectural claim that we never have to
    // recurse into SubagentGroupNode.children to filter.
    const items = [
      mkItem({ id: 'parent', kind: 'tool_completion', meta: withCaption('SUB1') }),
      mkItem({
        id: 'notif',
        itemIndex: 1,
        kind: 'notification',
        parentId: 'parent',
        summary: BELL,
        meta: withTaskId('SUB1'),
      }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['parent']);
  });

  it("keeps a watch task's notification rows even when its completion carries a caption", () => {
    // Claude's Monitor (claude-wire.md §E7) fires one notification per
    // output event of the stream it watches. Those rows ARE the interim
    // history and its completion row means only "the stream ended", so
    // the redundancy rule must not erase them the moment it lands.
    // Triage never captions a watch sibling, but the row-kind carve-out
    // must hold even against a hand-stamped caption.
    const items = [
      mkItem({ id: 'launch', kind: 'tool_call', status: 'running', meta: withWatchTask('W1') }),
      mkItem({ id: 'notif-1', itemIndex: 1, kind: 'notification', summary: 'tick 1', meta: withWatchTask('W1') }),
      mkItem({ id: 'notif-2', itemIndex: 2, kind: 'notification', summary: 'tick 2', meta: withWatchTask('W1') }),
      mkItem({ id: 'completion', itemIndex: 3, kind: 'tool_completion', meta: withCaption('W1', 'tick 2') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual([
      'launch',
      'notif-1',
      'notif-2',
      'completion',
    ]);
  });

  it("still hides an ordinary task's absorbed bell alongside a watch task in the same thread", () => {
    // The carve-out is per ROW, read off that row's own meta — a watch
    // task in the pane must not stop an ordinary task's redundant bell
    // from being suppressed.
    const items = [
      mkItem({ id: 'watch-notif', kind: 'notification', summary: 'tick 1', meta: withWatchTask('W1') }),
      mkItem({ id: 'watch-done', itemIndex: 1, kind: 'tool_completion', meta: withWatchTask('W1') }),
      mkItem({ id: 'plain-notif', itemIndex: 2, kind: 'notification', summary: BELL, meta: withTaskId('T7') }),
      mkItem({ id: 'plain-done', itemIndex: 3, kind: 'tool_completion', meta: withCaption('T7') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual([
      'watch-notif',
      'watch-done',
      'plain-done',
    ]);
  });

  it('hides every notification that a captioned completion absorbs (no per-task cap)', () => {
    // task_notification IDs are normally unique per task_id so this
    // wouldn't happen in production, but the contract is "set membership"
    // — locking it lets a future refactor swap the inner short-circuit
    // without silently breaking the second row.
    const items = [
      mkItem({ id: 'completion', kind: 'tool_completion', meta: withCaption('T6') }),
      mkItem({ id: 'notif-1', itemIndex: 1, kind: 'notification', summary: BELL, meta: withTaskId('T6') }),
      mkItem({ id: 'notif-2', itemIndex: 2, kind: 'notification', summary: BELL, meta: withTaskId('T6') }),
    ];
    expect(ids(filterRedundantNotifications(items))).toEqual(['completion']);
  });
});
