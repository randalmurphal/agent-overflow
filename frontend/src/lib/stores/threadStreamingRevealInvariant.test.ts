// The reveal invariant and its tripwire.
//
// Rule: while a smoother owns an assistant row, the row's published text
// IS that smoother's reveal cursor. Reconciliation may leave it there,
// hand the smoother a longer suffix to drain, or hand ownership over
// with a summary that WINS the row. It may never publish text that
// rewinds behind the cursor.
//
// Five separate bugs in the 2026-08-28/29 perf session were instances of
// that one rule (aad27067 is the latest: the final assistant answer
// froze at ~130 of 1021 chars because a fold eviction inside the drain
// window disposed the smoother and republished its partial summary).
// This suite pins the rewind SHAPE — a stale snapshot that prefixes the
// cursor arriving as a wholesale commit — on both sides of the drain,
// and proves the dev-mode tripwire fires when the chokepoint gets it
// wrong.
import { afterEach, describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import { makeItem } from '../../test/helpers/chat';
import { __setSmoothingClockForTest } from './threadPaneShared';
import {
  assertRevealCursorNotRewound,
  createThreadStreamingReveal,
} from './threadStreamingReveal.svelte';

class FakeSmoothingClock implements SmoothingClock {
  private nextHandle = 1;
  private readonly pending = new Map<number, () => void>();
  private current = 0;

  now(): number {
    return this.current;
  }

  schedule(callback: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, callback);
    return handle;
  }

  cancel(handle: number): void {
    this.pending.delete(handle);
  }

  tick(ms: number): void {
    this.current += ms;
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const callback of pending) callback();
  }
}

function makeReveal(initialItems: Item[]) {
  let items = initialItems;
  const indexById = new Map(items.map((item, index) => [item.id, index]));
  const reveal = createThreadStreamingReveal({
    getItemById: (itemId) => {
      const index = indexById.get(itemId);
      return index === undefined ? undefined : items[index];
    },
    getItemIndex: (itemId) => indexById.get(itemId),
    getItems: () => items,
    setItemAt: (index, item) => {
      const next = items.slice();
      next[index] = item;
      items = next;
    },
    appendDirectAssistantLiteral: (index, _itemId, append, updatedAt) => {
      const current = items[index];
      current.summary = append.next;
      current.updatedAt = Math.max(current.updatedAt, updatedAt);
    },
    stampLiveContent: () => {},
    armStructuralSpring: () => {},
    appendLivePayloadDeltaForItem: () => {},
  });
  return {
    reveal,
    getItems: () => items,
    commit: (prepared: Item) => {
      const index = indexById.get(prepared.id);
      if (index === undefined) throw new Error('test committed an unknown row');
      const next = items.slice();
      next[index] = prepared;
      items = next;
    },
  };
}

// Word-aligned so the smoother lands on exact code-unit counts: the
// incident's numbers scaled down to 26 revealed chars behind a 46-char
// cursor.
const SNAPSHOT_26 = 'aaaa bbbb cccc dddd eeeee ';
const SUFFIX_20 = 'ffff gggg hhhh iiii ';
const CURSOR_46 = SNAPSHOT_26 + SUFFIX_20;
const UNREVEALED_TAIL = 'jjjj kkkk llll mmmm nnnn oooo pppp qqqq rrrr ssss '.repeat(4);

/** Drain until the row's summary reaches `length`, or fail loudly. */
function drainTo(
  clock: FakeSmoothingClock,
  read: () => string,
  length: number,
): void {
  for (let frame = 0; frame < 500 && read().length < length; frame++) {
    clock.tick(16);
  }
  expect(read()).toHaveLength(length);
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
});

