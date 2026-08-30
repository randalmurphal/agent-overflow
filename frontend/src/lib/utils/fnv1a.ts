// Shared 32-bit FNV-1a over UTF-16 code units. Content-hash keying for
// the span caches (diff files, markdown code blocks): fast, allocation
// free, and stable across sessions. Not cryptographic — collisions are
// tolerable because a collision only yields a wrong-but-valid span set
// for one render, self-corrected on the next content change.

const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

/** Continue an FNV-1a hash with an appended UTF-16 suffix. */
export function appendFNV1a32(hash: number, suffix: string): number {
  for (let i = 0; i < suffix.length; i += 1) {
    hash ^= suffix.charCodeAt(i);
    hash = Math.imul(hash, FNV_PRIME_32) >>> 0;
  }
  return hash >>> 0;
}

export function fnv1a32(input: string): number {
  return appendFNV1a32(FNV_OFFSET_BASIS_32, input);
}

/** Compact string form for cache keys: `<length>:<hash base36>`. */
export function contentKey(input: string): string {
  return `${input.length}:${fnv1a32(input).toString(36)}`;
}
