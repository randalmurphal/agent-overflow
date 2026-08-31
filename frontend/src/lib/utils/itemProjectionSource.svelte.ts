import { GetThreadItemProjectionSource } from '../stores/bindings';
import { payloadVersionKey } from './payloadDataCache';

/**
 * The client half of the wire projection's recovery route.
 *
 * The backend bounds what an item window carries (internal/itemwire):
 * values it removes are named by a marker on the row, and the complete
 * stored value stays one RPC away. This module is that RPC — bounded,
 * deduped, and deliberately NOT part of the row.
 *
 * ## Why the fetched value never goes back into the row
 *
 * A row is the shape the wire delivered. Merging a fetched patch into
 * `item.payloadMeta` would produce a fourth thing: a row that is neither
 * what the backend sent nor what it stores, carrying a marker that no
 * longer describes it. Three consequences make that a bug rather than a
 * convenience:
 *
 *  - The replica (IndexedDB) and the L1 window cache persist rows. A
 *    merged row written there is an elided row masquerading as a
 *    complete one — it would paint on a later cold open with no marker
 *    to tell the card its patch is partial, and nothing would fetch.
 *  - `reconcileItemWindow` preserves row identity with `===` so
 *    unchanged rows do not repaint. Rewriting rows on fetch would
 *    invalidate that identity and repaint the window.
 *  - The invariant the marker rests on — a row is elided if and only if
 *    it says so — survives only while nothing edits rows after arrival.
 *
 * So markers are render-time signals. The row keeps its marker for its
 * whole life, this cache holds the fetched value, and the component
 * composes the two at render. A cached-elided row and a
 * fetched-full-on-expand row therefore coexist without a merge: the row
 * is the same object in both cases, and only this cache differs.
 *
 * ## Bounds
 *
 * Keyed by (threadId, itemId, version) exactly like payloadDataCache, so
 * an item update invalidates cleanly. Bytes-aware LRU rather than an
 * entry count, evicted per thread on switch/close/delete alongside the
 * highlight spans. Nothing here is persisted.
 */

/**
 * ~2 MB of recovered field text. An entry is one item's stored
 * `payloadMeta` plus its preview spans — single-digit KB for the diff
 * rows that reach this path — so this holds a few hundred expanded
 * cards, well past any thread's visible working set, while bounding the
 * heap if a reader expands their way through a long session.
 */
export const ITEM_PROJECTION_SOURCE_MAX_BYTES = 2 * 1024 * 1024;

export interface ItemProjectionSource {
  /** Complete stored `items.meta`. Empty when the row had none. */
  meta: string;
  /** Complete stored `payloadMeta`, previews and all. */
  payloadMeta: string;
  /** Complete stored `payloads.preview_spans`. */
  payloadPreviewSpans: string;
}

interface SourceEntry {
  source: ItemProjectionSource | null;
  error: string | null;
  bytes: number;
}

const entries = new Map<string, SourceEntry>();
let totalBytes = 0;
const inFlight = new Set<string>();

/**
 * Reactivity: consumers read through `itemProjectionSourceGeneration()`
 * so a landed fetch repaints exactly the cards that asked. Same shape as
 * the diff span cache — one module-level counter rather than a reactive
 * Map, so a hit costs a plain lookup.
 */
let generation = $state(0);

export function itemProjectionSourceGeneration(): number {
  return generation;
}

function sourceKey(threadId: string, itemId: string, version: unknown): string {
  return JSON.stringify([threadId, itemId, payloadVersionKey(version)]);
}

function threadPrefix(threadId: string): string {
  const encoded = JSON.stringify([threadId]);
  return `${encoded.slice(0, -1)},`;
}

