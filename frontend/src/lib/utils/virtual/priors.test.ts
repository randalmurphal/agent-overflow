import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearAllThreadSizePriorsForTest,
  clearThreadSizePriors,
  createRowEstimate,
  getThreadSizePriors,
  isSizePriorsGeometryKey,
  latestSizePriors,
  peekThreadSizePriorsForTest,
  setSizePriorsStorageAdapter,
  setThreadSizePriors,
  sizePriorsAtGeometry,
  sizePriorsGeometryKey,
  sizePriorsStats,
  MAX_ROWS_PER_BUCKET,
  type SizePriorsBucket,
  type SizePriorsEntry,
  type SizePriorsGeometry,
  type SizePriorsStorageAdapter,
} from './priors';

/** Stand-in for `typographySignature()`; the store only ever compares it. */
const TYPO = 'f15/sgeist/mgeist/c1/w1';

const geom = (width = 800, typography = TYPO): SizePriorsGeometry => ({ width, typography });
const key = (width = 800, typography = TYPO): string =>
  sizePriorsGeometryKey(geom(width, typography));

const bucket = (overrides: Partial<SizePriorsBucket> = {}): SizePriorsBucket => ({
  expansionSig: '',
  rows: new Map([
    ['L:a:completed:2:1', 100],
    ['L:b:completed:2:1', 200],
  ]),
  ...overrides,
});

/** A thread entry holding a single geometry bucket, as one capture produces. */
const entry = (width = 800, overrides: Partial<SizePriorsBucket> = {}): SizePriorsEntry => ({
  byGeometry: new Map([[key(width), bucket(overrides)]]),
});

interface FakeAdapter extends SizePriorsStorageAdapter {
  store: Map<string, SizePriorsEntry>;
  persistCalls: string[];
  removeCalls: string[];
}

function fakeAdapter(): FakeAdapter {
  const store = new Map<string, SizePriorsEntry>();
  const persistCalls: string[] = [];
  const removeCalls: string[] = [];
  return {
    store,
    persistCalls,
    removeCalls,
    load: (threadId) => store.get(threadId),
    persist: (threadId, e) => {
      persistCalls.push(threadId);
      store.set(threadId, e);
    },
    remove: (threadId) => {
      removeCalls.push(threadId);
      store.delete(threadId);
    },
  };
}

beforeEach(() => {
  clearAllThreadSizePriorsForTest();
  setSizePriorsStorageAdapter(undefined);
});

