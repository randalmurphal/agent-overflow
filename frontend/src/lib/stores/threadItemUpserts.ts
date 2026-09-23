import type { Item } from '../types/models';
import { rowUiRetentionChanged } from '../utils/rowUiRetention';
import { activityRunSummaryFieldsChanged } from '../utils/activityRunGrouping';
import { itemTimelineStructureChanged } from '../utils/timelineStructure';
import { userMessageIdentity } from '../utils/userMessageIdentity';
import { adoptRevIfEqual, compareItemsByTimelinePosition, compareItemToCursor, cursorsAfterItemUpserts, isItemStatusRegression, type TimelineCursorLike } from './threadItems';

export interface ApplyItemUpsertsToWindowOptions {
  current: readonly Item[];
  incoming: readonly Item[];
  itemIndexById: ReadonlyMap<string, number>;
  optimisticItemIds?: ReadonlySet<string>;
  currentThreadId: string | null;
  scopeRootId?: string;
  oldestLoadedCursor?: TimelineCursorLike | null;
  newestLoadedCursor?: TimelineCursorLike | null;
  oldestLoadedTurnIndex?: number | null;
  newestLoadedTurnIndex?: number | null;
  hasMoreHistory?: boolean;
  hasMoreNewer: boolean;
  /**
   * The activity run whose UNSHIPPED region covers a pushed row's
   * coordinate, or null — `threadActivityRuns.runCoveringUnshipped`.
   *
   * A history page ships only a window of each run's members
   * (docs/architecture/timeline-window-pages.md §6), so a row can land
   * inside the loaded window's coordinate range and still belong to a
   * part of a run the pane does not hold. Inserting it would put a row
   * next to members it is not adjacent to and make the run's loaded span
   * discontiguous, which every count on the record is stated against.
   * Such a row is refused and its run marked dirty instead; the debounced
   * stub refresh restates the run and the reader sees the new member in
   * the boundary's count.
   *
   * Omitted by callers with no registry (tests, the agent-scope view),
   * which reads as "no run covers anything".
   */
  runCoveringUnshipped?: (item: Item) => string | null;
}

/** The batch's last write to a row the window already held. */
export interface ItemRowWrite {
  /** The row's index in `current`. */
  index: number;
  /** The row `current` holds at `index`. */
  previous: Item;
  item: Item;
}

export interface ApplyItemUpsertsToWindowResult {
  /**
   * The window after the merge. `current` itself when the batch only
   * rewrote rows in their places: the commit applies `rowWrites` in place
   * and the window keeps its identity. A batch that admits a row, moves
   * one or confirms a provisional send yields a new array.
   */
  items: Item[];
  appendedItems: readonly Item[];
  changedItems: readonly Item[];
  /** Provisional records replaced by the same send's authoritative record. */
  replacedItems: readonly Item[];
  /** One entry per held row the batch wrote, with its last value. */
  rowWrites: readonly ItemRowWrite[];
  /**
   * First index of `items` whose id→index entry moved or is new; the
   * commit rewrites the map from here. `items.length` when no entry moved.
   */
  reindexFrom: number;
  structureChanged: boolean;
  droppedNewerItems: boolean;
  droppedOlderItems?: boolean;
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
   * Run record keys of rows refused because they fell inside a held run's
   * unshipped region. The caller marks each dirty
   * (`threadActivityRuns.markRunDirty`), which schedules the stub
   * refresh that restates the run.
   */
  dirtiedRunKeys: readonly string[];
}

/** Shared empty list, so the overwhelmingly common "nothing moved" batch allocates none. */
const NO_CHANGED_IDS: readonly string[] = Object.freeze([]);
const NO_ITEMS: readonly Item[] = Object.freeze([]);
const NO_ROW_WRITES: readonly ItemRowWrite[] = Object.freeze([]);

