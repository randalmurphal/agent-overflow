import { describe, it, expect } from 'vitest';
import { createTokenCache, tokenCacheKey } from './tokenCache';

describe('tokenCache', () => {
  it('round-trips tokens by key', () => {
    const cache = createTokenCache(10);
    const key = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'const x = 1;');
    cache.set(key, [{ content: 'const', color: '#ff79c6' }]);
    expect(cache.get(key)).toEqual([{ content: 'const', color: '#ff79c6' }]);
  });

  it('returns undefined for missing keys', () => {
    const cache = createTokenCache(10);
    expect(cache.get('missing')).toBeUndefined();
  });

  it('LRU-evicts the oldest entry when over cap', () => {
    const cache = createTokenCache(3);
    cache.set('a', [{ content: 'a' }]);
    cache.set('b', [{ content: 'b' }]);
    cache.set('c', [{ content: 'c' }]);
    cache.set('d', [{ content: 'd' }]);
    expect(cache.size).toBe(3);
    expect(cache.get('a')).toBeUndefined();
    expect(cache.get('b')).toBeDefined();
    expect(cache.get('c')).toBeDefined();
    expect(cache.get('d')).toBeDefined();
  });

  it('touches entries on read so frequently-read lines stay hot', () => {
    const cache = createTokenCache(3);
    cache.set('a', [{ content: 'a' }]);
    cache.set('b', [{ content: 'b' }]);
    cache.set('c', [{ content: 'c' }]);
    // Read 'a' to touch it
    cache.get('a');
    // Inserting 'd' should now evict 'b' (the oldest after touch), not 'a'
    cache.set('d', [{ content: 'd' }]);
    expect(cache.get('a')).toBeDefined();
    expect(cache.get('b')).toBeUndefined();
  });

  it('evictTheme drops only entries with matching theme prefix', () => {
    const cache = createTokenCache(100);
    const dark = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'const x = 1;');
    const dark2 = tokenCacheKey('thread-1', 'github-dark', 'go', 'package main');
    const light = tokenCacheKey('thread-1', 'github-light', 'typescript', 'const x = 1;');
    cache.set(dark, [{ content: 'const' }]);
    cache.set(dark2, [{ content: 'package' }]);
    cache.set(light, [{ content: 'const' }]);

    const evicted = cache.evictTheme('github-dark');
    expect(evicted).toBe(2);
    expect(cache.get(dark)).toBeUndefined();
    expect(cache.get(dark2)).toBeUndefined();
    expect(cache.get(light)).toBeDefined();
  });

  it('evictThread drops only entries from the named thread', () => {
    const cache = createTokenCache(100);
    const a = tokenCacheKey('thread-a', 'github-dark', 'typescript', 'const x = 1;');
    const a2 = tokenCacheKey('thread-a', 'github-dark', 'go', 'package main');
    const b = tokenCacheKey('thread-b', 'github-dark', 'typescript', 'const x = 1;');
    cache.set(a, [{ content: 'a-ts' }]);
    cache.set(a2, [{ content: 'a-go' }]);
    cache.set(b, [{ content: 'b-ts' }]);

    const evicted = cache.evictThread('thread-a');
    expect(evicted).toBe(2);
    expect(cache.get(a)).toBeUndefined();
    expect(cache.get(a2)).toBeUndefined();
    expect(cache.get(b)).toBeDefined();
  });

  it('evictThread does not disturb other threads when the cache is full', () => {
    // Pin: a full cache (at the LRU cap) plus an evictThread on one
    // thread should free up slots WITHOUT triggering further LRU
    // evictions on the surviving threads. Without this guarantee the
    // full-cache + thread-switch flow would over-evict.
    const cache = createTokenCache(4);
    const a1 = tokenCacheKey('thread-a', 'github-dark', 'typescript', 'a1');
    const a2 = tokenCacheKey('thread-a', 'github-dark', 'typescript', 'a2');
    const b1 = tokenCacheKey('thread-b', 'github-dark', 'typescript', 'b1');
    const b2 = tokenCacheKey('thread-b', 'github-dark', 'typescript', 'b2');
    cache.set(a1, [{ content: 'a1' }]);
    cache.set(a2, [{ content: 'a2' }]);
    cache.set(b1, [{ content: 'b1' }]);
    cache.set(b2, [{ content: 'b2' }]);
    expect(cache.size).toBe(4);

    const evicted = cache.evictThread('thread-a');
    expect(evicted).toBe(2);
    expect(cache.size).toBe(2);
    expect(cache.get(b1)).toBeDefined();
    expect(cache.get(b2)).toBeDefined();
  });

  it('evictThread skips keys whose layout is malformed', () => {
    // Defensive: keys are in a fixed format but a future bug could write
    // a single-segment key. evictThread should leave such entries alone
    // rather than mass-evict.
    const cache = createTokenCache(10);
    cache.set('malformed', [{ content: 'x' }]);
    cache.set(tokenCacheKey('thread-a', 'github-dark', 'typescript', 'foo'), [{ content: 'a' }]);

    const evicted = cache.evictThread('thread-a');
    expect(evicted).toBe(1);
    expect(cache.get('malformed')).toBeDefined();
  });

  it('different threads keep the same line under independent cache entries', () => {
    const cache = createTokenCache(10);
    const a = tokenCacheKey('thread-a', 'github-dark', 'typescript', 'foo');
    const b = tokenCacheKey('thread-b', 'github-dark', 'typescript', 'foo');
    expect(a).not.toBe(b);
    cache.set(a, [{ content: 'foo', color: '#fff' }]);
    cache.set(b, [{ content: 'foo', color: '#000' }]);
    expect(cache.get(a)?.[0]?.color).toBe('#fff');
    expect(cache.get(b)?.[0]?.color).toBe('#000');
  });

  it('different themes for the same line are independent cache entries', () => {
    const cache = createTokenCache(10);
    const dark = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'foo');
    const light = tokenCacheKey('thread-1', 'github-light', 'typescript', 'foo');
    expect(dark).not.toBe(light);
    cache.set(dark, [{ content: 'foo', color: '#fff' }]);
    cache.set(light, [{ content: 'foo', color: '#000' }]);
    expect(cache.get(dark)?.[0]?.color).toBe('#fff');
    expect(cache.get(light)?.[0]?.color).toBe('#000');
  });

  it('clear() empties the cache', () => {
    const cache = createTokenCache(10);
    cache.set('a', [{ content: 'a' }]);
    cache.set('b', [{ content: 'b' }]);
    cache.clear();
    expect(cache.size).toBe(0);
    expect(cache.get('a')).toBeUndefined();
  });

  it('tokenCacheKey is deterministic and length-prefixed', () => {
    const k1 = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'const');
    const k2 = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'const');
    expect(k1).toBe(k2);
    expect(k1.startsWith('github-dark:thread-1:typescript:5:')).toBe(true);
  });
});
