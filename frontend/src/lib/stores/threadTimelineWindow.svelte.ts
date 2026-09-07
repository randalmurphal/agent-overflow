import { tick } from 'svelte';
import type { Item, Thread } from '../types/models';
import type { PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
import type { ThreadItemSnapshot } from './threadItemCache';
import {
  GetThreadItem,
  ListItemsAfterCursor,
  ListItemsBeforeCursor,
  ListThreadSliceAround,
} from './bindings';
import { addToast } from './toast.svelte';
import {
  compareCursors,
  compareItemsByTimelinePosition,
  cursorFromItem,
  cursorsAfterItemUpserts,
  cursorIsValid,
  itemsForThread,
  mergeItemsById,
  mergeMissingItemsById,
  reconcileItemWindow,
  type TimelineCursorLike,
} from './threadItems';
import { getActiveTurn } from './threadStatuses.svelte';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  LOAD_OLDER_ITEM_BUDGET,
  loadOlderResult,
  wantsInlinePreviews,
  type LoadOlderResult,
  type PaneScrollController,
} from './threadPaneShared';

export interface ThreadTimelineWindowOptions {
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  /** The pane's items-replacement chokepoint (index rebuild, fold retention, dispose, revision bump). */
  replaceTimelineItems(
    nextItems: Item[],
    options?: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
      afterCommit?: () => void;
    },
  ): boolean;
  /**
   * Install the initial cache/backend slice without classifying the slice
   * itself as a live mutation.
   */
  installTimelineItems(
    nextItems: Item[],
    options?: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
      afterCommit?: () => void;
    },
  ): boolean;
  getThread(): Thread | null;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** Registered pane scroll controller (or null). applyPrunedWindow queries its retention guard. */
  getScrollController(): PaneScrollController | null;
  /** Pane-owned subagent transcript hydration — loadUntilItem's subtree hydration. */
  hydrateSubagentChildren(rootItemID: string): Promise<boolean>;
  /**
   * Every row the OPEN agent companion is rendering (the scope trail's
   * whole subtree), or null when no pane is open. The prune cuts consult
   * it so a window cut can never fold rows out from under a mounted
   * companion — the same blanking the eviction chokepoint's
   * `agentPaneHeldRows` exists to prevent (live incident 2026-08-22).
   */
  getHeldRowIds?(): ReadonlySet<string> | null;
}

/**
 * Windowed-history / paging machinery for a thread pane's timeline: the
 * loaded window's cursors and flags, the prune paths, and the four load
 * methods (`loadOlder`, `loadNewer`, `loadRecentTail`, `loadUntilItem`).
 * The pane data layer stays the sole mutator of `items` — this factory
 * reads/replaces the window through `options.getItems()` /
 * `options.replaceTimelineItems()`, so item-array assignment still
 * happens inside the pane's own reactive scope.
 */
export interface ThreadTimelineWindow {
  /**
   * Inclusive floor of the loaded history window. Consumers use this
   * to render "Load older messages" and, in scroll-to-item flows, to
   * decide whether a target coordinate is already in view.
   */
  readonly oldestLoadedCursor: TimelineCursorLike | null;
  readonly newestLoadedCursor: TimelineCursorLike | null;
  readonly oldestLoadedTurnIndex: number | null;
  readonly newestLoadedTurnIndex: number | null;
  readonly hasMoreHistory: boolean;
  readonly hasMoreNewer: boolean;
  readonly hasDeferredRecentWindowPrune: boolean;
  readonly loadingOlder: boolean;
  readonly loadingNewer: boolean;
  /**
   * The reader explicitly paged history into this window (`loadOlder`,
   * or a `loadUntilItem` recenter). While set, NO automatic prune may
   * drop rows: the reader asked to see this history, and reclaiming a
   * few MB of summary rows is never worth taking their conversation
   * away (user ruling, 2026-08-31). Clears when the window is rebuilt
   * at a bounded size — thread switch, cache restore, tail reload.
   */
  readonly userPinnedHistory: boolean;
  /** Apply `switchThread`'s single initial paged load (cache-miss path). */
  applyInitialSlice(paged: PagedItems, threadID: string): void;
  /** Refresh cursors + hasMore flags from a paged response against the current window. Also used directly by `refreshFromBackend`. */
  applyWindowMetadataFromPaged(paged: PagedItems): void;
  /** Cache-hit branch of `installCacheOrFreshState`: cursor/flag bookkeeping only — `replaceTimelineItems` + fold restore stay pane-side. */
  installFromSnapshot(cached: ThreadItemSnapshot): void;
  /** Fresh-thread branch of `installCacheOrFreshState`, and `clear()`. Never resets `pagingGeneration` (stays monotonic for the pane's lifetime). */
  resetForFreshThread(): void;
  /** `runParallelLoad`'s load-items error branch: window nulls only, no loading-flag or prune-pending touch. */
  resetAfterLoadError(): void;
  /** Streaming upsert dropped newer items below/above the window: re-arm the "load newer" affordance. */
  noteDroppedNewerItems(): void;
  /** Follow repositioned anchors, retaining the capped floor and ordinary tail-append policy. */
  refreshCursorsAfterUpserts(changedItems: readonly Item[], appended: boolean, previousItems: readonly Item[]): void;
  pruneToRecentWindowIfNeeded(options?: {
    hasMoreNewerAfterPrune?: boolean;
  }): void;
  retryDeferredRecentWindowPrune(): void;
  /**
   * `settleTurn`'s prune entry: records the prune as pending for the
   * quiet scheduler when a mounted timeline can avoid the structural
   * reconciliation during activity, and applies it immediately otherwise.
   */
  settleRecentWindowPrune(): void;
  loadOlder(): Promise<LoadOlderResult>;
  loadUntilItem(itemID: string): Promise<boolean>;
  loadNewer(): Promise<LoadOlderResult>;
  loadRecentTail(): Promise<boolean>;
}

