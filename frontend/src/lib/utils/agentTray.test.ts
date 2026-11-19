import { describe, expect, it } from 'vitest';
import {
  agentRowBlockedLabel, agentRowCallers, agentRowOpenTarget, agentRowRuntimeMs, agentRowShownState, agentRowStateLabel, agentWorkerCount,
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
    // A paired computer dropped the late reply, as it did the first answer
    // or after this computer collected it.
    expect(agentRowStateLabel(live('expired', undefined, { lateReplyExpiredAt: 1 }))).toBe('expired, late reply expired');
    expect(agentRowStateLabel(live('expired'))).toBe('expired');
    expect(agentRowStateLabel(live('finished', undefined, { lateReplyExpiredAt: 1 }))).toBe('finished, late reply expired');
    // An open request reads as its own state, whatever its worker does,
    // but one held for its worker's rest: its turn ended and its worker
    // waits on what it started.
    expect(agentRowShownState(live('accepted', 'running'))).toBe('accepted');
    expect(agentRowStateLabel(live('accepted', 'running'))).toBe('queued');
    expect(agentRowShownState(live('running', 'blocked'))).toBe('running');
    const held = live('running', 'waiting');
    expect(agentRowShownState(held)).toBe('waiting');
    expect(agentRowStateLabel(held)).toBe('waiting');
    expect(agentWorkerCount([held], (r) => agentRowShownState(r) === 'running')).toBe(0);
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
  it('names the thread and the local threads its spawn and send rows stand for, not a paired computer\'s or an ask\'s copy', () => {
    const rows = [
      row('local', 0), row('remote', 0, 'running', { computerId: 'peer', threadId: 'far' }),
      row('send', 0, 'running', { kind: 'send', threadId: 'other' }), row('ask', 0, 'running', { kind: 'ask', threadId: 'copy' }),
    ];
    expect([...agentRowCallers('me', rows)].sort()).toEqual(['c', 'me', 'other', 't-local']);
  });
});

describe('agentRowBlockedLabel', () => {
  it('names the thread on a paired computer that a prompt went to, else a person there', () => {
    expect(agentRowBlockedLabel({ blockedOn: 'person', computer: 'Studio', blockedRoutedTo: 'Lead' })).toBe('Waiting on Lead on Studio');
    expect(agentRowBlockedLabel({ blockedOn: 'person', computer: 'Studio' })).toBe('Waiting on a person on Studio');
    expect(agentRowBlockedLabel({ blockedOn: 'approval', blockedDetail: 'Bash', blockedRoutedTo: 'Lead' })).toBe('Needs approval for Bash, waiting on Lead');
  });

  it('names a prompt a paired computer routed back to the spawner here by its title alone', () => {
    expect(agentRowBlockedLabel({ blockedOn: 'person', computer: 'Studio', blockedRoutedTo: 'Lead', blockedRoutedHere: true })).toBe('Waiting on Lead');
    expect(agentRowBlockedLabel({ blockedOn: 'question', blockedDetail: 'Which branch?', computer: 'Studio', blockedRoutedTo: 'Lead', blockedRoutedHere: true }))
      .toBe('Asks: Which branch?, waiting on Lead');
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

describe('agentRowRuntimeMs', () => {
  it("is the turns' time as read, the open turn's going on from the row's own read", () => {
    const open = row('a', 0, 'running', { runtimeMs: 4_000, turnOpen: true });
    expect(agentRowRuntimeMs(open, 10_000, 10_000)).toBe(4_000);
    expect(agentRowRuntimeMs(open, 12_500, 10_000)).toBe(6_500);
    // A clock read before the row was is no time.
    expect(agentRowRuntimeMs(open, 9_000, 10_000)).toBe(4_000);
  });

  it('stops between turns', () => {
    const idle = row('a', 0, 'finished', { runtimeMs: 4_000 });
    expect(agentRowRuntimeMs(idle, 10_000, 1_000)).toBe(4_000);
    expect(agentRowRuntimeMs(idle, 90_000, 1_000)).toBe(4_000);
  });

  it('is unknown for a row that does not report it', () => {
    expect(agentRowRuntimeMs(row('a', 0, 'running'), 10_000, 0)).toBeNull();
    expect(agentRowRuntimeMs(row('a', 0, 'running', { kind: 'send' }), 10_000, 0)).toBeNull();
  });

  it("is a send's run as its row reports it", () => {
    expect(agentRowRuntimeMs(row('a', 0, 'running', { kind: 'send', runtimeMs: 4_000, turnOpen: true }), 11_000, 10_000)).toBe(5_000);
  });
});
