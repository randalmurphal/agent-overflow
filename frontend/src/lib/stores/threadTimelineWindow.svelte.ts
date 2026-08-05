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
  compareItemToCursor,
  compareItemsByTimelinePosition,
  cursorFromItem,
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
    },
  ): boolean;
  getThread(): Thread | null;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** streamingReveal.recomputeReveal — commitWindow calls it after every window swap. */
  recomputeReveal(): void;
  /** Registered pane scroll controller (or null) — applyPrunedWindow uses preserveTimelineWindowAnchor. */
  getScrollController(): PaneScrollController | null;
  /** Pane-owned subagent transcript hydration — loadUntilItem's subtree hydration. */
  hydrateSubagentChildren(rootItemID: string): Promise<boolean>;
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
   * Direction hint for the virtualizer's `shift` on the NEXT timeline
   * length change. See the field doc inside the factory for the full
   * rationale (bug-prone: the prepend/prune choreography is spread
   * across two flushes).
   */
  readonly pendingTimelineShiftAtHead: boolean;

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
  /** Streaming upsert appended to the tail: refresh the floor (if not already capped) and ceiling cursors. */
  refreshCursorsAfterTailAppend(): void;
  pruneToRecentWindowIfNeeded(options?: {
    hasMoreNewerAfterPrune?: boolean;
    /**
     * 'shift' is used by `loadNewer` (a paging op): the head-drop holds
     * position via the virtualizer's `shift` head-splice. 'preserve'
     * (default) is the streaming/settle path, which keeps the explicit
     * anchor-restore transaction (preserveTimelineWindowAnchor) and its
     * active-turn defer.
     */
    positionMode?: 'shift' | 'preserve';
  }): void;
  retryDeferredRecentWindowPrune(): void;
  /**
   * `settleTurn`'s prune entry: records the prune as pending for the
   * quiet scheduler when a mounted timeline would have to repaint the
   * head-drop, applies it immediately otherwise. See the factory doc.
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

  /**
   * Direction hint for the virtualizer's `shift` on the NEXT timeline length
   * change: `true` when the change happens at the HEAD (older rows prepended
   * by `loadOlder`, or the head dropped by `loadNewer`'s prune), `false` for
   * tail changes. MessageTimeline binds this to
   * `<TimelineVirtualizer shift={...}>`.
   *
   * Without it the engine treats every length change as tail growth and
   * misindexes its entire size store on a prepend — forcing a re-measure of
   * every visible row (the "scrollbar jumps around" load jank). Set
   * synchronously immediately before the `items` mutation so the engine reads
   * the right value in the same flush, and reset in the paging method's
   * `finally`. Only `loadOlder` / `loadNewer` touch it; the streaming-prune
   * path keeps its own anchor-restore (preserveTimelineWindowAnchor) and
   * leaves this `false`. The prepend/append and the prune are deliberately
   * split across two flushes so a coalesced head-grow + tail-shrink can't
   * collapse into one net length change a single `shift` can't represent.
   */
  let pendingTimelineShiftAtHead: boolean = $state(false);

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

  function includeAncestorClosure(
    keepIds: Set<string>,
    sourceItems: readonly Item[],
  ): void {
    const byId = new Map(sourceItems.map((item) => [item.id, item]));
    let changed = true;
    while (changed) {
      changed = false;
      for (const item of sourceItems) {
        if (!keepIds.has(item.id)) continue;
        if (!item.parentId || keepIds.has(item.parentId)) continue;
        if (!byId.has(item.parentId)) continue;
        keepIds.add(item.parentId);
        changed = true;
      }
    }
  }

  function keepRecentWindowItems(
    sourceItems: readonly Item[],
    targetCount: number,
  ): PrunedWindow {
    if (sourceItems.length <= targetCount) {
      return {
        items: sourceItems as Item[],
        oldestCursor: oldestCursorFromItems(sourceItems),
        newestCursor: newestCursorFromItems(sourceItems),
      };
    }
    const cutoffIndex = Math.max(0, sourceItems.length - targetCount);
    const cutoffItem = sourceItems[cutoffIndex] ?? sourceItems[0];
    const cutoffCursor = cursorFromItem(cutoffItem);
    const keepIds = new Set(
      sourceItems
        .filter((item) => compareItemToCursor(item, cutoffCursor) >= 0)
        .map((item) => item.id),
    );
    includeAncestorClosure(keepIds, sourceItems);
    return {
      items: sourceItems.filter((item) => keepIds.has(item.id)),
      oldestCursor: cutoffCursor,
      newestCursor: newestCursorFromItems(sourceItems),
    };
  }

  function keepHeadWindowItems(
    sourceItems: readonly Item[],
    targetCount: number,
  ): PrunedWindow {
    if (sourceItems.length <= targetCount) {
      return {
        items: sourceItems as Item[],
        oldestCursor: oldestCursorFromItems(sourceItems),
        newestCursor: newestCursorFromItems(sourceItems),
      };
    }
    const cutoffItem =
      sourceItems[Math.min(sourceItems.length - 1, targetCount - 1)];
    const cutoffCursor = cursorFromItem(cutoffItem);
    const keepIds = new Set(
      sourceItems
        .filter((item) => compareItemToCursor(item, cutoffCursor) <= 0)
        .map((item) => item.id),
    );
    includeAncestorClosure(keepIds, sourceItems);
    return {
      items: sourceItems.filter((item) => keepIds.has(item.id)),
      oldestCursor: oldestCursorFromItems(sourceItems),
      newestCursor: cutoffCursor,
    };
  }

  function pruneToRecentWindowIfNeeded(
    pruneOptions: {
      hasMoreNewerAfterPrune?: boolean;
      /**
       * 'shift' is used by `loadNewer` (a paging op): the head-drop holds
       * position via the virtualizer's `shift` head-splice. 'preserve' (default) is the
       * streaming/settle path, which keeps the explicit anchor-restore
       * transaction (preserveTimelineWindowAnchor) and its active-turn defer.
       */
      positionMode?: 'shift' | 'preserve';
    } = {},
  ): void {
    const items = options.getItems();
    if (items.length <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const thread = options.getThread();
    const activeTurn = thread !== null ? getActiveTurn(thread.id) : null;
    const exceedsHardCeiling =
      items.length > ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS;
    // A head-drop on a visible, bottom-pinned timeline repaints the whole
    // viewport: the content height collapses by the dropped rows, the
    // browser clamps scrollTop, and the virtualizer re-measures — seen as a blank
    // flash mid-stream (incident 2026-06-10). Defer the prune while a
    // turn is active, holding the hard ceiling as the memory backstop
    // against a runaway turn. The debt is recorded as pending so the
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
    // loadNewer paging: the dropped head sits above the viewport, so the
    // virtualizer's `shift` head-splice holds the reading position — no
    // anchor transaction, no veto.
    if (pruneOptions.positionMode === 'shift') {
      applyPagedPrune(next, {
        shiftAtHead: true,
        hasMoreHistoryAfterPrune: true,
        hasMoreNewerAfterPrune: pruneOptions.hasMoreNewerAfterPrune ?? false,
      });
      recentWindowPrunePending = false;
      return;
    }
    const vetoPolicy = exceedsHardCeiling ? 'force' : 'defer';
    const result = applyPrunedWindow(next, {
      hasMoreHistoryAfterPrune: true,
      hasMoreNewerAfterPrune: pruneOptions.hasMoreNewerAfterPrune ?? false,
      vetoPolicy,
    });
    recentWindowPrunePending = result === 'deferred';
  }

  function pruneToHeadWindowIfNeeded(): void {
    const items = options.getItems();
    if (items.length <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) return;
    const next = keepHeadWindowItems(
      items,
      ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
    );
    // loadOlder paging: the dropped tail sits below the viewport (tail change,
    // no shift, no jump), so the virtualizer leaves the reading position alone.
    applyPagedPrune(next, { shiftAtHead: false, hasMoreNewerAfterPrune: true });
  }

  // Shared window swap used by both prune paths: replace items + cursors +
  // history flags, then recompute reveal. Funnelling the mutation through one
  // place keeps the paged and preserve paths from drifting.
  function commitWindow(
    next: PrunedWindow,
    flags: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    options.replaceTimelineItems(next.items, { disposeDropped: true });
    setLoadedCursors(next.oldestCursor, next.newestCursor);
    if (flags.hasMoreHistoryAfterPrune !== undefined) {
      hasMoreHistory = flags.hasMoreHistoryAfterPrune;
    }
    if (flags.hasMoreNewerAfterPrune !== undefined) {
      hasMoreNewer = flags.hasMoreNewerAfterPrune;
    }
    options.recomputeReveal();
  }

  // Paging prune (loadOlder tail-drop / loadNewer head-drop). The dropped end
  // is always opposite the reading viewport, so there is nothing to veto and
  // no anchor to restore — the virtualizer's `shift` head-splice holds
  // position. Set the shift direction at the mutation point so the engine
  // reads it in the same flush as this length change (head-drop → splice the
  // size store from the front; tail-drop → no shift).
  function applyPagedPrune(
    next: PrunedWindow,
    pruneOptions: {
      shiftAtHead: boolean;
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
    },
  ): void {
    if (next.items.length === options.getItems().length) return;
    pendingTimelineShiftAtHead = pruneOptions.shiftAtHead;
    commitWindow(next, pruneOptions);
  }

  // Streaming / settle prune. Holds position via the explicit anchor
  // transaction (preserveTimelineWindowAnchor) because it can fire under a
  // bottom-pinned, mid-turn viewport, and it can be vetoed/deferred when the
  // prune would drop the visible anchor (vetoPolicy). Leaves the shift flag
  // false — MessageTimeline owns the rendered-node head-shift hint because
  // the virtualizer receives grouped/revealed nodes, not raw pane items.
  function applyPrunedWindow(
    next: PrunedWindow,
    pruneOptions: {
      hasMoreHistoryAfterPrune?: boolean;
      hasMoreNewerAfterPrune?: boolean;
      vetoPolicy: PrunedWindowVetoPolicy;
    },
  ): PrunedWindowApplyResult {
    if (next.items.length === options.getItems().length) return 'applied';
    let operationApplied = false;
    const apply = (): void => {
      if (operationApplied) return;
      operationApplied = true;
      commitWindow(next, pruneOptions);
    };
    const preserve = options.getScrollController()?.preserveTimelineWindowAnchor;
    if (!preserve) {
      apply();
      return 'applied';
    }
    const keptItemIds = new Set(next.items.map((item) => item.id));
    preserve({
      keepsItem: (itemId) => keptItemIds.has(itemId),
      run: apply,
    });
    if (operationApplied) return 'applied';
    if (pruneOptions.vetoPolicy === 'defer') return 'deferred';
    apply();
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
    options.replaceTimelineItems(nextItems, { disposeDropped: true });
    applyWindowMetadataFromPaged(paged);
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

  function refreshCursorsAfterTailAppend(): void {
    const thread = options.getThread();
    if (thread && !hasMoreHistory) {
      oldestLoadedCursor = oldestCursorFromItems(options.getItems());
      oldestLoadedTurnIndex = oldestLoadedCursor?.turnIndex ?? null;
    }
    if (thread) {
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
   * rushed), and the head-drop's flush is the most expensive in the
   * app, so landing it here put the stall inside the glide the reader
   * was watching (bug-report-20260801T214455Z traces; measured 78–186ms).
   * When a mounted timeline is behind the pane (the controller offers
   * the anchor transaction), the prune is recorded as pending and the
   * quiet scheduler (timelineQuietWork) retries it once nothing is
   * animating. Without one — discussion surface, headless pane — the
   * head-drop repaints nothing, so it applies immediately.
   * The hard ceiling stays with the append path and is unaffected.
   * See docs/architecture/scroll-arbitration-plan.md.
   */
  function settleRecentWindowPrune(): void {
    if (hasMoreNewer) return;
    if (options.getItems().length <= ACTIVE_TIMELINE_WINDOW_MAX_ITEMS) {
      recentWindowPrunePending = false;
      return;
    }
    if (options.getScrollController()?.preserveTimelineWindowAnchor) {
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
      const previousNewest = cloneCursor(newestLoadedCursor);
      const paged = await ListItemsBeforeCursor(
        currentThread.id,
        cursorForBinding(floor),
        LOAD_OLDER_ITEM_BUDGET,
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
      // Head-grow: the engine unshifts its size store and reports a
      // head-splice compensation so the reading position holds. Set
      // before the mutation so the engine reads it in the same flush.
      pendingTimelineShiftAtHead = true;
      const next = mergeItemsById(prepend, options.getItems());
      options.replaceTimelineItems(next, { disposeDropped: true });
      const nextFloor = pagedOldestCursor(paged, prepend) ?? floor;
      setLoadedCursors(
        nextFloor,
        previousNewest ?? newestCursorFromItems(options.getItems()),
      );
      // Progress guard. If the backend returned no items AND the floor
      // didn't decrease, another click would fire the same query for
      // the same range. Force hasMore=false so the UI stops offering a
      // button that can't actually load anything. A later in-flight
      // upsert that lands an older item will re-enable paging through
      // the normal streaming path.
      if (prepend.length === 0 && compareCursors(nextFloor, floor) >= 0) {
        hasMoreHistory = false;
      } else {
        hasMoreHistory = pagedHasMoreOlder(paged);
      }
      // Let the engine process the head-grow (shift=true) before the
      // prune. The two MUST be separate flushes: coalesced, the net length
      // change can't represent "prepend at head + drop at tail" and the
      // size store scrambles (spike-verified — see frontend-scroll.md).
      await tick();
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      // Flush 2: tail-prune (shift=false). Dropped rows are below the
      // viewport, so this is transparent to the reading position.
      pruneToHeadWindowIfNeeded();
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
      // Both flushes have run by now; clear the one-shot shift hint so a
      // later streaming length change isn't misread as a head mutation.
      pendingTimelineShiftAtHead = false;
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
      options.replaceTimelineItems(next, { disposeDropped: true });
      applyWindowMetadataFromPaged(paged);
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
      const previousOldest = cloneCursor(oldestLoadedCursor);
      const paged = await ListItemsAfterCursor(
        currentThread.id,
        cursorForBinding(ceiling),
        LOAD_OLDER_ITEM_BUDGET,
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
      // Tail-grow: shift stays false (the engine appends size slots at the
      // end, no scroll compensation — rows arrive below the viewport).
      pendingTimelineShiftAtHead = false;
      const next = mergeItemsById(append, options.getItems());
      options.replaceTimelineItems(next, { disposeDropped: true });
      const nextCeiling = pagedNewestCursor(paged, append) ?? ceiling;
      setLoadedCursors(
        previousOldest ?? oldestCursorFromItems(options.getItems()),
        nextCeiling,
      );
      const nextHasMoreNewer =
        append.length === 0 && compareCursors(nextCeiling, ceiling) <= 0
          ? false
          : pagedHasMoreNewer(paged);
      hasMoreNewer = nextHasMoreNewer;
      // Flush 1: the engine processes the tail-grow before the head-prune.
      // Separate flushes (see loadOlder): a coalesced tail-grow +
      // head-shrink can't be expressed by one `shift`.
      await tick();
      if (
        gen !== options.getSwitchGeneration() ||
        pageGen !== pagingGeneration
      )
        return loadOlderResult('stale');
      // Flush 2: head-prune (shift=true) — the engine splices its size
      // store from the front and compensates scrollTop by the dropped
      // height, holding the reading position. No explicit anchor restore
      // needed.
      pruneToRecentWindowIfNeeded({
        hasMoreNewerAfterPrune: nextHasMoreNewer,
        positionMode: 'shift',
      });
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
      pendingTimelineShiftAtHead = false;
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
      options.replaceTimelineItems(next, { disposeDropped: true });
      applyWindowMetadataFromPaged(paged);
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
    get pendingTimelineShiftAtHead() {
      return pendingTimelineShiftAtHead;
    },
    applyInitialSlice,
    applyWindowMetadataFromPaged,
    installFromSnapshot,
    resetForFreshThread,
    resetAfterLoadError,
    noteDroppedNewerItems,
    refreshCursorsAfterTailAppend,
    pruneToRecentWindowIfNeeded,
    retryDeferredRecentWindowPrune,
    settleRecentWindowPrune,
    loadOlder,
    loadUntilItem,
    loadNewer,
    loadRecentTail,
  };
}