function store(key: string, entry: Omit<SourceEntry, 'bytes'>): void {
  const existing = entries.get(key);
  if (existing) {
    entries.delete(key);
    totalBytes -= existing.bytes;
  }
  const source = entry.source;
  const bytes = source
    ? source.meta.length + source.payloadMeta.length + source.payloadPreviewSpans.length
    : 0;
  // An entry too large to cache would evict everything and then itself;
  // drop it instead. The card re-fetches on its next expand, which is
  // the same cost it already paid, rather than emptying the cache for
  // every other card on screen.
  if (bytes > ITEM_PROJECTION_SOURCE_MAX_BYTES) {
    generation += 1;
    return;
  }
  entries.set(key, { ...entry, bytes });
  totalBytes += bytes;
  while (totalBytes > ITEM_PROJECTION_SOURCE_MAX_BYTES) {
    const oldestKey = entries.keys().next().value;
    if (oldestKey === undefined || oldestKey === key) break;
    const oldest = entries.get(oldestKey);
    entries.delete(oldestKey);
    if (oldest) totalBytes -= oldest.bytes;
  }
  generation += 1;
}

export interface ItemProjectionSourceState {
  source: ItemProjectionSource | null;
  loading: boolean;
  error: string | null;
}

/**
 * Read what is known about one item's stored fields. Reads the
 * generation so a caller inside a `$derived` re-evaluates when a fetch
 * lands; never starts one itself.
 */
export function readItemProjectionSource(
  threadId: string,
  itemId: string,
  version: unknown,
): ItemProjectionSourceState {
  void generation;
  const key = sourceKey(threadId, itemId, version);
  const entry = entries.get(key);
  if (entry) {
    // LRU touch.
    entries.delete(key);
    entries.set(key, entry);
    return { source: entry.source, loading: false, error: entry.error };
  }
  return { source: null, loading: inFlight.has(key), error: null };
}

/**
 * Fetch the stored fields for one item unless they are already cached,
 * already failing, or already in flight.
 *
 * Called from a card's expand, never from its mount: a collapsed card
 * renders nothing the projection removed, so fetching on arrival would
 * spend on the wire exactly what the projection just saved.
 */
export function requestItemProjectionSource(
  threadId: string,
  itemId: string,
  version: unknown,
): void {
  if (!threadId || !itemId) return;
  const key = sourceKey(threadId, itemId, version);
  if (entries.has(key) || inFlight.has(key)) return;
  inFlight.add(key);
  void (async () => {
    try {
      const result = (await GetThreadItemProjectionSource(threadId, itemId)) as {
        meta?: string;
        payloadMeta?: string;
        payloadPreviewSpans?: string;
      } | null;
      inFlight.delete(key);
      store(key, {
        source: {
          meta: result?.meta ?? '',
          payloadMeta: result?.payloadMeta ?? '',
          payloadPreviewSpans: result?.payloadPreviewSpans ?? '',
        },
        error: null,
      });
    } catch (err) {
      inFlight.delete(key);
      // Cached as a failure rather than left absent, so an expanded card
      // shows the error and its retry instead of re-firing the same
      // failing call on every repaint.
      store(key, { source: null, error: err instanceof Error ? err.message : String(err) });
    }
  })();
}

/** Drop a cached failure so the next request re-fetches. */
export function retryItemProjectionSource(
  threadId: string,
  itemId: string,
  version: unknown,
): void {
  const key = sourceKey(threadId, itemId, version);
  const entry = entries.get(key);
  if (!entry || entry.error === null) return;
  entries.delete(key);
  totalBytes -= entry.bytes;
  generation += 1;
  requestItemProjectionSource(threadId, itemId, version);
}

/**
 * Drop everything recovered for `threadId`. Called wherever the
 * highlight spans are dropped — pane switch, pane close, thread delete —
 * because both are render-time caches for rows the pane no longer shows.
 */
export function clearItemProjectionSourcesForThread(threadId: string): void {
  const prefix = threadPrefix(threadId);
  let removed = false;
  for (const key of entries.keys()) {
    if (!key.startsWith(prefix)) continue;
    const entry = entries.get(key);
    entries.delete(key);
    if (entry) totalBytes -= entry.bytes;
    removed = true;
  }
  if (removed) generation += 1;
}

/** Test-only reset. */
export function __resetItemProjectionSourceCacheForTest(): void {
  entries.clear();
  inFlight.clear();
  totalBytes = 0;
  generation += 1;
}

/** Test-only inspection. */
export function __itemProjectionSourceStatsForTest(): { entries: number; bytes: number } {
  return { entries: entries.size, bytes: totalBytes };
}
