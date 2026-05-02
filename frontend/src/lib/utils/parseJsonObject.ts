// Defensive JSON.parse for backend-supplied "JSON-string-or-undefined"
// fields like `Item.payloadMeta` and `Item.meta`. Garbage strings,
// non-object roots (numbers, arrays, "null"), and parse errors all
// fall through to `null` so callers don't have to reason about them.
//
// Results are cached by source string so repeated calls with the same
// input return the same object reference. The transcript hot path
// (SubagentGroup, TimelineLeaf, ToolCallCard) reads `parseJsonObject`
// off the same item.meta / item.payloadMeta many times per render
// cycle; without caching, every read produced a fresh object that
// flowed through every $derived chain and forced needless DOM update
// passes downstream. Returned objects are intentionally treated as
// read-only — callers do not (and must not) mutate them.
//
// Cache is bounded so a long session can't accumulate every meta
// string ever seen. The cap is generous (1024 entries) and eviction
// is FIFO via Map insertion order — good enough; meta strings are
// short and sticky, so reuse rates are high relative to churn.

const PARSE_CACHE_CAP = 1024;
const cache = new Map<string, Record<string, unknown> | null>();

export function parseJsonObject(raw: string | undefined | null): Record<string, unknown> | null {
  if (!raw) return null;
  const cached = cache.get(raw);
  if (cached !== undefined) return cached;

  let parsedResult: Record<string, unknown> | null = null;
  try {
    const parsed = JSON.parse(raw) as unknown;
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      parsedResult = parsed as Record<string, unknown>;
    }
  } catch {
    parsedResult = null;
  }

  if (cache.size >= PARSE_CACHE_CAP) {
    // Map iterates in insertion order — drop the oldest entry. One
    // eviction per insert past the cap keeps the cache size stable
    // without paying for a full LRU.
    const oldest = cache.keys().next().value;
    if (oldest !== undefined) cache.delete(oldest);
  }
  cache.set(raw, parsedResult);
  return parsedResult;
}

/**
 * Test-only hook to drop the cache between cases. Production code
 * never calls this — the cache lives for the page lifetime.
 */
export function __resetParseJsonObjectCacheForTest(): void {
  cache.clear();
}
