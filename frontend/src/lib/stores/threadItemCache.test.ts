import { describe, expect, it } from 'vitest';
import type { Item } from '../types/models';
import {
  THREAD_ITEM_CACHE_CAP,
  createThreadItemCache,
} from './threadItemCache';

function makeItem(id: string, turnIndex = 0, itemIndex = 0): Item {
  return {
    id,
    threadId: 't',
    turnIndex,
    itemIndex,
    kind: 'assistant_text',
    role: 'assistant',
    status: 'completed',
    summary: id,
    createdAt: turnIndex * 10 + itemIndex,
    updatedAt: turnIndex * 10 + itemIndex,
  };
}

function makeSnapshot(items: Item[]) {
  return {
    items,
    oldestLoadedTurnIndex: items[0]?.turnIndex ?? null,
    hasMoreHistory: false,
    latestSettledTurn: null,
  };
}

describe('threadItemCache', () => {
  it('returns null for unknown threads', () => {
    const cache = createThreadItemCache();
    expect(cache.get('missing')).toBeNull();
  });

  it('round-trips a snapshot', () => {
    const cache = createThreadItemCache();
    const items = [makeItem('a'), makeItem('b', 1)];
    cache.set('t1', makeSnapshot(items));

    const got = cache.get('t1');
    expect(got).not.toBeNull();
    expect(got?.items.map((i) => i.id)).toEqual(['a', 'b']);
    expect(got?.oldestLoadedTurnIndex).toBe(0);
    expect(got?.latestSettledTurn).toBeNull();
    expect(got?.hasMoreHistory).toBe(false);
  });

  it('clones items on write so caller mutations do not poison the cache', () => {
    const cache = createThreadItemCache();
    const items = [makeItem('a')];
    cache.set('t1', makeSnapshot(items));

    items[0].summary = 'mutated-after-set';

    const got = cache.get('t1');
    expect(got?.items[0].summary).toBe('a');
  });

  it('returns the stored reference on get so virtua sees stable identity', () => {
    // Contract: callers (the pane) treat the snapshot as immutable —
    // they reassign `items =` rather than mutating in place. Reusing
    // the stored reference avoids a per-get O(n) clone and lets virtua
    // skip per-row remeasurement when the same snapshot is read twice.
    const cache = createThreadItemCache();
    cache.set('t1', makeSnapshot([makeItem('a')]));

    const first = cache.get('t1');
    const second = cache.get('t1');
    expect(first).not.toBeNull();
    expect(second).toBe(first);
    expect(second!.items).toBe(first!.items);
  });

  it('overwrites an existing entry without leaking the old items array', () => {
    const cache = createThreadItemCache();
    cache.set('t1', makeSnapshot([makeItem('old')]));
    cache.set('t1', makeSnapshot([makeItem('new')]));

    expect(cache.size).toBe(1);
    expect(cache.get('t1')?.items.map((i) => i.id)).toEqual(['new']);
  });

  it('evicts the least-recently-touched entry when capacity is exceeded', () => {
    const cache = createThreadItemCache(3);

    cache.set('a', makeSnapshot([makeItem('a-only')]));
    cache.set('b', makeSnapshot([makeItem('b-only')]));
    cache.set('c', makeSnapshot([makeItem('c-only')]));

    // Touch 'a' so it moves to the end of the LRU order.
    expect(cache.get('a')).not.toBeNull();

    // Adding a fourth pushes 'b' out (now the oldest).
    cache.set('d', makeSnapshot([makeItem('d-only')]));

    expect(cache.get('a')).not.toBeNull();
    expect(cache.get('b')).toBeNull();
    expect(cache.get('c')).not.toBeNull();
    expect(cache.get('d')).not.toBeNull();
    expect(cache.size).toBe(3);
  });

  it('evict() removes a specific entry', () => {
    const cache = createThreadItemCache();
    cache.set('t1', makeSnapshot([makeItem('a')]));
    cache.set('t2', makeSnapshot([makeItem('b')]));

    cache.evict('t1');
    expect(cache.get('t1')).toBeNull();
    expect(cache.get('t2')).not.toBeNull();
    expect(cache.size).toBe(1);
  });

  it('evict() on an unknown thread is a no-op', () => {
    const cache = createThreadItemCache();
    cache.set('t1', makeSnapshot([makeItem('a')]));

    cache.evict('missing');
    expect(cache.get('t1')).not.toBeNull();
    expect(cache.size).toBe(1);
  });

  it('clear() drops every entry', () => {
    const cache = createThreadItemCache();
    cache.set('a', makeSnapshot([makeItem('a-only')]));
    cache.set('b', makeSnapshot([makeItem('b-only')]));

    cache.clear();
    expect(cache.size).toBe(0);
    expect(cache.get('a')).toBeNull();
    expect(cache.get('b')).toBeNull();
  });

  it('coerces invalid capacities to >= 1', () => {
    const cache = createThreadItemCache(0);
    cache.set('a', makeSnapshot([makeItem('a-only')]));
    cache.set('b', makeSnapshot([makeItem('b-only')]));
    expect(cache.size).toBe(1);
    expect(cache.get('a')).toBeNull();
    expect(cache.get('b')).not.toBeNull();
  });

  it('default cap matches the documented constant', () => {
    const cache = createThreadItemCache();
    for (let i = 0; i < THREAD_ITEM_CACHE_CAP + 2; i++) {
      cache.set(`t${i}`, makeSnapshot([makeItem(`item-${i}`)]));
    }
    expect(cache.size).toBe(THREAD_ITEM_CACHE_CAP);
  });

  it('preserves snapshot fields (oldest/latest/hasMore) verbatim across round-trip', () => {
    const cache = createThreadItemCache();
    const settled = {
      turnId: 'turn-9',
      turnIndex: 9,
      startedAt: 100,
      completedAt: 200,
      stopReason: 'end_turn',
      assistantMessageId: 'msg-9',
      tokenUsage: null,
      aborted: false,
      errorMessage: '',
      // SettledTurn requires more fields in the production type, but the
      // cache only round-trips the value — it never inspects the shape.
      // The cast here mirrors how the runtime call site builds it from
      // `turnRowToSettled`.
    } as never;
    cache.set('t1', {
      items: [makeItem('a', 5), makeItem('b', 9)],
      oldestLoadedTurnIndex: 5,
      hasMoreHistory: true,
      latestSettledTurn: settled,
    });

    const got = cache.get('t1')!;
    expect(got.oldestLoadedTurnIndex).toBe(5);
    expect(got.hasMoreHistory).toBe(true);
    expect(got.latestSettledTurn).toBe(settled);
  });

  it('round-trips the virtua row-size cache verbatim', () => {
    // The virtua CacheSnapshot is opaque to us — virtua serializes it
    // as `[number[], number]` (per-row sizes + computed offsets epoch
    // counter) but we must not depend on that shape. The contract here
    // is reference-preserving round-trip: whatever the caller sets is
    // what we return, with no transformation.
    const cache = createThreadItemCache();
    const virtuaCache = [[90, 240, 180], 7] as unknown as never;
    cache.set('t1', {
      items: [makeItem('a')],
      oldestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      latestSettledTurn: null,
      virtuaCache,
    });

    const got = cache.get('t1')!;
    expect(got.virtuaCache).toBe(virtuaCache);
  });

  it('omits virtuaCache when the caller did not provide one', () => {
    // Callers (the pane) pass the field as `undefined` for surfaces
    // without a virtualizer (Discussion's ChannelView uses the same
    // LRU shape but never registers a getter). The stored snapshot
    // must reflect that — replaying `undefined` makes virtua mount
    // fresh, which is what we want for any non-virtualized capture
    // path.
    const cache = createThreadItemCache();
    cache.set('t1', {
      items: [makeItem('a')],
      oldestLoadedTurnIndex: 0,
      hasMoreHistory: false,
      latestSettledTurn: null,
    });

    const got = cache.get('t1')!;
    expect(got.virtuaCache).toBeUndefined();
  });
});
