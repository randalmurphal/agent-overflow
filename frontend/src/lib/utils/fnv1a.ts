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
 * A 64-bit value as two 32-bit halves. The held-window digest XORs row
 * hashes together (`stores/threadWindowDigest.ts`), so the hash has to be
 * available as numbers rather than only as its rendered hex.
 */
export interface Fnv1a64 {
  hi: number;
  lo: number;
}

/** The identity of the digest's XOR fold: what an empty set folds to. */
export const FNV1A64_ZERO: Readonly<Fnv1a64> = Object.freeze({ hi: 0, lo: 0 });

/**
 * 64-bit FNV-1a over `input`'s UTF-16 code units. Used for the
 * held-window digest (`stores/threadWindowDigest.ts`), whose Go
 * counterpart folds the same code units so the two agree for any input,
 * not just ASCII.
 */
export function fnv1a64(input: string): Fnv1a64 {
  let hi = OFFSET_HI_64;
  let lo = OFFSET_LO_64;

  // 64-bit multiply by the FNV prime, keeping the low 64 bits. Every
  // partial product below stays under 2^53, so plain number arithmetic
  // is exact: the largest term is (2^32 - 1) * 0x1b3 ~= 1.9e12.
  for (let i = 0; i < input.length; i += 1) {
    lo = (lo ^ input.charCodeAt(i)) >>> 0;
    const loProduct = lo * PRIME_LO_64;
    const nextLo = loProduct >>> 0;
    const carry = Math.floor(loProduct / 0x1_0000_0000);
    hi = (hi * PRIME_LO_64 + lo * PRIME_HI_64 + carry) >>> 0;
    lo = nextLo;
  }

  return { hi, lo };
}

/** 16 lowercase hex characters, the wire form of a 64-bit digest. */
export function fnv1a64Hex(input: string): string {
  return formatFnv1a64(fnv1a64(input));
}

/** Render a folded value as the digest's 16 lowercase hex characters. */
export function formatFnv1a64(value: Fnv1a64): string {
  return (
    (value.hi >>> 0).toString(16).padStart(8, '0')
    + (value.lo >>> 0).toString(16).padStart(8, '0')
  );
}

/**
 * Read a rendered digest back into halves, or null when it is not this
 * algorithm's output. Callers fold a server-produced digest
 * (`ActivityRunStub.unshippedDigest`) into their own, so a malformed one
 * has to be refused rather than folded as garbage.
 */
export function parseFnv1a64(hex: string): Fnv1a64 | null {
  if (hex.length !== 16 || !/^[0-9a-f]{16}$/.test(hex)) return null;
  return {
    hi: Number.parseInt(hex.slice(0, 8), 16) >>> 0,
    lo: Number.parseInt(hex.slice(8), 16) >>> 0,
  };
}

/** XOR two folded values. Order-free, which is what makes the digest composable. */
export function xorFnv1a64(a: Fnv1a64, b: Fnv1a64): Fnv1a64 {
  return { hi: (a.hi ^ b.hi) >>> 0, lo: (a.lo ^ b.lo) >>> 0 };
}
