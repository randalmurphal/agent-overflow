import { afterEach, describe, expect, it } from 'vitest';
import {
  clearThreadScrollSnapshotsForTest,
  getThreadScrollSnapshot,
  setThreadScrollSnapshot,
} from './threadScrollSnapshots';

describe('threadScrollSnapshots', () => {
  afterEach(() => {
    clearThreadScrollSnapshotsForTest();
  });

  it('round-trips a bottom snapshot', () => {
    setThreadScrollSnapshot('t1', { kind: 'bottom' });
    expect(getThreadScrollSnapshot('t1')).toEqual({ kind: 'bottom' });
  });

  it('round-trips an anchor snapshot', () => {
    setThreadScrollSnapshot('t1', { kind: 'anchor', itemId: 'item-7', offsetTop: 42 });
    expect(getThreadScrollSnapshot('t1')).toEqual({ kind: 'anchor', itemId: 'item-7', offsetTop: 42 });
  });

  it('overwrites a prior snapshot for the same thread', () => {
    setThreadScrollSnapshot('t1', { kind: 'bottom' });
    setThreadScrollSnapshot('t1', { kind: 'anchor', itemId: 'x', offsetTop: 10 });
    expect(getThreadScrollSnapshot('t1')).toEqual({ kind: 'anchor', itemId: 'x', offsetTop: 10 });
  });

  it('returns undefined for unknown threads', () => {
    expect(getThreadScrollSnapshot('never-seen')).toBeUndefined();
  });

  it('evicts oldest entries beyond the LRU bound', () => {
    // Insert 102 entries; only the most recent 100 should remain.
    for (let i = 0; i < 102; i += 1) {
      setThreadScrollSnapshot(`t${i}`, { kind: 'bottom' });
    }
    expect(getThreadScrollSnapshot('t0')).toBeUndefined();
    expect(getThreadScrollSnapshot('t1')).toBeUndefined();
    expect(getThreadScrollSnapshot('t2')).toEqual({ kind: 'bottom' });
    expect(getThreadScrollSnapshot('t101')).toEqual({ kind: 'bottom' });
  });

  it('re-inserting a known thread bumps it to most-recent (LRU)', () => {
    for (let i = 0; i < 100; i += 1) {
      setThreadScrollSnapshot(`t${i}`, { kind: 'bottom' });
    }
    // Touch the oldest entry — it should survive the next eviction.
    setThreadScrollSnapshot('t0', { kind: 'anchor', itemId: 'x', offsetTop: 0 });
    setThreadScrollSnapshot('t100', { kind: 'bottom' });
    expect(getThreadScrollSnapshot('t0')).toEqual({ kind: 'anchor', itemId: 'x', offsetTop: 0 });
    // t1 is now the oldest and was evicted by inserting t100.
    expect(getThreadScrollSnapshot('t1')).toBeUndefined();
  });
});
