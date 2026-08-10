import type { Item, Thread } from '../types/models';
import {
  isItemActive,
  normalizePreviewText,
  subagentLaunchKind,
} from '../utils/subagentGrouping';
import type {
  SubagentFoldAggregate,
  SubagentFoldSnapshot,
} from '../utils/subagentFold';
import { createSubagentFoldRegistry } from '../utils/subagentFold';
import { ListSubagentDescendants } from './bindings';
import { itemsForThread, mergeMissingItemsById } from './threadItems';
import { addToast } from './toast.svelte';

export interface ThreadSubagentMemoryOptions {
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The pane's items-replacement chokepoint (index rebuild, fold retention, dispose, revision bump). */
  replaceTimelineItems(
    nextItems: Item[],
    options?: {
      disposeDropped?: boolean;
      exhaustedScope?: ReadonlySet<string>;
    },
  ): boolean;
  /**
   * Same chokepoint, for the drops this module already knows the shape
   * of: one pass splits the window instead of filtering it and then
   * diffing the result back against the previous array.
   */
  dropTimelineItems(
    shouldDrop: (item: Item) => boolean,
    options?: { exhaustedScope?: ReadonlySet<string> },
  ): Item[];
  getThread(): Thread | null;
  /** Pane switch generation — captured at load start, compared after awaits. */
  getSwitchGeneration(): number;
  /** streamingReveal.recomputeReveal — commitSubagentEvictions recomputes after the drop. */
  recomputeReveal(): void;
  /** rowUiState.isSubagentGroupExpanded — retention check for inline launches. */
  isSubagentGroupExpanded(groupKey: string): boolean;
}

/** One row leaving pane memory for the fold, keyed by its launch anchor. */
interface SubagentEviction {
  item: Item;
  anchorId: string;
}

export interface ThreadSubagentMemory {
  /** True when the id is folded under any anchor (upsert replay swallow). */
  isEvicted(itemId: string): boolean;
  /** Fold-and-drop settled subagent children from the changed-row set of an upsert batch or status patch. */
  evictSettledChildren(candidates: readonly Item[]): void;
  /** Collapse-time eviction: fold every settled descendant under `anchorId` out of pane memory. */
  evictCollapsedSubtree(anchorId: string): void;
  /** Hydrate the child transcript under a subagent launch anchor. */
  hydrateChildren(rootItemID: string): Promise<boolean>;
  /** Live fold aggregate for a launch anchor, or undefined when nothing is folded. */
  aggregate(anchorId: string): SubagentFoldAggregate | undefined;
  /** Drop folds whose anchor has left the loaded window. */
  retainFoldAnchors(): void;
  /** Re-arm hydratable anchors after rows drop out of the window (scoped, or wholesale when no scope is given). */
  resetHydrationExhausted(exhaustedScope?: ReadonlySet<string>): void;
  /** Plain-data copy of the fold registry for the thread-switch snapshot cache. */
  snapshotFolds(): SubagentFoldSnapshot | null;
  /** Replace the fold registry from a cached snapshot (thread re-entry). */
  restoreFolds(snapshot: SubagentFoldSnapshot | null | undefined): void;
  /** Clear the fold registry only. */
  clearFolds(): void;
  /** Clear the hydration in-flight/exhausted dedupe sets only. */
  clearHydrationState(): void;
  /** Fresh-thread reset: fold registry + both hydration sets. */
  resetForFreshThread(): void;
}

/**
 * Owns the subagent transcript-memory domain for a thread pane: the
 * live-eviction fold registry (see utils/subagentFold.ts), the
 * settled-child eviction policy, and on-demand child hydration. The
 * pane data layer remains the sole mutator of `items` — this factory
 * reads/replaces the window through `options.getItems()` /
 * `options.replaceTimelineItems()`, so item-array assignment still
 * happens inside the pane's own reactive scope.
 */
