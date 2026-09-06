// stores/threadItemWindow.svelte.ts
//
// OWNS the pane's loaded item window and every write to it: the `items`
// array, the id→index map, the per-row signal boxes, the two structural
// revisions derived from row fields, and the four commit chokepoints
// (`replaceTimelineItems`, `installTimelineItems`, `dropTimelineItems`,
// `commitUpsertResult`) plus the two single-row writers (`writeItemAt`,
// `appendDirectAssistantLiteral`). Nothing outside this module assigns
// `items`, which is what lets the reconciliation, disposal and gate
// finalization live at the writes instead of at every caller.
//
// MUST NOT decide row TEXT, own history paging, or know about threads.
// The text of a wholesale replacement is decided by
// `threadStreamingReveal.svelte.ts`'s chokepoint, which this module calls;
// cursors and the load methods belong to `threadTimelineWindow.svelte.ts`;
// removals that are ABOUT a thread (cache eviction on revert) stay on the
// pane. The collaborators arrive as lazy getters because they are
// constructed after this module and several of them take its commit
// entry points in their own option bags.

import { matchesProvenAppend, type ProvenAppend } from '../markdown';
import type { Item } from '../types/models';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { rowUiRetentionChanged } from '../utils/rowUiRetention';
import { activityRunSummaryFieldsChanged } from '../utils/activityRunGrouping';
import type { ApplyItemUpsertsToWindowResult } from './threadItems';
import type { ThreadStreamingReveal } from './threadStreamingReveal.svelte';
import type { ThreadRowUiState } from './threadRowUiState.svelte';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { ThreadSubagentMemory } from './threadSubagentMemory';
import type { ThreadSwitchLoad } from './threadSwitchLoad.svelte';

/** Shared "nothing was dropped" list, so the common replacement allocates none. */
const NO_ITEMS: readonly Item[] = Object.freeze([]);
/** Shared empty error list for the successful commit path. */
const NO_ERRORS: readonly unknown[] = Object.freeze([]);

export interface TimelineCommitOptions {
  disposeDropped?: boolean;
  exhaustedScope?: ReadonlySet<string>;
  afterCommit?: () => void;
}

/**
 * The collaborators a commit has to notify. Each arrives as a getter
 * because every one of them is constructed AFTER the window (three of them
 * take its commit entry points), and none is read during construction.
 */
export interface ThreadItemWindowOptions {
  streamingReveal(): ThreadStreamingReveal;
  rowUiState(): ThreadRowUiState;
  activityRuns(): ThreadActivityRuns;
  subagentMemory(): ThreadSubagentMemory;
  switchLoad(): ThreadSwitchLoad;
}

