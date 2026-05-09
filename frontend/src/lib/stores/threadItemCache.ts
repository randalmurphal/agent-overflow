import type { CacheSnapshot } from 'virtua';
import type { Item } from '../types/models';
// Type-only import: SettledTurn lives next to ThreadPane in
// thread.svelte.ts. A value-level import would cycle through the
// pane's runtime; `import type` compiles to nothing at runtime so the
// cycle is purely structural.
import type { SettledTurn } from './thread.svelte';

/**
 * Snapshot of a thread's hydrated timeline state captured at the moment
 * the user navigated away. The cache holds the same shape `switchThread`
 * sets up: the windowed item list plus the pagination cursors and the
 * latest-settled-turn watermark. Streaming flags, approvals, design
 * artifacts, and channel messages deliberately stay out — they are
 * owned by the live ThreadPane and re-hydrated on the next switch.
 *
 * Items are treated as immutable by callers (the pane reassigns
 * `items =` rather than mutating in place), so snapshots store
 * references to the same string heap the live pane already holds.
 *
 * `virtuaCache` is virtua's per-row size cache (see
 * `VirtualizerHandle.getCache`). Replaying it on the next mount via
 * `<Virtualizer cache={...}>` avoids the lazy-measurement startup pass
 * where virtua underestimates `totalSize` at `ESTIMATED_ROW_SIZE × N`
 * until per-row ResizeObservers fire. Without it, a cache-hit thread
 * switch paints with a too-short `scrollHeight`, the bottom snapshot
 * lands above the eventual bottom, and the controller has to re-pin
 * across many positive contentRO deltas as rows remeasure. Optional
 * because virtua's mount path tolerates `undefined` (fresh start).
 */
export interface ThreadItemSnapshot {
  items: Item[];
  oldestLoadedTurnIndex: number | null;
  hasMoreHistory: boolean;
  latestSettledTurn: SettledTurn | null;
  virtuaCache?: CacheSnapshot;
}

/**
 * Bounded LRU cache for hydrated thread snapshots. Used by
 * `pane.switchThread` so re-entering a recently-visited thread can paint
 * the timeline immediately while phase 2 of the load runs in parallel.
 *
 * Capacity stays small (default 5). Memory cost per snapshot is
 * dominated by per-item `summary` and `payloadMeta` strings, which are
 * unbounded provider text — typical threads land at a few MB total
 * across the cache, but a long streaming-heavy thread can push a single
 * snapshot into the tens of MB. `MAX_CACHED_SNAPSHOT_ITEMS` rejects
 * snapshots above the size where caching pays off, keeping the worst-
 * case footprint bounded. Strings are reference-shared with the live
 * pane until the user navigates away; once an entry is the sole root,
 * eviction (LRU or event-bus invalidation) lets GC reclaim the strings.
 */
export const THREAD_ITEM_CACHE_CAP = 5;

/**
 * Soft cap on per-snapshot item count. Snapshots exceeding this are
 * not cached — the cost-to-benefit on huge threads inverts (cache
 * occupancy grows linearly while the win shrinks because phase 1's
 * 50-item slice already paints the visible viewport quickly).
 */
export const MAX_CACHED_SNAPSHOT_ITEMS = 1000;

export interface ThreadItemCache {
  get(threadId: string): ThreadItemSnapshot | null;
  set(threadId: string, snapshot: ThreadItemSnapshot): void;
  evict(threadId: string): void;
  clear(): void;
  /** Test/diagnostic only — exposes current entry count without
   *  unfreezing the LRU contract. */
  readonly size: number;
}

export function createThreadItemCache(cap: number = THREAD_ITEM_CACHE_CAP): ThreadItemCache {
  if (cap < 1) cap = 1;
  const byThread = new Map<string, ThreadItemSnapshot>();

  return {
    get(threadId) {
      const entry = byThread.get(threadId);
      if (!entry) return null;
      // Touch: re-insert at the end so eviction order is true LRU.
      byThread.delete(threadId);
      byThread.set(threadId, entry);
      // Return the stored reference. Callers (the pane) treat
      // snapshot fields as immutable — `items` is reassigned via
      // `items = cached.items` and the next mutation goes through
      // `mergeMissingItemsById` which always allocates a fresh
      // array. Skipping the read-side clone halves snapshot
      // allocation cost on a hot toggle-back.
      return entry;
    },

    set(threadId, snapshot) {
      // Snapshot the array with one shallow per-item clone so a
      // post-set caller mutation can't poison the cache. Item is a
      // flat primitive shape (see frontend/src/lib/types/models.ts);
      // strings/numbers are value types or reference-immutable.
      // `virtuaCache` is treated as opaque — virtua owns its shape and
      // freezes it for us via the public `getCache()` API.
      const stored: ThreadItemSnapshot = {
        items: snapshot.items.map((it) => ({ ...it })),
        oldestLoadedTurnIndex: snapshot.oldestLoadedTurnIndex,
        hasMoreHistory: snapshot.hasMoreHistory,
        latestSettledTurn: snapshot.latestSettledTurn,
        virtuaCache: snapshot.virtuaCache,
      };
      byThread.delete(threadId);
      byThread.set(threadId, stored);
      while (byThread.size > cap) {
        const oldest = byThread.keys().next().value;
        if (!oldest) break;
        byThread.delete(oldest);
      }
    },

    evict(threadId) {
      byThread.delete(threadId);
    },

    clear() {
      byThread.clear();
    },

    get size() {
      return byThread.size;
    },
  };
}

/**
 * Process-wide cache instance. One owner across all panes — multi-pane
 * tiling can grow this into per-pane caches if the working-set
 * assumption (a few panes share a small set of hot threads) ever stops
 * matching usage.
 */
export const threadItemCache: ThreadItemCache = createThreadItemCache();

/** Test helper: drop every cached snapshot. Real code should use
 *  `evict(threadId)` for the targeted-eviction path. */
export function clearThreadItemCacheForTest(): void {
  threadItemCache.clear();
}