describe('thread size priors store', () => {
  it('setThreadSizePriors replaces the captured geometry bucket wholesale', () => {
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['a', 1], ['b', 2]]) }));
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['c', 3]]) }));
    const stored = sizePriorsAtGeometry(peekThreadSizePriorsForTest('t1')!, geom(800));
    expect(stored?.rows.has('a')).toBe(false);
    expect(stored?.rows.has('b')).toBe(false);
    expect(stored?.rows.get('c')).toBe(3);
  });

  it('keeps one bucket per geometry, so a capture at one leaves the others alone', () => {
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['a', 100]]) }));
    setThreadSizePriors('t1', geom(600), bucket({ rows: new Map([['a', 160]]) }));

    const stored = peekThreadSizePriorsForTest('t1')!;
    expect(sizePriorsAtGeometry(stored, geom(800))?.rows.get('a')).toBe(100);
    expect(sizePriorsAtGeometry(stored, geom(600))?.rows.get('a')).toBe(160);
    expect([...stored.byGeometry.keys()]).toEqual([key(800), key(600)]); // LRU order, newest last
  });

  it('buckets the same width separately per typography signature', () => {
    // A font change rescales every row at an unchanged wrap point, so the
    // two measurement sets must not share a bucket: replaying the old one
    // would feed the engine heights from the old typeface/scale.
    const small = geom(800, 'f13/sgeist/mgeist/c1/w1');
    const large = geom(800, 'f18/sgeist/mgeist/c1/w1');
    setThreadSizePriors('t1', small, bucket({ rows: new Map([['a', 100]]) }));
    setThreadSizePriors('t1', large, bucket({ rows: new Map([['a', 140]]) }));

    const stored = peekThreadSizePriorsForTest('t1')!;
    expect(sizePriorsAtGeometry(stored, small)?.rows.get('a')).toBe(100);
    expect(sizePriorsAtGeometry(stored, large)?.rows.get('a')).toBe(140);
    expect(stored.byGeometry.size).toBe(2);
  });

  it('misses a bucket captured under a different typography signature', () => {
    setThreadSizePriors('t1', geom(800, 'f13/sgeist/mgeist/c1/w1'), bucket());
    const stored = peekThreadSizePriorsForTest('t1')!;
    expect(sizePriorsAtGeometry(stored, geom(800, 'f18/sgeist/mgeist/c1/w1'))).toBeUndefined();
  });

  it('typography changes at one width evict past the per-thread bucket cap', () => {
    // Cycling the font size through four values at a fixed pane width
    // fills the cap exactly as four widths would; the stalest goes.
    const sizes = [13, 14, 15, 16];
    for (const size of sizes) {
      setThreadSizePriors('t1', geom(800, `f${size}/sgeist/mgeist/c1/w1`), bucket());
    }
    const stored = peekThreadSizePriorsForTest('t1')!;
    expect([...stored.byGeometry.keys()]).toEqual(
      sizes.slice(1).map((size) => key(800, `f${size}/sgeist/mgeist/c1/w1`)),
    );
  });

  it('rounds the width into the key so a fractional re-read hits the same bucket', () => {
    setThreadSizePriors('t1', geom(800.4), bucket({ rows: new Map([['a', 100]]) }));
    expect(sizePriorsAtGeometry(peekThreadSizePriorsForTest('t1')!, geom(799.6))?.rows.get('a'))
      .toBe(100);
    expect([...peekThreadSizePriorsForTest('t1')!.byGeometry.keys()]).toEqual([key(800)]);
  });

  it('evicts the least recently captured geometry past the per-thread cap', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    setThreadSizePriors('t1', geom(700), bucket());
    setThreadSizePriors('t1', geom(600), bucket());
    setThreadSizePriors('t1', geom(500), bucket());

    const stored = peekThreadSizePriorsForTest('t1')!;
    expect([...stored.byGeometry.keys()]).toEqual([key(700), key(600), key(500)]);
    expect(sizePriorsAtGeometry(stored, geom(800))).toBeUndefined();
  });

  it('re-capturing a geometry bumps it to LRU-last, so the stalest one is evicted', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    setThreadSizePriors('t1', geom(700), bucket());
    setThreadSizePriors('t1', geom(800), bucket()); // 800 is now the most recent
    setThreadSizePriors('t1', geom(600), bucket());
    setThreadSizePriors('t1', geom(500), bucket());

    expect([...peekThreadSizePriorsForTest('t1')!.byGeometry.keys()]).toEqual([
      key(800), key(600), key(500),
    ]);
  });

  it('latestSizePriors returns the most recently captured bucket', () => {
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['a', 100]]) }));
    setThreadSizePriors('t1', geom(600), bucket({ rows: new Map([['a', 160]]) }));
    expect(latestSizePriors(peekThreadSizePriorsForTest('t1')!)?.rows.get('a')).toBe(160);

    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['a', 101]]) }));
    expect(latestSizePriors(peekThreadSizePriorsForTest('t1')!)?.rows.get('a')).toBe(101);
  });

  it('getThreadSizePriors returns the stored entry on a memory hit', () => {
    setThreadSizePriors('t1', geom(800), bucket());
    expect(getThreadSizePriors('t1')).toEqual(entry(800));
  });

  it('returns undefined for an unknown thread with no adapter installed', () => {
    expect(getThreadSizePriors('nope')).toBeUndefined();
  });

  it('notifies the adapter with the whole thread entry on every set', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    setThreadSizePriors('t1', geom(800), bucket());
    setThreadSizePriors('t1', geom(600), bucket());
    // One thread is one persisted value: the second capture hands the
    // adapter both buckets, not just the width that changed.
    expect(adapter.persistCalls).toEqual(['t1', 't1']);
    expect([...adapter.store.get('t1')!.byGeometry.keys()]).toEqual([key(800), key(600)]);
  });

  it('extends the stored entry after a memory eviction rather than truncating it', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    setThreadSizePriors('t1', geom(800), bucket());
    clearAllThreadSizePriorsForTest(); // memory only; the adapter still holds t1

    // The capture path reads before it writes, and the read is what
    // rehydrates the evicted entry — `setThreadSizePriors` merges into
    // whatever the in-memory LRU holds, so the stored buckets survive.
    getThreadSizePriors('t1');
    setThreadSizePriors('t1', geom(600), bucket());
    expect([...adapter.store.get('t1')!.byGeometry.keys()]).toEqual([key(800), key(600)]);
  });

  it('clears a single thread from memory AND the adapter', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    setThreadSizePriors('t1', geom(800), bucket());
    clearThreadSizePriors('t1');
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
    expect(adapter.removeCalls).toEqual(['t1']);
  });

  it('falls back to the adapter on a memory miss and installs the result into the LRU', () => {
    const adapter = fakeAdapter();
    adapter.store.set('t1', entry(999));
    setSizePriorsStorageAdapter(adapter);

    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined(); // memory miss
    const result = getThreadSizePriors('t1');
    expect(sizePriorsAtGeometry(result!, geom(999))).toBeDefined();
    expect(peekThreadSizePriorsForTest('t1')).toEqual(result); // now in memory
  });

  it('returns undefined when neither memory nor the adapter has the thread', () => {
    setSizePriorsStorageAdapter(fakeAdapter());
    expect(getThreadSizePriors('nope')).toBeUndefined();
  });

  it('evicts the least recently used memory entry past the cap without touching the adapter', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
    }
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();

    setThreadSizePriors('t50', geom(800), bucket());
    // Evicted from the in-memory LRU...
    expect(peekThreadSizePriorsForTest('t0')).toBeUndefined();
    expect(peekThreadSizePriorsForTest('t1')).toBeDefined();
    expect(peekThreadSizePriorsForTest('t50')).toBeDefined();
    // ...but eviction is memory housekeeping only — the adapter never sees a remove.
    expect(adapter.removeCalls).toEqual([]);
    expect(adapter.store.has('t0')).toBe(true);
  });

  it('bumps recency on a successful get', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
    }
    // t0 becomes most recent; the next eviction takes t1 instead.
    getThreadSizePriors('t0');

    setThreadSizePriors('t50', geom(800), bucket());
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('bumps recency on re-set of an existing thread', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
    }
    setThreadSizePriors('t0', geom(999), bucket());

    setThreadSizePriors('t50', geom(800), bucket());
    expect(peekThreadSizePriorsForTest('t0')?.byGeometry.has(key(999))).toBe(true);
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('installing an entry from an adapter hit past the cap also evicts memory-only', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, geom(800), bucket());
    }
    adapter.store.set('overflow', entry());
    getThreadSizePriors('overflow'); // memory miss → adapter hit → installed, pushing t0 out
    expect(peekThreadSizePriorsForTest('t0')).toBeUndefined();
    expect(adapter.removeCalls).toEqual([]); // still memory-only eviction
  });

  it('counts rows across every geometry bucket in the diagnostic stats', () => {
    setThreadSizePriors('t1', geom(800), bucket({ rows: new Map([['a', 1], ['b', 2]]) }));
    setThreadSizePriors('t1', geom(600), bucket({ rows: new Map([['a', 3]]) }));
    setThreadSizePriors('t2', geom(800), bucket({ rows: new Map([['a', 4]]) }));
    expect(sizePriorsStats()).toEqual({ threads: 2, buckets: 3, rows: 4 });
  });
});

