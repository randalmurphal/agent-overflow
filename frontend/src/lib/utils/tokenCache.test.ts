import { describe, it, expect } from 'vitest';
import { createTokenCache, tokenCacheKey } from './tokenCache';

describe('tokenCache', () => {
  it('round-trips tokens by key', () => {
    const cache = createTokenCache(10);
    const key = tokenCacheKey('github-dark', 'typescript', 'const x = 1;');
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
    const dark = tokenCacheKey('github-dark', 'typescript', 'const x = 1;');
    const dark2 = tokenCacheKey('github-dark', 'go', 'package main');
    const light = tokenCacheKey('github-light', 'typescript', 'const x = 1;');
    cache.set(dark, [{ content: 'const' }]);
    cache.set(dark2, [{ content: 'package' }]);
    cache.set(light, [{ content: 'const' }]);

    const evicted = cache.evictTheme('github-dark');
    expect(evicted).toBe(2);
    expect(cache.get(dark)).toBeUndefined();
    expect(cache.get(dark2)).toBeUndefined();
    expect(cache.get(light)).toBeDefined();
  });

  it('different themes for the same line are independent cache entries', () => {
    const cache = createTokenCache(10);
    const dark = tokenCacheKey('github-dark', 'typescript', 'foo');
    const light = tokenCacheKey('github-light', 'typescript', 'foo');
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
    const k1 = tokenCacheKey('github-dark', 'typescript', 'const');
    const k2 = tokenCacheKey('github-dark', 'typescript', 'const');
    expect(k1).toBe(k2);
    expect(k1.startsWith('github-dark:typescript:5:')).toBe(true);
  });
});
