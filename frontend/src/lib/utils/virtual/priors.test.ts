import { beforeEach, describe, expect, it } from 'vitest';
import {
  clearAllThreadSizePriorsForTest,
  clearThreadSizePriors,
  createRowEstimate,
  getThreadSizePriors,
  peekThreadSizePriorsForTest,
  setSizePriorsStorageAdapter,
  setThreadSizePriors,
  type SizePriorsEntry,
  type SizePriorsStorageAdapter,
} from './priors';

const entry = (overrides: Partial<SizePriorsEntry> = {}): SizePriorsEntry => ({
  width: 800,
  expansionSig: '',
  rows: new Map([
    ['L:a:completed:2:1', 100],
    ['L:b:completed:2:1', 200],
  ]),
  ...overrides,
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
  it('setThreadSizePriors replaces the entry wholesale', () => {
    setThreadSizePriors('t1', entry({ rows: new Map([['a', 1], ['b', 2]]) }));
    setThreadSizePriors('t1', entry({ rows: new Map([['c', 3]]) }));
    const stored = peekThreadSizePriorsForTest('t1');
    expect(stored?.rows.has('a')).toBe(false);
    expect(stored?.rows.has('b')).toBe(false);
    expect(stored?.rows.get('c')).toBe(3);
  });

  it('getThreadSizePriors returns the stored entry on a memory hit', () => {
    setThreadSizePriors('t1', entry());
    expect(getThreadSizePriors('t1')).toEqual(entry());
  });

  it('returns undefined for an unknown thread with no adapter installed', () => {
    expect(getThreadSizePriors('nope')).toBeUndefined();
  });

  it('notifies the adapter on every set', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    setThreadSizePriors('t1', entry());
    expect(adapter.persistCalls).toEqual(['t1']);
    expect(adapter.store.get('t1')).toEqual(entry());
  });

  it('clears a single thread from memory AND the adapter', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    setThreadSizePriors('t1', entry());
    clearThreadSizePriors('t1');
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
    expect(adapter.removeCalls).toEqual(['t1']);
  });

  it('falls back to the adapter on a memory miss and installs the result into the LRU', () => {
    const adapter = fakeAdapter();
    adapter.store.set('t1', entry({ width: 999 }));
    setSizePriorsStorageAdapter(adapter);

    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined(); // memory miss
    const result = getThreadSizePriors('t1');
    expect(result?.width).toBe(999);
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
      setThreadSizePriors(`t${i}`, entry());
    }
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();

    setThreadSizePriors('t50', entry());
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
      setThreadSizePriors(`t${i}`, entry());
    }
    // t0 becomes most recent; the next eviction takes t1 instead.
    getThreadSizePriors('t0');

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')).toBeDefined();
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('bumps recency on re-set of an existing thread', () => {
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    setThreadSizePriors('t0', entry({ width: 999 }));

    setThreadSizePriors('t50', entry());
    expect(peekThreadSizePriorsForTest('t0')?.width).toBe(999);
    expect(peekThreadSizePriorsForTest('t1')).toBeUndefined();
  });

  it('installing an entry from an adapter hit past the cap also evicts memory-only', () => {
    const adapter = fakeAdapter();
    setSizePriorsStorageAdapter(adapter);
    for (let i = 0; i < 50; i++) {
      setThreadSizePriors(`t${i}`, entry());
    }
    adapter.store.set('overflow', entry());
    getThreadSizePriors('overflow'); // memory miss → adapter hit → installed, pushing t0 out
    expect(peekThreadSizePriorsForTest('t0')).toBeUndefined();
    expect(adapter.removeCalls).toEqual([]); // still memory-only eviction
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
