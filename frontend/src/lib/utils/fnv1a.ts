// Shared FNV-1a over UTF-16 code units, in a 32-bit and a 64-bit form.
// The 32-bit form is content-hash keying for the span caches (diff
// files, markdown code blocks): fast, allocation free, and stable across
// sessions. Not cryptographic — collisions are tolerable because a
// collision only yields a wrong-but-valid span set for one render,
// self-corrected on the next content change. The 64-bit form backs the
// held-window digest, where the width buys collision headroom against a
// server that answers `fresh` on a match.

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

// FNV-1a 64 offset basis 0xcbf29ce484222325 and prime 0x100000001b3,
// each split into two 32-bit halves. JavaScript has no 64-bit integer
// outside BigInt, and BigInt costs an allocation per operation: a
// 1000-row window is ~30k hashed code units, which is a scale where the
// split-halves form is the difference between microseconds and tens of
// milliseconds on the cold-open path.
const OFFSET_HI_64 = 0xcbf29ce4;
const OFFSET_LO_64 = 0x84222325;
const PRIME_HI_64 = 0x00000100;
const PRIME_LO_64 = 0x000001b3;

/**
 * 64-bit FNV-1a over `input`'s UTF-16 code units, as 16 lowercase hex
 * chars. Used for the held-window digest (`stores/threadWindowDigest.ts`),
 * whose Go counterpart folds the same code units so the two agree for
 * any input, not just ASCII.
 */
export function fnv1a64Hex(input: string): string {
  let hi = OFFSET_HI_64;
  let lo = OFFSET_LO_64;

  // 64-bit multiply by the FNV prime, keeping the low 64 bits. Every
  // partial product below stays under 2^53, so plain number arithmetic
  // is exact: the largest term is (2^32 - 1) * 0x1b3 ~= 1.9e12.
  const mixUnit = (unit: number): void => {
    lo = (lo ^ unit) >>> 0;
    const loProduct = lo * PRIME_LO_64;
    const nextLo = loProduct >>> 0;
    const carry = Math.floor(loProduct / 0x1_0000_0000);
    hi = (hi * PRIME_LO_64 + lo * PRIME_HI_64 + carry) >>> 0;
    lo = nextLo;
  };

  for (let i = 0; i < input.length; i += 1) mixUnit(input.charCodeAt(i));

  return hi.toString(16).padStart(8, '0') + lo.toString(16).padStart(8, '0');
}
