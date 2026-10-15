import { describe, expect, it } from 'vitest';
import { agentRowCallers, visibleAgentRows } from './agentTray';
import type { ThreadAgentRow } from '../stores/bindings';

function row(token: string, depth: number, state = 'running', over: Partial<ThreadAgentRow> = {}): ThreadAgentRow {
  return { token, callerThreadId: 'c', kind: 'spawn', threadId: `t-${token}`, title: token, state, createdAt: 0, depth, ...over } as ThreadAgentRow;
}

describe('visibleAgentRows', () => {
  const linger = 8_000;

  it('shows open rows, settled rows inside the linger or not seen yet, and none dismissed or past it', () => {
    const rows = [row('open', 0), row('fresh', 0, 'finished'), row('old', 0, 'finished'), row('gone', 0, 'replied'), row('unseen', 0, 'errored')];
    const seen = new Map([['fresh', 9_000], ['old', 1_000], ['gone', 9_000]]);
    const shown = visibleAgentRows(rows, seen, new Set(['gone']), 10_000, linger);
    expect(shown.map((r) => r.token)).toEqual(['open', 'fresh', 'unseen']);
  });

  it('keeps every settled ancestor of a shown row, and only those', () => {
    const rows = [
      row('a', 0, 'finished'), row('a1', 1, 'finished'), row('a1x', 2), row('a2', 1, 'finished'),
      row('b', 0, 'finished'), row('b1', 1, 'finished'),
    ];
    const seen = new Map(rows.map((r) => [r.token, 0]));
    const shown = visibleAgentRows(rows, seen, new Set(), 100_000, linger);
    expect(shown.map((r) => r.token)).toEqual(['a', 'a1', 'a1x']);
  });
});

describe('agentRowCallers', () => {
  it('names the thread and the local threads its spawns made, not a paired computer\'s', () => {
    const rows = [row('local', 0), row('remote', 0, 'running', { computerId: 'peer', threadId: 'far' }), row('send', 0, 'running', { kind: 'send', threadId: 'other' })];
    expect([...agentRowCallers('me', rows)].sort()).toEqual(['c', 'me', 't-local']);
  });
});
