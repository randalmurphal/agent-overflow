// A streaming row's text is placed by stream position (`Item.streamEnd`,
// `ItemDeltaEvent.offset`): a delta the row already holds is dropped, one
// past its text is held until a read reaches it, a read of the row is
// compared with the received text by position, and a settle patch's
// revision is adopted only by a row whose text ends where the stored row's
// does.
import { afterEach, describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import type { ItemPatchEvent } from '../types/events';
import { makeItem } from '../../test/helpers/chat';
import { createThreadStreamingReveal } from './threadStreamingReveal.svelte';
import { __setSmoothingClockForTest } from './threadPaneShared';
import type { SmoothingClock } from '../markdown/smoothing/PerItemSmoother';
import { utf8Length } from '../utils/utf8Offsets';

class FakeSmoothingClock implements SmoothingClock {
  private nextHandle = 1;
  private readonly pending = new Map<number, () => void>();
  private current = 0;
  now(): number { return this.current; }
  schedule(callback: () => void): number {
    const handle = this.nextHandle++;
    this.pending.set(handle, callback);
    return handle;
  }
  cancel(handle: number): void { this.pending.delete(handle); }
  tick(ms: number): void {
    this.current += ms;
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const callback of pending) callback();
  }
}

function harness(initial: Item[]) {
  let items = initial;
  let indexById = new Map(items.map((item, index) => [item.id, index]));
  const gaps: string[] = [];
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
    appendDirectAssistantLiteral: (index, _itemId, append, updatedAt, streamEnd) => {
      const current = items[index];
      current.summary = append.next;
      current.streamEnd = streamEnd;
      current.updatedAt = Math.max(current.updatedAt, updatedAt);
    },
    stampLiveContent: () => {},
    armStructuralSpring: () => {},
    appendLivePayloadDeltaForItem: () => {},
    onStreamGap: (itemId) => gaps.push(itemId),
  });
  const row = (id = 'r') => items[indexById.get(id)!];
  return {
    reveal,
    gaps,
    row,
    delta(delta: string, offset: number | undefined, id = 'r', updatedAt = 10) {
      reveal.appendStreamingDelta(row(id), delta, offset, updatedAt);
    },
    patch(patch: ItemPatchEvent['patch'], id = 'r') {
      reveal.applyPatch(id, patch);
    },
    commit(next: Item[]) {
      reveal.withReconciledItems(next, (prepared) => {
        items = prepared;
        indexById = new Map(prepared.map((item, index) => [item.id, index]));
      });
    },
  };
}

function streaming(overrides: Partial<Item>): Item {
  return makeItem({ id: 'r', kind: 'assistant_text', status: 'streaming', summary: '', streamEnd: 0, rev: -1, ...overrides });
}

afterEach(() => {
  __setSmoothingClockForTest(undefined);
});

describe('positioned deltas', () => {
  it('drops text the row holds and appends only the unseen part of an overlap', () => {
    const h = harness([streaming({ summary: 'w1 w2 ', streamEnd: 6 })]);
    h.delta('w2 w3 ', 3);
    h.delta('w1 ', 0);
    h.delta('w3 ', 6);
    h.delta('w4 ', 9);
    h.reveal.snapAllToReceived();
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 w4 ', streamEnd: 12 });
    expect(h.gaps).toEqual([]);
  });

  it('publishes where the revealed text ends as the row stream end', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([streaming({})]);
    const text = Array.from({ length: 40 }, (_, i) => `wörd${i} 日本 `).join('');
    h.delta(text, 0);
    let checked = 0;
    for (let frame = 0; frame < 2_000 && h.row().summary !== text; frame++) {
      clock.tick(16);
      if (h.row().summary.length > 0 && h.row().summary !== text) {
        expect(h.row().streamEnd).toBe(utf8Length(h.row().summary));
        checked++;
      }
    }
    expect(checked).toBeGreaterThan(2);
    expect(h.row()).toMatchObject({ summary: text, streamEnd: utf8Length(text) });
  });

  it('holds a delta past the row text until a read reaches it, then places it', () => {
    const h = harness([streaming({ kind: 'thinking', summary: 'w1 ', streamEnd: 3 })]);
    h.delta('w4 ', 9);
    h.delta('w5 ', 12);
    expect(h.gaps).toEqual(['r', 'r']);
    expect(h.row()).toMatchObject({ summary: 'w1 ', streamEnd: 3 });
    expect(h.reveal.hasStreamGaps()).toBe(true);

    h.commit([streaming({ kind: 'thinking', summary: 'w1 w2 w3 w4 ', streamEnd: 12, rev: 5 })]);
    h.reveal.snapAllToReceived();
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 w4 w5 ', streamEnd: 15 });
    expect(h.reveal.hasStreamGaps()).toBe(false);
  });

  it('keeps holding a delta a read does not reach', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3 })]);
    h.delta('w9 ', 24);
    h.commit([streaming({ summary: 'w1 w2 ', streamEnd: 6 })]);
    expect(h.reveal.hasStreamGaps()).toBe(true);
    h.delta('w3 ', 6);
    h.reveal.snapAllToReceived();
    expect(h.row().summary).toBe('w1 w2 w3 ');
  });

  it('drops held deltas when the row settles or leaves the window', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3 }), streaming({ id: 'q', itemIndex: 2, summary: 'a', streamEnd: 1 })]);
    h.delta('w4 ', 9);
    h.delta('z', 5, 'q');
    h.patch({ status: 'completed', streamEnd: 3, rev: 4 });
    h.reveal.disposeSmoothersForItems([{ id: 'q' }]);
    expect(h.reveal.hasStreamGaps()).toBe(false);
  });
});

