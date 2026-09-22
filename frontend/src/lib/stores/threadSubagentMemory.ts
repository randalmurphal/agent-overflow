import type { Item } from '../types/models';
import {
  isItemActive,
  subagentActivityPreview,
} from '../utils/subagentGrouping';
import {
  subagentLaunchContextFrom,
  subagentLaunchInfo,
  type SubagentLaunchContext,
} from '../utils/subagentLaunch';
import type {
  SubagentFoldAggregate,
  SubagentFoldSnapshot,
} from '../utils/subagentFold';
import { createSubagentFoldRegistry } from '../utils/subagentFold';
import {
  mergeMissingItemsById,
} from './threadItems';

export interface ThreadSubagentMemoryOptions {
  /** Current item window, sorted by (turnIndex, itemIndex). Re-read per call. */
  getItems(): Item[];
  getThreadId(): string | null;
  /** Index of an id in the current window, or undefined. */
  getItemIndex(itemId: string): number | undefined;
  /** The pane's items-replacement chokepoint (index rebuild, fold retention, dispose, revision bump). */
  replaceTimelineItems(
    nextItems: Item[],
    options?: {
      disposeDropped?: boolean;
    },
  ): boolean;
  /**
   * Same chokepoint, for the drops this module already knows the shape
   * of: one pass splits the window instead of filtering it and then
   * diffing the result back against the previous array.
   */
  dropTimelineItems(
    shouldDrop: (item: Item) => boolean,
  ): Item[];

}

/**
 * Bound on the parent walk `evictableAnchorIdFor` does. Real subagent trees
 * are two or three deep; the cap exists only so corrupt provider parentId
 * links cannot spin here.
 */
const MAX_ANCESTOR_HOPS = 16;

/** One row leaving pane memory for the fold, keyed by its launch anchor. */
interface SubagentEviction {
  item: Item;
  anchorId: string;
}

export interface ThreadSubagentMemory {
  /** True when the id is folded under any anchor (upsert replay swallow). */
  isEvicted(itemId: string): boolean;
  /**
   * Record a window-admission outcome: rows that `landed` leave the
   * swallow ledger (a swallowed child re-admitted once its anchor is
   * back must stream again), rows that were `rejected` enter it. The
   * admission DECISION itself lives in the window merges
   * (`applyItemUpsertsToWindow.rejectedParentedItems`,
   * `reconcileSnapshotPage.orphanedLiveChildren`) — this module only keeps the
   * ledger that silences the rejected rows' later deltas.
   */
  recordAdmission(landed: readonly Item[], rejected: readonly Item[]): void;
  /** True when the id was refused window admission (delta swallow). */
  isSwallowedChild(itemId: string): boolean;
  /** Fold-and-drop settled subagent children from the changed-row set of an upsert batch or status patch. */
  evictSettledChildren(candidates: readonly Item[]): void;
  /** Collapse-time eviction: fold every settled descendant under `anchorId` out of pane memory. */
  evictCollapsedSubtree(anchorId: string): void;
  /** Admit only an explicitly requested navigation target and its ancestors. */
  mountNavigationItems(items: readonly Item[]): void;
  /** Live fold aggregate for a launch anchor, or undefined when nothing is folded. */
  aggregate(anchorId: string): SubagentFoldAggregate | undefined;
  /** Drop folds whose anchor has left the loaded window. */
  retainFoldAnchors(): void;
  /** Plain-data copy of the fold registry for the thread-switch snapshot cache. */
  snapshotFolds(): SubagentFoldSnapshot | null;
  /** Replace the fold registry from a cached snapshot (thread re-entry). */
  restoreFolds(snapshot: SubagentFoldSnapshot | null | undefined): void;
  /** Clear the fold registry only. */
  clearFolds(): void;

  clearWindowDerivedState(): void;
  /** Fresh-thread reset: fold registry + `clearWindowDerivedState`. */
  resetForFreshThread(): void;
}