interface PrunedWindow {
  items: Item[];
  oldestCursor: TimelineCursorLike | null;
  newestCursor: TimelineCursorLike | null;
}

type PrunedWindowApplyResult = 'applied' | 'deferred';
type PrunedWindowVetoPolicy = 'defer' | 'force';

function cloneCursor(
  cursor: TimelineCursorLike | null | undefined,
): TimelineCursorLike | null {
  return cursorIsValid(cursor)
    ? {
        turnIndex: cursor.turnIndex,
        itemIndex: cursor.itemIndex,
        itemId: cursor.itemId ?? '',
      }
    : null;
}

function cursorForBinding(cursor: TimelineCursorLike): {
  turnIndex: number;
  itemIndex: number;
  itemId: string;
} {
  return {
    turnIndex: cursor.turnIndex,
    itemIndex: cursor.itemIndex,
    itemId: cursor.itemId ?? '',
  };
}

function oldestCursorFromItems(
  nextItems: readonly Item[],
): TimelineCursorLike | null {
  return nextItems.length === 0 ? null : cursorFromItem(nextItems[0]);
}

function newestCursorFromItems(
  nextItems: readonly Item[],
): TimelineCursorLike | null {
  return nextItems.length === 0
    ? null
    : cursorFromItem(nextItems[nextItems.length - 1]);
}

function firstCursorAtTurn(
  nextItems: readonly Item[],
  turnIndex: number,
): TimelineCursorLike | null {
  const item = nextItems.find(
    (candidate) => candidate.turnIndex === turnIndex,
  );
  return item ? cursorFromItem(item) : null;
}

function lastCursorAtTurn(
  nextItems: readonly Item[],
  turnIndex: number,
): TimelineCursorLike | null {
  for (let index = nextItems.length - 1; index >= 0; index -= 1) {
    const item = nextItems[index];
    if (item.turnIndex === turnIndex) return cursorFromItem(item);
  }
  return null;
}

function pagedOldestCursor(
  paged: PagedItems,
  fallbackItems: readonly Item[],
): TimelineCursorLike | null {
  const explicit = (
    paged as PagedItems & { oldestCursor?: TimelineCursorLike }
  ).oldestCursor;
  const cloned = cloneCursor(explicit);
  if (cloned) return cloned;
  const turnIndex = (paged as PagedItems & { oldestTurnIndex?: number })
    .oldestTurnIndex;
  if (turnIndex !== undefined && turnIndex >= 0) {
    return (
      firstCursorAtTurn(fallbackItems, turnIndex) ?? {
        turnIndex,
        itemIndex: 0,
        itemId: '',
      }
    );
  }
  return oldestCursorFromItems(fallbackItems);
}

function pagedNewestCursor(
  paged: PagedItems,
  fallbackItems: readonly Item[],
): TimelineCursorLike | null {
  const explicit = (
    paged as PagedItems & { newestCursor?: TimelineCursorLike }
  ).newestCursor;
  const cloned = cloneCursor(explicit);
  if (cloned) return cloned;
  const turnIndex = (paged as PagedItems & { newestTurnIndex?: number })
    .newestTurnIndex;
  if (turnIndex !== undefined && turnIndex >= 0) {
    return (
      lastCursorAtTurn(fallbackItems, turnIndex) ?? {
        turnIndex,
        itemIndex: Number.MAX_SAFE_INTEGER,
        itemId: '',
      }
    );
  }
  return newestCursorFromItems(fallbackItems);
}

function pagedHasMoreOlder(paged: PagedItems): boolean {
  return (
    (paged as PagedItems & { hasMoreOlder?: boolean }).hasMoreOlder ??
    paged.hasMore ??
    false
  );
}

function pagedHasMoreNewer(paged: PagedItems): boolean {
  return (
    (paged as PagedItems & { hasMoreNewer?: boolean }).hasMoreNewer ?? false
  );
}

