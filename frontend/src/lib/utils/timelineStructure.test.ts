import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import { itemTimelineStructureChanged } from './timelineStructure';
import { extractClaudeTaskID } from './claudeTaskMeta';
import { RAIL_EXEMPT_PAYLOAD_KINDS } from './timelineRail';

// ORACLE: the retired key-builder this module used before the field-wise
// rewrite (2026-08-25). Building two of these per streamed upsert — each
// copying meta/payloadMeta into joined strings — was 15MB/30s of garbage;
// the predicate must stay EXACTLY as sensitive as comparing these keys.
function notificationFilterFingerprint(item: Item): string {
  if (item.kind === 'notification') return `notification:${extractClaudeTaskID(item) ?? ''}`;
  if (item.kind === 'tool_completion') return `completion:${extractClaudeTaskID(item) ?? ''}`;
  if (item.kind !== 'tool_call') return '';
  const taskId = extractClaudeTaskID(item);
  if (!taskId) return '';
  const completedFlag = item.status === 'completed' ? 'completed' : 'not-completed';
  return `tool:${completedFlag}:${taskId}`;
}

function subagentGroupingFingerprint(item: Item): string {
  return [
    item.parentId ?? '',
    item.completionOf ?? '',
    item.toolName ?? '',
    item.isBackground === true ? 'background' : 'foreground',
    item.kind === 'terminal_interaction' ? item.meta ?? '' : '',
    item.kind === 'tool_call' && item.toolName === 'wait_agent' ? item.meta ?? '' : '',
    item.kind === 'tool_call' && (item.toolName === 'Agent' || item.toolName === 'Task') ? item.meta ?? '' : '',
    item.kind === 'tool_call' && item.toolName === 'collab_agent'
      ? [item.meta ?? '', item.payloadMeta ?? ''].join('\x1f')
      : '',
  ].join('\x1f');
}

function oracleKey(item: Item): string {
  return [
    item.id,
    item.threadId,
    item.turnIndex,
    item.itemIndex,
    item.kind,
    subagentGroupingFingerprint(item),
    [
      item.kind,
      item.toolName ?? '',
      item.isBackground === true ? 'background' : 'foreground',
    ].join('\x1f'),
    notificationFilterFingerprint(item),
    item.kind === 'tool_call' && item.toolName === 'collab_agent'
      ? [item.meta ?? '', item.payloadMeta ?? ''].join('\x1f')
      : '',
    item.kind === 'notification' || item.kind === 'tool_completion' || item.completionOf
      ? item.meta ?? ''
      : '',
    item.kind === 'tool_completion' ? item.payloadId ?? '' : '',
    RAIL_EXEMPT_PAYLOAD_KINDS.has(item.payloadKind ?? '') ? 'rail-exempt' : '',
  ].join('\x1e');
}

function baseItem(overrides: Partial<Item> = {}): Item {
  return {
    id: 'i1',
    threadId: 't1',
    turnIndex: 3,
    itemIndex: 7,
    kind: 'tool_call',
    status: 'running',
    summary: 'doing things',
    updatedAt: 100,
    ...overrides,
  } as Item;
}

// Every axis one of the old fingerprints read, plus axes they deliberately
// ignored (summary growth, updatedAt, meta on a plain tool call) — the
// matrix crosses each variant against each other variant and asserts the
// predicate answers exactly what oracle-key inequality answers.
const VARIANTS: Array<[string, Item]> = [
  ['base', baseItem()],
  ['id', baseItem({ id: 'i2' })],
  ['turn', baseItem({ turnIndex: 4 })],
  ['itemIndex', baseItem({ itemIndex: 8 })],
  ['kind-completion', baseItem({ kind: 'tool_completion' })],
  ['parent', baseItem({ parentId: 'p1' })],
  ['completionOf', baseItem({ completionOf: 'c1' })],
  ['completionOf-meta', baseItem({ completionOf: 'c1', meta: '{"x":1}' })],
  ['tool', baseItem({ toolName: 'Bash' })],
  ['tool-meta', baseItem({ toolName: 'Bash', meta: '{"x":1}' })],
  ['background', baseItem({ isBackground: true })],
  ['agent', baseItem({ toolName: 'Agent' })],
  ['agent-meta', baseItem({ toolName: 'Agent', meta: '{"a":1}' })],
  ['wait-meta', baseItem({ toolName: 'wait_agent', meta: '{"w":1}' })],
  ['collab-meta', baseItem({ toolName: 'collab_agent', meta: '{"c":1}' })],
  ['collab-payloadMeta', baseItem({ toolName: 'collab_agent', meta: '{"c":1}', payloadMeta: 'pm' })],
  ['task', baseItem({ meta: '{"task_id":"T1"}' })],
  ['task-completed', baseItem({ meta: '{"task_id":"T1"}', status: 'completed' })],
  ['task-other', baseItem({ meta: '{"task_id":"T2"}' })],
  ['task-meta-noise', baseItem({ meta: '{"task_id":"T1","noise":1}' })],
  ['completed-no-task', baseItem({ status: 'completed' })],
  ['notification', baseItem({ kind: 'notification' })],
  ['notification-meta', baseItem({ kind: 'notification', meta: '{"n":1}' })],
  ['notification-task', baseItem({ kind: 'notification', meta: '{"task_id":"T1"}' })],
  ['completion-meta', baseItem({ kind: 'tool_completion', meta: '{"m":1}' })],
  ['completion-payload', baseItem({ kind: 'tool_completion', payloadId: 'pl1' })],
  ['terminal', baseItem({ kind: 'terminal_interaction' })],
  ['terminal-meta', baseItem({ kind: 'terminal_interaction', meta: 'row' })],
  ['payload-kind-plain', baseItem({ payloadKind: 'command_output' })],
  ['summary-grew', baseItem({ summary: 'doing things and more' })],
  ['updatedAt', baseItem({ updatedAt: 200 })],
  ['plain-meta', baseItem({ meta: '{"ignored":true}' })],
  ['payloadId-on-call', baseItem({ payloadId: 'pl1' })],
];

describe('itemTimelineStructureChanged', () => {
  it('matches the retired key-builder across the variant matrix', () => {
    for (const [prevName, previous] of VARIANTS) {
      for (const [nextName, next] of VARIANTS) {
        const oracle = oracleKey(previous) !== oracleKey(next);
        const got = itemTimelineStructureChanged(previous, next);
        expect(got, `${prevName} -> ${nextName}`).toBe(oracle);
      }
    }
  });

  it('treats a missing previous as changed', () => {
    expect(itemTimelineStructureChanged(undefined, baseItem())).toBe(true);
  });

  it('ignores content growth on a plain streaming row', () => {
    // The whole point of the gate: reveal-cadence summary writes must not
    // rebuild the projection.
    expect(
      itemTimelineStructureChanged(baseItem(), baseItem({ summary: 'x'.repeat(5000), updatedAt: 999 })),
    ).toBe(false);
  });

  it('rail-exempt payload kinds compare by the bit, not the kind', () => {
    const kinds = [...RAIL_EXEMPT_PAYLOAD_KINDS];
    if (kinds.length >= 2) {
      expect(
        itemTimelineStructureChanged(
          baseItem({ payloadKind: kinds[0] }),
          baseItem({ payloadKind: kinds[1] }),
        ),
      ).toBe(false);
    }
    expect(
      itemTimelineStructureChanged(baseItem(), baseItem({ payloadKind: kinds[0] })),
    ).toBe(true);
  });
});
