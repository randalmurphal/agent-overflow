// What moving a Board card leaves while its write is out: the card waits
// where it was dropped until the write settles, whatever it answers; a
// card has one drop at a time, from a release until its write is sent and
// while it is out; the ranks of a column go one at a time, each naming the
// neighbors the board holds when it is sent.

import { describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import type { BoardView, MoveView, RowView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { cardBusy, cardWriteContext, dropOwner, moveCard, rankCard, waitingCards } from './trackerBoardCarry.svelte';
import { attachTrackerBoard } from './trackerBoard.svelte';
import { columnList } from './trackerBoardModel';
import { HOME_BACKEND } from '../transport/backendKey';
import { setBindingMock } from '../../test/mocks/bindings-app';

const BOARD = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
const row = (id: string, order: string) => ({ order, estimate: 1, card: { id, key: `WEB-${id}`, title: `Issue ${id}`, status: { id: 's1' }, created: 0, updated: 0 } }) as RowView;
const ROW = row('1', 'a');
const MOVE = { id: '21', name: 'Start', to: { id: 's2' } } as MoveView;
const applied = (step: string) => ({ parts: [{ step, state: 'applied' }] }) as WriteView;

function held<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve: (v: T) => void = () => {};
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 10; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

/** A held kanban board of one column, To Do, holding `rows`. */
async function heldBoard(rows: RowView[]) {
  const column = { kind: 'column', column: 0 };
  const view = {
    id: '1', lifetime: 1, revision: 1, totals_revision: 1, name: 'WEB board', plans_sprints: false, estimate: { unit: 'points' }, counts: 'points',
    columns: [{ name: 'To Do', statuses: [{ id: 's1', group: 'todo' }], total: { rows: rows.length, estimate: rows.length } }], sprints: [],
    all: { rows: rows.length, estimate: rows.length }, backlog: { rows: 0, estimate: 0 }, sync: { read_at: 1 }, screen: [column],
    sections: [{ section: column, rows, next: null }],
  } as unknown as BoardView;
  setBindingMock('SubscribeTrackerBoard', async () => ({ id: 'sub', seq: 10, loaded: true, view, poll_every: 10_000 }));
  setBindingMock('SetTrackerSubscriptionVisible', async () => {});
  setBindingMock('ReleaseTrackerSubscription', async () => {});
  const board = attachTrackerBoard(BOARD, true);
  await flush();
  return board;
}

describe('a card waiting where it was dropped', () => {
  it('waits in its new column while the move is out and goes when it is refused, sending one move', async () => {
    const answer = held<WriteView>();
    const move = setBindingMock('MoveTrackerIssue', () => answer.promise);
    const ctx = cardWriteContext(BOARD, ROW);
    const sent = moveCard('board', ctx, 1, MOVE);
    expect(waitingCards('board')).toEqual([{ issue: '1', list: columnList(1), status: 's2' }]);
    expect(waitingCards('another board')).toEqual([]);
    expect(cardBusy('board', ctx)).toBe(true);
    // A second drop while it is out sends nothing.
    expect(await moveCard('board', ctx, 0, MOVE)).toBeNull();
    expect(await rankCard('board', ctx, '1', columnList(1), '2')).toBeNull();
    expect(dropOwner('board').start(ctx)).toBeNull();
    expect(move).toHaveBeenCalledTimes(1);
    answer.resolve({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'No.' } }] } as WriteView);
    expect((await sent)?.kind).toBe('refused');
    expect(waitingCards('board')).toEqual([]);
    expect(cardBusy('board', ctx)).toBe(false);
  });

  it('holds a card for its drop from the release until its move is sent, or the drop ends with none', async () => {
    const move = setBindingMock('MoveTrackerIssue', async () => applied('move'));
    const ctx = cardWriteContext(BOARD, ROW);
    const owner = dropOwner('board');
    const drop = owner.start(ctx)!;
    expect(cardBusy('board', ctx)).toBe(true);
    expect(owner.start(ctx)).toBeNull();
    expect(dropOwner('board').start(ctx)).toBeNull();
    expect(await moveCard('board', ctx, 1, MOVE)).toBeNull();
    // Another card, and the same card on another board, move meanwhile.
    expect(cardBusy('board', cardWriteContext(BOARD, row('2', 'b')))).toBe(false);
    expect(cardBusy('another board', ctx)).toBe(false);
    drop.end();
    expect(cardBusy('board', ctx)).toBe(false);
    // An ended drop sends nothing.
    expect(await drop.move(1, MOVE)).toBeNull();
    const again = owner.start(ctx)!;
    const sent = again.move(1, MOVE);
    // The move took the hold over: still busy until it settles.
    expect(cardBusy('board', ctx)).toBe(true);
    again.end();
    expect((await sent)?.kind).toBe('applied');
    expect(cardBusy('board', ctx)).toBe(false);
    expect(move).toHaveBeenCalledTimes(1);
  });

  it("ends every drop its owner holds when the owner closes, starts none after, and leaves another owner's", async () => {
    const move = setBindingMock('MoveTrackerIssue', async () => applied('move'));
    const one = cardWriteContext(BOARD, ROW);
    const two = cardWriteContext(BOARD, row('2', 'b'));
    const owner = dropOwner('board');
    const other = dropOwner('board');
    const first = owner.start(one)!;
    const second = other.start(two)!;
    owner.close();
    expect(first.holds).toBe(false);
    expect(cardBusy('board', one)).toBe(false);
    expect(await first.move(1, MOVE)).toBeNull();
    expect(owner.start(one)).toBeNull();
    expect(second.holds).toBe(true);
    expect(cardBusy('board', two)).toBe(true);
    other.close();
    expect(cardBusy('board', two)).toBe(false);
    expect(move).not.toHaveBeenCalled();
  });

  it('ranks one card of a column at a time, each before the card it was dropped above and after the one the board holds before it', async () => {
    const board = await heldBoard([row('1', 'a'), row('2', 'b'), row('3', 'c'), row('4', 'd')]);
    const answers: ((view: WriteView) => void)[] = [];
    const rank = setBindingMock('RankTrackerIssues', () => new Promise<WriteView>((resolve) => answers.push(resolve)));
    const four = cardWriteContext(BOARD, row('4', 'd'));
    const three = cardWriteContext(BOARD, row('3', 'c'));
    const first = rankCard(board.key, four, '1', columnList(0), '2');
    const second = rankCard(board.key, three, '1', columnList(0), '2');
    await flush();
    expect(waitingCards(board.key)).toEqual([{ issue: '4', list: columnList(0), before: '2' }, { issue: '3', list: columnList(0), before: '2' }]);
    expect(rank.mock.calls).toEqual([['c1', { board: '1', issues: ['4'], prev: '1', next: '2' }]]);
    expect(cardBusy(board.key, three)).toBe(true);
    answers[0](applied('rank'));
    expect((await first)?.kind).toBe('applied');
    await flush();
    expect(rank.mock.calls[1]).toEqual(['c1', { board: '1', issues: ['3'], prev: '1', next: '2' }]);
    answers[1](applied('rank'));
    expect((await second)?.kind).toBe('applied');
    // At the end: after the last card the board holds.
    const end = rankCard(board.key, cardWriteContext(BOARD, row('1', 'a')), '1', columnList(0), null);
    await flush();
    expect(rank.mock.calls[2]).toEqual(['c1', { board: '1', issues: ['1'], prev: '4', next: undefined }]);
    answers[2](applied('rank'));
    await end;
    board.release();
  });

  it('sends no rank whose card it was dropped before has left the column, or whose board was let go, and says why', async () => {
    const board = await heldBoard([row('1', 'a'), row('2', 'b')]);
    const rank = setBindingMock('RankTrackerIssues', async () => applied('rank'));
    const one = cardWriteContext(BOARD, row('1', 'a'));
    expect(await rankCard(board.key, one, '1', columnList(0), '9')).toEqual({ kind: 'not_sent', message: 'The card it was dropped before is no longer in To Do, so it was not ranked.' });
    board.release();
    await flush();
    expect(await rankCard(board.key, one, '1', columnList(0), '2')).toEqual({ kind: 'not_sent', message: 'The board was closed before the rank was sent, so it was not ranked.' });
    expect(rank).not.toHaveBeenCalled();
  });
});
