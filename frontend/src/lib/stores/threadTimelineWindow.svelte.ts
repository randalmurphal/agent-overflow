import { threadBackend } from '../transport/entityIndex';
import { isWindowedTimelineRow } from './threadWindowDigest';
import { tick } from 'svelte';
import type { Item, Thread } from '../types/models';
import type { TimelineSelection, PagedItems } from '../../../bindings/agent-overflow/internal/store/models';
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
  compareItemToCursor,
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
import { groupActivityRunSpans } from '../utils/activityRunSpans';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  LOAD_OLDER_ITEM_BUDGET,
  loadOlderResult,
  SLICE_AROUND_ITEM_BUDGET,
  timelinePageShape,
  type LoadOlderResult,
  type LoadUntilItemResult,
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
      afterCommit?: () => void;
    },
  ): boolean;
  getThread(): Thread | null;
  selection?(): TimelineSelection;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** Registered pane scroll controller (or null). applyPrunedWindow queries its retention guard. */
  getScrollController(): PaneScrollController | null;
  /**
   * The pane's activity-run registry. Read per call: it is constructed
   * after this factory, so the option is an arrow, not a reference.
   *
   * The window owns where a cut's edges land; the registry owns what a
   * run still counts once they have. Neither can decide alone — an edge
   * inside a run is legal only if the run can record the members past it
   * (see `snapCutEdgesOffRuns`).
   */
  activityRuns(): ThreadActivityRuns;
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
  invalidatePendingReads(): void;
  readonly requestVersion: number;
  /** Streaming upsert dropped newer items below/above the window: re-arm the "load newer" affordance. */
  noteDroppedNewerItems(): void;
  noteDroppedOlderItems(): void;
  applyConversationCut(boundaryWasLoaded: boolean): void;
  /** Mount an authoritative member page, extending the held run at either edge. */
  mountActivityRunMembers(rows: readonly Item[], dropIds: ReadonlySet<string>): void;
  /** Follow repositioned anchors, retaining the capped floor and ordinary tail-append policy. */
  refreshCursorsAfterUpserts(changedItems: readonly Item[], appended: boolean, previousItems: readonly Item[]): void;
  /**
   * Streaming-path window cut. Over `ACTIVE_TIMELINE_WINDOW_MAX_ITEMS`
   * top-level rows it keeps `ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS` around
   * the visible rows (the newest rows when holding the bottom) and marks
   * whichever edge it dropped as loadable again. Never drops a visible
   * row: such a cut is deferred, at any count.
   */
  pruneToRecentWindowIfNeeded(): void;
  retryDeferredRecentWindowPrune(): void;
  /**
   * `settleTurn`'s prune entry: records the prune as pending for the
   * quiet scheduler when a mounted timeline can avoid the structural
   * reconciliation during activity, and applies it immediately otherwise.
   */
  settleRecentWindowPrune(): void;
  loadOlder(): Promise<LoadOlderResult>;
  loadUntilItem(itemID: string): Promise<LoadUntilItemResult>;
  loadNewer(): Promise<LoadOlderResult>;
  loadRecentTail(): Promise<boolean>;
}

interface PrunedWindow {
  items: Item[];
  oldestCursor: TimelineCursorLike | null;
  newestCursor: TimelineCursorLike | null;
  droppedHead: boolean;
  droppedTail: boolean;
}

type PrunedWindowApplyResult = 'applied' | 'deferred';

/**
 * Which side of the visible rows a cut spends its buffer on. `oldest`
 * (load-older) keeps the head so a page the reader just asked for is
 * never dropped by the cut that follows it; `newest` (load-newer, and the
 * bottom-held streaming tail) keeps the tail for the same reason;
 * `centered` (streaming cut under a reader who is up in history) buffers
 * both sides and drops the live tail, which `hasMoreNewer` then offers
 * back through load-newer and jump-to-latest.
 */
