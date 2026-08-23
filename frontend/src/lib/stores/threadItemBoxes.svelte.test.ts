// Row-level reactivity of the thread pane's item window. `pane.items` is
// `$state.raw` and `getItemById` reads a per-row `$state.raw` box, so a
// reader of one row re-derives only when THAT row is written — never
// because another row streamed. Before this, the deep `$state` array
// re-minted every proxy source per batch and every mounted row's
// `displayItem` changed identity on every batch (2026-08-23 profile).
import { describe, expect, it } from 'vitest';
import { flushSync } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { makeItem } from '../../test/helpers/chat';

function patch(pane: ReturnType<typeof createThreadPane>, itemId: string, summary: string): void {
  pane.applyItemPatch({
    threadId: 'thread-1',
    itemId,
    kind: 'assistant_text',
    patch: { summary, updatedAt: 2 },
  });
}

describe('thread pane item boxes', () => {
  it('a row reader wakes for its own row only; the array signal stays silent for in-place writes', () => {
    const pane = createThreadPane();
    pane.upsertItems([
      makeItem({ id: 'a', kind: 'assistant_text', status: 'streaming', summary: 'a1' }),
      makeItem({ id: 'b', kind: 'assistant_text', status: 'streaming', summary: 'b1' }),
    ]);

    let aRuns = 0;
    let arrayRuns = 0;
    let seenA: string | undefined;
    const dispose = $effect.root(() => {
      const rowA = $derived(pane.getItemById('a'));
      const window = $derived(pane.items);
      $effect(() => {
        aRuns += 1;
        seenA = rowA?.summary;
      });
      $effect(() => {
        arrayRuns += 1;
        void window;
      });
    });
    try {
      flushSync();
      expect(aRuns).toBe(1);
      expect(arrayRuns).toBe(1);
      expect(seenA).toBe('a1');

      const windowBefore = pane.items;
      patch(pane, 'b', 'b2');
      flushSync();
      expect(aRuns).toBe(1);
      expect(arrayRuns).toBe(1);
      expect(pane.items).toBe(windowBefore);
      expect(pane.getItemById('b')?.summary).toBe('b2');

      patch(pane, 'a', 'a2');
      flushSync();
      expect(aRuns).toBe(2);
      expect(arrayRuns).toBe(1);
      expect(seenA).toBe('a2');

      // A batch that lands a NEW row replaces the array (structure), and
      // that is exactly the write that used to wake every row: the deep
      // proxy handed each reader a fresh Item proxy per batch. Row a's
      // reader must not run, and its row must keep its identity.
      const rowABefore = pane.getItemById('a');
      pane.upsertItem(makeItem({ id: 'c', kind: 'assistant_text', status: 'streaming', summary: 'c1' }));
      flushSync();
      expect(arrayRuns).toBe(2);
      expect(aRuns).toBe(2);
      expect(pane.getItemById('a')).toBe(rowABefore);
    } finally {
      dispose();
    }
  });

  it('a reader of a row that is not loaded yet wakes when it lands, and again when it leaves', () => {
    const pane = createThreadPane();
    pane.upsertItem(makeItem({ id: 'a', kind: 'assistant_text', status: 'completed', summary: 'a1' }));

    let seen: string | undefined | null = null;
    const dispose = $effect.root(() => {
      const rowC = $derived(pane.getItemById('c'));
      $effect(() => {
        seen = rowC?.summary;
      });
    });
    try {
      flushSync();
      expect(seen).toBeUndefined();

      pane.upsertItem(makeItem({ id: 'c', kind: 'assistant_text', status: 'completed', summary: 'c1' }));
      flushSync();
      expect(seen).toBe('c1');

      pane.clear();
      flushSync();
      expect(seen).toBeUndefined();
      expect(pane.getItemById('a')).toBeUndefined();
    } finally {
      dispose();
    }
  });
});
