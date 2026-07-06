// Reactive shim around the pure tokenCache. Owns the module-level
// shared cache instance plus a `$state` generation counter that
// renderers depend on so token swap-in re-renders fire as soon as
// the worker dispatch lands. Kept separate from `tokenCache.ts` so
// the underlying data structure stays unit-testable as plain TS.

import type { DiffTheme } from './diffHighlighterPool';
import { stripPatchLinePrefix, type PatchLine } from './patchFiles';
import { patchLineSourceKey } from './patchLineHash';
import {
  createTokenCache,
  TOKENIZE_MAX_LINE_LENGTH,
  tokenCacheKeyFromSig,
  type LineToken,
  type TokenCache,
} from './tokenCache';

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
 * `getSharedTokenCacheGeneration()` re-evaluates. Used by diff render
 * paths.
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
 * before thread B's review or inline diffs start populating their own.
 *
 * No generation bump: nothing rendered against thread A's keys
 * survives the switch (diff renderers move to thread B's namespace),
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

/**
 * Reactive read-side helper shared by patch-line renderers
 * (DiffFileBlock, ReviewLineBlockRow). Returns the
 * cached Shiki tokens for `line`, or null when the line is meta,
 * empty, over the per-line cap, or simply not in the cache yet.
 *
 * Reading the generation counter registers a reactive dep, so when a
 * worker dispatch lands (cache.set bumps the generation), callers
 * re-evaluate against the now-populated cache. Filtering on
 * line.type / length before computing the hash key keeps the hot
 * render path cheap when most rows can't be tokenized.
 */
export function getCachedTokensForLine(
  line: PatchLine,
  threadId: string,
  theme: DiffTheme,
  lang: string,
): LineToken[] | null {
  getSharedTokenCacheGeneration();
  if (line.type === 'meta') return null;
  const text = stripPatchLinePrefix(line);
  if (text.length === 0 || text.length > TOKENIZE_MAX_LINE_LENGTH) return null;
  return (
    getSharedTokenCache().get(
      tokenCacheKeyFromSig(threadId, theme, lang, patchLineSourceKey(line)),
    ) ?? null
  );
}