describe('assertRevealCursorNotRewound', () => {
  it('fires when a publish falls behind the cursor', () => {
    expect(() => assertRevealCursorNotRewound('text', CURSOR_46, SNAPSHOT_26))
      .toThrow(/reveal invariant violated for text/);
    expect(() => assertRevealCursorNotRewound('text', CURSOR_46, SNAPSHOT_26))
      .toThrow(/26 chars behind the smoother cursor at 46/);
  });

  it('passes a publish that sits exactly on the cursor', () => {
    expect(() => assertRevealCursorNotRewound('text', CURSOR_46, CURSOR_46))
      .not.toThrow();
  });

  it('passes a publish that snaps forward past the cursor', () => {
    expect(() =>
      assertRevealCursorNotRewound('text', CURSOR_46, CURSOR_46 + UNREVEALED_TAIL),
    ).not.toThrow();
  });

  it('passes a genuinely divergent authoritative overwrite', () => {
    // Shorter, but NOT a prefix: an overwrite the row is meant to take.
    expect(() => assertRevealCursorNotRewound('text', CURSOR_46, 'corrected answer'))
      .not.toThrow();
  });
});

describe('reveal invariant at the reconciliation chokepoint', () => {
  it('keeps the cursor when a stale snapshot lands after the drain finished', () => {
    // The aad27067 sibling: same stale-prefix snapshot, but the smoother
    // happened to catch up first, so the settle branch ran and handed the
    // row to the shorter summary. Visible text went 46 -> 26.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems, commit } = makeReveal([item]);

    reveal.appendStreamingDelta(item.id, '', SNAPSHOT_26, 1);
    drainTo(clock, () => getItems()[0].summary, SNAPSHOT_26.length);
    reveal.appendStreamingDelta(item.id, SNAPSHOT_26, SUFFIX_20, 2);
    drainTo(clock, () => getItems()[0].summary, CURSOR_46.length);

    const [prepared] = reveal.prepareItemReplacements([{
      ...getItems()[0],
      status: 'completed',
      summary: SNAPSHOT_26,
      updatedAt: 3,
    }]);

    expect(prepared.status).toBe('completed');
    expect(prepared.summary).toBe(CURSOR_46);
    commit(prepared);
    expect(getItems()[0].summary).toBe(CURSOR_46);
  });

  it('keeps the drain alive when a stale snapshot lands mid-drain', () => {
    // aad27067 itself: terminal row, smoother still holding an unrevealed
    // backlog. The replacement must neither truncate nor strand the row.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems, commit } = makeReveal([item]);
    const full = CURSOR_46 + UNREVEALED_TAIL;

    reveal.appendStreamingDelta(item.id, '', SNAPSHOT_26, 1);
    drainTo(clock, () => getItems()[0].summary, SNAPSHOT_26.length);
    reveal.appendStreamingDelta(item.id, SNAPSHOT_26, SUFFIX_20 + UNREVEALED_TAIL, 2);
    drainTo(clock, () => getItems()[0].summary, CURSOR_46.length);
    expect(getItems()[0].summary.length).toBeLessThan(full.length);

    const [prepared] = reveal.prepareItemReplacements([{
      ...getItems()[0],
      status: 'completed',
      summary: SNAPSHOT_26,
      updatedAt: 3,
    }]);
    expect(prepared.summary).toBe(CURSOR_46);
    commit(prepared);
    expect(reveal.smootherCount()).toBe(1);

    // And the drain still reaches the full text, monotonically.
    let previousLength = CURSOR_46.length;
    for (let frame = 0; frame < 500 && reveal.smootherCount() > 0; frame++) {
      clock.tick(16);
      const length = getItems()[0].summary.length;
      expect(length).toBeGreaterThanOrEqual(previousLength);
      previousLength = length;
    }
    expect(getItems()[0].summary).toBe(full);
  });

  it('lets a divergent terminal summary win the row outright', () => {
    // The one legitimate handover: snap forward, never truncate.
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const item = makeItem({ id: 'text', status: 'streaming', summary: '' });
    const { reveal, getItems, commit } = makeReveal([item]);

    reveal.appendStreamingDelta(item.id, '', CURSOR_46 + UNREVEALED_TAIL, 1);
    drainTo(clock, () => getItems()[0].summary, CURSOR_46.length);

    const corrected = '[interrupted] the model rewrote this answer entirely.';
    const [prepared] = reveal.prepareItemReplacements([{
      ...getItems()[0],
      status: 'completed',
      summary: corrected,
      updatedAt: 3,
    }]);

    expect(prepared.summary).toBe(corrected);
    expect(reveal.smootherCount()).toBe(0);
    commit(prepared);
    expect(getItems()[0].summary).toBe(corrected);
  });
});