/** First index whose row sorts after `item`; rows at its position stay ahead of it. */
function positionUpperBound(rows: readonly Item[], item: Item): number {
  let lo = 0;
  let hi = rows.length;
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (compareItemsByTimelinePosition(rows[mid], item) <= 0) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/**
 * Place admitted rows into the sorted `rows`, in place. Rows sharing a
 * position keep held rows first, then arrival order, which is what a
 * stable sort of the appended array produced. Returns the first index
 * whose row changed.
 */
function placeAdmittedRows(rows: Item[], admitted: readonly Item[]): number {
  let sorted = admitted;
  for (let index = 1; index < admitted.length; index += 1) {
    if (compareItemsByTimelinePosition(admitted[index - 1], admitted[index]) > 0) {
      sorted = [...admitted].sort(compareItemsByTimelinePosition);
      break;
    }
  }
  const from = positionUpperBound(rows, sorted[0]);
  if (from === rows.length) {
    for (const item of sorted) rows.push(item);
    return from;
  }
  const held = rows.slice(from);
  rows.length = from;
  let h = 0;
  let a = 0;
  while (h < held.length && a < sorted.length) {
    if (compareItemsByTimelinePosition(held[h], sorted[a]) <= 0) rows.push(held[h++]);
    else rows.push(sorted[a++]);
  }
  while (h < held.length) rows.push(held[h++]);
  while (a < sorted.length) rows.push(sorted[a++]);
  return from;
}

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
  scopeRootId,
  oldestLoadedCursor,
  newestLoadedCursor,
  oldestLoadedTurnIndex,
  newestLoadedTurnIndex,
  hasMoreHistory,
  hasMoreNewer,
  runCoveringUnshipped,
}: ApplyItemUpsertsToWindowOptions): ApplyItemUpsertsToWindowResult | null {
  if (incoming.length === 0) return null;

  // Nothing is copied while the batch is read: a held row's last write is
  // kept by index, and admitted rows by arrival. Rows admitted in this
  // batch are addressed at `current.length + offset`.
  const writes = new Map<number, Item>();
  const admitted: Item[] = [];
  const batchIndexById = new Map<string, number>();
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
  let heldRowMoved = false;
  let firstConfirmedIndex = current.length;
  let structureChanged = false;
  let droppedNewerItems = false;
  let droppedOlderItems = false;
  let retentionChanged = false;
  let summaryFieldsChangedIds: string[] | null = null;
  let dirtiedRunKeys: Set<string> | null = null;
  // MIN_SAFE_INTEGER, not 0: head-healed prompts sit at NEGATIVE item
  // indexes, so 0 is not the start of a turn — a fallback floor at 0
  // would misclassify those rows as below the loaded window (mirror of
  // the ceiling's MAX_SAFE_INTEGER).
  // A batch can shift its loaded boundary and insert a row into the newly
  // covered coordinates. Resolve anchors before admission, independently of
  // event ordering, so that inserted row is not refused by the old bound.
  const moved = cursorsAfterItemUpserts(
    oldestLoadedCursor, newestLoadedCursor, current, incoming, currentThreadId,
    (item) => (item.parentId ?? '') === (scopeRootId ?? ''),
  );
  const floorCursor = moved.oldest
    ?? (oldestLoadedTurnIndex === null || oldestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: oldestLoadedTurnIndex, itemIndex: Number.MIN_SAFE_INTEGER });
  const ceilingCursor = moved.newest
    ?? (newestLoadedTurnIndex === null || newestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: newestLoadedTurnIndex, itemIndex: Number.MAX_SAFE_INTEGER });

  const rowAt = (index: number): Item | undefined => (index < current.length
    ? writes.get(index) ?? current[index]
    : admitted[index - current.length]);

  for (const item of incoming) {
    if (currentThreadId !== null && item.threadId !== currentThreadId) continue;
    // Only rows of this window's scope are admitted; the caller routes
    // subagent children elsewhere before calling.
    if ((item.parentId ?? '') !== (scopeRootId ?? '')) continue;

    const identity = optimisticIndexByIdentity.size > 0 ? userMessageIdentity(item) : null;
    const existingIndex = batchIndexById.get(item.id) ?? itemIndexById.get(item.id)
      ?? (identity === null ? undefined : optimisticIndexByIdentity.get(identity));
    if (existingIndex !== undefined) {
      const previous = rowAt(existingIndex);
      if (!previous || isItemStatusRegression(previous, item)) continue;
      // No-op dedupe: if the backend re-emits an upsert with identical
      // content, skip the write. Otherwise every redundant upsert
      // produces a new row reference, which cascades through
      // `groupedNodes`, the Virtualizer's `data` prop, and the mounted
      // row components, which showed as a 103 px row oscillation every
      // ~115 ms in plan-ready threads. See `itemsAreEqual` for the fields
      // compared. The skip MUTATES the held row's `rev` in place to the
      // incoming one: "identical content" means identical to a reader,
      // and the revision has no reader.
      if (adoptRevIfEqual(previous, item)) continue;
      if (previous.id !== item.id) {
        if (!optimisticItemIds?.has(previous.id) || userMessageIdentity(previous) !== identity) {
          throw new Error(`Conflicting confirmation ids for send: ${identity}`);
        }
        replacedItems.push(previous);
        batchIndexById.set(item.id, existingIndex);
        firstConfirmedIndex = Math.min(firstConfirmedIndex, existingIndex);
      }
      if (existingIndex < current.length) {
        writes.set(existingIndex, item);
        if (compareItemsByTimelinePosition(previous, item) !== 0) heldRowMoved = true;
      } else {
        admitted[existingIndex - current.length] = item;
      }
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
      continue;
    }

    if (
      floorCursor
      && compareItemToCursor(item, floorCursor) < 0
      && (scopeRootId !== undefined || item.turnIndex < floorCursor.turnIndex || hasMoreHistory === true)
    ) {
      droppedOlderItems = hasMoreHistory !== true;
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

    // A new row inside a held run's unshipped region is not a row this
    // window can hold: see `runCoveringUnshipped`. Checked after the
    // floor/ceiling filters, so a row those already refused costs no
    // lookup. Rows at or past the newest edge are outside every run's
    // range and append exactly as before.
    if (runCoveringUnshipped) {
      const runKey = runCoveringUnshipped(item);
      if (runKey !== null) {
        (dirtiedRunKeys ??= new Set()).add(runKey);
        continue;
      }
    }

    batchIndexById.set(item.id, current.length + admitted.length);
    admitted.push(item);
    changed = true;
    structureChanged = true;
    if (rowUiRetentionChanged(undefined, item)) {
      retentionChanged = true;
    }
    changedItems.push(item);
  }

  if (
    !changed
    && !droppedNewerItems
    && !droppedOlderItems
    && dirtiedRunKeys === null
  ) {
    return null;
  }
  const rowWrites: ItemRowWrite[] = [];
  for (const [index, item] of writes) rowWrites.push({ index, previous: current[index], item });
  // Rows rewritten where they stand keep the window's identity; the commit
  // writes them in place.
  let items = current as Item[];
  let reindexFrom = current.length;
  if (admitted.length > 0 || heldRowMoved || replacedItems.length > 0) {
    items = current.slice();
    for (const [index, item] of writes) items[index] = item;
    reindexFrom = firstConfirmedIndex;
    if (heldRowMoved) {
      // A held row changed position, which only corrections do: re-sort
      // the whole window rather than track the move.
      for (const item of admitted) items.push(item);
      items.sort(compareItemsByTimelinePosition);
      reindexFrom = 0;
    } else if (admitted.length > 0) {
      reindexFrom = Math.min(reindexFrom, placeAdmittedRows(items, admitted));
    }
  }
  return {
    items,
    appendedItems: admitted.length > 0 ? admitted : NO_ITEMS,
    changedItems,
    replacedItems,
    rowWrites: rowWrites.length > 0 ? rowWrites : NO_ROW_WRITES,
    reindexFrom,
    structureChanged,
    droppedNewerItems,
    droppedOlderItems,
    rowUiRetentionChanged: retentionChanged,
    summaryFieldsChangedIds: summaryFieldsChangedIds ?? NO_CHANGED_IDS,
    dirtiedRunKeys: dirtiedRunKeys ? [...dirtiedRunKeys] : NO_CHANGED_IDS,
  };
}
