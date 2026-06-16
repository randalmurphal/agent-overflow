// Regression tripwire for virtua's `shift` size-cache semantics — the
// external assumption the timeline's load-jank fix rests on.
//
// MessageTimeline binds `<Virtualizer shift={pane.pendingTimelineShiftAtHead}>`
// and the thread store sets that flag to "this length change is at the HEAD".
// The whole point is that on a prepend, virtua must UNSHIFT its size cache
// (so every already-measured row stays aligned) instead of appending fresh
// slots at the tail (which misindexes the entire cache and forces a
// re-measure of every visible row — the "scrollbar jumps around" jank).
//
// happy-dom reports zero geometry, so the rendered-component tests can't
// exercise this. Instead we drive the REAL installed core store directly:
// the cache resize is pure array logic that needs no layout. If a virtua
// upgrade changes these semantics, this fails loudly and the store's
// shift-direction wiring must be re-validated (see the spike write-up in
// docs/architecture/frontend-scroll.md).
//
// Coupling note: seeding measured sizes uses the cache-snapshot constructor
// arg ([sizes, estimate], the shape `$getCacheSnapshot()` returns) because
// the per-item resize action is not part of the public core export. That ties
// this test to the installed virtua version — which is intentional: it should
// break on a version bump so the assumption gets re-checked.
import { describe, it, expect } from 'vitest';
import {
  createVirtualStore,
  ACTION_ITEMS_LENGTH_CHANGE,
} from 'virtua/unstable_core';

const ESTIMATE = 56;

interface CoreStore {
  $getItemsLength(): number;
  $getItemSize(index: number): number;
  $isUnmeasuredItem(index: number): boolean;
  $update(action: number, payload: unknown): void;
}

function storeWithSizes(sizes: number[]): CoreStore {
  // (count, itemSize, ssrCount, cache=[sizes, estimate], autoEstimate)
  return (
    createVirtualStore as unknown as (
      count: number,
      itemSize: number,
      ssrCount: number,
      cache: [number[], number],
    ) => CoreStore
  )(sizes.length, ESTIMATE, 0, [sizes.slice(), ESTIMATE]);
}

// 'U' for an unmeasured slot (renders at the estimate), else the measured size.
function rows(store: CoreStore): Array<number | 'U'> {
  return Array.from({ length: store.$getItemsLength() }, (_, i) =>
    store.$isUnmeasuredItem(i) ? 'U' : store.$getItemSize(i),
  );
}

function changeLength(store: CoreStore, newLength: number, shift: boolean): void {
  store.$update(ACTION_ITEMS_LENGTH_CHANGE, [newLength, shift]);
}

describe('virtua shift cache semantics (load-jank fix assumption)', () => {
  it('head-grow with shift=true unshifts the cache — existing rows stay aligned', () => {
    const store = storeWithSizes([10, 20, 30, 40, 50]);
    changeLength(store, 8, true); // prepend 3 at the head (loadOlder)
    expect(rows(store)).toEqual(['U', 'U', 'U', 10, 20, 30, 40, 50]);
  });

  it('head-grow with shift=false misindexes the cache (the bug being fixed)', () => {
    const store = storeWithSizes([10, 20, 30, 40, 50]);
    changeLength(store, 8, false); // 3 conceptually prepended, but appended
    // New head rows inherit OLD sizes; the unmeasured slots land at the tail.
    expect(rows(store)).toEqual([10, 20, 30, 40, 50, 'U', 'U', 'U']);
  });

  it('head-shrink with shift=true splices from the front (loadNewer head-prune)', () => {
    const store = storeWithSizes([11, 12, 13, 14, 15, 16, 17, 18]);
    changeLength(store, 5, true); // drop 3 from the head
    expect(rows(store)).toEqual([14, 15, 16, 17, 18]);
  });

  it('tail-shrink with shift=false splices from the end (loadOlder tail-prune)', () => {
    const store = storeWithSizes([11, 12, 13, 14, 15, 16, 17, 18]);
    changeLength(store, 5, false); // drop 3 from the tail
    expect(rows(store)).toEqual([11, 12, 13, 14, 15]);
  });

  it('a prepend + tail-prune kept in SEPARATE length changes stays aligned', () => {
    // Why the store splits the two across flushes: done as two single-ended
    // length changes the cache is perfect; coalesced into one net change it
    // cannot be (a single `shift` can describe only one end).
    const store = storeWithSizes([11, 12, 13, 14, 15, 16, 17, 18]);
    changeLength(store, 11, true); // flush 1: prepend 3 at the head
    expect(rows(store)).toEqual(['U', 'U', 'U', 11, 12, 13, 14, 15, 16, 17, 18]);
    changeLength(store, 8, false); // flush 2: drop 3 from the tail
    expect(rows(store)).toEqual(['U', 'U', 'U', 11, 12, 13, 14, 15]);
  });
});
