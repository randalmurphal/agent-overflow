// The TypeScript half of the shared window-digest contract
// (docs/architecture/thread-replica-sync.md §3.4, §5). The vectors below
// are the same file the Go test reads
// (internal/store/testdata/window_digest_vectors.json, kept byte-identical
// by a Go test), so a change to either implementation that does not
// change the other fails here.
import { describe, expect, it } from 'vitest';
import vectors from '../../test/fixtures/windowDigestVectors.json';
import {
  MAX_HELD_WINDOW_ITEMS,
  UNSTAMPED_ITEM_REV,
  heldWindowOf,
  isWindowedTimelineRow,
  windowDigest,
} from './threadWindowDigest';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';

function row(id: string, rev: number, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, rev, itemIndex: 0, ...overrides });
}

describe('windowDigest', () => {
  it('declares the algorithm the Go side implements', () => {
    expect(vectors.algorithm).toBe('fnv1a64');
  });

  it('agrees with the shared vectors on the held-window cap', () => {
    // The cap is the server's refusal threshold, so a client that
    // described a larger window would spend the round trip for nothing.
    expect(MAX_HELD_WINDOW_ITEMS).toBe(vectors.maxHeldWindowItems);
  });

  for (const testCase of vectors.cases) {
    it(`matches the shared vector: ${testCase.name}`, () => {
      expect(windowDigest(testCase.rows)).toBe(testCase.digest);
    });
  }

  it('produces 16 lowercase hex characters', () => {
    for (const testCase of vectors.cases) {
      expect(windowDigest(testCase.rows)).toMatch(/^[0-9a-f]{16}$/);
    }
  });

  it('separates the id from the rev, so no two windows share a canonical string', () => {
    // Without the field separator `ab` + `1` and `a` + `b1` would fold
    // identically; without the record separator so would one row `a`/`11`
    // and two rows `a`/`1`, `` /`1`.
    expect(windowDigest([{ id: 'ab', rev: 1 }])).not.toBe(windowDigest([{ id: 'a', rev: 0 }]));
    expect(windowDigest([{ id: 'a', rev: 11 }])).not.toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: '', rev: 1 }]),
    );
  });

  it('folds a rev change and only a rev change', () => {
    const base = [{ id: 'i0', rev: 4 }, { id: 'i1', rev: 4 }];
    expect(windowDigest(base)).toBe(windowDigest([{ id: 'i0', rev: 4 }, { id: 'i1', rev: 4 }]));
    expect(windowDigest(base)).not.toBe(
      windowDigest([{ id: 'i0', rev: 4 }, { id: 'i1', rev: 5 }]),
    );
  });

  it('hashes a thousand rows without losing 64-bit width', () => {
    const rows = Array.from({ length: 1000 }, (_, index) => ({
      id: `item-${index}`,
      rev: index * 7,
    }));
    const digest = windowDigest(rows);
    expect(digest).toMatch(/^[0-9a-f]{16}$/);
    // The top half must actually carry entropy: a broken carry between
    // the two 32-bit halves shows up as a constant high word.
    expect(digest.slice(0, 8)).not.toBe(windowDigest(rows.slice(0, 999)).slice(0, 8));
  });
});

describe('isWindowedTimelineRow', () => {
  it('keeps ordinary top-level rows', () => {
    expect(isWindowedTimelineRow(row('i0', 1))).toBe(true);
    expect(isWindowedTimelineRow(row('i0', 1, { kind: 'notification' }))).toBe(true);
    expect(isWindowedTimelineRow(row('i0', 1, { toolName: 'plan_update' }))).toBe(true);
  });

  it('drops subagent children and plan_update notifications', () => {
    expect(isWindowedTimelineRow(row('c0', 1, { parentId: 'anchor' }))).toBe(false);
    expect(
      isWindowedTimelineRow(row('p0', 1, { kind: 'notification', toolName: 'plan_update' })),
    ).toBe(false);
  });
});

describe('heldWindowOf', () => {
  it('describes the window by its edges, count and digest', () => {
    const items = [row('i0', 3), row('i1', 4, { itemIndex: 1 }), row('i2', 9, { itemIndex: 2 })];
    expect(heldWindowOf(items, false, true)).toEqual({
      oldestItemId: 'i0',
      newestItemId: 'i2',
      count: 3,
      hasMoreOlder: false,
      hasMoreNewer: true,
      digest: windowDigest([
        { id: 'i0', rev: 3 },
        { id: 'i1', rev: 4 },
        { id: 'i2', rev: 9 },
      ]),
    });
  });

  it('excludes the rows a page would not return, edges included', () => {
    const items = [
      row('p0', 2, { kind: 'notification', toolName: 'plan_update' }),
      row('i0', 3, { itemIndex: 1 }),
      row('c0', 4, { itemIndex: 2, parentId: 'i0' }),
      row('i1', 5, { itemIndex: 3 }),
      row('p1', 6, { itemIndex: 4, kind: 'notification', toolName: 'plan_update' }),
    ];
    const held = heldWindowOf(items, true, false);
    expect(held).toMatchObject({ oldestItemId: 'i0', newestItemId: 'i1', count: 2 });
    expect(held?.digest).toBe(windowDigest([{ id: 'i0', rev: 3 }, { id: 'i1', rev: 5 }]));
  });

  it('returns null when no row survives the filter', () => {
    expect(heldWindowOf([], false, false)).toBeNull();
    expect(
      heldWindowOf([row('p0', 1, { kind: 'notification', toolName: 'plan_update' })], false, false),
    ).toBeNull();
  });

  it('still describes a window holding an imported (-1) row', () => {
    // The server refuses it — imported history carries no per-row stamp —
    // and that refusal costs one page, which is the same answer the pane
    // would have got by sending nothing. Suppressing it here would be a
    // silent special case with the same outcome and one more branch.
    const held = heldWindowOf(
      [row('imported', UNSTAMPED_ITEM_REV), row('i1', 4, { itemIndex: 1 })],
      false,
      false,
    );
    expect(held).toMatchObject({ count: 2, oldestItemId: 'imported' });
    expect(held?.digest).toBe(
      windowDigest([{ id: 'imported', rev: UNSTAMPED_ITEM_REV }, { id: 'i1', rev: 4 }]),
    );
  });

  it('refuses a window past the cap the server would verify', () => {
    // The pane's retention ceiling (2400) is above the cap, so this is a
    // window the pane can genuinely hold; describing it would only earn
    // an unverified refusal.
    const items = Array.from({ length: MAX_HELD_WINDOW_ITEMS + 1 }, (_, index) =>
      row(`i${index}`, 1, { itemIndex: index }),
    );
    expect(heldWindowOf(items, false, false)).toBeNull();
    expect(heldWindowOf(items.slice(0, MAX_HELD_WINDOW_ITEMS), false, false)).toMatchObject({
      count: MAX_HELD_WINDOW_ITEMS,
    });
  });
});
