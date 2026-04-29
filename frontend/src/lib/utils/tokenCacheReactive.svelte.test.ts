import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearTokensForThread,
  getSharedTokenCache,
  getSharedReactiveTokenCache,
  getSharedTokenCacheGeneration,
  resetSharedTokenCacheForTest,
} from './tokenCacheReactive.svelte';
import { tokenCacheKey } from './tokenCache';

const KEY = tokenCacheKey('thread-1', 'github-dark', 'typescript', 'const x = 1;');
const KEY2 = tokenCacheKey('thread-1', 'github-light', 'typescript', 'const x = 1;');

describe('tokenCacheReactive', () => {
  beforeEach(() => {
    resetSharedTokenCacheForTest();
  });

  it('reactive set bumps the generation counter', () => {
    const cache = getSharedReactiveTokenCache();
    const before = getSharedTokenCacheGeneration();
    cache.set(KEY, [{ content: 'const' }]);
    expect(getSharedTokenCacheGeneration()).toBe(before + 1);
  });

  it('plain (non-reactive) cache writes do NOT bump generation', () => {
    // Pin the contract: only the reactive wrapper bumps. Bypassing it
    // is the documented escape hatch for tests + low-level code.
    const before = getSharedTokenCacheGeneration();
    getSharedTokenCache().set(KEY, [{ content: 'const' }]);
    expect(getSharedTokenCacheGeneration()).toBe(before);
  });

  it('reactive evictTheme bumps generation only when entries actually evicted', () => {
    const cache = getSharedReactiveTokenCache();
    cache.set(KEY, [{ content: 'const' }]);
    const before = getSharedTokenCacheGeneration();

    // Evicting a theme with no entries: no bump.
    cache.evictTheme('github-light');
    expect(getSharedTokenCacheGeneration()).toBe(before);

    // Evicting the theme that has entries: bump.
    cache.evictTheme('github-dark');
    expect(getSharedTokenCacheGeneration()).toBe(before + 1);
  });

  it('reactive clear bumps generation', () => {
    const cache = getSharedReactiveTokenCache();
    cache.set(KEY, [{ content: 'const' }]);
    const before = getSharedTokenCacheGeneration();
    cache.clear();
    expect(getSharedTokenCacheGeneration()).toBe(before + 1);
  });

  it('reactive read returns same data as plain read (wrapper is pass-through for reads)', () => {
    const inner = getSharedTokenCache();
    const reactive = getSharedReactiveTokenCache();
    inner.set(KEY, [{ content: 'plain-write', color: '#abc' }]);
    expect(reactive.get(KEY)).toEqual([{ content: 'plain-write', color: '#abc' }]);
  });

  it('memoizes the reactive wrapper instance — same reference across calls', () => {
    const a = getSharedReactiveTokenCache();
    const b = getSharedReactiveTokenCache();
    expect(a).toBe(b);
  });

  it('writes through reactive wrapper land in the underlying shared cache', () => {
    const reactive = getSharedReactiveTokenCache();
    const inner = getSharedTokenCache();
    reactive.set(KEY, [{ content: 'reactive-write' }]);
    expect(inner.get(KEY)?.[0]?.content).toBe('reactive-write');
  });

  it('resetSharedTokenCacheForTest resets the generation counter', () => {
    const cache = getSharedReactiveTokenCache();
    cache.set(KEY, [{ content: 'a' }]);
    cache.set(KEY2, [{ content: 'b' }]);
    expect(getSharedTokenCacheGeneration()).toBeGreaterThan(0);
    resetSharedTokenCacheForTest();
    expect(getSharedTokenCacheGeneration()).toBe(0);
  });

  it('clearTokensForThread evicts only entries from the named thread', () => {
    const cache = getSharedReactiveTokenCache();
    const aThreadKey = tokenCacheKey('thread-a', 'github-dark', 'typescript', 'const x = 1;');
    const bThreadKey = tokenCacheKey('thread-b', 'github-dark', 'typescript', 'const x = 1;');
    cache.set(aThreadKey, [{ content: 'a' }]);
    cache.set(bThreadKey, [{ content: 'b' }]);

    clearTokensForThread('thread-a');

    const inner = getSharedTokenCache();
    expect(inner.get(aThreadKey)).toBeUndefined();
    expect(inner.get(bThreadKey)).toBeDefined();
  });

  it('clearTokensForThread is a no-op for an empty cache', () => {
    expect(() => clearTokensForThread('thread-x')).not.toThrow();
  });
});
