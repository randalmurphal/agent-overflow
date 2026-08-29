import type { Item, Thread } from '../types/models';
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
  /** rowUiState.isSubagentGroupExpanded — the retention check for every launch kind. */
  isSubagentGroupExpanded(groupKey: string): boolean;
  /**
   * Every row the OPEN agent companion is rendering (the scope trail's
   * whole subtree), or null when no pane is open. Consulted at the
   * eviction COMMIT chokepoint, because per-anchor expansion checks
   * cannot cover every path to it: collapse-time eviction runs on a
   * card that is by definition collapsed, and the collapsed-launch
   * sweep collects whole subtrees — both would fold the very rows the
   * pane has mounted (live incident 2026-08-22: collapsing the card
   * blanked the open pane into a hydrate-again flicker).
   */
  agentPaneHeldRows(): ReadonlySet<string> | null;
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
  /**
   * Clear every piece of window-derived bookkeeping except the fold
   * registry: the hydration in-flight/exhausted dedupe sets and the
   * admission swallow ledger. Called at window-identity changes (thread
   * switch, cache restore, replica paint, pane clear) — the fold
   * registry is the one part that survives via snapshot/restore. This is
   * hygiene, not the correctness guard: a stale swallow entry cannot
   * silence a loaded row, because `applyItemDelta` checks the item index
   * before it ever consults the ledger. Correctness lives in that check
   * order.
   */
  clearWindowDerivedState(): void;
  /** Fresh-thread reset: fold registry + `clearWindowDerivedState`. */
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
   * i.e. once their card is collapsed, whichever launch kind it is — and
   * their count/preview fold in here so the collapsed card stays honest.
   * SQLite keeps the rows (triage persists before emitting); expansion
   * re-hydrates and `reclaim`s the ids. Every fold mutation rides a
   * `replaceTimelineItems` revision bump, which is what re-runs the
   * grouping derivation that reads these aggregates.
   */
  const subagentFolds = createSubagentFoldRegistry();

  /**
   * Ids refused window admission because nothing loaded could anchor
   * them. History windows load top-level rows only, and subagent
   * children render from the backend-decorated aggregate on their anchor
   * plus on-demand hydration — so a streamed child whose anchor is not
   * in the window has nothing that can render it. Admitting it anyway is
   * what produced the orphan-leaf regression: settled children evict
   * into their fold, the anchor stops being retained by
   * `includeAncestorClosure` and prunes away, and from then on every new
   * child of that anchor lands as a top-level orphan row that no
   * eviction policy can reach (`evictableAnchorIdFor` needs the parent).
   * Refusal costs nothing: triage persisted the row before emitting it,
   * so the anchor's decorated count already includes it and
   * `hydrateChildren` renders it whenever the anchor is on screen again.
   * (One narrow exception, shared with the fold registry's replay
   * swallow: `designState`'s assistant-payload scan only sees rows that
   * land, so a design fence inside a refused subagent child is never
   * projected. Design payloads ride top-level assistant rows.)
   *
   * The ledger exists to keep the rejected rows' later deltas silent —
   * the same swallow contract the fold registry provides for evicted
   * ids. A stale entry is harmless by construction: `applyItemDelta`
   * consults it only after an index miss, so a LOADED row always
   * applies whatever this set says. Entries leave per id when the row
   * lands after all (re-admission, hydration), wholesale on
   * `clearWindowDerivedState`, and wholesale at the cap below.
   */
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

  /**
   * Eviction policy for one upserted or patched row. Returns the launch
   * anchor to fold the row under, or null when the row must stay in
   * pane memory: still active (the delta pipeline requires streaming
   * rows to exist), itself a launch anchor (anchors are the fold keys
   * and the cards), a flat non-subagent row, an orphan, or a child of a
   * card that is currently expanded. Every launch kind renders its
   * transcript inline now, so there is one retention rule rather than
   * one per kind. Retention is keyed on the direct parent's expansion
   * only; settled rows under collapsed ancestors are swept by
   * evictCollapsedSubtree when their own card collapses.
   */
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
        return options.isSubagentGroupExpanded(parent.id) ? null : parent.id;
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

  /**
   * Commit evictions: record each row in the fold registry, then drop
   * the rows through the pane's drop chokepoint so smoothers and row UI
   * state are cleaned like any other dropped row, and recompute the
   * reveal gate. Exhausted-hydration markers clear only for the anchors
   * whose transcripts changed — see disposeDroppedItemState. Duplicate
   * entries are harmless: the registry and the drop set both dedupe by
   * id.
   */
  function commitSubagentEvictions(candidates: readonly SubagentEviction[]): void {
    // Rows the open agent pane is rendering never fold, whatever path
    // nominated them — see the option's doc. They fold normally on the
    // first eviction after the pane closes or re-scopes.
    const held = candidates.length > 0 ? options.agentPaneHeldRows() : null;
    const evictions = held
      ? candidates.filter(({ item }) => !held.has(item.id))
      : candidates;
    if (evictions.length === 0) return;
    const evictedIds = new Set<string>();
    const anchorIds = new Set<string>();
    for (const { item, anchorId } of evictions) {
      subagentFolds.recordEvicted(
        anchorId,
        item,
        subagentActivityPreview(item),
      );
      evictedIds.add(item.id);
      anchorIds.add(anchorId);
    }
    options.dropTimelineItems((it) => evictedIds.has(it.id), {
      exhaustedScope: anchorIds,
    });
  }

  /**
   * Fold-and-drop settled subagent children that nothing can render.
   * `candidates` is the changed-row set of the upsert batch or status
   * patch that just applied: children that arrived terminal, children
   * whose stored row just flipped terminal, and — when a COLLAPSED launch
   * anchor itself changed — a sweep of its settled subtree.
   */
  function evictSettledChildren(candidates: readonly Item[]): void {
    const ctx = launchContext();
    let evictions: SubagentEviction[] | null = null;
    for (const candidate of candidates) {
      if (subagentLaunchInfo(candidate, ctx) !== null) {
        // A launch row is never evictable itself — it is the fold key and
        // the card. But a launch row changing while its card is COLLAPSED
        // is the one moment a whole settled transcript can be sitting in
        // memory with nothing able to render it: the rows may have landed
        // before the anchor was recognisable as a launch (a `Skill` only
        // becomes a fork once something is attributed to it). Sweep it.
        if (!options.isSubagentGroupExpanded(candidate.id)) {
          collectSettledSubtree(candidate.id, (evictions ??= []), ctx);
        }
        continue;
      }
      const anchorId = evictableAnchorIdFor(candidate, ctx);
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
    const ctx = launchContext();
    if (subagentLaunchInfo(options.getItems()[anchorIndex], ctx) === null) return;
    const evictions: SubagentEviction[] = [];
    collectSettledSubtree(anchorId, evictions, ctx);
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
      const hydratedIds = incoming.map((child) => child.id);
      // Rows coming back into memory leave the live-eviction fold first —
      // the invariant is an id is folded XOR loaded, so the card's count
      // (loaded + folded) stays exact through the hydration round-trip.
      subagentFolds.reclaim(hydratedIds);
      // Same reason, other swallow: hydration loads these rows for real,
      // so their deltas must apply instead of being silenced as
      // anchorless stream arrivals.
      for (const id of hydratedIds) swallowedChildIds.delete(id);
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

  function clearWindowDerivedState(): void {
    subagentHydrationInFlight.clear();
    subagentHydrationExhausted.clear();
    swallowedChildIds.clear();
  }

  return {
    isEvicted: (itemId) => subagentFolds.isEvicted(itemId),
    recordAdmission,
    isSwallowedChild: (itemId) => swallowedChildIds.has(itemId),
    evictSettledChildren,
    evictCollapsedSubtree,
    hydrateChildren,
    aggregate: (anchorId) => subagentFolds.aggregate(anchorId),
    retainFoldAnchors,
    resetHydrationExhausted,
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
