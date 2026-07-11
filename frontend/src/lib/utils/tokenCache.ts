// Theme + thread keyed LRU cache for Shiki tokens. Pure data
// structure — no Svelte runes here so this module stays unit-testable
// as plain TypeScript. The reactive generation counter + shared
// instance live in `tokenCacheReactive.svelte.ts`.
//
// Keys are `${theme}:${threadId}:${lang}:${len}:${fnv1a(line)}` so:
//   - same line in different themes does not collide
//   - same line in different threads does not collide
//   - same line in different languages does not collide
//   - the cache namespace is partitioned by theme AND thread so we
//     can evict all entries of a previous theme on theme switch and
//     all entries of a thread on thread switch

export interface LineToken {
  content: string;
  /** Hex color from the Shiki theme, e.g. "#79c0ff". */
  color?: string;
  /** Font-style flags from Shiki: 1=italic, 2=bold, 4=underline. */
  fontStyle?: number;
}

export type LineTokens = LineToken[];

/**
 * Max line length the tokenizer is willing to handle. Lines past
 * this are returned as a single plain token (no syntax tinting) —
 * cheap insurance against minified files crashing the tokenizer or
 * blowing up the worker queue. Imported by every site that decides
 * "should this line get tokenized" so the cap stays in sync.
 */
export const TOKENIZE_MAX_LINE_LENGTH = 1000;

const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

function fnv1a32(input: string): string {
  let hash = FNV_OFFSET_BASIS_32 >>> 0;
  for (let i = 0; i < input.length; i += 1) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, FNV_PRIME_32) >>> 0;
  }
  return hash.toString(36);
}

export function tokenCacheKey(threadId: string, theme: string, lang: string, line: string): string {
  return `${theme}:${threadId}:${lang}:${line.length}:${fnv1a32(line)}`;
}

/**
 * Key from a precomputed `${length}:${hash}` signature. Used by hot
 * paths that already have a memoized signature for a stable line
 * object (see `patchLineSourceKey`) — avoids re-hashing on every
 * cache lookup.
 */
export function tokenCacheKeyFromSig(threadId: string, theme: string, lang: string, sourceKey: string): string {
  return `${theme}:${threadId}:${lang}:${sourceKey}`;
}

/**
 * Fixed-cap LRU. Map insertion order = LRU order. On insert, the
 * oldest entry is evicted when over cap. On read, the entry is
 * touched (move-to-end) so frequently-read lines stay hot.
 */
export interface TokenCache {
  get(key: string): LineTokens | undefined;
  set(key: string, tokens: LineTokens): void;
  evictTheme(theme: string): number;
  evictThread(threadId: string): number;
  clear(): void;
  readonly size: number;
}

// 10000 entries × ~1 KB/entry worst case (key + LineTokens array + Map
// overhead) ≈ 10 MB ceiling, typically far less. The per-thread
// partition + `evictThread` on switch is what bounds long-session
// memory; the cap exists only to absorb repeat-visit cache pressure
// within a single thread. Keep it large enough that scrolling a
// multi-thousand-line review diff (lines + context routinely clear
// 5k) doesn't self-evict mid-pass — re-tokenizing visible lines on
// scroll-back shows as a brief flash of unstyled code, which is the
// worst case to avoid.
const DEFAULT_CAP = 10000;

export function createTokenCache(cap = DEFAULT_CAP): TokenCache {
  const store = new Map<string, LineTokens>();

  return {
    get size() {
      return store.size;
    },
    get(key: string): LineTokens | undefined {
      const tokens = store.get(key);
      if (tokens === undefined) return undefined;
      // Touch — re-insert moves to end of insertion order.
      store.delete(key);
      store.set(key, tokens);
      return tokens;
    },
    set(key: string, tokens: LineTokens): void {
      if (store.has(key)) {
        store.delete(key);
      }
      store.set(key, tokens);
      while (store.size > cap) {
        const oldest = store.keys().next().value;
        if (!oldest) break;
        store.delete(oldest);
      }
    },
    evictTheme(theme: string): number {
      const prefix = `${theme}:`;
      let evicted = 0;
      for (const key of store.keys()) {
        if (key.startsWith(prefix)) {
          store.delete(key);
          evicted += 1;
        }
      }
      return evicted;
    },
    evictThread(threadId: string): number {
      // Format: ${theme}:${threadId}:${lang}:${len}:${hash}. Match by
      // the second segment — split limit avoids allocating the trailing
      // segments when we only need the first two.
      let evicted = 0;
      for (const key of store.keys()) {
        const firstColon = key.indexOf(':');
        if (firstColon < 0) continue;
        const secondColon = key.indexOf(':', firstColon + 1);
        if (secondColon < 0) continue;
        if (key.substring(firstColon + 1, secondColon) === threadId) {
          store.delete(key);
          evicted += 1;
        }
      }
      return evicted;
    },
    clear(): void {
      store.clear();
    },
  };
}