export function createThreadSubagentMemory(
  options: ThreadSubagentMemoryOptions,
): ThreadSubagentMemory {

  const subagentFolds = createSubagentFoldRegistry();

  const swallowedChildIds = new Set<string>();

  /**
   * A visit that keeps streaming children under long-pruned anchors adds
   * entries for the whole visit (the production incident held ~5900);
   * the cap keeps the ledger from becoming a leak of its own. Clearing
   * wholesale on overflow mirrors `warnedMissingDeltaIds`: the fallout
   * is a capped re-warn per still-streaming cleared id, cheaper than
   * growth.
   */
  const MAX_SWALLOWED_CHILD_IDS = 4096;

  function recordAdmission(
    landed: readonly Item[],
    rejected: readonly Item[],
  ): void {
    if (swallowedChildIds.size > 0) {
      for (const item of landed) swallowedChildIds.delete(item.id);
    }
    if (rejected.length === 0) return;
    if (
      swallowedChildIds.size + rejected.length
      > MAX_SWALLOWED_CHILD_IDS
    ) {
      swallowedChildIds.clear();
    }
    for (const item of rejected) swallowedChildIds.add(item.id);
  }

  /**
   * Launch-predicate context over the CURRENT window. Built per public
   * entry point rather than cached: the window changes under us, and the
   * context's parent-id index is lazy, so a batch that touches no `Skill`
   * row never materializes one.
   */
  function launchContext(): SubagentLaunchContext {
    return subagentLaunchContextFrom(options.getItems());
  }

  function evictableAnchorIdFor(item: Item, ctx: SubagentLaunchContext): string | null {
    if (isItemActive(item)) return null;
    if (subagentLaunchInfo(item, ctx) !== null) return null;
    const items = options.getItems();
    let parentId = item.parentId ?? '';
    // Walk to the nearest launch ANCESTOR, which is the same anchor
    // `groupItemsBySubagent` buckets the row under: a row parented on an
    // ordinary tool call inside an agent still renders in that agent's card,
    // so the two must agree on where it folds. `MAX_ANCESTOR_HOPS` bounds a
    // corrupt parentId cycle — the grouping pass carries the same guard.
    for (let hops = 0; parentId && hops < MAX_ANCESTOR_HOPS; hops++) {
      const parentIndex = options.getItemIndex(parentId);
      if (parentIndex === undefined) return null;
      const parent = items[parentIndex];
      if (subagentLaunchInfo(parent, ctx) !== null) {
        return parent.id;
      }
      parentId = parent.parentId ?? '';
    }
    return null;
  }

  /**
   * Collect every settled non-launch descendant under `anchorId` into
   * `out`. Nested launches — of ANY kind, including a forked skill or a
   * Codex spawn inside a Claude agent — stay loaded (they are fold keys
   * and render as nested cards); their settled children fold under their
   * own anchor so nested entry counters stay honest. One forward pass
   * resolves the whole chain because items are in (turnIndex, itemIndex)
   * order — a launch precedes its rows (invariants #10/#11).
   */
  function collectSettledSubtree(
    anchorId: string,
    out: SubagentEviction[],
    ctx: SubagentLaunchContext,
  ): void {
    const launchIds = new Set([anchorId]);
    // Nearest-launch-ancestor resolution, mirroring the grouping pass's
    // bucketing exactly: an ACTIVE row is still recorded here (it just is
    // not evicted) so its own settled children can resolve through it.
    const anchorOf = new Map<string, string>();
    for (const item of options.getItems()) {
      const parentId = item.parentId ?? '';
      if (!parentId) continue;
      const anchor = launchIds.has(parentId) ? parentId : anchorOf.get(parentId);
      if (anchor === undefined) continue;
      if (subagentLaunchInfo(item, ctx) !== null) {
        launchIds.add(item.id);
        continue;
      }
      anchorOf.set(item.id, anchor);
      if (isItemActive(item)) continue;
      out.push({ item, anchorId: anchor });
    }
  }

  function commitSubagentEvictions(candidates: readonly SubagentEviction[]): void {
    const evictions = candidates;
    if (evictions.length === 0) return;
    const evictedIds = new Set<string>();
    for (const { item, anchorId } of evictions) {
      subagentFolds.recordEvicted(
        anchorId,
        item,
        subagentActivityPreview(item),
      );
      evictedIds.add(item.id);
    }
    options.dropTimelineItems((it) => evictedIds.has(it.id));
  }

  function evictSettledChildren(candidates: readonly Item[]): void {
    const ctx = launchContext();
    let evictions: SubagentEviction[] | null = null;
    for (const candidate of candidates) {
      if (subagentLaunchInfo(candidate, ctx) !== null) {
        // The launch owns aggregates; expanded cards own separate paged windows.
        collectSettledSubtree(candidate.id, (evictions ??= []), ctx);
        continue;
      }
      const anchorId = evictableAnchorIdFor(candidate, ctx);
      if (anchorId === null) continue;
      (evictions ??= []).push({ item: candidate, anchorId });
    }
    if (evictions) commitSubagentEvictions(evictions);
  }

  function evictCollapsedSubtree(anchorId: string): void {
    const anchorIndex = options.getItemIndex(anchorId);
    if (anchorIndex === undefined) return;
    const ctx = launchContext();
    if (subagentLaunchInfo(options.getItems()[anchorIndex], ctx) === null) return;
    const evictions: SubagentEviction[] = [];
    collectSettledSubtree(anchorId, evictions, ctx);
    commitSubagentEvictions(evictions);
  }

  function mountNavigationItems(items: readonly Item[]): void {
    const threadId = options.getThreadId();
    if (!threadId || items.some(item => item.threadId !== threadId)) {
      throw new Error('Navigation items do not belong to the current thread');
    }
    subagentFolds.reclaim(items.map(item => item.id));
    for (const item of items) swallowedChildIds.delete(item.id);
    options.replaceTimelineItems(mergeMissingItemsById(items, options.getItems()));
  }

  function retainFoldAnchors(): void {
    subagentFolds.retainAnchors(
      (anchorId) => options.getItemIndex(anchorId) !== undefined,
    );
  }

  function clearFolds(): void {
    subagentFolds.clear();
  }

  function clearWindowDerivedState(): void {
    swallowedChildIds.clear();
  }

  return {
    isEvicted: (itemId) => subagentFolds.isEvicted(itemId),
    recordAdmission,
    isSwallowedChild: (itemId) => swallowedChildIds.has(itemId),
    evictSettledChildren,
    evictCollapsedSubtree,
    mountNavigationItems,
    aggregate: (anchorId) => subagentFolds.aggregate(anchorId),
    retainFoldAnchors,
    snapshotFolds: () => subagentFolds.snapshot(),
    restoreFolds: (snapshot) => subagentFolds.restore(snapshot),
    clearFolds,
    clearWindowDerivedState,
    resetForFreshThread(): void {
      clearFolds();
      clearWindowDerivedState();
    },
  };
}
