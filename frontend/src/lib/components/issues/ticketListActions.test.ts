// What a ticket list's selection can do: the statuses every ticket can
// move to (a ticket already there left as it is, a move that asks for
// fields or none at all naming the ticket), the batch bound shared by
// every list, and the people every project of the selection may assign.

import { describe, expect, it } from 'vitest';
import type { MoveView } from '../../../../bindings/agent-overflow/internal/trackerapp/models';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import { HOME_BACKEND } from '../../transport/backendKey';
import { ticketWrite } from '../../stores/trackerTicketWrites.svelte';
import { assignablePeople, BULK_AT_ONCE, commonMoves, eachBounded, moveTickets, type ListTicket } from './ticketListActions';

function ticket(key: string, issue: string, status: string): ListTicket {
  return { ctx: { key: `k-${issue}`, ref: { backend: HOME_BACKEND, connection: 'c1', issue }, label: key }, status };
}

function move(id: string, to: string, name: string, fields = false): MoveView {
  return { id, name: `Move ${id}`, to: { id: to, name }, ...(fields ? { fields: [{ id: 'resolution', name: 'Resolution' }] } : {}) } as MoveView;
}

describe('commonMoves', () => {
  const a = ticket('WEB-1', '1', 'To Do');
  const b = ticket('WEB-2', '2', 'In Progress');
  const c = ticket('WEB-3', '3', 'Done');

  it('takes each ticket\'s first move that asks for nothing, and leaves a ticket already there', () => {
    const statuses = commonMoves([a, b, c], [
      [move('11', 's2', 'In Progress'), move('12', 's3', 'Done', true), move('13', 's3', 'Done')],
      [move('21', 's3', 'Done'), move('22', 's3', 'Done'), move('23', 's1', 'To Do')],
      [move('31', 's1', 'To Do'), move('32', 's2', 'In Progress')],
    ]);
    expect(statuses.map((s) => [s.name, s.off, Object.fromEntries(s.moves)])).toEqual([
      ['In Progress', '', { 'k-1': '11', 'k-2': null, 'k-3': '32' }],
      ['Done', '', { 'k-1': '13', 'k-2': '21', 'k-3': null }],
      ['To Do', '', { 'k-1': null, 'k-2': '23', 'k-3': '31' }],
    ]);
  });

  it('turns a status off when a ticket reaches it only by a move that asks for fields, naming the ticket', () => {
    const [done] = commonMoves([a, b], [[move('12', 's3', 'Done', true)], [move('21', 's3', 'Done')]]);
    expect(done.off).toBe('Moving WEB-1 to Done asks for fields; move it from the ticket');
  });

  it('turns a status off that a ticket does not reach, naming it and counting the rest', () => {
    const d = ticket('WEB-4', '4', 'To Do');
    const [review] = commonMoves([a, b, d, c], [[], [move('21', 's4', 'Review')], [], []]);
    expect(review.off).toBe('WEB-1 and 2 more cannot move to Review from where they are');
  });

  it('turns a status off that every ticket is in already', () => {
    const [done] = commonMoves([c], [[move('99', 's3', 'Done')]]);
    expect(done.off).toBe('It is in Done already');
  });
});

describe('eachBounded', () => {
  it(`keeps at most ${BULK_AT_ONCE} calls of every batch out at a time, in order, and runs the rest after a failure`, async () => {
    let out = 0;
    let most = 0;
    const started: number[] = [];
    const finish = new Map<number, (ok: boolean) => void>();
    const step = (n: number) => {
      started.push(n);
      out += 1;
      most = Math.max(most, out);
      return new Promise<number>((resolve, reject) => {
        finish.set(n, (ok) => { out -= 1; if (ok) resolve(n); else reject(new Error(`no ${n}`)); });
      });
    };
    const first = eachBounded([1, 2, 3, 4, 5, 6], step);
    const second = eachBounded([7, 8], step);
    await new Promise((r) => setTimeout(r, 0));
    expect(started).toEqual([1, 2, 3, 4]);
    finish.get(2)!(false);
    await new Promise((r) => setTimeout(r, 0));
    expect(started).toEqual([1, 2, 3, 4, 5]);
    for (let n = 1; n <= 8; n += 1) {
      if (n === 2) continue;
      while (!finish.has(n)) await new Promise((r) => setTimeout(r, 0));
      finish.get(n)!(true);
    }
    const results = await first;
    await second;
    expect(most).toBe(BULK_AT_ONCE);
    expect(started).toEqual([1, 2, 3, 4, 5, 6, 7, 8]);
    expect(results.map((r) => r.status)).toEqual(['fulfilled', 'rejected', 'fulfilled', 'fulfilled', 'fulfilled', 'fulfilled']);
  });
});

describe('moveTickets', () => {
  it(`shows every ticket's write as pending from the press and sends ${BULK_AT_ONCE} at a time`, async () => {
    const sent: Array<() => void> = [];
    setBindingMock('MoveTrackerIssue', () => new Promise((resolve) => { sent.push(() => resolve({ parts: [{ step: 'move', state: 'applied' }] })); }));
    const tickets = Array.from({ length: 6 }, (_, i) => ticket(`WEB-${i + 1}`, String(i + 1), 'To Do'));
    const status = { id: 's3', name: 'Done', off: '', moves: new Map(tickets.map((t) => [t.ctx.key, '31'])) };
    const done = moveTickets(tickets, status);
    for (let i = 0; i < 5; i += 1) await new Promise((r) => setTimeout(r, 0));
    expect(tickets.map((t) => ticketWrite(t.ctx.key, 'status')?.pending)).toEqual([true, true, true, true, true, true]);
    expect(sent).toHaveLength(BULK_AT_ONCE);
    while (sent.length > 0) {
      sent.shift()!();
      for (let i = 0; i < 5; i += 1) await new Promise((r) => setTimeout(r, 0));
    }
    expect(await done).toEqual({ made: 6, sent: 6 });
  });
});

describe('assignablePeople', () => {
  it('asks once per project and lists the people every project may assign; a project that fails says so', async () => {
    const asked: string[] = [];
    setBindingMock('TrackerAssignableUsers', async (_c: string, scope: { issue: string }) => {
      asked.push(scope.issue);
      if (scope.issue === '20') throw new Error('the tracker did not answer');
      return scope.issue === '1'
        ? { users: [{ id: 'u1', name: 'User 1' }, { id: 'u2', name: 'User 2' }, { id: 'u3', name: 'User 3' }] }
        : { users: [{ id: 'u3', name: 'User 3' }, { id: 'u1', name: 'User 1' }], more: true };
    });
    const list = await assignablePeople([ticket('WEB-1', '1', ''), ticket('WEB-2', '2', ''), ticket('APP-1', '10', ''), ticket('OPS-1', '20', '')], 'u');
    expect(asked).toEqual(['1', '10', '20']);
    expect(list.items.map((p) => p.name)).toEqual(['User 1', 'User 3']);
    expect(list.more).toBe(true);
    expect(list.failed).toEqual(['Could not read who may be assigned OPS tickets: The tracker did not answer.']);
  });
});
