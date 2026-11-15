import { describe, expect, it } from 'vitest';
import {
  agentRowBlockedLabel, agentRowCallers, agentRowOpenTarget, agentRowShownState, agentRowStateLabel, agentWorkerCount,
  isOpenAgentRow, visibleAgentRows,
} from './agentTray';
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

  it('keeps a settled row whose worker is not at rest, past the linger and dismissed alike', () => {
    const rows = [row('live', 0, 'replied', { worker: 'running' }), row('done', 0, 'replied')];
    const seen = new Map([['live', 0], ['done', 0]]);
    const shown = visibleAgentRows(rows, seen, new Set(['live', 'done']), 100_000, linger);
    expect(shown.map((r) => r.token)).toEqual(['live']);
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

describe('an open row', () => {
  it('is one whose request is open, or whose worker is not at rest whatever its request says', () => {
    expect(isOpenAgentRow(row('a', 0, 'running'))).toBe(true);
    expect(isOpenAgentRow(row('a', 0, 'accepted'))).toBe(true);
    expect(isOpenAgentRow(row('a', 0, 'replied'))).toBe(false);
    expect(isOpenAgentRow(row('a', 0, 'finished', { worker: 'queued' }))).toBe(true);
  });

  it('shows its worker\'s state once its request settled, in the words a request state already reads as', () => {
    const live = (state: string, worker?: string, over: Partial<ThreadAgentRow> = {}) => row('a', 0, state, { worker, ...over });
    expect(agentRowShownState(live('replied', 'running'))).toBe('running');
    expect(agentRowStateLabel(live('replied', 'running'))).toBe('running');
    expect(agentRowStateLabel(live('finished', 'blocked'))).toBe('blocked');
    expect(agentRowStateLabel(live('replied', 'queued'))).toBe('queued');
    // A worker waiting on its background work, a loop, a reminder or a
    // timer is open, reads as waiting, and is no running worker.
    const waiting = live('replied', 'waiting');
    expect(isOpenAgentRow(waiting)).toBe(true);
    expect(agentRowShownState(waiting)).toBe('waiting');
    expect(agentRowStateLabel(waiting)).toBe('waiting');
    expect(agentWorkerCount([waiting], (r) => agentRowShownState(r) === 'running')).toBe(0);
    expect(agentRowStateLabel(live('replied', undefined, { lateReply: true }))).toBe('replied late');
    // An open request reads as its own state, whatever its worker does.
    expect(agentRowShownState(live('accepted', 'running'))).toBe('accepted');
    expect(agentRowStateLabel(live('accepted', 'running'))).toBe('queued');
  });

  it('counts each worker once, however many open requests went to it', () => {
    const rows = [
      row('spawn', 0), row('send-a', 0, 'running', { kind: 'send', threadId: 't-spawn' }),
      row('send-b', 0, 'blocked', { kind: 'send', threadId: 't-spawn' }),
      row('other', 0, 'replied', { worker: 'running' }), row('done', 0, 'finished'),
      row('there', 0, 'running', { computerId: 'peer', threadId: 't-spawn' }),
      row('unmade', 0, 'accepted', { threadId: undefined }),
    ];
    expect(agentWorkerCount(rows)).toBe(4);
    expect(agentWorkerCount(rows, (r) => agentRowShownState(r) === 'running')).toBe(3);
  });
});

describe('agentRowCallers', () => {
  it('names the thread and the local threads its spawns made, not a paired computer\'s', () => {
    const rows = [row('local', 0), row('remote', 0, 'running', { computerId: 'peer', threadId: 'far' }), row('send', 0, 'running', { kind: 'send', threadId: 'other' })];
    expect([...agentRowCallers('me', rows)].sort()).toEqual(['c', 'me', 't-local']);
  });
});

describe('agentRowBlockedLabel', () => {
  it('names the thread on a paired computer that a prompt went to, else a person there', () => {
    expect(agentRowBlockedLabel({ blockedOn: 'person', computer: 'Studio', blockedRoutedTo: 'Lead' })).toBe('Waiting on Lead on Studio');
    expect(agentRowBlockedLabel({ blockedOn: 'person', computer: 'Studio' })).toBe('Waiting on a person on Studio');
    expect(agentRowBlockedLabel({ blockedOn: 'approval', blockedDetail: 'Bash', blockedRoutedTo: 'Lead' })).toBe('Needs approval for Bash, waiting on Lead');
  });
});

describe('agentRowOpenTarget', () => {
  it('opens a spawn or send thread, an ask copy while it is named, then the thread asked as that', () => {
    expect(agentRowOpenTarget(row('s', 0))).toEqual({ threadId: 't-s', asked: false });
    expect(agentRowOpenTarget(row('s', 0, 'running', { openThreadId: 't-open' }))).toEqual({ threadId: 't-open', asked: false });
    const ask = { kind: 'ask', threadId: 'copy', askedThreadId: 'origin' };
    expect(agentRowOpenTarget(row('a', 0, 'running', { ...ask, openThreadId: 'copy' }))).toEqual({ threadId: 'copy', asked: false });
    // The copy is never opened once the backend stops naming it.
    expect(agentRowOpenTarget(row('a', 0, 'replied', ask))).toEqual({ threadId: 'origin', asked: true });
    expect(agentRowOpenTarget(row('a', 0, 'replied', { kind: 'ask', threadId: 'copy' }))).toBeNull();
    expect(agentRowOpenTarget(row('q', 0, 'accepted', { threadId: '' }))).toBeNull();
  });
});
