import type { Item } from '../types/models';
import type { SubagentFoldSnapshot } from '../utils/subagentFold';
import type { TimelineCursorLike } from './threadItems';
import type { SettledTurn } from './threadTurnProjection';

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
 * Row-size snapshots stay out of THIS cache — they live in a separate
 * session store (`utils/threadVirtuaSizeCache.ts`) because their validity
 * key (scroll-pane width + structure signature + expansion signature) is
 * MessageTimeline state, not store state. Virtua's measured sizes are only
 * valid with the row UI state that produced them, so that store replays a
 * snapshot only when the key still matches and otherwise falls back to fresh
 * measurement behind MessageTimeline's warm-up visibility gate.
 */
export interface ThreadItemSnapshot {
  items: Item[];
  oldestLoadedCursor?: TimelineCursorLike | null;
  newestLoadedCursor?: TimelineCursorLike | null;
  oldestLoadedTurnIndex: number | null;
  newestLoadedTurnIndex: number | null;
  hasMoreHistory: boolean;
  hasMoreNewer: boolean;
  latestSettledTurn: SettledTurn | null;
  /**
   * Folded (evicted) subagent children keyed by launch anchor. The
   * snapshot's `items` deliberately exclude these rows, so the fold must
   * travel with them or a warm re-entry renders collapsed cards with
   * zeroed counts until the next live event or hydration.
   */
  subagentFolds?: SubagentFoldSnapshot | null;
}

/**
 * Bounded LRU cache for hydrated thread snapshots. Used by
 * `pane.switchThread` so re-entering a recently-visited thread can paint
 * the timeline immediately while phase 2 of the load runs in parallel.
 *
 * Capacity stays small (default 5). Memory cost per snapshot is
 * dominated by per-item `summary`, `meta`, and `payloadMeta` strings,
 * which are unbounded provider text. The item-count cap catches broad
 * windows; the character budgets catch normal-sized windows with huge
 * assistant prose. Strings are reference-shared with the live pane until
 * the user navigates away; once an entry is the sole root, eviction (LRU,
 * budget pressure, or event-bus invalidation) lets GC reclaim the strings.
 */
export const THREAD_ITEM_CACHE_CAP = 5;

/**
 * Soft cap on per-snapshot item count. Snapshots exceeding this are
 * not cached — the cost-to-benefit on huge threads inverts (cache
 * occupancy grows linearly while the win shrinks because the 200-item
 * initial slice already paints the visible viewport quickly).
 */
export const MAX_CACHED_SNAPSHOT_ITEMS = 1000;
export const MAX_CACHED_SNAPSHOT_CHARS = 2 * 1024 * 1024;
export const THREAD_ITEM_CACHE_MAX_CHARS = 6 * 1024 * 1024;

interface StoredThreadItemSnapshot {
  snapshot: ThreadItemSnapshot;
  chars: number;
}

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
  const byThread = new Map<string, StoredThreadItemSnapshot>();
  let cachedChars = 0;

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
      return entry.snapshot;
    },

    set(threadId, snapshot) {
      const chars = estimateSnapshotChars(snapshot);
      if (snapshot.items.length > MAX_CACHED_SNAPSHOT_ITEMS || chars > MAX_CACHED_SNAPSHOT_CHARS) {
        const existing = byThread.get(threadId);
        if (existing) cachedChars -= existing.chars;
        byThread.delete(threadId);
        return;
      }
      // Snapshot the array with one shallow per-item clone so a
      // post-set caller mutation can't poison the cache. Item is a
      // flat primitive shape (see frontend/src/lib/types/models.ts);
      // strings/numbers are value types or reference-immutable.
      const stored: ThreadItemSnapshot = {
        items: snapshot.items.map((it) => ({ ...it })),
        oldestLoadedCursor: snapshot.oldestLoadedCursor ? { ...snapshot.oldestLoadedCursor } : null,
        newestLoadedCursor: snapshot.newestLoadedCursor ? { ...snapshot.newestLoadedCursor } : null,
        oldestLoadedTurnIndex: snapshot.oldestLoadedTurnIndex,
        newestLoadedTurnIndex: snapshot.newestLoadedTurnIndex,
        hasMoreHistory: snapshot.hasMoreHistory,
        hasMoreNewer: snapshot.hasMoreNewer,
        latestSettledTurn: snapshot.latestSettledTurn,
        // Reference-shared, not cloned: `snapshot()` allocates fresh
        // plain data each call and `restore()` copies out of it, so no
        // caller can mutate a stored fold after set().
        subagentFolds: snapshot.subagentFolds ?? null,
      };
      const existing = byThread.get(threadId);
      if (existing) cachedChars -= existing.chars;
      byThread.delete(threadId);
      byThread.set(threadId, { snapshot: stored, chars });
      cachedChars += chars;
      while (byThread.size > cap || cachedChars > THREAD_ITEM_CACHE_MAX_CHARS) {
        const oldest = byThread.keys().next().value;
        if (!oldest) break;
        const evicted = byThread.get(oldest);
        byThread.delete(oldest);
        if (evicted) cachedChars -= evicted.chars;
      }
    },

    evict(threadId) {
      const existing = byThread.get(threadId);
      if (existing) cachedChars -= existing.chars;
      byThread.delete(threadId);
    },

    clear() {
      byThread.clear();
      cachedChars = 0;
    },

    get size() {
      return byThread.size;
    },
  };
}

function estimateSnapshotChars(snapshot: ThreadItemSnapshot): number {
  let chars = 0;
  for (const item of snapshot.items) {
    chars += item.summary?.length ?? 0;
    chars += item.meta?.length ?? 0;
    chars += item.payloadMeta?.length ?? 0;
  }
  // Folded subagent children ride the snapshot as one id string per
  // evicted row; a subagent-heavy turn can make the fold outweigh the
  // visible rows, so it must count against the same budgets.
  for (const anchor of snapshot.subagentFolds?.anchors ?? []) {
    chars += anchor.terminalPreview.length;
    for (const id of anchor.evictedIds) chars += id.length;
  }
  return chars;
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