export function createThreadSubagentMemory(
  options: ThreadSubagentMemoryOptions,
): ThreadSubagentMemory {
  /**
   * Subagent-children hydration dedupe, keyed by launch anchor item id.
   * `inFlight` stops a re-running expansion effect from double-fetching;
   * `exhausted` marks anchors whose last fetch added nothing new, so a
   * stale decorated descendant count on the anchor's meta can't loop
   * the expansion effect against a backend with nothing more to give.
   * Both reset on thread switch / clear.
   */
  const subagentHydrationInFlight = new Set<string>();
  const subagentHydrationExhausted = new Set<string>();

  /**
   * Live-eviction fold for subagent children (see utils/subagentFold.ts).
   * Terminal child rows leave pane memory once nothing can render them —
   * collapsed inline cards, backgrounded launches, Codex spawns — and
   * their count/preview fold in here so the collapsed card stays honest.
   * SQLite keeps the rows (triage persists before emitting); expansion
   * re-hydrates and `reclaim`s the ids. Every fold mutation rides a
   * `replaceTimelineItems` revision bump, which is what re-runs the
   * grouping derivation that reads these aggregates.
   */
  const subagentFolds = createSubagentFoldRegistry();

  /**
   * Eviction policy for one upserted or patched row. Returns the launch
   * anchor to fold the row under, or null when the row must stay in
   * pane memory: still active (the delta pipeline requires streaming
   * rows to exist), itself a launch anchor (anchors are the fold keys
   * and the cards), a flat non-subagent row, an orphan, or a child of
   * an inline card that is currently expanded. Retention is keyed on
   * the direct parent's expansion only; settled rows under collapsed
   * ancestors are swept by evictCollapsedSubtree when their own
   * card collapses.
   */
  function evictableAnchorIdFor(item: Item): string | null {
    if (isItemActive(item)) return null;
    const parentId = item.parentId ?? '';
    if (!parentId) return null;
    if (subagentLaunchKind(item) !== null) return null;
    const parentIndex = options.getItemIndex(parentId);
    if (parentIndex === undefined) return null;
    const parent = options.getItems()[parentIndex];
    const launchKind = subagentLaunchKind(parent);
    if (launchKind === null) return null;
    if (launchKind === 'inline' && options.isSubagentGroupExpanded(parent.id)) {
      return null;
    }
    return parent.id;
  }

  /**
   * Collect every settled non-launch descendant under `anchorId` into
   * `out`. Nested launches stay loaded (they are fold keys and render
   * as nested cards); their settled children fold under their own
   * anchor so nested entry counters stay honest. One forward pass
   * suffices because items are in (turnIndex, itemIndex) order — a
   * launch precedes its rows.
   */
  function collectSettledSubtree(anchorId: string, out: SubagentEviction[]): void {
    const launchIds = new Set([anchorId]);
    for (const item of options.getItems()) {
      const parentId = item.parentId ?? '';
      if (!parentId || !launchIds.has(parentId)) continue;
      if (subagentLaunchKind(item) !== null) {
        launchIds.add(item.id);
        continue;
      }
      if (isItemActive(item)) continue;
      out.push({ item, anchorId: parentId });
    }
  }

  /**
   * Commit evictions: record each row in the fold registry, then drop
   * the rows through the pane's drop chokepoint so smoothers and row UI
   * state are cleaned like any other dropped row, and recompute the
   * reveal gate. Exhausted-hydration markers clear only for the anchors
   * whose transcripts changed — see disposeDroppedItemState. Duplicate
   * entries are harmless: the registry and the drop set both dedupe by
   * id.
   */
  function commitSubagentEvictions(evictions: readonly SubagentEviction[]): void {
    if (evictions.length === 0) return;
    const evictedIds = new Set<string>();
    const anchorIds = new Set<string>();
    for (const { item, anchorId } of evictions) {
      subagentFolds.recordEvicted(
        anchorId,
        item,
        normalizePreviewText(item.summary ?? ''),
      );
      evictedIds.add(item.id);
      anchorIds.add(anchorId);
    }
    options.dropTimelineItems((it) => evictedIds.has(it.id), {
      exhaustedScope: anchorIds,
    });
    options.recomputeReveal();
  }

  /**
   * Fold-and-drop settled subagent children that nothing can render.
   * `candidates` is the changed-row set of the upsert batch or status
   * patch that just applied: children that arrived terminal, children
   * whose stored row just flipped terminal, and — when a launch anchor
   * itself changed — a sweep of its settled subtree (covers a
   * foreground launch being backgrounded mid-run, which flips its
   * whole transcript from expandable to suppressed).
   */
  function evictSettledChildren(candidates: readonly Item[]): void {
    let evictions: SubagentEviction[] | null = null;
    for (const candidate of candidates) {
      if (subagentLaunchKind(candidate) === 'suppressed') {
        collectSettledSubtree(candidate.id, (evictions ??= []));
        continue;
      }
      const anchorId = evictableAnchorIdFor(candidate);
      if (anchorId === null) continue;
      (evictions ??= []).push({ item: candidate, anchorId });
    }
    if (evictions) commitSubagentEvictions(evictions);
  }

  /**
   * Collapse-time eviction: fold every settled descendant under
   * `anchorId` out of pane memory (counts and preview survive in the
   * fold registry; rows re-hydrate from SQLite on the next expand).
   */
  function evictCollapsedSubtree(anchorId: string): void {
    const anchorIndex = options.getItemIndex(anchorId);
    if (anchorIndex === undefined) return;
    if (subagentLaunchKind(options.getItems()[anchorIndex]) === null) return;
    const evictions: SubagentEviction[] = [];
    collectSettledSubtree(anchorId, evictions);
    commitSubagentEvictions(evictions);
  }

  /**
   * Hydrate the child transcript under a subagent launch anchor.
   * History windows deliver only top-level rows — the collapsed
   * SubagentGroup card renders from backend-decorated aggregates, and
   * this loads the actual rows when the card expands (or when a
   * scroll-to-item target lives inside the subtree).
   *
   * Additive merge only: rows already in memory (live-streamed
   * children) keep their references, missing rows are inserted at
   * their (turnIndex, itemIndex) position. Child rows are never
   * top-level, so the reveal boundary is unaffected — same exception
   * as `loadOlder` (see the reveal-gate invariant note above).
   *
   * Returns true when new rows were merged in.
   */
  async function hydrateChildren(rootItemID: string): Promise<boolean> {
    const currentThread = options.getThread();
    if (!currentThread || !rootItemID) return false;
    if (
      subagentHydrationInFlight.has(rootItemID) ||
      subagentHydrationExhausted.has(rootItemID)
    ) {
      return false;
    }

    const gen = options.getSwitchGeneration();
    subagentHydrationInFlight.add(rootItemID);
    try {
      const children = (await ListSubagentDescendants(
        currentThread.id,
        rootItemID,
      )) as Item[];
      if (gen !== options.getSwitchGeneration()) return false;
      const incoming = itemsForThread(children ?? [], currentThread.id);
      // Rows coming back into memory leave the live-eviction fold first —
      // the invariant is an id is folded XOR loaded, so the card's count
      // (loaded + folded) stays exact through the hydration round-trip.
      subagentFolds.reclaim(incoming.map((child) => child.id));
      const currentItems = options.getItems();
      const next = mergeMissingItemsById(incoming, currentItems);
      if (next === currentItems) {
        subagentHydrationExhausted.add(rootItemID);
        return false;
      }
      options.replaceTimelineItems(next);
      return true;
    } catch (err) {
      if (gen !== options.getSwitchGeneration()) return false;
      console.error('hydrateSubagentChildren failed:', err);
      addToast('error', 'Failed to load subagent activity');
      return false;
    } finally {
      subagentHydrationInFlight.delete(rootItemID);
    }
  }

  /** Drop folds whose anchor has left the loaded window — see the call
   *  site's comment in the pane's `replaceTimelineItems` for the
   *  fold↔items chokepoint rationale. */
  function retainFoldAnchors(): void {
    subagentFolds.retainAnchors(
      (anchorId) => options.getItemIndex(anchorId) !== undefined,
    );
  }

  /**
   * Dropped rows can include hydrated subagent children. Their
   * anchors must become hydratable again — a stale exhausted marker
   * would otherwise suppress the next expansion fetch and wedge the
   * card on its loading placeholder. Live-eviction callers know
   * exactly which anchors lost rows and pass them as
   * `exhaustedScope`; unrelated markers survive, because clearing
   * wholesale at eviction cadence re-arms any expanded card whose
   * loaded count persistently trails its total into a refetch per
   * eviction. Bulk window replacements (prune, reconcile, revert)
   * clear wholesale: mapping a dropped grandchild back to its launch
   * root would need an ancestor walk over rows we just dropped, and
   * the cost of breadth is one no-op refetch per re-expanded anchor.
   */
  function resetHydrationExhausted(exhaustedScope?: ReadonlySet<string>): void {
    if (exhaustedScope) {
      for (const anchorId of exhaustedScope) {
        subagentHydrationExhausted.delete(anchorId);
      }
    } else {
      subagentHydrationExhausted.clear();
    }
  }

  function clearFolds(): void {
    subagentFolds.clear();
  }

  function clearHydrationState(): void {
    subagentHydrationInFlight.clear();
    subagentHydrationExhausted.clear();
  }

  return {
    isEvicted: (itemId) => subagentFolds.isEvicted(itemId),
    evictSettledChildren,
    evictCollapsedSubtree,
    hydrateChildren,
    aggregate: (anchorId) => subagentFolds.aggregate(anchorId),
    retainFoldAnchors,
    resetHydrationExhausted,
    snapshotFolds: () => subagentFolds.snapshot(),
    restoreFolds: (snapshot) => subagentFolds.restore(snapshot),
    clearFolds,
    clearHydrationState,
    resetForFreshThread(): void {
      clearFolds();
      clearHydrationState();
    },
  };
}
