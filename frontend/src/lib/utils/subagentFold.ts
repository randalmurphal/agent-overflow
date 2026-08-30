// Per-launch-anchor live aggregates for subagent children that have been
// evicted from pane memory.
//
// During a live turn, subagent child rows stream into `pane.items` so the
// card UX (per-agent entry counters, latest-action preview, expanded live
// transcript) can render from real rows. Once a child reaches a terminal
// status and its card is collapsed — every launch kind renders its
// transcript inline in the same card, so that is the whole rule — the row
// is dropped from pane memory and folded here: the id is remembered for
// replay dedupe, the count is remembered for the collapsed-card entry
// counter, and the latest terminal preview is remembered for the card's
// "what did it do last" line. SQLite keeps the authoritative rows (triage
// persists every item before emitting it), so expansion re-hydrates the
// transcript through ListSubagentDescendants and `reclaim`s the ids from
// this registry — counts never double.
//
// Invariant: an item id is never simultaneously in pane.items and in a
// fold. Eviction adds it here as it leaves the items array; hydration
// removes it here before the row merges back in.
//
// Pure bookkeeping — no Svelte reactivity. The owning pane bumps its
// `timelineRevision` whenever a fold mutates so the grouping derivation
// re-reads the aggregates.

import type { Item } from '../types/models';

/** Live aggregate exposed to the grouping pipeline for one launch anchor. */
export interface SubagentFoldAggregate {
  /** Terminal descendants evicted from pane memory (none currently loaded). */
  evictedCount: number;
  /** Normalized preview of the highest-position evicted terminal with text. */
  terminalPreview: string;
  /** Timeline position of `terminalPreview`'s source row. */
  terminalTurnIndex: number;
  terminalItemIndex: number;
}

/** Cache-snapshot shape — plain data so thread switch can carry folds. */
export interface SubagentFoldSnapshot {
  anchors: Array<{
    anchorId: string;
    evictedIds: string[];
    terminalPreview: string;
    terminalTurnIndex: number;
    terminalItemIndex: number;
  }>;
}

interface AnchorFold {
  evictedIds: Set<string>;
  terminalPreview: string;
  terminalTurnIndex: number;
  terminalItemIndex: number;
  /**
   * Memoized `aggregate()` result, cleared by every mutation of this fold.
   * The grouping pipeline reads aggregates once per card per projection
   * pass (~10Hz while streaming), and a stable reference is what lets the
   * card-node cache in `subagentGrouping.ts` ref-compare its fold input
   * instead of comparing fields — a fresh object per call would both churn
   * and defeat that compare.
   */
  built: SubagentFoldAggregate | null;
}

export interface SubagentFoldRegistry {
  /**
   * Fold a terminal child that is leaving (or never entering) pane memory.
   * Returns whether the id was newly recorded; false means it was
   * already folded (idempotence guard — never double-counts).
   */
  recordEvicted(anchorId: string, item: Item, preview: string): boolean;
  /** True when the id is folded under any anchor (replay / re-insert guard). */
  isEvicted(itemId: string): boolean;
  /** Hydration merged these rows back into pane memory — stop counting them. */
  reclaim(itemIds: Iterable<string>): void;
  /** Aggregate for one anchor, or undefined when nothing is folded. */
  aggregate(anchorId: string): SubagentFoldAggregate | undefined;
  /** Drop one anchor's fold (anchor row removed by revert / prune). */
  dropAnchor(anchorId: string): void;
  /** Drop folds whose anchor no longer satisfies `keep` (post-prune sweep). */
  retainAnchors(keep: (anchorId: string) => boolean): void;
  clear(): void;
  /** Plain-data copy for the thread-switch snapshot cache. */
  snapshot(): SubagentFoldSnapshot | null;
  /** Replace contents from a cached snapshot (thread re-entry). */
  restore(snapshot: SubagentFoldSnapshot | null | undefined): void;
}