describe('settle patch revision', () => {
  it('adopts the revision of a settle that ends where the row does', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3 })]);
    h.delta('w2 ', 3);
    h.patch({ status: 'completed', streamEnd: 6, updatedAt: 20, rev: 7 });
    h.reveal.__flushForTest();
    expect(h.row()).toMatchObject({ status: 'completed', rev: 7, streamEnd: undefined, summary: 'w1 w2 ' });
    expect(h.reveal.staleRowIds().size).toBe(0);
    expect(h.gaps).toEqual([]);
  });

  it('keeps its own revision and marks the row stale when the stored row holds more', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3, rev: -1 })]);
    h.delta('w4 ', 9);
    h.patch({ status: 'completed', streamEnd: 12, updatedAt: 20, rev: 7 });
    expect(h.row()).toMatchObject({ status: 'completed', rev: -1, streamEnd: undefined });
    expect([...h.reveal.staleRowIds()]).toEqual(['r']);
    expect(h.gaps).toEqual(['r', 'r']);
    expect(h.reveal.hasStreamGaps()).toBe(true);

    // The settled read wins the row outright and clears the mark.
    const read = makeItem({ id: 'r', kind: 'assistant_text', status: 'completed', summary: 'w1 w2 w3 w4 ', rev: 7 });
    h.commit([read]);
    expect(h.row()).toBe(read);
    expect(h.reveal.staleRowIds().size).toBe(0);
    expect(h.reveal.hasStreamGaps()).toBe(false);
  });

  it('leaves a row unstamped while its reveal is behind and adopts the revision when it drains', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([streaming({})]);
    const text = Array.from({ length: 60 }, (_, i) => `w${i} `).join('');
    h.delta(text, 0);
    clock.tick(16);
    h.patch({ status: 'completed', streamEnd: utf8Length(text), updatedAt: 20, rev: 7 });
    expect(h.row().summary).not.toBe(text);
    expect(h.row()).toMatchObject({ status: 'completed', rev: -1 });
    for (let frame = 0; frame < 2_000 && h.reveal.isSmoothing('r'); frame++) {
      clock.tick(16);
      if (h.reveal.isSmoothing('r')) expect(h.row().rev).toBe(-1);
    }
    expect(h.row()).toMatchObject({ summary: text, rev: 7 });
  });

  it('leaves a row unstamped when its reveal is disposed before it drains', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([streaming({})]);
    h.delta('w1 w2 w3 w4 w5 w6 ', 0);
    clock.tick(16);
    h.patch({ status: 'completed', streamEnd: 18, rev: 7 });
    h.reveal.disposeAll();
    expect(h.row().summary).not.toBe('w1 w2 w3 w4 w5 w6 ');
    expect(h.row().rev).toBe(-1);
  });

  it('unstamps a settled read committed while the reveal is behind until it drains', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([streaming({})]);
    h.delta('w1 w2 ', 0);
    const read = makeItem({ id: 'r', kind: 'assistant_text', status: 'completed', summary: 'w1 w2 w3 ', rev: 7 });
    h.commit([read]);
    expect(h.row()).toMatchObject({ status: 'completed', summary: '', rev: -1 });
    h.reveal.snapAllToReceived();
    expect(h.reveal.isSmoothing('r')).toBe(false);
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 ', rev: 7 });
  });

  it('does not adopt a revision named before more text arrived', () => {
    const clock = new FakeSmoothingClock();
    __setSmoothingClockForTest(clock);
    const h = harness([streaming({})]);
    h.delta('w1 w2 w3 ', 0);
    h.commit([streaming({ summary: 'w1 w2 w3 ', streamEnd: 9, rev: 5 })]);
    expect(h.row().rev).toBe(-1);
    h.delta('w4 ', 9);
    h.commit([makeItem({ id: 'r', kind: 'assistant_text', status: 'completed', summary: 'w1 w2 w3 ', rev: 5 })]);
    h.reveal.snapAllToReceived();
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 w4 ', rev: -1 });
  });

  it('keeps its own revision when a settled read trails the revealed text', () => {
    const h = harness([streaming({})]);
    h.delta('w1 w2 w3 ', 0);
    h.reveal.snapAllToReceived();
    h.commit([makeItem({ id: 'r', kind: 'assistant_text', status: 'completed', summary: 'w1 w2 ', rev: 7 })]);
    expect(h.row()).toMatchObject({ status: 'completed', summary: 'w1 w2 w3 ', rev: -1 });
  });

  it('keeps a stale row when the pane re-commits the same row', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3 })]);
    h.patch({ status: 'completed', streamEnd: 12, rev: 7 });
    h.commit([h.row()]);
    expect([...h.reveal.staleRowIds()]).toEqual(['r']);
  });
});

