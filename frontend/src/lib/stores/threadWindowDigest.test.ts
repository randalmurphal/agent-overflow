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
  NO_HELD_RUNS,
  UNSTAMPED_ITEM_REV,
  heldWindowOf,
  isWindowedTimelineRow,
  windowDigest,
} from './threadWindowDigest';
import { FNV1A64_ZERO, formatFnv1a64, parseFnv1a64, xorFnv1a64 } from '../utils/fnv1a';
import { makeItem } from '../../test/helpers/chat';
import type { Item } from '../types/models';

function row(id: string, rev: number, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, rev, itemIndex: 0, ...overrides });
}

describe('windowDigest', () => {
  it('declares the algorithm the Go side implements', () => {
    expect(vectors.algorithm).toBe('fnv1a64-xor');
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

  it('separates the id from the rev inside one row', () => {
    // Per ROW now, not per window: the fold is an XOR of independent row
    // hashes, so the separator is what keeps `ab`+`1` from hashing as
    // `a`+`b1`. There is no record separator — that is what makes the
    // digest composable across loaded rows, shed rows and stubs (§5).
    expect(windowDigest([{ id: 'ab', rev: 1 }])).not.toBe(windowDigest([{ id: 'a', rev: 0 }]));
    expect(windowDigest([{ id: 'a', rev: 11 }])).not.toBe(
      windowDigest([{ id: 'a', rev: 1 }, { id: '', rev: 1 }]),
    );
  });

  it('is order-free and composable', () => {
    // The two properties the XOR fold buys, and the reason the client can
    // add a run's UnshippedDigest to the rows it holds without knowing
    // where those members sort.
    const rows = [{ id: 'a', rev: 1 }, { id: 'b', rev: 2 }, { id: 'c', rev: 3 }];
    expect(windowDigest([...rows].reverse())).toBe(windowDigest(rows));
    const parts = [
      parseFnv1a64(windowDigest(rows.slice(0, 1)))!,
      parseFnv1a64(windowDigest(rows.slice(1)))!,
    ];
    expect(formatFnv1a64(xorFnv1a64(parts[0], parts[1]))).toBe(windowDigest(rows));
  });

  it('folds the empty window to zero, so an absent part contributes nothing', () => {
    expect(windowDigest([])).toBe('0000000000000000');
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
    expect(heldWindowOf(items, false, true, NO_HELD_RUNS)).toEqual({
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
    const held = heldWindowOf(items, true, false, NO_HELD_RUNS);
    expect(held).toMatchObject({ oldestItemId: 'i0', newestItemId: 'i1', count: 2 });
    expect(held?.digest).toBe(windowDigest([{ id: 'i0', rev: 3 }, { id: 'i1', rev: 5 }]));
  });

  it('returns null when no row survives the filter', () => {
    expect(heldWindowOf([], false, false, NO_HELD_RUNS)).toBeNull();
    expect(
      heldWindowOf(
        [row('p0', 1, { kind: 'notification', toolName: 'plan_update' })],
        false,
        false,
        NO_HELD_RUNS,
      ),
    ).toBeNull();
  });

  it('describes no window when a row carries no revision', () => {
    // Restated from "describes it anyway and lets the server refuse":
    // with runs folded in, a stub's UnshippedDigest is composed from the
    // same rows, and an unstamped row makes the composition a claim the
    // server cannot check. Refusing here costs the same one page the
    // server's refusal cost, without spending the round trip.
    expect(
      heldWindowOf(
        [row('imported', UNSTAMPED_ITEM_REV), row('i1', 4, { itemIndex: 1 })],
        false,
        false,
        NO_HELD_RUNS,
      ),
    ).toBeNull();
  });

  it('describes no window when the pane cannot state its runs', () => {
    expect(heldWindowOf([row('i0', 3)], false, false, null)).toBeNull();
  });

  it('folds a held run into the count and the digest', () => {
    // The run's members the pane does NOT hold are physical rows of the
    // window all the same (§5): they count, and their digest XORs in.
    const items = [row('i0', 3), row('i1', 4, { itemIndex: 1 })];
    const unshipped = parseFnv1a64(windowDigest([{ id: 'u0', rev: 2 }]))!;
    const held = heldWindowOf(items, false, false, { count: 1, digest: unshipped });
    expect(held).toMatchObject({ count: 3, oldestItemId: 'i0', newestItemId: 'i1' });
    expect(held?.digest).toBe(
      windowDigest([{ id: 'i0', rev: 3 }, { id: 'i1', rev: 4 }, { id: 'u0', rev: 2 }]),
    );
  });

  it('counts a run\'s unheld members against the cap', () => {
    const items = Array.from({ length: 10 }, (_, index) =>
      row(`i${index}`, 1, { itemIndex: index }),
    );
    expect(
      heldWindowOf(items, false, false, {
        count: MAX_HELD_WINDOW_ITEMS - 9,
        digest: FNV1A64_ZERO,
      }),
    ).toBeNull();
  });

  it('refuses a window past the cap the server would verify', () => {
    // The pane's retention ceiling (2400) is above the cap, so this is a
    // window the pane can genuinely hold; describing it would only earn
    // an unverified refusal.
    const items = Array.from({ length: MAX_HELD_WINDOW_ITEMS + 1 }, (_, index) =>
      row(`i${index}`, 1, { itemIndex: index }),
    );
    expect(heldWindowOf(items, false, false, NO_HELD_RUNS)).toBeNull();
    expect(heldWindowOf(items.slice(0, MAX_HELD_WINDOW_ITEMS), false, false, NO_HELD_RUNS)).toMatchObject({
      count: MAX_HELD_WINDOW_ITEMS,
    });
  });
});
