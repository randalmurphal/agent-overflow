// What moving a Board card leaves while its write is out: the card waits
// where it was dropped until the write settles, whatever it answers; a
// card waits for one write at a time; a rank to its own place sends
// nothing.

import { describe, expect, it } from 'vitest';
import type { MoveView, RowView, WriteView } from '../../../bindings/agent-overflow/internal/trackerapp/models';
import { cardMovePending, cardWriteContext, moveCard, rankCard, waitingCards } from './trackerBoardCarry.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { setBindingMock } from '../../test/mocks/bindings-app';

const BOARD = { backend: HOME_BACKEND, projectId: 'p1', connection: 'c1', board: '1' };
const ROW = { order: 'a', estimate: 1, card: { id: '1', key: 'WEB-1', title: 'Issue 1', status: { id: 's1' }, created: 0, updated: 0 } } as RowView;
const MOVE = { id: '21', name: 'Start', to: { id: 's2' } } as MoveView;

function held<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve: (v: T) => void = () => {};
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

describe('a card waiting where it was dropped', () => {
  it('waits in its new column while the move is out and goes when it is refused, sending one move', async () => {
    const answer = held<WriteView>();
    const move = setBindingMock('MoveTrackerIssue', () => answer.promise);
    const ctx = cardWriteContext(BOARD, ROW);
    const sent = moveCard('board', ctx, ROW, 1, MOVE);
    expect(waitingCards('board')).toEqual([{ row: ROW, column: 1 }]);
    expect(waitingCards('another board')).toEqual([]);
    expect(cardMovePending(ctx)).toBe(true);
    // A second drop while it is out sends nothing.
    expect(await moveCard('board', ctx, ROW, 0, MOVE)).toBeNull();
    expect(await rankCard('board', ctx, '1', ROW, 1, '2', '')).toBeNull();
    expect(move).toHaveBeenCalledTimes(1);
    answer.resolve({ parts: [{ step: 'move', state: 'refused', refusal: { reason: 'tracker', message: 'No.' } }] } as WriteView);
    expect((await sent)?.kind).toBe('refused');
    expect(waitingCards('board')).toEqual([]);
    expect(cardMovePending(ctx)).toBe(false);
  });

  it('waits before the card it was dropped above while the rank is out, and sends nothing for its own place', async () => {
    const answer = held<WriteView>();
    const rank = setBindingMock('RankTrackerIssues', () => answer.promise);
    const ctx = cardWriteContext(BOARD, ROW);
    expect(await rankCard('board', ctx, '1', ROW, 0, '', '')).toBeNull();
    const sent = rankCard('board', ctx, '1', ROW, 0, '', '2');
    expect(waitingCards('board')).toEqual([{ row: ROW, column: 0, before: '2' }]);
    expect(rank).toHaveBeenCalledWith('c1', { board: '1', issues: ['1'], prev: undefined, next: '2' });
    answer.resolve({ parts: [{ step: 'rank', state: 'applied' }] } as WriteView);
    expect((await sent)?.kind).toBe('applied');
    expect(waitingCards('board')).toEqual([]);
  });
});