describe('positioned reads of a smoothed row', () => {
  it('extends the reveal with the part of a trimmed tail past the received text', () => {
    // The received text is an interior slice (a smoother seeded from a
    // trimmed summary): it ends at 106. The read's tail overlaps it and ends
    // at 108. By text alone the two look unrelated.
    const h = harness([streaming({ kind: 'thinking', summary: 'x y ', streamEnd: 104 })]);
    h.delta('z ', 104);
    h.commit([streaming({ kind: 'thinking', summary: 'z w ', streamEnd: 108 })]);
    expect(h.reveal.isSmoothing('r')).toBe(true);
    h.reveal.snapAllToReceived();
    expect(h.reveal.liveThinkingTailFor('r')).toBe('x y z w ');
    expect(h.row().streamEnd).toBe(108);
  });

  it('keeps the cursor for a read that ends before the received text', () => {
    const h = harness([streaming({ summary: '', streamEnd: 0 })]);
    h.delta('w1 w2 w3 ', 0);
    h.commit([streaming({ summary: 'w1 ', streamEnd: 3, rev: 4 })]);
    h.reveal.snapAllToReceived();
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 ', streamEnd: 9 });
  });

  it('replaces the reveal with a read that starts past the received text', () => {
    const h = harness([streaming({ kind: 'thinking', summary: 'a ', streamEnd: 2 })]);
    h.delta('b ', 2);
    const read = streaming({ kind: 'thinking', summary: 'y z ', streamEnd: 40 });
    h.commit([read]);
    expect(h.reveal.isSmoothing('r')).toBe(false);
    expect(h.row()).toBe(read);
  });
});

describe('positioned reads of an unsmoothed row', () => {
  it('keeps the text, position and revision of a row a read trails', () => {
    const h = harness([streaming({ summary: 'w1 w2 w3 ', streamEnd: 9, rev: 3 })]);
    h.commit([streaming({ summary: 'w1 ', streamEnd: 3, rev: 8, updatedAt: 50 })]);
    expect(h.row()).toMatchObject({ summary: 'w1 w2 w3 ', streamEnd: 9, rev: 3, updatedAt: 50 });
  });

  it('takes a read that holds more of the stream', () => {
    const h = harness([streaming({ summary: 'w1 ', streamEnd: 3 })]);
    const read = streaming({ summary: 'w1 w2 ', streamEnd: 6, rev: 8 });
    h.commit([read]);
    expect(h.row()).toBe(read);
  });
});
