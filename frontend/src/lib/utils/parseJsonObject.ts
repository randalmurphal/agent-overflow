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
// string ever seen — by entry count AND by retained source bytes.
// Most meta strings are short, but persisted highlight span blobs
// (items.meta `codeSpans`, Item.payloadPreviewSpans) run up to
// ~256 KB each; a count-only cap would let scroll-through of many
// diff rows retain hundreds of megabytes of strings + parsed arrays
// long after their rows unmounted. Eviction is FIFO via Map insertion
// order — good enough; hot entries that get evicted early simply
// reinsert on their next read (one fresh object identity, then stable).

const PARSE_CACHE_CAP = 1024;
// Approximate retained-source budget. UTF-16 units ≈ bytes for the
// ASCII-dominated JSON we cache; the parsed object roughly mirrors the
// source size, so the real footprint is a small multiple of this.
const PARSE_CACHE_BYTE_BUDGET = 8 << 20; // 8M chars
const cache = new Map<string, Record<string, unknown> | null>();
let cacheChars = 0;

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

  if (raw.length > PARSE_CACHE_BYTE_BUDGET) {
    // A pathological over-budget string must not flush the whole cache
    // to make room; it parses fresh per call (losing reference
    // stability only for itself).
    return parsedResult;
  }
  while (
    cache.size > 0 &&
    (cache.size >= PARSE_CACHE_CAP || cacheChars + raw.length > PARSE_CACHE_BYTE_BUDGET)
  ) {
    // Map iterates in insertion order — drop oldest entries until both
    // bounds hold. Amortized O(1): each entry is evicted at most once.
    const oldest = cache.keys().next().value;
    if (oldest === undefined) break;
    cache.delete(oldest);
    cacheChars -= oldest.length;
  }
  cache.set(raw, parsedResult);
  cacheChars += raw.length;
  return parsedResult;
}

/**
 * Test-only hook to drop the cache between cases. Production code
 * never calls this — the cache lives for the page lifetime.
 */
export function __resetParseJsonObjectCacheForTest(): void {
  cache.clear();
  cacheChars = 0;
}