export function createThreadItemWindow(options: ThreadItemWindowOptions) {
  // `$state.raw`, not `$state`: the window is replaced wholesale on every
  // upsert batch, and a deep proxy re-minted a source per index and per
  // item field on every read after each replacement (9.9MB/min of proxy
  // `get` allocation in the 2026-08-23 profile) — and because the nested
  // Item proxies were new each time, every mounted row's `displayItem`
  // changed identity on every batch and re-derived whether or not its row
  // had been written. Row-level reactivity comes from `itemBoxes` instead:
  // one `$state.raw` box per LOADED item id, written at the same
  // chokepoints that write `items`, so a row re-derives only when its own
  // row is written. The array signal itself fires on replacement only
  // (structure, and the batch commit); an in-place `writeItemAt` is
  // silent at the array and loud at the row's box.
  let items: Item[] = $state.raw([]);
  // Structural revision for timeline projections that should skip
  // summary-only streaming deltas. Bump whenever the item window's array
  // changes shape or identity; `applyItemDelta` intentionally does not bump.
  let timelineRevision = $state(0);
  // Revision of the item-side inputs to offscreen row-UI-state retention
  // (`utils/rowUiRetention.ts`): bumped by an items write only when it
  // changed which rows the prune retains unconditionally, or what it
  // retains for one. The prune's no-op bail reads it as a scalar instead
  // of walking `items` per callback — that walk wedged the renderer for
  // 6-19s mid-turn while `items` was a deep `$state` array (replaced on
  // every upsert batch, each walk re-created a proxy source per index).
  // `items` is `$state.raw` now, but the walk is still O(window) per
  // callback and stays off the hot path.
  //
  // Deliberately NOT `$state`, same reason as the pane's
  // `lastLiveContentAt`: the only reader is the quiet scheduler's prune
  // pass, which runs off a microtask/timer and reads imperatively.
  // Scheduling is a separate concern and stays on `timelineRevision` +
  // the other structural triggers; this value only decides whether a
  // scheduled pass is a no-op.
  let rowUiRetentionRevision = 0;
  const itemIndexById: Map<string, number> = new Map();
  // Invariant: a box exists for exactly the ids in `items`. `writeItemAt`
  // and the two commit chokepoints are the only writers; `syncItemBoxes`
  // is the only place a box is dropped. A box-less id is "not loaded",
  // and a reactive reader of one tracks the registry's creation version
  // so it wakes when that row lands.
  const itemBoxes = createKeyedSignalRegistry<Item | undefined>(undefined);

  function getItems(): Item[] {
    return items;
  }

  function getItemById(itemId: string): Item | undefined {
    return itemBoxes.get(itemId);
  }

  /** Wholesale replacement: box every surviving row, drop every lost one. */
  function syncItemBoxes(previous: readonly Item[], nextItems: readonly Item[]): void {
    for (const item of nextItems) itemBoxes.set(item.id, item);
    for (const item of previous) {
      if (!itemIndexById.has(item.id)) itemBoxes.drop(item.id);
    }
  }

  /**
   * Reset masked parser checkpoints before a wholesale row replacement becomes
   * observable. Every affected row is attempted even when one sink reports a
   * reset failure, then the commit is refused so `items`, indexes, and boxes
   * cannot describe different windows.
   */
  function reconcileItemReplacements(
    previous: readonly Item[],
    nextItems: readonly Item[],
  ): void {
    const streamingReveal = options.streamingReveal();
    const errors: unknown[] = [];
    for (const item of nextItems) {
      const previousIndex = itemIndexById.get(item.id);
      if (previousIndex === undefined) continue;
      const prior = previous[previousIndex];
      if (!prior || prior.id !== item.id) {
        errors.push(new Error(`timeline item index is stale for ${item.id}`));
        continue;
      }
      try {
        streamingReveal.reconcileItemWrite(prior, item);
      } catch (error) {
        errors.push(error);
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, 'timeline item replacement reconciliation failed');
    }
  }

  /**
   * The one reactive in-place row write. Every path that replaces a
   * single loaded row (authoritative smoother reveal, delta, meta, field
   * patch) goes through here. Preflighted literal assistant suffixes use the
   * narrow quiet writer below. A new caller cannot miss revisions derived
   * from row fields because the bump belongs to the write, not to each writer.
   * Both revisions keep an O(window) walk off a ~50Hz path. They cover
   * the offscreen row-UI prune's no-op bail and the activity-run header's
   * summary signature. This function decides both from the comparison it
   * already holds.
   *
   * Wholesale replacements go through `commitTimelineItems` instead,
   * which bumps retention unconditionally; a run's membership change
   * there is re-stamped by the projection's own epoch.
   */
  function writeItemAt(index: number, next: Item): void {
    if (!Number.isInteger(index) || index < 0 || index >= items.length) {
      throw new RangeError(`timeline item write index ${index} is outside the loaded window`);
    }
    const previous = items[index];
    if (previous.id !== next.id) {
      throw new Error(
        `timeline item write cannot replace ${previous.id} with ${next.id} at index ${index}`,
      );
    }
    options.streamingReveal().reconcileItemWrite(previous, next);
    if (rowUiRetentionChanged(previous, next)) rowUiRetentionRevision += 1;
    const errors: unknown[] = [];
    // Same chokepoint logic for the activity-run header: it summarises the
    // rows in a run from five fields, and this is the write that fires at
    // reveal cadence. Comparing them here is what lets the header key on a
    // number instead of rebuilding the tuple for every member per tick.
    if (activityRunSummaryFieldsChanged(previous, next)) {
      try {
        options.activityRuns().noteMemberContentChanged(next.id);
      } catch (error) {
        errors.push(error);
      }
    }
    items[index] = next;
    try {
      itemBoxes.set(next.id, next);
    } catch (error) {
      errors.push(error);
    }
    try {
      options.switchLoad().noteItemMutation(next.id);
    } catch (error) {
      errors.push(error);
    }
    if (errors.length > 0) {
      throw new AggregateError(
        errors,
        `timeline item write finalization failed for ${next.id}`,
      );
    }
  }

  /**
   * Direct literal reveal keeps the canonical raw row current while every
   * mounted representation paints the same suffix. The reveal router is the
   * only caller and passes the opaque append proof minted for that suffix.
   * Verifying the proof keeps misuse impossible without a startsWith scan:
   * V8 can flatten the growing cons string and copy the whole message on every
   * reveal when code inspects its prefix.
   */
  function appendDirectAssistantLiteral(
    index: number,
    itemId: string,
    append: ProvenAppend,
    updatedAt: number,
  ): void {
    if (!Number.isInteger(index) || index < 0 || index >= items.length) {
      throw new RangeError(`direct assistant reveal index ${index} is outside the loaded window`);
    }
    const current = items[index];
    if (!current || current.id !== itemId) {
      throw new Error(`direct assistant reveal lost item ${itemId} at index ${index}`);
    }
    if (
      current.kind !== 'assistant_text' ||
      !matchesProvenAppend(append, current.summary, append.next)
    ) {
      throw new Error(`invalid direct assistant reveal for ${itemId}`);
    }
    if (itemBoxes.get(itemId) !== current) {
      throw new Error(`direct assistant reveal lost the canonical row box for ${itemId}`);
    }
    // Stamp first. If conflict tracking ever fails, the canonical row must
    // remain at the source the router still knows how to render.
    options.switchLoad().noteItemMutation(itemId);
    current.summary = append.next;
    current.updatedAt = Math.max(updatedAt, current.updatedAt);
  }

  function rebuildItemIndexes(nextItems: Item[]): void {
    itemIndexById.clear();
    for (let index = 0; index < nextItems.length; index += 1) {
      const item = nextItems[index];
      itemIndexById.set(item.id, index);
    }
  }

  function disposeDroppedItemState(
    droppedItems: readonly Item[],
    exhaustedScope?: ReadonlySet<string>,
  ): void {
    if (droppedItems.length === 0) return;
    // Dropped rows can include hydrated subagent children — re-arm their
    // anchors for hydration. See threadSubagentMemory.ts
    // `resetHydrationExhausted` for the full rationale.
    const errors: unknown[] = [];
    try {
      options.subagentMemory().resetHydrationExhausted(exhaustedScope);
    } catch (error) {
      errors.push(error);
    }
    try {
      options.streamingReveal().disposeSmoothersForItems(droppedItems);
    } catch (error) {
      errors.push(error);
    }
    try {
      options.rowUiState().disposeItems(droppedItems);
    } catch (error) {
      errors.push(error);
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, 'dropped timeline item disposal failed');
    }
  }

  /**
   * Complete an item-window commit before control returns to its caller.
   * `afterCommit` owns domain work that must see the newly installed window;
   * the enclosing withReconciledItems operation derives the reveal gate after
   * this finalizer, including when preparation or post-commit work throws.
   */
  function finalizeItemsCommit<T>(
    context: string,
    afterCommit: ((committed: T) => void) | undefined,
    committed: T,
    priorErrors: readonly unknown[] = NO_ERRORS,
  ): void {
    let errors: unknown[] | null =
      priorErrors.length > 0 ? [...priorErrors] : null;
    if (afterCommit) {
      try {
        afterCommit(committed);
      } catch (error) {
        (errors ??= []).push(error);
      }
    }
    if (errors) {
      throw new AggregateError(errors, `${context} finalization failed`);
    }
  }

  /** Set difference, for the callers that hand over a finished array. */
  function droppedItemsBetween(
    previous: readonly Item[],
    nextItems: readonly Item[],
  ): readonly Item[] {
    if (previous.length === 0) return NO_ITEMS;
    const keptIds = new Set<string>();
    for (const item of nextItems) keptIds.add(item.id);
    const dropped: Item[] = [];
    for (const item of previous) {
      if (!keptIds.has(item.id)) dropped.push(item);
    }
    return dropped;
  }

  /**
   * The window-replacement chokepoint. `droppedItems` must be exactly the
   * rows `nextItems` lost, which is why this is private: the two public
   * entry points below each derive it, so no caller can supply a pair
   * that disagrees (a short list leaks row UI state; a long one releases
   * state a surviving row still reads).
   */
  interface TimelineItemsCommitOptions {
    exhaustedScope?: ReadonlySet<string>;
    recordLiveReplacement?: boolean;
    afterCommit?: () => void;
  }

  function commitTimelineItems(
    nextItems: Item[],
    droppedItems: readonly Item[],
    commitOptions: TimelineItemsCommitOptions = {},
  ): boolean {
    return options.streamingReveal().withReconciledItems(nextItems, (nextItems) => {
      const previous = items;
      reconcileItemReplacements(previous, nextItems);
      items = nextItems;
      const errors: unknown[] = [];
      if (commitOptions.recordLiveReplacement) {
        try {
          options.switchLoad().noteItemWindowReplacement(previous, nextItems);
        } catch (error) {
          errors.push(error);
        }
      }
      // Indexes first: the box sync drops a previous row only when
      // `itemIndexById` no longer knows it.
      rebuildItemIndexes(items);
      syncItemBoxes(previous, items);
      // Fold↔items chokepoint: folds are only meaningful while their
      // anchor row is loaded — once an anchor leaves the window, the
      // next load of its region decorates from SQLite. Every wholesale
      // window replacement (prune, reconcile, revert, cache install,
      // eviction) flows through here, so one sweep after the index
      // rebuild keeps the registry consistent everywhere. The upsert
      // fast path bypasses this function but never drops existing rows.
      // Eviction callers record their folds BEFORE replacing, with the
      // anchors still loaded, so those folds are retained.
      try {
        options.subagentMemory().retainFoldAnchors();
      } catch (error) {
        errors.push(error);
      }
      try {
        disposeDroppedItemState(droppedItems, commitOptions.exhaustedScope);
      } catch (error) {
        errors.push(error);
      }
      timelineRevision++;
      // Unconditional: a wholesale replacement can drop an active row, land
      // one, or re-link a payload, and proving otherwise would cost the very
      // walk the revision exists to remove. These paths are rare (prune,
      // reconcile, revert, cache install, eviction) — one extra prune pass
      // is cheaper than the proof.
      rowUiRetentionRevision += 1;
      // Same reasoning, the other consumer: the per-item revision the
      // activity-run headers key on is fed by `writeItemAt`, which a
      // wholesale replacement does not go through. A replace can change
      // every summary-relevant field on rows whose run membership is
      // untouched (the cache paint reconciled by `SyncThreadWindow`), and
      // that is invisible to both of the header's per-run signals.
      try {
        options.activityRuns().noteWholesaleReplace();
      } catch (error) {
        errors.push(error);
      }
      finalizeItemsCommit(
        'timeline window replacement',
        commitOptions.afterCommit,
        undefined,
        errors,
      );
      return true;
    });
  }

  function replaceTimelineItems(
    nextItems: Item[],
    commitOptions: TimelineCommitOptions = {},
  ): boolean {
    if (items === nextItems) {
      if (commitOptions.afterCommit) {
        options.streamingReveal().withReconciledItems([], () => finalizeItemsCommit(
          'timeline window replacement',
          commitOptions.afterCommit,
          undefined,
        ));
      }
      return false;
    }
    return commitTimelineItems(
      nextItems,
      commitOptions.disposeDropped
        ? droppedItemsBetween(items, nextItems)
        : NO_ITEMS,
      {
        exhaustedScope: commitOptions.exhaustedScope,
        recordLiveReplacement: true,
        afterCommit: commitOptions.afterCommit,
      },
    );
  }

  /**
   * Install a cache/backend snapshot without reporting the snapshot's own
   * changes as mutations that raced it. Only the switch/load pipeline gets
   * this handle. Every external replacement uses replaceTimelineItems above.
   */
  function installTimelineItems(
    nextItems: Item[],
    commitOptions: TimelineCommitOptions = {},
  ): boolean {
    if (items === nextItems) {
      if (commitOptions.afterCommit) {
        options.streamingReveal().withReconciledItems([], () => finalizeItemsCommit(
          'timeline window installation',
          commitOptions.afterCommit,
          undefined,
        ));
      }
      return false;
    }
    return commitTimelineItems(
      nextItems,
      commitOptions.disposeDropped
        ? droppedItemsBetween(items, nextItems)
        : NO_ITEMS,
      {
        exhaustedScope: commitOptions.exhaustedScope,
        afterCommit: commitOptions.afterCommit,
      },
    );
  }

  /**
   * Replace the window by dropping the rows `shouldDrop` selects. ONE
   * pass yields both the surviving array and the dropped rows, where
   * `replaceTimelineItems` has to diff the two arrays afterwards — a
   * second full walk plus a Set of every surviving id. Any caller that
   * already knows which rows are leaving belongs here; subagent
   * eviction, which drops a settled subtree on every settling batch, is
   * why it exists. Returns the dropped rows in their previous order; a
   * no-op drop leaves the window untouched, so it costs no revision
   * bump.
   */
  function dropTimelineItems(
    shouldDrop: (item: Item) => boolean,
    dropOptions: { exhaustedScope?: ReadonlySet<string> } = {},
  ): Item[] {
    const kept: Item[] = [];
    const dropped: Item[] = [];
    for (const item of items) {
      if (shouldDrop(item)) dropped.push(item);
      else kept.push(item);
    }
    if (dropped.length === 0) return dropped;
    commitTimelineItems(kept, dropped, {
      exhaustedScope: dropOptions.exhaustedScope,
      recordLiveReplacement: true,
    });
    return dropped;
  }

  /**
   * The upsert path's commit chokepoint, and the reason
   * `threadItemStreamApply.ts` does not write `items` itself: the merge
   * in `applyItemUpsertsToWindow` never DROPS a row, so unlike
   * `commitTimelineItems` there is nothing to dispose and no fold to
   * retain — but the same three revisions still have to move, and they
   * move from what the merge already computed rather than from a fresh
   * walk. Index maintenance rides along because the result says which
   * of the two shapes it is (full rebuild vs. tail-append patch).
   */
  function commitUpsertResult(
    next: ApplyItemUpsertsToWindowResult,
    afterCommit: (committed: ApplyItemUpsertsToWindowResult) => void,
  ): void {
    const streamingReveal = options.streamingReveal();
    const errors: unknown[] = [];
    for (const changed of next.changedItems) {
      const previousIndex = itemIndexById.get(changed.id);
      if (previousIndex !== undefined) {
        try {
          streamingReveal.reconcileItemWrite(items[previousIndex], changed);
        } catch (error) {
          errors.push(error);
        }
      }
    }
    if (errors.length > 0) {
      throw new AggregateError(errors, 'timeline item upsert reconciliation failed');
    }
    items = next.items;
    try {
      options.switchLoad().noteItemMutations(next.changedItems);
    } catch (error) {
      errors.push(error);
    }
    try {
      if (next.indexesNeedRebuild) {
        rebuildItemIndexes(items);
      } else {
        const firstAppendIndex = items.length - next.appendedItems.length;
        for (let index = 0; index < next.appendedItems.length; index += 1) {
          itemIndexById.set(
            next.appendedItems[index].id,
            firstAppendIndex + index,
          );
        }
      }
    } catch (error) {
      errors.push(error);
    }
    // The merge never drops a row, so there is nothing to un-box;
    // `changedItems` carries the appended rows too.
    for (const item of next.changedItems) {
      try {
        itemBoxes.set(item.id, item);
      } catch (error) {
        errors.push(error);
      }
    }
    if (next.structureChanged) timelineRevision++;
    if (next.rowUiRetentionChanged) rowUiRetentionRevision += 1;
    for (const id of next.summaryFieldsChangedIds) {
      try {
        options.activityRuns().noteMemberContentChanged(id);
      } catch (error) {
        errors.push(error);
      }
    }
    finalizeItemsCommit('timeline item upsert', afterCommit, next, errors);
  }

  return {
    /**
     * The loaded window. Reading it inside a `$derived`/`$effect` tracks the
     * array signal, which fires on replacement only — a single-row write is
     * silent here and loud at that row's box.
     */
    getItems,
    getItemById,
    /**
     * The id→index map itself, not a lookup wrapper: the streaming apply
     * path reads it per row per batch and the reveal router per frame.
     */
    itemIndexById,
    get timelineRevision() {
      return timelineRevision;
    },
    get rowUiRetentionRevision() {
      return rowUiRetentionRevision;
    },
    writeItemAt,
    appendDirectAssistantLiteral,
    replaceTimelineItems,
    installTimelineItems,
    dropTimelineItems,
    commitUpsertResult,
  };
}

export type ThreadItemWindow = ReturnType<typeof createThreadItemWindow>;