type WindowCutPolicy = 'oldest' | 'newest' | 'centered';

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
  let loadingOlder = $state<number | null>(null);
  let loadingNewer = $state<number | null>(null);

  /**
   * Separate generation counter for `loadOlder` / `loadUntilItem` so a
   * second click doesn't race with a slow first fetch. `switchGeneration`
   * covers thread swaps; this guards against same-thread concurrent
   * paging fetches (double-click, keyboard repeat).
   */
  let pagingGeneration = 0;
  // Recovery must also detect paging that starts or finishes during a snapshot.
  let observationVersion = 0;
  async function trackRead<T>(read: () => Promise<T>): Promise<T> {
    observationVersion++;
    try { return await read(); }
    finally { observationVersion++; }
  }

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
    // The page's run stubs describe the members it did NOT ship, against
    // the span it did. Folded here, after the rows are installed, so the
    // records are compared with the window the pane actually holds.
    options.activityRuns().syncRunSpans(nextItems, paged.runs);
  }

  /**
   * Whether a row counts toward the window caps. A window holds only rows
   * of its own scope (`threadItemWindow` refuses the rest), so this
   * excludes just the rows the timeline never renders as their own entry
   * (`isWindowedTimelineRow`).
   */
  const includes = (item: Item) => isWindowedTimelineRow(item, options.selection?.());

  function topLevelCount(items: readonly Item[]): number {
    let count = 0;
    for (const item of items) {
      if (includes(item)) count += 1;
    }
    return count;
  }

  /** Keep the rows inside `[oldest, newest]`. */
  function cutWindowToRange(
    sourceItems: readonly Item[],
    oldest: TimelineCursorLike,
    newest: TimelineCursorLike,
  ): Item[] {
    return sourceItems.filter((item) =>
      compareItemToCursor(item, oldest) >= 0 && compareItemToCursor(item, newest) <= 0);
  }

  /**
   * Top-level index range `[first, last]` of the rows the viewport shows.
   * Null when the reader holds the bottom or no visible row is in the
   * window.
   */
  function visibleTopLevelRange(
    sourceItems: readonly Item[],
    topLevel: readonly Item[],
  ): { first: number; last: number } | null {
    const visible = options.getScrollController()?.visibleTimelineItemIds?.() ?? null;
    if (!visible || visible.size === 0) return null;
    let oldest: TimelineCursorLike | null = null;
    let newest: TimelineCursorLike | null = null;
    for (const item of sourceItems) {
      if (!visible.has(item.id)) continue;
      if (!oldest || compareItemToCursor(item, oldest) < 0) oldest = cursorFromItem(item);
      if (!newest || compareItemToCursor(item, newest) > 0) newest = cursorFromItem(item);
    }
    if (!oldest || !newest) return null;
    let first = -1;
    let last = -1;
    for (let index = 0; index < topLevel.length; index += 1) {
      const item = topLevel[index];
      if (first < 0 && compareItemToCursor(item, oldest) >= 0) first = index;
      if (compareItemToCursor(item, newest) <= 0) last = index;
      else break;
    }
    if (first < 0 || last < first) return null;
    return { first, last };
  }

  /**
   * Move a chosen cut edge off the inside of an activity run, where that
   * is free.
   *
   * A run is one held object: the pane holds its loaded members as rows
   * and everything else as counts on its record
   * (docs/architecture/timeline-window-pages.md §6). Where the edges may
   * land follows from what a record can say:
   *
   *  - OLDER edge inside a run: allowed and unchanged. The members before
   *    it are shed — narrow copies on the record, which is exactly the
   *    shape §6 defines, contiguous with the surviving span's older side
   *    — so the run keeps counting them and the header stays right.
   *  - NEWER edge inside a run: a record cannot record members past its
   *    span, so this edge is moved back to the run's first row, dropping
   *    the run whole, whenever the kept range survives it. A run LARGER
   *    than the whole target cannot be moved off: the edge stays inside
   *    it, the members past it are dropped, and the record goes dirty —
   *    one debounced stub refresh restates what the run now has after the
   *    span. Bounded memory wins over an exact count for 200ms; keeping
   *    such a run whole would defeat the cut entirely.
   *
   * Runs fully outside the kept range need nothing here: they leave with
   * their rows and `activityRuns.applyWindowCut` drops their records in
   * the same commit.
   */
  function snapCutEdgesOffRuns(
    topLevel: readonly Item[],
    start: number,
    end: number,
  ): { start: number; end: number } {
    const spans = groupActivityRunSpans(topLevel, item => options.activityRuns().isLoadedMember(item.id), includes);
    if (spans.length === 0) return { start, end };
    const indexById = new Map<string, number>();
    for (let index = 0; index < topLevel.length; index += 1) {
      indexById.set(topLevel[index].id, index);
    }
    let nextEnd = end;
    for (const span of spans) {
      const first = indexById.get(span.firstItemId);
      const last = indexById.get(span.lastItemId);
      if (first === undefined || last === undefined) continue;
      if (nextEnd > first && nextEnd <= last && first > start) nextEnd = first;
    }
    return { start, end: nextEnd };
  }

  /**
   * The window cut. Keeps `targetCount` top-level rows: the visible range
   * whole, plus buffer placed by `policy`. A visible range wider than the
   * target is kept in full. Reports which edges were dropped so the
   * commit can mark them loadable again.
   *
   * The newer edge is then pulled off the inside of an activity run where
   * that is free (`snapCutEdgesOffRuns`), which can keep FEWER rows than
   * `targetCount`. The target is a retention target, not a floor.
   */
  function keepWindowNearReader(
    sourceItems: readonly Item[],
    targetCount: number,
    policy: WindowCutPolicy,
  ): PrunedWindow {
    const topLevel = sourceItems.filter(
      includes,
    );
    const length = topLevel.length;
    const unchanged = (): PrunedWindow => ({
      items: sourceItems as Item[],
      oldestCursor: oldestCursorFromItems(sourceItems),
      newestCursor: newestCursorFromItems(sourceItems),
      droppedHead: false,
      droppedTail: false,
    });
    if (length <= targetCount) return unchanged();
    const visible = visibleTopLevelRange(sourceItems, topLevel);
    let start: number;
    let end: number;
    if (!visible) {
      start = policy === 'oldest' ? 0 : length - targetCount;
      end = start + targetCount;
    } else if (policy === 'oldest') {
      start = 0;
      end = Math.max(visible.last + 1, targetCount);
    } else if (policy === 'newest') {
      end = length;
      start = Math.min(visible.first, length - targetCount);
    } else {
      const extra = Math.max(0, targetCount - (visible.last - visible.first + 1));
      const above = Math.floor(extra / 2);
      start = visible.first - above;
      end = visible.last + 1 + (extra - above);
      if (start < 0) {
        end -= start;
        start = 0;
      }
      if (end > length) {
        start -= end - length;
        end = length;
      }
    }
    start = Math.max(0, start);
    end = Math.min(length, end);
    const snapped = snapCutEdgesOffRuns(topLevel, start, end);
    start = Math.max(0, snapped.start);
    end = Math.min(length, snapped.end);
    if (start === 0 && end === length) return unchanged();
    const oldestKeep = cursorFromItem(topLevel[start]);
    const newestKeep = cursorFromItem(topLevel[end - 1]);
    return {
      items: cutWindowToRange(sourceItems, oldestKeep, newestKeep),
      oldestCursor: oldestKeep,
      newestCursor: newestKeep,
      droppedHead: start > 0,
      droppedTail: end < length,
    };
  }

  function pruneToRecentWindowIfNeeded(): void {
    const items = options.getItems();
    const loadedTopLevel = topLevelCount(items);
    if (loadedTopLevel <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const thread = options.getThread();
    const activeTurn = thread !== null ? getActiveTurn(thread.id) : null;
    const exceedsHardCeiling =
      loadedTopLevel > ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS;
    // A large keyed reconciliation is avoidable main-thread work while a
    // turn is active. Defer it until the count passes the ceiling. Paint
    // correctness does not depend on this timing: the cut keeps every
    // visible row, and the virtualizer's stable row plane preserves
    // surviving rows. The debt is recorded so the quiet scheduler's
    // retry keeps standing off a turn that started while the cut waited,
    // and later append-path calls short-circuit on the pending flag
    // instead of re-slicing the window.
    if (!exceedsHardCeiling && activeTurn) {
      recentWindowPrunePending = true;
      return;
    }
    if (recentWindowPrunePending && !exceedsHardCeiling) return;
    const next = keepWindowNearReader(
      items,
      ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
      'centered',
    );
    recentWindowPrunePending = applyPrunedWindow(next) === 'deferred';
  }

  // Shared window swap used by every cut: replace items and cursors, and
  // mark a dropped edge loadable again. The pane's replacement chokepoint
  // synchronizes the reveal gate as part of the commit, so callers cannot
  // omit it.
  function commitWindow(
    next: PrunedWindow,
    afterCommit?: () => void,
  ): void {
    // Before the swap: a shed row keeps narrow copies of fields that only
    // exist while the `Item` does (§6), so the records have to see both
    // windows while the outgoing rows are still in hand.
    options.activityRuns().applyWindowCut(options.getItems(), next.items);
    options.replaceTimelineItems(next.items, {
      disposeDropped: true,
      afterCommit: () => {
        setLoadedCursors(next.oldestCursor, next.newestCursor);
        if (next.droppedHead) hasMoreHistory = true;
        if (next.droppedTail) hasMoreNewer = true;
        options.activityRuns().syncRunSpans(options.getItems());
        afterCommit?.();
      },
    });
  }

  // Streaming / settle cut. The cut keeps the visible rows by
  // construction; the anchor guard is the check that it did, and a cut
  // that would still drop the row under the reader is deferred, never
  // forced.
  function applyPrunedWindow(next: PrunedWindow): PrunedWindowApplyResult {
    if (next.items.length === options.getItems().length) return 'applied';
    const keptItemIds = new Set(next.items.map((item) => item.id));
    const guard = options.getScrollController()?.canPreserveTimelineWindow;
    const safe = !guard || guard((itemId) => keptItemIds.has(itemId));
    if (!safe) return 'deferred';
    commitWindow(next);
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
    loadingOlder = null;
    loadingNewer = null;
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
    loadingOlder = null;
    loadingNewer = null;
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

  function applyConversationCut(boundaryWasLoaded: boolean): void {
    ++pagingGeneration;
    observationVersion++;
    if (boundaryWasLoaded || !hasMoreNewer) {
      hasMoreNewer = false;
      const items = options.getItems();
      // The cut changes the tail; preserve the previously loaded head edge.
      setLoadedCursors(
        items.length === 0 ? null : (oldestLoadedCursor ?? oldestCursorFromItems(items)),
        newestCursorFromItems(items),
      );
      recentWindowPrunePending = false;
    }
  }

  function noteDroppedNewerItems(): void {
    hasMoreNewer = true;
  }

  function mountActivityRunMembers(rows: readonly Item[], dropIds: ReadonlySet<string>): void {
    observationVersion++;
    const current = options.getItems();
    const byId = new Map(current.map(item => [item.id, item]));
    const kept = dropIds.size === 0 ? current : current.filter(item => !dropIds.has(item.id));
    const incoming = rows.map(item => {
      const live = byId.get(item.id);
      return live && live.rev >= 0 && item.rev >= 0 && live.rev > item.rev ? live : item;
    });
    options.replaceTimelineItems(mergeItemsById(incoming, kept), { disposeDropped: true });
    let oldest = oldestLoadedCursor;
    let newest = newestLoadedCursor;
    for (const item of rows) {
      if (!includes(item)) continue;
      const cursor = cursorFromItem(item);
      if (!oldest || compareCursors(cursor, oldest) < 0) oldest = cursor;
      if (!newest || compareCursors(cursor, newest) > 0) newest = cursor;
    }
    setLoadedCursors(oldest, newest);
  }

  function refreshCursorsAfterUpserts(changedItems: readonly Item[], appended: boolean, previousItems: readonly Item[]): void {
    const thread = options.getThread();
    if (!thread) return;
    const { oldest, newest } = cursorsAfterItemUpserts(
      oldestLoadedCursor, newestLoadedCursor, previousItems, changedItems, thread.id, includes,
    );
    if (oldest !== oldestLoadedCursor || newest !== newestLoadedCursor) {
      setLoadedCursors(oldest, newest);
    }
    if (!appended) return;
    // Stubs cover unshipped members beyond the physical rows. A live append
    // can extend those bounds, but cannot shrink them to the shipped slice.
    for (const item of changedItems) {
      if (!includes(item)) continue;
      const cursor = cursorFromItem(item);
      if (!hasMoreHistory && (!oldestLoadedCursor || compareCursors(cursor, oldestLoadedCursor) < 0)) {
        oldestLoadedCursor = cursor;
        oldestLoadedTurnIndex = cursor.turnIndex;
      }
      if (!hasMoreNewer && (!newestLoadedCursor || compareCursors(cursor, newestLoadedCursor) > 0)) {
        newestLoadedCursor = cursor;
        newestLoadedTurnIndex = cursor.turnIndex;
      }
    }
    options.activityRuns().syncRunSpans(options.getItems());
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
   * See docs/architecture/scroll-arbitration-plan.md.
   */
  function settleRecentWindowPrune(): void {
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
    if (!hasMoreHistory || loadingOlder !== null) return loadOlderResult('noop');
    const floor = cloneCursor(oldestLoadedCursor);
    if (!floor) return loadOlderResult('noop');

    const ownership = threadBackend(currentThread.id);
    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingOlder = pageGen;
    try {
      const paged = await ListItemsBeforeCursor(
        currentThread.id,
        cursorForBinding(floor),
        LOAD_OLDER_ITEM_BUDGET,
        timelinePageShape(),
        options.selection?.(),
      );
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
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
      const merged = mergeItemsById(prepend, options.getItems());
      const pageBounds = cursorsAfterItemUpserts(
        pagedOldestCursor(paged, prepend), pagedNewestCursor(paged, prepend),
        prepend, options.getItems(), currentThread.id, includes,
      );
      let nextFloor = pageBounds.oldest ?? cloneCursor(oldestLoadedCursor) ?? floor;
      if (oldestLoadedCursor && compareCursors(nextFloor, oldestLoadedCursor) > 0) {
        nextFloor = { ...oldestLoadedCursor };
      }
      // Prepend and the opposite-edge cut land in one flush. The cut keeps
      // the head (the page the reader just asked for) and the visible rows,
      // and drops the tail, which `hasMoreNewer` offers back below. The
      // dropped end is opposite the reading viewport, so there is nothing to
      // veto and no anchor to restore.
      const cut = topLevelCount(merged) > ACTIVE_TIMELINE_WINDOW_MAX_ITEMS
        ? keepWindowNearReader(merged, ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS, 'oldest')
        : null;
      const next = cut?.items ?? merged;
      const nextNewest = cut?.droppedTail
        ? cut.newestCursor
        : cloneCursor(newestLoadedCursor) ?? newestCursorFromItems(next);
      // The backend's answer is the only source for "more older". An empty
      // page reports false itself (finalizePagedItems), and the auto-load
      // gate's progress guard keeps an unmoved floor from re-probing.
      const nextHasMoreHistory = pagedHasMoreOlder(paged);
      // Fold the page's stubs against the merged window, then account for
      // the cut over the same array — both while the rows the cut drops
      // are still in hand (a shed row copies fields off the `Item`).
      const mergedBounds = { oldest: nextFloor, newest: newestLoadedCursor ?? pagedNewestCursor(paged, merged) };
      options.activityRuns().syncRunSpans(merged, paged.runs, mergedBounds);
      if (cut) options.activityRuns().applyWindowCut(merged, next, mergedBounds);
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => {
          setLoadedCursors(nextFloor, nextNewest);
          hasMoreHistory = nextHasMoreHistory;
          if (cut?.droppedTail) hasMoreNewer = true;
          options.activityRuns().syncRunSpans(options.getItems());
        },
      });
      await tick();
      return loadOlderResult('loaded', insertedBeforeWindow, insertedRows);
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      console.error('loadOlder failed:', err);
      addToast('error', 'Failed to load older messages');
      return loadOlderResult('error');
    } finally {
      if (loadingOlder === pageGen) loadingOlder = null;
    }
  }

  /**
   * Ensure the item with `itemID` is present in the loaded window.
   * Used by scroll-to-item callers (search hits, plan sidebar, tray,
   * the nav rail) before they dispatch the scroll intent. When the item
   * is already in the window this is a cheap `Array.some` and no backend
   * call. Otherwise the window is replaced by a bounded slice around the
   * item. A row outside the window's scope (a subagent child in the main
   * window) is `missing` here.
   *
   * The result names why the item is not scrollable, because the callers
   * answer differently: `missing` is the only outcome that means the row
   * is gone from the thread; `superseded` means a newer switch or page
   * owns the window now; `failed` has already been reported to the user
   * here and is a defect or transport fault, never the row's absence.
   */
  async function loadUntilItem(itemID: string): Promise<LoadUntilItemResult> {
    const currentThread = options.getThread();
    if (!currentThread || !itemID) return 'missing';
    if (options.getItems().some((it) => it.id === itemID)) return 'loaded';

    const ownership = threadBackend(currentThread.id);
    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    const superseded = (): boolean =>
      gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership || pageGen !== pagingGeneration;
    let fetched: Item;
    try {
      fetched = (await GetThreadItem(currentThread.id, itemID)) as Item;
    } catch (err) {
      if (superseded()) return 'superseded';
      console.error('loadUntilItem GetThreadItem failed:', err);
      addToast('error', 'Failed to load message');
      return 'failed';
    }
    if (superseded()) return 'superseded';
    if (!fetched || !fetched.id) return 'missing';
    // Defense-in-depth: the backend already filters by threadId, but a
    // mislayered binding or a future cache that returns stale rows
    // shouldn't cross-pollute between panes.
    if (fetched.threadId !== currentThread.id) return 'missing';

    // Race: another upsert or loadOlder might have pulled the item in
    // between our check and the backend round-trip. Re-check before
    // paging in a whole turn window we don't need.
    if (options.getItems().some((it) => it.id === itemID)) return 'loaded';

    // Only rows of this window's scope can load here. A subagent child
    // lives in its agent's scoped surface (`navigateToThreadItem` opens it
    // there), never in the main window.
    if (options.selection?.().scopeRootId ? !includes(fetched) : (fetched.parentId ?? '') !== '') return 'missing';

    // The anchor sits inside a run the window already holds, in the part
    // of it the page did not ship (timeline-window-pages §6): one members
    // call re-centers that run's loaded span on it, and the rest of the
    // window stays exactly where the reader left it. A covering run that
    // cannot produce the row (a failed fetch, already reported) falls
    // through to the whole-window slice below.
    if (await options.activityRuns().loadUnshippedMember(fetched)) {
      if (superseded()) return 'superseded';
      if (options.getItems().some((it) => it.id === itemID)) return 'loaded';
    }
    if (superseded()) return 'superseded';

    loadingOlder = pageGen;
    try {
      const paged = await ListThreadSliceAround(
        currentThread.id,
        itemID,
        SLICE_AROUND_ITEM_BUDGET,
        timelinePageShape(),
        options.selection?.(),
      );
      if (superseded()) return 'superseded';
      const next = reconcileItemWindow(
        itemsForThread((paged.items ?? []) as Item[], currentThread.id),
        options.getItems(),
      );
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => applyWindowMetadataFromPaged(paged),
      });
    } catch (err) {
      if (superseded()) return 'superseded';
      console.error('loadUntilItem ListThreadSliceAround failed:', err);
      addToast('error', 'Failed to load message');
      return 'failed';
    } finally {
      if (loadingOlder === pageGen) loadingOlder = null;
    }
    if (options.getItems().some((it) => it.id === itemID)) return 'loaded';
    // The backend confirmed the row exists, then shipped a window that
    // does not hold it: a contract fault (an anchored slice that dropped
    // its anchor), not a deleted row.
    console.error(
      `loadUntilItem: window loaded around ${itemID} does not contain it`,
    );
    addToast('error', 'Failed to load message');
    return 'failed';
  }

  async function loadNewer(): Promise<LoadOlderResult> {
    const currentThread = options.getThread();
    if (!currentThread) return loadOlderResult('noop');
    if (!hasMoreNewer || loadingNewer !== null) return loadOlderResult('noop');
    const ceiling = cloneCursor(newestLoadedCursor);
    if (!ceiling) return loadOlderResult('noop');

    const ownership = threadBackend(currentThread.id);
    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingNewer = pageGen;
    try {
      const paged = await ListItemsAfterCursor(
        currentThread.id,
        cursorForBinding(ceiling),
        LOAD_OLDER_ITEM_BUDGET,
        timelinePageShape(),
        options.selection?.(),
      );
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
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
      const merged = mergeItemsById(append, options.getItems());
      const pageBounds = cursorsAfterItemUpserts(
        pagedOldestCursor(paged, append), pagedNewestCursor(paged, append),
        append, options.getItems(), currentThread.id, includes,
      );
      let nextCeiling = pageBounds.newest ?? cloneCursor(newestLoadedCursor) ?? ceiling;
      if (newestLoadedCursor && compareCursors(nextCeiling, newestLoadedCursor) < 0) {
        nextCeiling = { ...newestLoadedCursor };
      }
      // Mirror of loadOlder's cut: append and head-drop in one flush, the
      // tail (the page just asked for) and the visible rows kept. The keyed
      // virtualizer derives the combined tail grow + head-drop and preserves
      // the reading coordinate through its head-splice compensation.
      const cut = topLevelCount(merged) > ACTIVE_TIMELINE_WINDOW_MAX_ITEMS
        ? keepWindowNearReader(merged, ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS, 'newest')
        : null;
      const next = cut?.items ?? merged;
      const nextOldest = cut?.droppedHead
        ? cut.oldestCursor
        : cloneCursor(oldestLoadedCursor) ?? oldestCursorFromItems(next);
      const nextHasMoreNewer = pagedHasMoreNewer(paged);
      // Mirror of loadOlder: stubs first, then the cut, both over the
      // merged array while the dropped rows still exist.
      const mergedBounds = { oldest: oldestLoadedCursor ?? pagedOldestCursor(paged, merged), newest: nextCeiling };
      options.activityRuns().syncRunSpans(merged, paged.runs, mergedBounds);
      if (cut) options.activityRuns().applyWindowCut(merged, next, mergedBounds);
      options.replaceTimelineItems(next, {
        disposeDropped: true,
        afterCommit: () => {
          setLoadedCursors(nextOldest, nextCeiling);
          hasMoreNewer = nextHasMoreNewer;
          if (cut?.droppedHead) hasMoreHistory = true;
          options.activityRuns().syncRunSpans(options.getItems());
        },
      });
      await tick();
      return loadOlderResult('loaded', insertedAfterWindow, insertedRows);
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      console.error('loadNewer failed:', err);
      addToast('error', 'Failed to load newer messages');
      return loadOlderResult('error');
    } finally {
      if (loadingNewer === pageGen) loadingNewer = null;
    }
  }

  async function loadRecentTail(): Promise<boolean> {
    const currentThread = options.getThread();
    if (!currentThread) return false;
    const ownership = threadBackend(currentThread.id);
    const gen = options.getSwitchGeneration();
    const pageGen = ++pagingGeneration;
    loadingNewer = pageGen;
    try {
      const paged = await ListThreadSliceAround(
        currentThread.id,
        '',
        SLICE_AROUND_ITEM_BUDGET,
        timelinePageShape(),
        options.selection?.(),
      );
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
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
      return true;
    } catch (err) {
      if (
        gen !== options.getSwitchGeneration() || threadBackend(currentThread.id) !== ownership ||
        pageGen !== pagingGeneration
      )
        return false;
      console.error('loadRecentTail failed:', err);
      addToast('error', 'Failed to load latest messages');
      return false;
    } finally {
      if (loadingNewer === pageGen) loadingNewer = null;
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
      return loadingOlder !== null;
    },
    get loadingNewer() {
      return loadingNewer !== null;
    },
    applyInitialSlice,
    applyWindowMetadataFromPaged,
    installFromSnapshot,
    resetForFreshThread,
    resetAfterLoadError,
    invalidatePendingReads: () => { pagingGeneration++; observationVersion++; },
    get requestVersion() { return observationVersion; },
    noteDroppedNewerItems,
    noteDroppedOlderItems: () => { hasMoreHistory = true; },
    applyConversationCut,
    mountActivityRunMembers,
    refreshCursorsAfterUpserts,
    pruneToRecentWindowIfNeeded,
    retryDeferredRecentWindowPrune,
    settleRecentWindowPrune,
    loadOlder: () => trackRead(loadOlder),
    loadUntilItem: (itemId: string) => trackRead(() => loadUntilItem(itemId)),
    loadNewer: () => trackRead(loadNewer),
    loadRecentTail: () => trackRead(loadRecentTail),
  };
}