export function createThreadTimelineWindow(
  options: ThreadTimelineWindowOptions,
): ThreadTimelineWindow {
  /**
   * Windowed-history state. The pane holds a contiguous tail of the
   * thread's items (~50 items on initial load); older history loads
   * on demand via `loadOlder()` or `loadUntilItem()`.
   *
   *  - `oldestLoadedCursor` / `newestLoadedCursor` are the inclusive
   *    item-coordinate bounds of the single contiguous logical window.
   *    The turn-index fields are compatibility projections for tests and
   *    existing consumers; they are not used as memory boundaries.
   *  - `hasMoreHistory` drives the "Load older" button's visibility.
   *  - `hasMoreNewer` drives the bottom "newer messages" gap.
   *  - loading flags disable the matching controls while a fetch is in flight.
   *
   * Upsert events whose item coordinates fall below the window floor
   * are silently dropped — the canonical copy lives in SQLite and will
   * be pulled in the next time the user loads older history. See
   * `upsertItem` below.
   */
  let oldestLoadedCursor: TimelineCursorLike | null = $state(null);
  let newestLoadedCursor: TimelineCursorLike | null = $state(null);
  let oldestLoadedTurnIndex: number | null = $state(null);
  let newestLoadedTurnIndex: number | null = $state(null);
  let hasMoreHistory: boolean = $state(false);
  let hasMoreNewer: boolean = $state(false);
  let recentWindowPrunePending: boolean = $state(false);
  let loadingOlder: boolean = $state(false);
  let loadingNewer: boolean = $state(false);
  /** See the interface doc — set by user paging, disables every automatic prune. */
  let userPinnedHistory: boolean = $state(false);

  /**
   * Separate generation counter for `loadOlder` / `loadUntilItem` so a
   * second click doesn't race with a slow first fetch. `switchGeneration`
   * covers thread swaps; this guards against same-thread concurrent
   * paging fetches (double-click, keyboard repeat).
   */
  let pagingGeneration = 0;

  function setLoadedCursors(
    oldest: TimelineCursorLike | null,
    newest: TimelineCursorLike | null,
  ): void {
    oldestLoadedCursor = cloneCursor(oldest);
    newestLoadedCursor = cloneCursor(newest);
    oldestLoadedTurnIndex = oldestLoadedCursor?.turnIndex ?? null;
    newestLoadedTurnIndex = newestLoadedCursor?.turnIndex ?? null;
  }

  /**
   * Refresh cursors + hasMore flags from a paged response against the
   * current window. `nextItems` is always read fresh via
   * `options.getItems()` — every call site invokes this immediately after
   * a `replaceTimelineItems`, so the pane's live window is the right
   * fallback source for turn-index-only paged responses.
   */
  function applyWindowMetadataFromPaged(paged: PagedItems): void {
    const nextItems = options.getItems();
    setLoadedCursors(
      pagedOldestCursor(paged, nextItems),
      pagedNewestCursor(paged, nextItems),
    );
    hasMoreHistory = pagedHasMoreOlder(paged);
    hasMoreNewer = pagedHasMoreNewer(paged);
  }

  /**
   * Bound on the parent walks below. Real subagent trees are two or
   * three deep; the cap exists only so corrupt provider parentId links
   * cannot spin here (same guard as threadSubagentMemory's).
   */
  const MAX_PARENT_HOPS = 16;

  /**
   * Count of top-level rows — what every window cap below measures.
   * Raw `items.length` is the wrong proxy for "screens of content":
   * subagent children render inside their anchor's card (or an open
   * agent companion), so a busy agent can hold a thousand loaded child
   * rows while the timeline shows one card. Counting them made the
   * forced prune evict the actual conversation to keep an invisible
   * subtree (incident 2026-08-31, the sibling of the backend pagers'
   * `topLevelItemsFilter` rule — see internal/store/paging.go).
   */
  function topLevelCount(items: readonly Item[]): number {
    let count = 0;
    for (const item of items) {
      if ((item.parentId ?? '') === '') count += 1;
    }
    return count;
  }

  /**
   * Cut the window by TOP-LEVEL position: an item is kept iff its
   * top-level root (itself, or the launch anchor its parent chain
   * resolves to) passes `keepsRoot`. Children therefore always travel
   * with their anchor — a cut can neither strand a child without the
   * anchor that renders it (the admission invariant) nor spend the
   * retained budget on child rows while evicting the conversation.
   * Rows the open agent companion renders are additionally kept, with
   * their ancestor chains, whatever side of the cut they fall on.
   */
  function cutWindowByRootCursor(
    sourceItems: readonly Item[],
    keepsRoot: (rootCursor: TimelineCursorLike) => boolean,
  ): Item[] {
    const byId = new Map<string, Item>();
    for (const item of sourceItems) byId.set(item.id, item);
    const rootCursorMemo = new Map<string, TimelineCursorLike>();
    const rootCursorOf = (item: Item): TimelineCursorLike => {
      const chain: string[] = [];
      let walker = item;
      let cursor: TimelineCursorLike | null = null;
      for (let hops = 0; hops <= MAX_PARENT_HOPS; hops += 1) {
        const memoized = rootCursorMemo.get(walker.id);
        if (memoized) {
          cursor = memoized;
          break;
        }
        chain.push(walker.id);
        const parentId = walker.parentId ?? '';
        const parent = parentId === '' ? undefined : byId.get(parentId);
        if (!parent) break; // top-level, or orphan: nearest loaded root
        walker = parent;
      }
      cursor ??= cursorFromItem(walker);
      for (const id of chain) rootCursorMemo.set(id, cursor);
      return cursor;
    };
    const keepIds = new Set<string>();
    for (const item of sourceItems) {
      if (keepsRoot(rootCursorOf(item))) keepIds.add(item.id);
    }
    const held = options.getHeldRowIds?.() ?? null;
    if (held !== null && held.size > 0) {
      for (const item of sourceItems) {
        if (!held.has(item.id) || keepIds.has(item.id)) continue;
        keepIds.add(item.id);
        let parentId = item.parentId ?? '';
        for (let hops = 0; parentId !== '' && hops < MAX_PARENT_HOPS; hops += 1) {
          if (keepIds.has(parentId)) break;
          keepIds.add(parentId);
          parentId = byId.get(parentId)?.parentId ?? '';
        }
      }
    }
    return sourceItems.filter((item) => keepIds.has(item.id));
  }

  function keepRecentWindowItems(
    sourceItems: readonly Item[],
    targetCount: number,
  ): PrunedWindow {
    const topLevel = sourceItems.filter(
      (item) => (item.parentId ?? '') === '',
    );
    if (topLevel.length <= targetCount) {
      return {
        items: sourceItems as Item[],
        oldestCursor: oldestCursorFromItems(sourceItems),
        newestCursor: newestCursorFromItems(sourceItems),
      };
    }
    const cutoffCursor = cursorFromItem(
      topLevel[topLevel.length - targetCount],
    );
    return {
      items: cutWindowByRootCursor(
        sourceItems,
        (rootCursor) => compareCursors(rootCursor, cutoffCursor) >= 0,
      ),
      oldestCursor: cutoffCursor,
      newestCursor: newestCursorFromItems(sourceItems),
    };
  }

  function pruneToRecentWindowIfNeeded(
    pruneOptions: {
      hasMoreNewerAfterPrune?: boolean;
    } = {},
  ): void {
    if (userPinnedHistory) {
      recentWindowPrunePending = false;
      return;
    }
    const items = options.getItems();
    const loadedTopLevel = topLevelCount(items);
    if (loadedTopLevel <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const thread = options.getThread();
    const activeTurn = thread !== null ? getActiveTurn(thread.id) : null;
    const exceedsHardCeiling =
      loadedTopLevel > ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS;
    // A large keyed reconciliation is avoidable main-thread work while a
    // turn is active. Defer it, with the hard ceiling as the memory
    // backstop against a runaway turn. Paint correctness does not depend on
    // this timing: the virtualizer's stable row plane preserves surviving
    // rows even when the ceiling forces the prune. The debt is recorded so the
    // quiet scheduler's retry keeps standing off a turn that started
    // while the prune waited, and later append-path calls short-circuit
    // on the pending flag instead of re-slicing the window.
    if (
      !exceedsHardCeiling
      && activeTurn
    ) {
      recentWindowPrunePending = true;
      return;
    }
    if (recentWindowPrunePending && !exceedsHardCeiling) return;
    const next = keepRecentWindowItems(
      items,
      ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
    );
    const vetoPolicy = exceedsHardCeiling ? 'force' : 'defer';
    const result = applyPrunedWindow(next, {
      hasMoreHistoryAfterPrune: true,
      hasMoreNewerAfterPrune: pruneOptions.hasMoreNewerAfterPrune ?? false,
      vetoPolicy,
    });
    recentWindowPrunePending = result === 'deferred';
  }

  // loadNewer's opposite-edge prune. Gated on the user pin like every
  // other automatic drop: once the reader has explicitly paged history
  // in, catching the window up toward the tail must not throw that
  // history away behind them.
  function prunePagedRecentWindowIfNeeded(hasMoreNewerAfterPrune: boolean): void {
    if (userPinnedHistory) return;
    const items = options.getItems();
    if (topLevelCount(items) <= ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS) return;
    const next = keepRecentWindowItems(items, ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
    applyPagedPrune(next, {
      hasMoreHistoryAfterPrune: true,
      hasMoreNewerAfterPrune,
    });
    recentWindowPrunePending = false;
  }

  // Shared window swap used by both prune paths: replace items, cursors, and
  // history flags. The pane's replacement chokepoint synchronizes the reveal
  // gate as part of the commit, so callers cannot omit it.
  function commitWindow(
    next: PrunedWindow,
    flags: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    options.replaceTimelineItems(next.items, {
      disposeDropped: true,
      afterCommit: () => {
        setLoadedCursors(next.oldestCursor, next.newestCursor);
        if (flags.hasMoreHistoryAfterPrune !== undefined) {
          hasMoreHistory = flags.hasMoreHistoryAfterPrune;
        }
        if (flags.hasMoreNewerAfterPrune !== undefined) {
          hasMoreNewer = flags.hasMoreNewerAfterPrune;
        }
      },
    });
  }

  // Paging prune (loadOlder tail-drop / loadNewer head-drop). The dropped end
  // is always opposite the reading viewport, so there is nothing to veto and
  // no anchor to restore. The virtualizer derives the keyed mutation and
  // carries surviving measurements and paint coordinates with their rows.
  function applyPagedPrune(
    next: PrunedWindow,
    pruneOptions: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    if (next.items.length === options.getItems().length) return;
    commitWindow(next, pruneOptions);
  }

  // Streaming / settle prune. Holds position via the explicit anchor
  // guard because it can fire under a bottom-pinned, mid-turn viewport, and
  // it can be vetoed/deferred when the prune would drop the visible anchor.
  function applyPrunedWindow(
    next: PrunedWindow,
    pruneOptions: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
      vetoPolicy: PrunedWindowVetoPolicy;
    },
  ): PrunedWindowApplyResult {
    if (next.items.length === options.getItems().length) return 'applied';
    const keptItemIds = new Set(next.items.map((item) => item.id));
    const guard = options.getScrollController()?.canPreserveTimelineWindow;
    const safe = !guard || guard((itemId) => keptItemIds.has(itemId));
    if (!safe && pruneOptions.vetoPolicy === 'defer') return 'deferred';
    commitWindow(next, pruneOptions);
    return 'applied';
  }

  /**
   * Apply a paged-load result to pane state. Used by `switchThread`'s
   * single initial load. Items merge additively — anything already
   * present (from cache or streamed events that landed mid-load)
   * keeps its current reference; missing rows are added and the
   * array is re-sorted by (turnIndex, itemIndex). Cursors
   * (`oldestLoadedTurnIndex` / `hasMoreHistory`) are taken straight
   * from the load — there is no second phase whose wider window
   * would need to be preserved.
   */
  function applyInitialSlice(paged: PagedItems, threadID: string): void {
    const incoming = itemsForThread((paged.items ?? []) as Item[], threadID);
    const nextItems = mergeMissingItemsById(incoming, options.getItems());
    options.installTimelineItems(nextItems, {
      disposeDropped: true,
      afterCommit: () => applyWindowMetadataFromPaged(paged),
    });
  }

  /**
   * Cache-hit branch of `installCacheOrFreshState`: cursor/flag
   * bookkeeping only. `replaceTimelineItems(cached.items)` and
   * `subagentFolds.restore(...)` stay pane-side — this method owns
   * only the window bookkeeping.
   */
  function installFromSnapshot(cached: ThreadItemSnapshot): void {
    setLoadedCursors(
      cached.oldestLoadedCursor ?? oldestCursorFromItems(cached.items),
      cached.newestLoadedCursor ?? newestCursorFromItems(cached.items),
    );
    if (!oldestLoadedCursor && cached.oldestLoadedTurnIndex != null) {
      oldestLoadedTurnIndex = cached.oldestLoadedTurnIndex;
    }
    if (!newestLoadedCursor && cached.newestLoadedTurnIndex != null) {
      newestLoadedTurnIndex = cached.newestLoadedTurnIndex;
    }
    hasMoreHistory = cached.hasMoreHistory;
    hasMoreNewer = cached.hasMoreNewer;
    recentWindowPrunePending = false;
    loadingOlder = false;
    loadingNewer = false;
    userPinnedHistory = false;
  }

  /**
   * Fresh-thread branch of `installCacheOrFreshState`, and `clear()`.
   * A null floor disables the upsert floor check until the backend
   * tells us otherwise — between thread clear and the initial-slice
   * response any streamed upserts are already ours to append normally.
   * `pagingGeneration` is NOT reset here — see its declaration above,
   * it stays monotonic for the pane's lifetime.
   */
  function resetForFreshThread(): void {
    oldestLoadedCursor = null;
    newestLoadedCursor = null;
    oldestLoadedTurnIndex = null;
    newestLoadedTurnIndex = null;
    hasMoreHistory = false;
    hasMoreNewer = false;
    recentWindowPrunePending = false;
    loadingOlder = false;
    loadingNewer = false;
    userPinnedHistory = false;
  }

  /**
   * `runParallelLoad`'s load-items error branch: window nulls only.
   * Loading flags and `recentWindowPrunePending` are untouched to match
   * current behavior exactly.
   */
  function resetAfterLoadError(): void {
    oldestLoadedCursor = null;
    newestLoadedCursor = null;
    oldestLoadedTurnIndex = null;
    newestLoadedTurnIndex = null;
    hasMoreHistory = false;
    hasMoreNewer = false;
  }

  function noteDroppedNewerItems(): void {
    hasMoreNewer = true;
  }

  function refreshCursorsAfterUpserts(changedItems: readonly Item[], appended: boolean, previousItems: readonly Item[]): void {
    const thread = options.getThread();
    if (!thread) return;
    const { oldest, newest } = cursorsAfterItemUpserts(
      oldestLoadedCursor, newestLoadedCursor, previousItems, changedItems, thread.id,
    );
    if (oldest !== oldestLoadedCursor || newest !== newestLoadedCursor) {
      setLoadedCursors(oldest, newest);
    }
    if (!appended) return;
    if (!hasMoreHistory) {
      oldestLoadedCursor = oldestCursorFromItems(options.getItems());
      oldestLoadedTurnIndex = oldestLoadedCursor?.turnIndex ?? null;
    }
    if (!hasMoreNewer) {
      newestLoadedCursor = newestCursorFromItems(options.getItems());
      newestLoadedTurnIndex = newestLoadedCursor?.turnIndex ?? null;
    }
  }

  function retryDeferredRecentWindowPrune(): void {
    if (!recentWindowPrunePending) return;
    recentWindowPrunePending = false;
    pruneToRecentWindowIfNeeded();
  }

  /**
   * Turn-settle entry point for the recent-window prune. Wire settle is
   * NOT visual quiet: the reveal smoother keeps draining the tail for
   * seconds after the turn completes (deliberately — the reveal is never
   * rushed), and the head-drop's reconciliation is the most expensive in the
   * app, so landing it here put the stall inside the glide the reader
   * was watching (bug-report-20260801T214455Z traces; measured 78–186ms).
   * When a mounted timeline is behind the pane (the controller offers
   * the anchor-survival guard), the prune is recorded as pending and the
   * quiet scheduler (timelineQuietWork) retries it once nothing is
   * animating. Without one, such as a discussion surface or headless pane,
   * it applies immediately.
   * The hard ceiling stays with the append path and is unaffected.
   * See docs/architecture/scroll-arbitration-plan.md.
   */
  function settleRecentWindowPrune(): void {
    if (userPinnedHistory) {
      recentWindowPrunePending = false;
      return;
    }
    if (hasMoreNewer) return;
    if (topLevelCount(options.getItems()) <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) {
      recentWindowPrunePending = false;
      return;
    }
    if (options.getScrollController()?.canPreserveTimelineWindow) {
      recentWindowPrunePending = true;
      return;
    }
    recentWindowPrunePending = false;
    pruneToRecentWindowIfNeeded();
  }

  /**
   * Fetch the next batch of older turns and prepend them to the window.
   * Respects both the switch generation (thread swapped mid-flight) and
   * a paging-specific generation (concurrent invocations from double-
   * clicks or keyboard repeats). The return value is for scroll
   * anchoring: `insertedBeforeWindow` means at least one new row sorted
   * before the current in-memory first row. Components that know the
   * actual visible anchor still restore that anchor directly.
   */
  async function loadOlder(): Promise<LoadOlderResult> {
    const currentThread = options.getThread();
    if (!currentThread) return loadOlderResult('noop');
    if (!hasMoreHistory || loadingOlder) return loadOlderResult('noop');
    const floor = cloneCursor(oldestLoadedCursor);
    if (!floor) return loadOlderResult('noop');

    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingOlder = true;
    try {
      const paged = await ListItemsBeforeCursor(
        currentThread.id,
        cursorForBinding(floor),
        LOAD_OLDER_ITEM_BUDGET,
        wantsInlinePreviews(),
      );
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      const prepend = itemsForThread(
        (paged.items ?? []) as Item[],
        currentThread.id,
      );
      const currentIds = new Set(options.getItems().map((item) => item.id));
      const insertedRows = prepend.some((item) => !currentIds.has(item.id));
      const currentFirst = options.getItems()[0] ?? null;
      const insertedBeforeWindow =
        currentFirst === null
          ? insertedRows
          : prepend.some(
              (item) =>
                !currentIds.has(item.id) &&
                compareItemsByTimelinePosition(item, currentFirst) < 0,
            );
      const next = mergeItemsById(prepend, options.getItems());
      const pageBounds = cursorsAfterItemUpserts(
        pagedOldestCursor(paged, prepend), pagedNewestCursor(paged, prepend),
        prepend, options.getItems(), currentThread.id,
      );
      let nextFloor = pageBounds.oldest ?? cloneCursor(oldestLoadedCursor) ?? floor;
      if (oldestLoadedCursor && compareCursors(nextFloor, oldestLoadedCursor) > 0) {
        nextFloor = { ...oldestLoadedCursor };
      }
      const nextNewest = cloneCursor(newestLoadedCursor) ?? newestCursorFromItems(next);
      // Progress guard. If the backend returned no items AND the floor
      // didn't decrease, another click would fire the same query for
      // the same range. Force hasMore=false so the UI stops offering a
      // button that can't actually load anything. A later in-flight
      // upsert that lands an older item will re-enable paging through
      // the normal streaming path.
      const nextHasMoreHistory =
        prepend.length === 0 && compareCursors(nextFloor, floor) >= 0
          ? false
          : pagedHasMoreOlder(paged);
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => {
          setLoadedCursors(nextFloor, nextNewest);
          hasMoreHistory = nextHasMoreHistory;
        },
      });
      // The reader asked for this history: pin the window so no
      // automatic prune (streaming, settle, or the loadNewer edge drop)
      // can take it back. There is deliberately no opposite-edge prune
      // here either — the window grows as far as the reader pages
      // (incident 2026-08-25 was the capped version of this path eating
      // the thread tail; the pin replaces that tolerance dance).
      if (insertedRows) userPinnedHistory = true;
      await tick();
      return loadOlderResult('loaded', insertedBeforeWindow, insertedRows);
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      console.error('loadOlder failed:', err);
      addToast('error', 'Failed to load older messages');
      return loadOlderResult('error');
    } finally {
      // Always clear the button's busy flag. The generation guard on
      // the happy path protects state mutation from late resolutions,
      // but `loadingOlder` is a UI-only flag — leaving it stuck true
      // after a pagingGeneration bump (e.g. a concurrent
      // loadUntilItem) would greys out the Load Older button
      // indefinitely. The worst outcome of clearing unconditionally
      // is a brief flash of the non-busy state while another pager
      // is still in-flight; the concurrent call will re-raise the
      // flag on its next write.
      loadingOlder = false;
    }
  }

  /**
   * Ensure the item with `itemID` is present in the loaded window.
   * Used by scroll-to-item callers (search hits, plan sidebar, tray)
   * before they dispatch the scroll intent. When the item is already
   * in the window this is a cheap `Array.some` and no backend call.
   * When the item lives below the floor the pane loads every turn
   * from the item's turn_index up to the existing tail in one
   * replacement — the window grows to cover the hit, no cumulative
   * multi-page ratchet.
   *
   * Returns `true` when the item is (now) loaded and scrollable,
   * `false` when the backend reports the item doesn't exist on this
   * thread (scroll callers show a toast and abandon the request).
   */
  async function loadUntilItem(itemID: string): Promise<boolean> {
    const currentThread = options.getThread();
    if (!currentThread || !itemID) return false;
    if (options.getItems().some((it) => it.id === itemID)) return true;

    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    let fetched: Item;
    try {
      fetched = (await GetThreadItem(currentThread.id, itemID)) as Item;
    } catch (err) {
      if (gen !== options.getSwitchGeneration()) return false;
      console.error('loadUntilItem GetThreadItem failed:', err);
      return false;
    }
    if (gen !== options.getSwitchGeneration() || pageGen !== pagingGeneration)
      return false;
    if (!fetched || !fetched.id) return false;
    // Defense-in-depth: the backend already filters by threadId, but a
    // mislayered binding or a future cache that returns stale rows
    // shouldn't cross-pollute between panes.
    if (fetched.threadId !== currentThread.id) return false;

    // Race: another upsert or loadOlder might have pulled the item in
    // between our check and the backend round-trip. Re-check before
    // paging in a whole turn window we don't need.
    if (options.getItems().some((it) => it.id === itemID)) return true;

    // Subagent children never appear in history windows. Walk the
    // parent chain to the top-level launch root so the slice anchors
    // on a row the window will actually contain, then hydrate the
    // root's subtree so the scroll can resolve to the containing
    // group card. The visited set bounds corrupt parent cycles; a
    // broken chain falls back to anchoring on the child's own
    // coordinates (the slice still positions correctly — only the
    // subtree hydration is skipped, and the trailing containment
    // check reports the miss).
    let sliceAnchorID = itemID;
    let subagentRootID = '';
    if ((fetched.parentId ?? '') !== '') {
      let walker = fetched;
      const visited = new Set<string>([walker.id]);
      while (
        (walker.parentId ?? '') !== '' &&
        !visited.has(walker.parentId ?? '')
      ) {
        let parentItem: Item;
        try {
          parentItem = (await GetThreadItem(
            currentThread.id,
            walker.parentId ?? '',
          )) as Item;
        } catch (err) {
          console.error('loadUntilItem parent walk failed:', err);
          break;
        }
        if (
          gen !== options.getSwitchGeneration() ||
          pageGen !== pagingGeneration
        )
          return false;
        if (!parentItem?.id || parentItem.threadId !== currentThread.id)
          break;
        visited.add(parentItem.id);
        walker = parentItem;
      }
      if ((walker.parentId ?? '') === '') {
        sliceAnchorID = walker.id;
        subagentRootID = walker.id;
      }
    }

    loadingOlder = true;
    try {
      const paged = await ListThreadSliceAround(
        currentThread.id,
        sliceAnchorID,
        ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        wantsInlinePreviews(),
      );
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return false;
      const next = reconcileItemWindow(
        itemsForThread((paged.items ?? []) as Item[], currentThread.id),
        options.getItems(),
      );
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => applyWindowMetadataFromPaged(paged),
      });
      // A scroll-to-item recenter is explicit navigation into history:
      // pin the window so the streaming prunes cannot yank the reader's
      // target out from under them while a turn keeps appending.
      userPinnedHistory = true;
      if (subagentRootID) {
        await options.hydrateSubagentChildren(subagentRootID);
        if (
          gen !== options.getSwitchGeneration() ||
          pageGen !== pagingGeneration
        )
          return false;
      }
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return false;
      console.error('loadUntilItem ListThreadSliceAround failed:', err);
      addToast('error', 'Failed to load message');
      return false;
    } finally {
      // Match loadOlder's unconditional reset — see comment there.
      loadingOlder = false;
    }
    return options.getItems().some((it) => it.id === itemID);
  }

  async function loadNewer(): Promise<LoadOlderResult> {
    const currentThread = options.getThread();
    if (!currentThread) return loadOlderResult('noop');
    if (!hasMoreNewer || loadingNewer) return loadOlderResult('noop');
    const ceiling = cloneCursor(newestLoadedCursor);
    if (!ceiling) return loadOlderResult('noop');

    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingNewer = true;
    try {
      const paged = await ListItemsAfterCursor(
        currentThread.id,
        cursorForBinding(ceiling),
        LOAD_OLDER_ITEM_BUDGET,
        wantsInlinePreviews(),
      );
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      const append = itemsForThread(
        (paged.items ?? []) as Item[],
        currentThread.id,
      );
      const currentIds = new Set(options.getItems().map((item) => item.id));
      const insertedRows = append.some((item) => !currentIds.has(item.id));
      const currentLast = options.getItems().at(-1) ?? null;
      const insertedAfterWindow =
        currentLast === null
          ? insertedRows
          : append.some(
              (item) =>
                !currentIds.has(item.id) &&
                compareItemsByTimelinePosition(item, currentLast) > 0,
            );
      const next = mergeItemsById(append, options.getItems());
      const pageBounds = cursorsAfterItemUpserts(
        pagedOldestCursor(paged, append), pagedNewestCursor(paged, append),
        append, options.getItems(), currentThread.id,
      );
      let nextCeiling = pageBounds.newest ?? cloneCursor(newestLoadedCursor) ?? ceiling;
      if (newestLoadedCursor && compareCursors(nextCeiling, newestLoadedCursor) < 0) {
        nextCeiling = { ...newestLoadedCursor };
      }
      const nextOldest = cloneCursor(oldestLoadedCursor) ?? oldestCursorFromItems(next);
      const nextHasMoreNewer =
        append.length === 0 && compareCursors(nextCeiling, ceiling) <= 0
          ? false
          : pagedHasMoreNewer(paged);
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => {
          setLoadedCursors(nextOldest, nextCeiling);
          hasMoreNewer = nextHasMoreNewer;
        },
      });
      // The keyed virtualizer derives the combined tail grow + head prune
      // and preserves the reading coordinate in one flush.
      prunePagedRecentWindowIfNeeded(nextHasMoreNewer);
      await tick();
      return loadOlderResult('loaded', insertedAfterWindow, insertedRows);
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      console.error('loadNewer failed:', err);
      addToast('error', 'Failed to load newer messages');
      return loadOlderResult('error');
    } finally {
      loadingNewer = false;
    }
  }

  async function loadRecentTail(): Promise<boolean> {
    const currentThread = options.getThread();
    if (!currentThread) return false;
    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingNewer = true;
    try {
      const paged = await ListThreadSliceAround(
        currentThread.id,
        '',
        ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        wantsInlinePreviews(),
      );
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return false;
      const next = reconcileItemWindow(
        itemsForThread((paged.items ?? []) as Item[], currentThread.id),
        options.getItems(),
      );
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => applyWindowMetadataFromPaged(paged),
      });
      // The window is a bounded tail slice again — the pinned history it
      // may have replaced is gone, so bounded steady-state pruning re-arms.
      userPinnedHistory = false;
      return true;
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return false;
      console.error('loadRecentTail failed:', err);
      addToast('error', 'Failed to load latest messages');
      return false;
    } finally {
      loadingNewer = false;
    }
  }

  return {
    get oldestLoadedCursor() {
      return oldestLoadedCursor;
    },
    get newestLoadedCursor() {
      return newestLoadedCursor;
    },
    get oldestLoadedTurnIndex() {
      return oldestLoadedTurnIndex;
    },
    get newestLoadedTurnIndex() {
      return newestLoadedTurnIndex;
    },
    get hasMoreHistory() {
      return hasMoreHistory;
    },
    get hasMoreNewer() {
      return hasMoreNewer;
    },
    get hasDeferredRecentWindowPrune() {
      return recentWindowPrunePending;
    },
    get loadingOlder() {
      return loadingOlder;
    },
    get loadingNewer() {
      return loadingNewer;
    },
    get userPinnedHistory() {
      return userPinnedHistory;
    },
    applyInitialSlice,
    applyWindowMetadataFromPaged,
    installFromSnapshot,
    resetForFreshThread,
    resetAfterLoadError,
    noteDroppedNewerItems,
    refreshCursorsAfterUpserts,
    pruneToRecentWindowIfNeeded,
    retryDeferredRecentWindowPrune,
    settleRecentWindowPrune,
    loadOlder,
    loadUntilItem,
    loadNewer,
    loadRecentTail,
  };
}
