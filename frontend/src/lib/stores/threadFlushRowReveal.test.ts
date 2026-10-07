// stores/threadFlushRowReveal.test.ts
//
// The predicate behind the send-queue preview's Zone 2 XOR timeline
// invariant: a flushed message's entry leaves the preview only for a row
// in the timeline: one the pane renders, which needs all of held,
// confirmed, inside the loaded window and past the reveal boundary, or a
// confirmed row in the history before the window.

import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import type { FlushedItem } from './sendQueue.svelte';
import { flushedUserItemIdsInTimeline } from './threadFlushRowReveal';

const flushed = (userItemId: string): FlushedItem =>
  ({ queueItemId: `q-${userItemId}`, userItemId, message: 'queued', flushedAt: 1 });

const row = (id: string, itemIndex: number, meta = '{}'): Item =>
  makeItem({ id, turnIndex: 3, itemIndex, kind: 'user_text', role: 'user', meta });

const cursor = (item: Item) => ({ turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id });

describe('flushedUserItemIdsInTimeline', () => {
  const oldest = row('floor', 1);
  const newest = row('ceiling', 5);
  const window = { oldest: cursor(oldest), newest: cursor(newest) };
  const lookup = (...items: Item[]) => (id: string) => items.find((item) => item.id === id);

  it('reports a held, confirmed row inside the window with no boundary standing', () => {
    const confirmed = row('confirmed', 3);
    expect(flushedUserItemIdsInTimeline([flushed('confirmed')], lookup(confirmed), null, window))
      .toEqual(['confirmed']);
  });

  it('answers nothing for no pending entries or an entry whose row the pane does not hold', () => {
    expect(flushedUserItemIdsInTimeline([], lookup(row('any', 3)), null, window)).toEqual([]);
    expect(flushedUserItemIdsInTimeline([flushed('missing')], lookup(), null, window)).toEqual([]);
  });

  it('keeps a quiet reservation in the preview', () => {
    const quiet = row('quiet', 3, '{"pendingFlush":true}');
    expect(flushedUserItemIdsInTimeline([flushed('quiet')], lookup(quiet), null, window)).toEqual([]);
  });

  it('keeps a row the reveal gate still withholds', () => {
    const confirmed = row('confirmed', 4);
    const boundary = { turnIndex: 3, itemIndex: 3 };
    expect(flushedUserItemIdsInTimeline([flushed('confirmed')], lookup(confirmed), boundary, window)).toEqual([]);
    expect(flushedUserItemIdsInTimeline([flushed('confirmed')], lookup(confirmed), { turnIndex: 3, itemIndex: 4 }, window))
      .toEqual(['confirmed']);
  });

  it('keeps a row past the newest edge, whatever the reveal gate says', () => {
    // The row the backend moved past the newest cursor on confirmation: held
    // in place, refused by the projection until the cursor follows it.
    const moved = row('moved', 7);
    expect(flushedUserItemIdsInTimeline([flushed('moved')], lookup(moved), null, window)).toEqual([]);
    // Once the cursor follows, the same row renders.
    expect(flushedUserItemIdsInTimeline([flushed('moved')], lookup(moved), null, { ...window, newest: cursor(moved) }))
      .toEqual(['moved']);
  });

  it('hands over a confirmed row before the oldest edge as history, held or known only by position', () => {
    const beforeFloor = row('early', 0);
    const boundary = { turnIndex: 3, itemIndex: 0 };
    expect(flushedUserItemIdsInTimeline([flushed('early')], lookup(beforeFloor), boundary, window)).toEqual(['early']);
    const unheld = { ...flushed('history'), deliveredAt: { turnIndex: 1, itemIndex: 12, itemId: 'history' } };
    expect(flushedUserItemIdsInTimeline([unheld], lookup(), null, window)).toEqual(['history']);
    // Without loaded edges there is no history to place it in yet.
    expect(flushedUserItemIdsInTimeline([unheld], lookup(), null, { oldest: null, newest: null })).toEqual([]);
  });

  it('keeps a quiet reservation before the oldest edge, and an unheld row known to be newer', () => {
    const quietEarly = row('quiet-early', 0, '{"pendingFlush":true}');
    expect(flushedUserItemIdsInTimeline([flushed('quiet-early')], lookup(quietEarly), null, window)).toEqual([]);
    const newer = { ...flushed('newer'), deliveredAt: { turnIndex: 3, itemIndex: 7, itemId: 'newer' } };
    expect(flushedUserItemIdsInTimeline([newer], lookup(), null, window)).toEqual([]);
  });

  it('applies no window rule while the pane has no loaded edges', () => {
    const confirmed = row('confirmed', 9);
    expect(flushedUserItemIdsInTimeline([flushed('confirmed')], lookup(confirmed), null, { oldest: null, newest: null }))
      .toEqual(['confirmed']);
  });

  it('answers each pending entry on its own', () => {
    const shown = row('shown', 3);
    const quiet = row('quiet', 4, '{"pendingFlush":true}');
    const moved = row('moved', 8);
    expect(flushedUserItemIdsInTimeline(
      [flushed('quiet'), flushed('shown'), flushed('moved')], lookup(shown, quiet, moved), null, window,
    )).toEqual(['shown']);
  });
});