export function createSubagentFoldRegistry(): SubagentFoldRegistry {
  const byAnchor = new Map<string, AnchorFold>();
  const anchorByEvictedId = new Map<string, string>();

  function dropAnchorFold(anchorId: string): void {
    const fold = byAnchor.get(anchorId);
    if (!fold) return;
    for (const id of fold.evictedIds) anchorByEvictedId.delete(id);
    byAnchor.delete(anchorId);
  }

  function foldFor(anchorId: string): AnchorFold {
    let fold = byAnchor.get(anchorId);
    if (!fold) {
      fold = {
        evictedIds: new Set(),
        terminalPreview: '',
        terminalTurnIndex: -1,
        terminalItemIndex: -1,
        built: null,
      };
      byAnchor.set(anchorId, fold);
    }
    return fold;
  }

  function positionAtOrAfter(fold: AnchorFold, item: Item): boolean {
    if (item.turnIndex !== fold.terminalTurnIndex) {
      return item.turnIndex > fold.terminalTurnIndex;
    }
    return item.itemIndex >= fold.terminalItemIndex;
  }

  return {
    recordEvicted(anchorId, item, preview) {
      if (anchorByEvictedId.has(item.id)) return false;
      const fold = foldFor(anchorId);
      fold.evictedIds.add(item.id);
      fold.built = null;
      anchorByEvictedId.set(item.id, anchorId);
      if (preview && positionAtOrAfter(fold, item)) {
        fold.terminalPreview = preview;
        fold.terminalTurnIndex = item.turnIndex;
        fold.terminalItemIndex = item.itemIndex;
      }
      return true;
    },

    isEvicted(itemId) {
      return anchorByEvictedId.has(itemId);
    },

    reclaim(itemIds) {
      for (const id of itemIds) {
        const anchorId = anchorByEvictedId.get(id);
        if (anchorId === undefined) continue;
        anchorByEvictedId.delete(id);
        const fold = byAnchor.get(anchorId);
        if (!fold) continue;
        fold.evictedIds.delete(id);
        fold.built = null;
        // The preview intentionally survives reclaim: the rows are back in
        // memory, so the grouping's loaded-children preview wins by
        // position, and an empty fold is dropped wholesale below.
        if (fold.evictedIds.size === 0) byAnchor.delete(anchorId);
      }
    },

    aggregate(anchorId) {
      const fold = byAnchor.get(anchorId);
      if (!fold || fold.evictedIds.size === 0) return undefined;
      fold.built ??= {
        evictedCount: fold.evictedIds.size,
        terminalPreview: fold.terminalPreview,
        terminalTurnIndex: fold.terminalTurnIndex,
        terminalItemIndex: fold.terminalItemIndex,
      };
      return fold.built;
    },

    dropAnchor: dropAnchorFold,

    retainAnchors(keep) {
      // Deleting the current entry during Map iteration is
      // spec-guaranteed safe; no key snapshot needed.
      for (const anchorId of byAnchor.keys()) {
        if (!keep(anchorId)) dropAnchorFold(anchorId);
      }
    },

    clear() {
      byAnchor.clear();
      anchorByEvictedId.clear();
    },

    snapshot() {
      if (byAnchor.size === 0) return null;
      const anchors: SubagentFoldSnapshot['anchors'] = [];
      for (const [anchorId, fold] of byAnchor) {
        anchors.push({
          anchorId,
          evictedIds: [...fold.evictedIds],
          terminalPreview: fold.terminalPreview,
          terminalTurnIndex: fold.terminalTurnIndex,
          terminalItemIndex: fold.terminalItemIndex,
        });
      }
      return { anchors };
    },

    restore(snapshot) {
      byAnchor.clear();
      anchorByEvictedId.clear();
      if (!snapshot) return;
      for (const entry of snapshot.anchors) {
        const fold: AnchorFold = {
          evictedIds: new Set(entry.evictedIds),
          terminalPreview: entry.terminalPreview,
          terminalTurnIndex: entry.terminalTurnIndex,
          terminalItemIndex: entry.terminalItemIndex,
          built: null,
        };
        byAnchor.set(entry.anchorId, fold);
        for (const id of fold.evictedIds) anchorByEvictedId.set(id, entry.anchorId);
      }
    },
  };
}
