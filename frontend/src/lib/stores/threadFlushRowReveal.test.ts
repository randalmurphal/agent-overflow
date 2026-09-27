// stores/threadFlushRowReveal.test.ts
//
// The predicate behind the send-queue preview's Zone 2 XOR timeline
// invariant: a flushed message's entry leaves the preview only for a row
// the pane renders, which needs all of held, confirmed, inside the loaded
// window and past the reveal boundary.

import { describe, expect, it } from 'vitest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';
import type { FlushedItem } from './sendQueue.svelte';
import { renderedFlushedUserItemIds } from './threadFlushRowReveal';

const flushed = (userItemId: string): FlushedItem =>
  ({ queueItemId: `q-${userItemId}`, userItemId, message: 'queued', flushedAt: 1 });

const row = (id: string, itemIndex: number, meta = '{}'): Item =>
  makeItem({ id, turnIndex: 3, itemIndex, kind: 'user_text', role: 'user', meta });

const cursor = (item: Item) => ({ turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id });

describe('renderedFlushedUserItemIds', () => {
  const oldest = row('floor', 1);
  const newest = row('ceiling', 5);
  const window = { oldest: cursor(oldest), newest: cursor(newest) };
  const lookup = (...items: Item[]) => (id: string) => items.find((item) => item.id === id);

  it('reports a held, confirmed row inside the window with no boundary standing', () => {
    const confirmed = row('confirmed', 3);
    expect(renderedFlushedUserItemIds([flushed('confirmed')], lookup(confirmed), null, window))
      .toEqual(['confirmed']);
  });

  it('answers nothing for no pending entries or an entry whose row the pane does not hold', () => {
    expect(renderedFlushedUserItemIds([], lookup(row('any', 3)), null, window)).toEqual([]);
    expect(renderedFlushedUserItemIds([flushed('missing')], lookup(), null, window)).toEqual([]);
  });

  it('keeps a quiet reservation in the preview', () => {
    const quiet = row('quiet', 3, '{"pendingFlush":true}');
    expect(renderedFlushedUserItemIds([flushed('quiet')], lookup(quiet), null, window)).toEqual([]);
  });

  it('keeps a row the reveal gate still withholds', () => {
    const confirmed = row('confirmed', 4);
    const boundary = { turnIndex: 3, itemIndex: 3 };
    expect(renderedFlushedUserItemIds([flushed('confirmed')], lookup(confirmed), boundary, window)).toEqual([]);
    expect(renderedFlushedUserItemIds([flushed('confirmed')], lookup(confirmed), { turnIndex: 3, itemIndex: 4 }, window))
      .toEqual(['confirmed']);
  });

  it('keeps a row the window does not admit, whatever the reveal gate says', () => {
    // The row the backend moved past the newest cursor on confirmation: held
    // in place, refused by the projection until the cursor follows it.
    const moved = row('moved', 7);
    expect(renderedFlushedUserItemIds([flushed('moved')], lookup(moved), null, window)).toEqual([]);
    const beforeFloor = row('early', 0);
    expect(renderedFlushedUserItemIds([flushed('early')], lookup(beforeFloor), null, window)).toEqual([]);
    // Once the cursor follows, the same row renders.
    expect(renderedFlushedUserItemIds([flushed('moved')], lookup(moved), null, { ...window, newest: cursor(moved) }))
      .toEqual(['moved']);
  });

  it('applies no window rule while the pane has no loaded edges', () => {
    const confirmed = row('confirmed', 9);
    expect(renderedFlushedUserItemIds([flushed('confirmed')], lookup(confirmed), null, { oldest: null, newest: null }))
      .toEqual(['confirmed']);
  });

  it('answers each pending entry on its own', () => {
    const shown = row('shown', 3);
    const quiet = row('quiet', 4, '{"pendingFlush":true}');
    const moved = row('moved', 8);
    expect(renderedFlushedUserItemIds(
      [flushed('quiet'), flushed('shown'), flushed('moved')], lookup(shown, quiet, moved), null, window,
    )).toEqual(['shown']);
  });
});
