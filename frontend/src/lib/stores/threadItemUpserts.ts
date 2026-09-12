import type { Item } from '../types/models';
import { rowUiRetentionChanged } from '../utils/rowUiRetention';
import { activityRunSummaryFieldsChanged } from '../utils/activityRunGrouping';
import { itemTimelineStructureChanged } from '../utils/timelineStructure';
import { userMessageIdentity } from '../utils/userMessageIdentity';
import { compareItemsByTimelinePosition, compareItemToCursor, cursorsAfterItemUpserts, isItemStatusRegression, itemsAreEqual, type TimelineCursorLike } from './threadItems';

export interface ApplyItemUpsertsToWindowOptions {
  current: readonly Item[];
  incoming: readonly Item[];
  itemIndexById: ReadonlyMap<string, number>;
  optimisticItemIds?: ReadonlySet<string>;
  currentThreadId: string | null;
  oldestLoadedCursor?: TimelineCursorLike | null;
  newestLoadedCursor?: TimelineCursorLike | null;
  oldestLoadedTurnIndex?: number | null;
  newestLoadedTurnIndex?: number | null;
  hasMoreHistory?: boolean;
  hasMoreNewer: boolean;
}

export interface ApplyItemUpsertsToWindowResult {
  items: Item[];
  appendedItems: readonly Item[];
  changedItems: readonly Item[];
  /** Provisional records replaced by the same send's authoritative record. */
  replacedItems: readonly Item[];
  indexesNeedRebuild: boolean;
  structureChanged: boolean;
  droppedNewerItems: boolean;
  /**
   * Any applied row changed what the offscreen row-UI prune retains
   * (`utils/rowUiRetention.ts`). Computed here because the merge is the
   * one place that holds both the previous row and its replacement; the
   * pane turns it into a revision the prune's no-op bail reads as a
   * scalar. Structure-independent by construction: a streaming row
   * settling changes retention without changing structure, and a
   * regrouping change moves structure without touching retention.
   */
  rowUiRetentionChanged: boolean;
  /**
   * Ids of REPLACED rows whose activity-run summary fields moved
   * (`utils/activityRunGrouping.ts`). Same reason as above — the merge is
   * the one place holding both versions — and replacements only: an
   * appended row always sets `structureChanged`, which re-projects the
   * runs and stamps a fresh membership epoch on the node.
   */
  summaryFieldsChangedIds: readonly string[];
  /**
   * NEW parented rows refused because their anchor is not loadable in
   * this window — neither already loaded nor landed earlier in the same
   * batch. Deciding this inside the merge, after the floor/ceiling
   * filters, is what makes the no-orphan contract airtight: a pre-filter
   * that vouched for a same-batch anchor could disagree with the filter
   * that then strips that anchor (below the floor after a prune),
   * landing the child as an unreachable orphan row. The caller swallows
   * these (`threadSubagentMemory.recordAdmission`) — SQLite holds the
   * canonical rows, and hydration renders them once the anchor is back.
   */
  rejectedParentedItems: readonly Item[];
}

/** Shared empty list, so the overwhelmingly common "nothing moved" batch allocates none. */
const NO_CHANGED_IDS: readonly string[] = Object.freeze([]);
/** Shared empty list, so batches with no refused parented rows allocate none. */
const NO_REJECTED_ITEMS: readonly Item[] = Object.freeze([]);

/**
 * Apply streamed/upserted items to the currently loaded timeline window.
 * Existing rows always win the floor guard so corrections to in-window rows
 * are not dropped; new rows below the loaded floor stay in SQLite until the
 * user pages that part of history in.
 */
