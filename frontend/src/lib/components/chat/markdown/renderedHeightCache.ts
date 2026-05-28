// Module-level rendered-height caches used by `StreamdownMathHost` and
// `StreamdownMermaidHost` to pin the wrapper at its last observed
// rendered height during a remount transient (see those hosts'
// long-form comments for why).
//
// Lives in a plain .ts module (not a `.svelte` component file)
// because vitest's Svelte test transform re-evaluates `.svelte`
// modules on every fresh mount, which would clear a cache declared
// inside the component script — defeating both the cache's runtime
// purpose (survive remount) and any regression test we might write
// against it. Plain `.ts` modules are imported once per test render
// root, so the cache survives mount → unmount → remount across
// `{#if}` toggles inside a single `render()`.
//
// Encapsulation mirrors `src/lib/utils/payloadDataCache.ts`: Maps and
// caps stay file-private, and consumers read/write through narrow
// `read*` / `write*` functions. Tests reset via
// `__resetRenderedHeightCachesForTest()` so the module-level state
// can't leak across cases.
//
// Memory bound. Both caches are entry-count-capped, not byte-aware.
// Math sources are small (typically < 1 KB of LaTeX) so the 200-entry
// cap pins worst-case retention near 200 KB. Mermaid sources are
// larger (typically 5–30 KB per diagram, occasionally larger) so the
// cap is halved to 100 — bringing worst-case retention to roughly
// 1–3 MB, comparable to the math cache despite the bigger per-entry
// payload. Bump either cap with the same arithmetic in mind:
// `CAP × typical_source_kb` is the working ceiling.

const mathRenderedHeightCache = new Map<string, number>();
const MATH_RENDERED_HEIGHT_CACHE_MAX = 200;
const mermaidRenderedHeightCache = new Map<string, number>();
const MERMAID_RENDERED_HEIGHT_CACHE_MAX = 100;

// LRU bump-on-write: delete-then-set moves the entry to the most-
// recent position so the eviction loop drops stale entries first.
// File-private — hosts go through the typed read/write API below.
function bumpAndSet(
  cache: Map<string, number>,
  key: string,
  value: number,
  cap: number,
): void {
  cache.delete(key);
  cache.set(key, value);
  while (cache.size > cap) {
    const oldest = cache.keys().next().value;
    if (oldest === undefined) break;
    cache.delete(oldest);
  }
}

export function readMathRenderedHeight(key: string): number | undefined {
  return mathRenderedHeightCache.get(key);
}

export function writeMathRenderedHeight(key: string, height: number): void {
  bumpAndSet(mathRenderedHeightCache, key, height, MATH_RENDERED_HEIGHT_CACHE_MAX);
}

export function readMermaidRenderedHeight(key: string): number | undefined {
  return mermaidRenderedHeightCache.get(key);
}

export function writeMermaidRenderedHeight(key: string, height: number): void {
  bumpAndSet(
    mermaidRenderedHeightCache,
    key,
    height,
    MERMAID_RENDERED_HEIGHT_CACHE_MAX,
  );
}

/** Test-only reset. */
export function __resetRenderedHeightCachesForTest(): void {
  mathRenderedHeightCache.clear();
  mermaidRenderedHeightCache.clear();
}

/** Test-only inspection. */
export function __renderedHeightCacheSizesForTest(): { math: number; mermaid: number } {
  return {
    math: mathRenderedHeightCache.size,
    mermaid: mermaidRenderedHeightCache.size,
  };
}
