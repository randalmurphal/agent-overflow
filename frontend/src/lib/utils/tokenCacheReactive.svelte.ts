// Reactive shim around the pure tokenCache. Owns the module-level
// shared cache instance plus a `$state` generation counter that
// renderers depend on so token swap-in re-renders fire as soon as
// the worker dispatch lands. Kept separate from `tokenCache.ts` so
// the underlying data structure stays unit-testable as plain TS.

import { createTokenCache, type TokenCache } from './tokenCache';

let sharedCache: TokenCache | null = null;
let sharedReactiveCache: TokenCache | null = null;

// Bumped on shared-cache mutations (`set`, theme eviction, clear).
// Renderers depend on it via `getSharedTokenCacheGeneration()` to
// re-evaluate cache reads when tokens land. Lives at module scope
// so the body and the file components both observe cache mutations
// without each owning their own counter.
let sharedGeneration: number = $state(0);

/** Reactive read — registers `sharedGeneration` as a dependency. */
export function getSharedTokenCacheGeneration(): number {
  return sharedGeneration;
}

function bumpSharedGeneration(): void {
  sharedGeneration += 1;
}

/**
 * Underlying non-reactive cache. Mutations DO NOT bump the generation
 * counter — for that, use `getSharedReactiveTokenCache`. Tests + low-
 * level code that doesn't need re-render plumbing can use this directly.
 */
export function getSharedTokenCache(): TokenCache {
  sharedCache ??= createTokenCache();
  return sharedCache;
}

/**
 * Reactive wrapper: writes pass through to the underlying cache
 * AND bump the generation counter, so any consumer that reads
 * `getSharedTokenCacheGeneration()` re-evaluates. Used by the diff
 * sidebar's render path.
 */
export function getSharedReactiveTokenCache(): TokenCache {
  if (sharedReactiveCache) return sharedReactiveCache;
  const inner = getSharedTokenCache();
  sharedReactiveCache = {
    get size() { return inner.size; },
    get(key) { return inner.get(key); },
    set(key, tokens) {
      inner.set(key, tokens);
      bumpSharedGeneration();
    },
    evictTheme(theme) {
      const evicted = inner.evictTheme(theme);
      if (evicted > 0) bumpSharedGeneration();
      return evicted;
    },
    evictThread(threadId) {
      const evicted = inner.evictThread(threadId);
      if (evicted > 0) bumpSharedGeneration();
      return evicted;
    },
    clear() {
      inner.clear();
      bumpSharedGeneration();
    },
  };
  return sharedReactiveCache;
}

/**
 * Pane hook for thread switch: drops every cached token line that
 * was tokenized while the named thread was the active diff target.
 * Called from `pane.switchThread` so leaving thread A frees tokens
 * before thread B's diff sidebar starts populating its own.
 *
 * No generation bump: nothing rendered against thread A's keys
 * survives the switch (the sidebar moves to thread B's namespace),
 * so the eviction has no visible reactive consumer.
 */
export function clearTokensForThread(threadId: string): void {
  if (!threadId) return;
  if (!sharedCache) return;
  sharedCache.evictThread(threadId);
}

export function resetSharedTokenCacheForTest(): void {
  sharedCache = null;
  sharedReactiveCache = null;
  sharedGeneration = 0;
}