export function applyItemUpsertsToWindow({
  current,
  incoming,
  itemIndexById,
  optimisticItemIds,
  currentThreadId,
  oldestLoadedCursor,
  newestLoadedCursor,
  oldestLoadedTurnIndex,
  newestLoadedTurnIndex,
  hasMoreHistory,
  hasMoreNewer,
}: ApplyItemUpsertsToWindowOptions): ApplyItemUpsertsToWindowResult | null {
  if (incoming.length === 0) return null;

  let next: Item[] | null = null;
  const batchIndexById = new Map<string, number>();
  const appendedItems: Item[] = [];
  const changedItems: Item[] = [];
  const replacedItems: Item[] = [];
  const optimisticIndexByIdentity = new Map<string, number>();
  for (const id of optimisticItemIds ?? []) {
    const index = itemIndexById.get(id);
    if (index === undefined) continue;
    const item = current[index];
    if (!item || (currentThreadId !== null && item.threadId !== currentThreadId)) continue;
    const identity = userMessageIdentity(item);
    if (identity === null) continue;
    if (optimisticIndexByIdentity.has(identity)) throw new Error(`Duplicate optimistic send: ${identity}`);
    optimisticIndexByIdentity.set(identity, index);
  }
  let changed = false;
  let needsSort = false;
  let structureChanged = false;
  let droppedNewerItems = false;
  let retentionChanged = false;
  let summaryFieldsChangedIds: string[] | null = null;
  let rejectedParentedItems: Item[] | null = null;
  // MIN_SAFE_INTEGER, not 0: head-healed prompts sit at NEGATIVE item
  // indexes, so 0 is not the start of a turn — a fallback floor at 0
  // would misclassify those rows as below the loaded window (mirror of
  // the ceiling's MAX_SAFE_INTEGER).
  // A batch can shift its loaded boundary and insert a row into the newly
  // covered coordinates. Resolve anchors before admission, independently of
  // event ordering, so that inserted row is not refused by the old bound.
  const moved = cursorsAfterItemUpserts(
    oldestLoadedCursor, newestLoadedCursor, current, incoming, currentThreadId,
  );
  const floorCursor = moved.oldest
    ?? (oldestLoadedTurnIndex === null || oldestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: oldestLoadedTurnIndex, itemIndex: Number.MIN_SAFE_INTEGER });
  const ceilingCursor = moved.newest
    ?? (newestLoadedTurnIndex === null || newestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: newestLoadedTurnIndex, itemIndex: Number.MAX_SAFE_INTEGER });

  const workingItems = (): Item[] => {
    if (next === null) next = current.slice();
    return next;
  };

  for (const item of incoming) {
    if (currentThreadId !== null && item.threadId !== currentThreadId) continue;

    const identity = optimisticIndexByIdentity.size > 0 ? userMessageIdentity(item) : null;
    const existingIndex = batchIndexById.get(item.id) ?? itemIndexById.get(item.id)
      ?? (identity === null ? undefined : optimisticIndexByIdentity.get(identity));
    if (existingIndex !== undefined) {
      const previous = (next ?? current)[existingIndex];
      if (!previous || isItemStatusRegression(previous, item)) continue;
      // No-op dedupe: if the backend re-emits an upsert with identical
      // content, skip the array replace. Otherwise every redundant
      // upsert produces a new `pane.items` reference, which cascades
      // through `groupedNodes`, the Virtualizer's `data` prop, and the
      // mounted row components — observed as a 103 px row oscillation
      // every ~115 ms in plan-ready threads. See `itemsAreEqual` for
      // the fields compared.
      if (itemsAreEqual(previous, item)) continue;
      if (previous.id !== item.id) {
        if (!optimisticItemIds?.has(previous.id) || userMessageIdentity(previous) !== identity) {
          throw new Error(`Conflicting confirmation ids for send: ${identity}`);
        }
        replacedItems.push(previous);
        batchIndexById.set(item.id, existingIndex);
      }
      workingItems()[existingIndex] = item;
      changed = true;
      if (itemTimelineStructureChanged(previous, item)) {
        structureChanged = true;
      }
      if (rowUiRetentionChanged(previous, item)) {
        retentionChanged = true;
      }
      if (activityRunSummaryFieldsChanged(previous, item)) {
        (summaryFieldsChangedIds ??= []).push(item.id);
      }
      changedItems.push(item);
      if (batchIndexById.has(item.id) && existingIndex >= current.length) {
        const appendedOffset = existingIndex - current.length;
        appendedItems[appendedOffset] = item;
      }
      if (compareItemsByTimelinePosition(previous, item) !== 0) {
        needsSort = true;
      }
      continue;
    }

    if (
      floorCursor
      && compareItemToCursor(item, floorCursor) < 0
      && (item.turnIndex < floorCursor.turnIndex || hasMoreHistory === true)
    ) {
      continue;
    }

    if (
      ceilingCursor !== null
      && hasMoreNewer
      && compareItemToCursor(item, ceilingCursor) > 0
    ) {
      droppedNewerItems = true;
      continue;
    }

    // Admission for new subagent children, checked against what actually
    // landed (batchIndexById excludes floor/ceiling-refused rows, and
    // a rejected anchor never enters it, so grandchildren are refused
    // transitively). Wire order puts a parent's upsert before its
    // children's, so a same-batch anchor is always decided first.
    const parentId = item.parentId ?? '';
    if (
      parentId
      && itemIndexById.get(parentId) === undefined
      && !batchIndexById.has(parentId)
    ) {
      (rejectedParentedItems ??= []).push(item);
      continue;
    }

    const source = next ?? current;
    const previousTail = source.at(-1);
    if (previousTail && compareItemsByTimelinePosition(previousTail, item) > 0) {
      needsSort = true;
    }
    const target = workingItems();
    batchIndexById.set(item.id, target.length);
    target.push(item);
    changed = true;
    structureChanged = true;
    if (rowUiRetentionChanged(undefined, item)) {
      retentionChanged = true;
    }
    appendedItems.push(item);
    changedItems.push(item);
  }

  if (!changed && !droppedNewerItems && rejectedParentedItems === null) {
    return null;
  }
  if (!changed) {
    return {
      items: current as Item[],
      appendedItems,
      changedItems,
      replacedItems,
      indexesNeedRebuild: false,
      structureChanged: false,
      droppedNewerItems,
      rowUiRetentionChanged: false,
      summaryFieldsChangedIds: NO_CHANGED_IDS,
      rejectedParentedItems: rejectedParentedItems ?? NO_REJECTED_ITEMS,
    };
  }
  const result = next ?? current.slice();

  if (needsSort) {
    result.sort(compareItemsByTimelinePosition);
  }
  return {
    items: result,
    appendedItems,
    changedItems,
    replacedItems,
    indexesNeedRebuild: needsSort || replacedItems.length > 0,
    structureChanged,
    droppedNewerItems,
    rowUiRetentionChanged: retentionChanged,
    summaryFieldsChangedIds: summaryFieldsChangedIds ?? NO_CHANGED_IDS,
    rejectedParentedItems: rejectedParentedItems ?? NO_REJECTED_ITEMS,
  };
}
