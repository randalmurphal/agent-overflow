// Memoizes the FNV-1a hash + length signature of a PatchLine's
// stripped source text. Renderers + the body's dispatch coordinator
// both feed lines into the token cache via `tokenCacheKey(...)`,
// which re-walks the line each call. With 30 visible files × 200
// lines × ~5 cache-gen bumps per dispatch cycle, that's ~30K
// hashes per cycle on the hot path. The PatchLine objects are
// stable for the lifetime of a parsed payload, so a WeakMap keyed
// on the line gets us O(1) lookups after first hash.

import { stripPatchLinePrefix, type PatchLine } from './patchFiles';

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

const sourceKeyCache = new WeakMap<PatchLine, string>();

/**
 * Returns the `${length}:${hash}` signature for the line's stripped
 * source text. Cached per-line via WeakMap so successive renders
 * and dispatches don't re-hash. The cache entry is reclaimed
 * automatically when the PatchLine is no longer referenced.
 */
export function patchLineSourceKey(line: PatchLine): string {
  const cached = sourceKeyCache.get(line);
  if (cached !== undefined) return cached;
  const source = stripPatchLinePrefix(line);
  const sig = `${source.length}:${fnv1a32(source)}`;
  sourceKeyCache.set(line, sig);
  return sig;
}

// No reset helper exposed: WeakMap entries are released when the
// keying PatchLine is GC'd. Each test that wants a clean slate just
// constructs fresh PatchLine objects.
