import type { Item } from '../types/models';
import type { SubagentFoldSnapshot } from '../utils/subagentFold';
import type { TimelineCursorLike } from './threadItems';
import type { SettledTurn } from './threadTurnProjection';
import type { ThreadHistoryStamp } from './threadHistoryStamps';
import { onThreadHistoryInvalidated } from './threadIdentityInvalidation';

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
 * Row-size snapshots stay out of THIS cache — they live in the size-priors
 * store (`utils/virtual/priors.ts`) because their validity key (scroll-pane
 * width + structure signature + expansion signature) is MessageTimeline
 * state, not store state. Measured sizes are only valid with the row UI
 * state that produced them, so the engine consumes a snapshot lazily per
 * row only when the key still matches and otherwise falls back to kind /
 * default estimates behind MessageTimeline's warm-up visibility gate.
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
  /**
   * The history stamp that described `items` at the moment they were
   * snapshotted (docs/architecture/thread-replica-sync.md §3). Paired here
   * rather than looked up per open, because a stamp is only safe to send
   * as `haveEpoch`/`haveRev` alongside the content it describes: a
   * `fresh` answer obliges the client to keep the rows it already holds,
   * so a stamp sent without them would resolve to an empty pane, and a
   * stamp that advanced past them (a revert applied to a thread no pane
   * was showing) would resurrect cut rows. Null means "ask for a page".
   */
  historyStamp?: ThreadHistoryStamp | null;
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
  evictMatching(matches: (threadId: string) => boolean): void;
  /**
   * Transport-gap recovery (docs/architecture/thread-replica-sync.md §3.4):
   * strip the paired stamp from every snapshot whose stamp was NOT
   * sync-attested, keeping the snapshots themselves.
   *
   * A gap means frames were dropped with no way to say which thread's
   * they were. An UNATTESTED stamp (adopted from `turn_completed` /
   * `user_message:reverted`) names a rev this client may never have
   * received the content for, so a warm re-entry could send it, get
   * `fresh`, and keep a window missing rows — for the rest of the
   * session. An ATTESTED stamp cannot lie the same way: it describes
   * rows a sync actually returned, and any mutation since then advanced
   * the backend's rev past it, so the same `fresh` is impossible.
   * Dropping the whole cache would work but would also throw away every
   * warm paint the gap did not endanger.
   */
  dropUnattestedStamps(): void;
  /** Test/diagnostic only — exposes current entry count without
   *  unfreezing the LRU contract. */
  readonly size: number;
  /** Diagnostic accounting (memoryReport): entry/item counts and the
   *  tracked char budget. Does not touch LRU order. */
  stats(): { threads: number; items: number; chars: number };
}

export function createThreadItemCache(cap: number = THREAD_ITEM_CACHE_CAP): ThreadItemCache {
  if (cap < 1) cap = 1;
  const byThread = new Map<string, StoredThreadItemSnapshot>();
  let cachedChars = 0;

  return {
    evictMatching(matches) {
      for (const [id, row] of byThread) {
        if (!matches(id)) continue;
        cachedChars -= row.chars;
        byThread.delete(id);
      }
    },
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
        historyStamp: snapshot.historyStamp ? { ...snapshot.historyStamp } : null,
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

    dropUnattestedStamps() {
      // Safe to mutate in place: `set` stores a private clone of the
      // stamp, and `get` hands out the stored snapshot only for the
      // pane to read.
      for (const entry of byThread.values()) {
        if (entry.snapshot.historyStamp && !entry.snapshot.historyStamp.attested) {
          entry.snapshot.historyStamp = null;
        }
      }
    },

    get size() {
      return byThread.size;
    },

    stats() {
      let items = 0;
      for (const entry of byThread.values()) items += entry.snapshot.items.length;
      return { threads: byThread.size, items, chars: cachedChars };
    },
  };
}

function estimateSnapshotChars(snapshot: ThreadItemSnapshot): number {
  let chars = 0;
  for (const item of snapshot.items) {
    chars += item.summary?.length ?? 0;
    chars += item.meta?.length ?? 0;
    chars += item.payloadMeta?.length ?? 0;
    // Preview spans are a highlight blob that rides the item row on the
    // wire and can dwarf the summary it decorates. Counted here so the
    // in-memory snapshot and its durable counterpart (replica
    // `estimateBodyChars`) are measured on ONE scale — the two tiers
    // share their per-window caps, so an estimator that skipped this
    // would let a window the replica refuses sit in the LRU.
    chars += item.payloadPreviewSpans?.length ?? 0;
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

// A backend identity change (new backend, or a generation re-mint after
// a database restore) makes every snapshot's paired `historyStamp` a
// claim about a history that no longer exists — and the rows themselves
// may belong to a divergent lineage whose counters coincidentally
// match, which a later `fresh` echo would never correct. Same rule as
// the durable replica: drop, never migrate. L1 is not exempt just
// because it is in memory.
onThreadHistoryInvalidated((owns) => threadItemCache.evictMatching(owns));

/** Test helper: drop every cached snapshot. Real code should use
 *  `evict(threadId)` for the targeted-eviction path. */
export function clearThreadItemCacheForTest(): void {
  threadItemCache.clear();
}
