import { describe, expect, it } from 'vitest';
import { extractNotificationSummary } from './claudeTaskMeta';
import type { Item } from '../types/models';

function itemWithMeta(meta: Record<string, unknown>): Item {
  return {
    id: 'complete:tool-1',
    threadId: 't1',
    kind: 'tool_completion',
    role: 'assistant',
    summary: 'Bash: sleep 5 -> done',
    status: 'completed',
    turnIndex: 0,
    itemIndex: 0,
    createdAt: 0,
    updatedAt: 0,
    meta: JSON.stringify(meta),
  } as Item;
}

describe('extractNotificationSummary', () => {
  it('returns the trimmed caption for an ordinary background command', () => {
    const item = itemWithMeta({
      task_id: 'task-1',
      notification_summary: '  Background command "sleep 5" completed (exit code 0)  ',
    });
    expect(extractNotificationSummary(item)).toBe(
      'Background command "sleep 5" completed (exit code 0)',
    );
  });

  it('returns null when the caption is absent or blank', () => {
    expect(extractNotificationSummary(itemWithMeta({ task_id: 'task-1' }))).toBeNull();
    expect(
      extractNotificationSummary(itemWithMeta({ notification_summary: '   ' })),
    ).toBeNull();
  });

  it('vetoes the caption on an output_file sibling — the summary IS the report', () => {
    // Rows persisted before the triage-side veto (2026-08-22) carry the
    // agent's entire final report as notification_summary; the payload
    // already holds that content, so the caption must not render.
    const item = itemWithMeta({
      task_id: 'task-agent',
      notification_summary: 'Confirmed:\n\n1. **Agent tool availability** — yes...',
      notification_output_loaded: true,
      notification_output_state: 'loaded',
      notification_output_file: '/tmp/agent-output.txt',
    });
    expect(extractNotificationSummary(item)).toBeNull();
  });

  it('vetoes on any non-empty output state, including a failed read', () => {
    const item = itemWithMeta({
      notification_summary: 'the whole report',
      notification_output_state: 'error',
      notification_output_error: 'open: no such file',
    });
    expect(extractNotificationSummary(item)).toBeNull();
  });
});
