// Shared 32-bit FNV-1a over UTF-16 code units. Content-hash keying for
// the span caches (diff files, markdown code blocks): fast, allocation
// free, and stable across sessions. Not cryptographic — collisions are
// tolerable because a collision only yields a wrong-but-valid span set
// for one render, self-corrected on the next content change.

const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

export function fnv1a32(input: string): number {
  let hash = FNV_OFFSET_BASIS_32 >>> 0;
  for (let i = 0; i < input.length; i += 1) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, FNV_PRIME_32) >>> 0;
  }
  return hash >>> 0;
}

/** Compact string form for cache keys: `<length>:<hash base36>`. */
export function contentKey(input: string): string {
  return `${input.length}:${fnv1a32(input).toString(36)}`;
}