describe('row cap', () => {
  it('matches the pane retention ceiling it stands in for', async () => {
    // `utils/` must not import `stores/`, so the cap is duplicated. This
    // is the check that keeps the copy honest: a bucket may hold as many
    // rows as a pane can retain, and not one more.
    const { ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS } = await import('../../stores/threadPaneShared');
    expect(MAX_ROWS_PER_BUCKET).toBe(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);
  });
});

describe('geometry keys', () => {
  it('composes the rounded width and the typography signature', () => {
    expect(sizePriorsGeometryKey({ width: 799.6, typography: 'f15/sgeist' })).toBe('800|f15/sgeist');
  });

  it('accepts a key it produced', () => {
    expect(isSizePriorsGeometryKey(sizePriorsGeometryKey(geom(800)))).toBe(true);
    expect(isSizePriorsGeometryKey(sizePriorsGeometryKey(geom(0)))).toBe(true);
  });

  it('rejects keys this module could not have written', () => {
    // A bare number is the v1 shape; the rest can only come from a
    // corrupted or hand-edited profile, and none names a real geometry.
    expect(isSizePriorsGeometryKey(800)).toBe(false);
    expect(isSizePriorsGeometryKey('800')).toBe(false);
    expect(isSizePriorsGeometryKey('800|')).toBe(false);
    expect(isSizePriorsGeometryKey('|f15')).toBe(false);
    expect(isSizePriorsGeometryKey('-800|f15')).toBe(false);
    expect(isSizePriorsGeometryKey('80.5|f15')).toBe(false);
    expect(isSizePriorsGeometryKey(' 800|f15')).toBe(false);
    expect(isSizePriorsGeometryKey('NaN|f15')).toBe(false);
    expect(isSizePriorsGeometryKey(undefined)).toBe(false);
  });
});

