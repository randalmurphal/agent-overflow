import { afterEach, describe, expect, it } from 'vitest';
import {
  clearThreadVirtuaSizeCache,
  clearThreadVirtuaSizeCacheForTest,
  getReplayableVirtuaCache,
  setThreadVirtuaSizeCache,
  type VirtuaCacheSnapshot,
  type VirtuaSizeCacheKey,
} from './threadVirtuaSizeCache';

// virtua's snapshot is opaque to this module — only its identity matters, so
// a distinct sentinel object per case is enough to assert what was returned.
function snap(tag: string): VirtuaCacheSnapshot {
  return { tag } as unknown as VirtuaCacheSnapshot;
}

const KEY: VirtuaSizeCacheKey = { width: 800, structureSig: 'sig-a', expansionSig: '' };

afterEach(() => {
  clearThreadVirtuaSizeCacheForTest();
});

describe('threadVirtuaSizeCache', () => {
  it('replays the snapshot when every key dimension matches', () => {
    const s = snap('a');
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: s });
    expect(getReplayableVirtuaCache('t1', KEY)).toBe(s);
  });

  it('returns undefined for an unknown thread', () => {
    expect(getReplayableVirtuaCache('missing', KEY)).toBeUndefined();
  });

  // Each dimension is load-bearing: a mismatch must refuse the replay so the
  // mount falls back to the flat estimate rather than restoring stale heights.
  it('refuses the replay when width differs (re-wrap would change row heights)', () => {
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: snap('a') });
    expect(getReplayableVirtuaCache('t1', { ...KEY, width: 801 })).toBeUndefined();
  });

  it('refuses the replay when structure signature differs (items changed)', () => {
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: snap('a') });
    expect(getReplayableVirtuaCache('t1', { ...KEY, structureSig: 'sig-b' })).toBeUndefined();
  });

  it('refuses the replay when expansion signature differs (rows expanded/collapsed)', () => {
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: snap('a') });
    expect(getReplayableVirtuaCache('t1', { ...KEY, expansionSig: 'diff:x' })).toBeUndefined();
  });

  it('overwrites the entry for a thread on re-capture', () => {
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: snap('old') });
    const fresh = snap('new');
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: fresh });
    expect(getReplayableVirtuaCache('t1', KEY)).toBe(fresh);
  });

  it('clears a single thread entry', () => {
    setThreadVirtuaSizeCache('t1', { ...KEY, snapshot: snap('a') });
    clearThreadVirtuaSizeCache('t1');
    expect(getReplayableVirtuaCache('t1', KEY)).toBeUndefined();
  });

  it('evicts the least-recently-used entry past the 50-entry bound', () => {
    const newest = snap('s59');
    // Fill past the cap. The first-inserted thread should be evicted; the
    // most-recent survives.
    for (let i = 0; i < 60; i++) {
      setThreadVirtuaSizeCache(`t${i}`, { ...KEY, snapshot: i === 59 ? newest : snap(`s${i}`) });
    }
    expect(getReplayableVirtuaCache('t0', KEY)).toBeUndefined();
    expect(getReplayableVirtuaCache('t59', KEY)).toBe(newest);
  });

  it('a successful replay bumps recency so the entry survives later eviction', () => {
    setThreadVirtuaSizeCache('keep', { ...KEY, snapshot: snap('keep') });
    // Insert 49 more to reach the cap (50 total) without evicting 'keep'.
    for (let i = 0; i < 49; i++) {
      setThreadVirtuaSizeCache(`t${i}`, { ...KEY, snapshot: snap(`s${i}`) });
    }
    // Touch 'keep' so it becomes most-recent.
    expect(getReplayableVirtuaCache('keep', KEY)).toBeTruthy();
    // One more insertion overflows the cap; the now-oldest (t0), not 'keep',
    // is evicted.
    setThreadVirtuaSizeCache('overflow', { ...KEY, snapshot: snap('overflow') });
    expect(getReplayableVirtuaCache('keep', KEY)).toBeTruthy();
    expect(getReplayableVirtuaCache('t0', KEY)).toBeUndefined();
  });
});
