import { boundedPayloadVersionString } from './payloadVersion';

// Module-level LRU cache for fetched payload chunks. Survives both
// virtua row remount AND switchThread, so re-entering a thread that
// already loaded a payload renders synchronously from cache instead of
// replaying the empty-then-loaded paint cycle that whipsaws virtua's
// per-row size cache and triggers visible scroll-anchoring jumps.
//
// Keyed by JSON-encoded (threadId, payloadId, version) tuples so item
// updates that bump `updatedAt` invalidate cleanly without delimiter
// collision risk from provider-derived ids. Version fragments are
// bounded before key construction so large provider metadata cannot
// hide uncounted megabytes in Map keys.
//
// Cache is bytes-aware rather than entry-count: 16 MB ≈ 50 typical
// preview-loaded diffs, comfortably bigger than any realistic
// per-thread visible-row working set, while still bounding worst-case
// heap if the user opens a long string of large threads in sequence.
const PAYLOAD_CACHE_MAX_BYTES = 16 * 1024 * 1024;

export interface PayloadCacheEntry {
  chunks: string[];
  hasFullChunks: boolean;
  totalSize: number;
  isComplete: boolean;
  loadedBytes: number;
  bytes: number;
}

const payloadCache = new Map<string, PayloadCacheEntry>();
let payloadCacheBytes = 0;

function payloadCacheKey(threadId: string, payloadId: string, version: unknown): string {
  return JSON.stringify([threadId, payloadId, payloadVersionKey(version)]);
}

function payloadCacheThreadPrefix(threadId: string): string {
  const encodedThreadTuple = JSON.stringify([threadId]);
  return `${encodedThreadTuple.slice(0, -1)},`;
}

// Type-tag so `null`, `undefined`, `''`, `0`, `false`, `'null'` all
// map to distinct keys. The plain stringification path that v1 used
// folded `null`/`undefined`/`''` together — fine for `item.updatedAt`
// (always a number) but a foot-gun for any future caller.
export function payloadVersionKey(version: unknown): string {
  if (version === undefined) return 'u';
  if (version === null) return 'n';
  if (typeof version === 'string') return `s:${boundedPayloadVersionString(version)}`;
  if (typeof version === 'number') return `n:${version}`;
  if (typeof version === 'boolean') return `b:${version}`;
  try {
    return `j:${boundedPayloadVersionString(JSON.stringify(version))}`;
  } catch {
    return `r:${boundedPayloadVersionString(String(version))}`;
  }
}

export function readPayloadCache(
  threadId: string,
  payloadId: string,
  version: unknown,
): PayloadCacheEntry | undefined {
  const key = payloadCacheKey(threadId, payloadId, version);
  const entry = payloadCache.get(key);
  if (!entry) return undefined;
  // LRU touch.
  payloadCache.delete(key);
  payloadCache.set(key, entry);
  return entry;
}

export function writePayloadCache(
  threadId: string,
  payloadId: string,
  version: unknown,
  entry: Omit<PayloadCacheEntry, 'bytes'>,
): void {
  const key = payloadCacheKey(threadId, payloadId, version);
  const existing = payloadCache.get(key);
  if (existing) {
    payloadCache.delete(key);
    payloadCacheBytes -= existing.bytes;
  }
  // String length is UTF-16 code units; the actual heap footprint is
  // ~2× this value because V8 stores strings as UTF-16 internally.
  // The cap name reflects "char budget" — see PAYLOAD_CACHE_MAX_BYTES.
  let bytes = 0;
  for (const c of entry.chunks) bytes += c.length;
  if (bytes > PAYLOAD_CACHE_MAX_BYTES) return;
  const stored: PayloadCacheEntry = { ...entry, bytes };
  payloadCache.set(key, stored);
  payloadCacheBytes += bytes;
  while (payloadCacheBytes > PAYLOAD_CACHE_MAX_BYTES) {
    const oldestKey = payloadCache.keys().next().value;
    if (oldestKey === undefined) break;
    const oldest = payloadCache.get(oldestKey);
    payloadCache.delete(oldestKey);
    if (oldest) payloadCacheBytes -= oldest.bytes;
  }
}

/** Drop every cached payload owned by `threadId`. Called from
 *  `removeThread` so a deleted thread can't wedge cached bytes. */
export function clearPayloadCacheForThread(threadId: string): void {
  const prefix = payloadCacheThreadPrefix(threadId);
  // Iterate keys lazily so we don't allocate a full snapshot on every
  // delete — Map iteration order is insertion order, and Map.delete
  // during iteration is well-defined (only the deleted key is skipped).
  for (const key of payloadCache.keys()) {
    if (!key.startsWith(prefix)) continue;
    const entry = payloadCache.get(key);
    payloadCache.delete(key);
    if (entry) payloadCacheBytes -= entry.bytes;
  }
}

/** Test-only reset. */
export function __resetPayloadCacheForTest(): void {
  payloadCache.clear();
  payloadCacheBytes = 0;
}

/** Test-only inspection. */
export function __payloadCacheStatsForTest(): { entries: number; bytes: number } {
  return { entries: payloadCache.size, bytes: payloadCacheBytes };
}