describe('createRowEstimate', () => {
  it('resolves rowPrior → kind → default in that order', () => {
    const estimate = createRowEstimate({
      rowPrior: (index) => (index === 0 ? 120 : undefined),
      kindOf: (index) => (index === 1 ? 'tool' : index === 2 ? 'unknown-kind' : undefined),
      kindHeights: { tool: 44 },
      defaultSize: 56,
    });
    expect(estimate.at(0)).toBe(120); // rowPrior hit
    expect(estimate.at(1)).toBe(44); // rowPrior miss → kind
    expect(estimate.at(2)).toBe(56); // kind not in table → default
    expect(estimate.at(3)).toBe(56); // no rowPrior, no kind → default
  });

  it('treats a measured 0 as a valid prior', () => {
    const estimate = createRowEstimate({ rowPrior: () => 0, defaultSize: 56 });
    expect(estimate.at(0)).toBe(0);
  });

  it('falls back to kind heights without any rowPrior', () => {
    const estimate = createRowEstimate({
      kindOf: () => 'prose',
      kindHeights: { prose: 88 },
      defaultSize: 56,
    });
    expect(estimate.at(7)).toBe(88);
  });

  it('uses the flat default with no inputs', () => {
    const estimate = createRowEstimate({ defaultSize: 56 });
    expect(estimate.at(0)).toBe(56);
  });

  it('is index-free: rowPrior and kindOf are consulted per call with no positional remap', () => {
    // The deleted snapshot+shiftBase design carried index-keyed bias
    // state across head splices; a signature-based rowPrior has nothing
    // to remap — every call resolves fresh against whatever the caller
    // reports for that index right now.
    const rowSeen: number[] = [];
    const kindSeen: number[] = [];
    const estimate = createRowEstimate({
      rowPrior: (index) => {
        rowSeen.push(index);
        return index === 2 ? 77 : undefined;
      },
      kindOf: (index) => {
        kindSeen.push(index);
        return undefined;
      },
      defaultSize: 56,
    });
    expect(estimate.at(2)).toBe(77);
    expect(estimate.at(5)).toBe(56);
    expect(rowSeen).toEqual([2, 5]);
    expect(kindSeen).toEqual([5]); // rowPrior hit at 2 short-circuits kindOf
  });
});
