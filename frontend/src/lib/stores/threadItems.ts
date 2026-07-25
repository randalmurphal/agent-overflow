import type { Item } from '../types/models';
import { itemTimelineStructureChanged } from '../utils/timelineStructure';

export interface TimelineCursorLike {
  turnIndex: number;
  itemIndex: number;
  itemId?: string;
}

export function compareItemsByTimelinePosition(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

export function compareCursors(a: TimelineCursorLike, b: TimelineCursorLike): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

export function compareItemToCursor(item: Item, cursor: TimelineCursorLike): number {
  return compareCursors(
    { turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id },
    cursor,
  );
}

export function cursorFromItem(item: Item): TimelineCursorLike {
  return {
    turnIndex: item.turnIndex,
    itemIndex: item.itemIndex,
    itemId: item.id,
  };
}

// Validity keys on turnIndex alone: turn indexes are never negative,
// but item indexes can be (head-healed prompts persist at negative
// indexes), and a page bounded by one must keep paging. The backend's
// empty sentinel is turnIndex -1.
export function cursorIsValid(cursor: TimelineCursorLike | null | undefined): cursor is TimelineCursorLike {
  if (!cursor) return false;
  return Number.isFinite(cursor.turnIndex)
    && Number.isFinite(cursor.itemIndex)
    && cursor.turnIndex >= 0;
}

/**
 * Field-wise equality for two items at the same id. Returns true when the
 * incoming upsert carries no observable change vs. the existing row.
 *
 * Why this exists: the backend can re-emit the same item event with
 * unchanged content (event-loop resyncs, redundant `provider:item_event`
 * upserts on durable-status flag updates, etc.). Each such redundant
 * upsert otherwise replaces the row in `pane.items` with a new object
 * reference, which forces `groupedNodes` to rebuild, which forces
 * `<TimelineVirtualizer data={revealedNodes}>` to re-iterate, which can
 * land as a remount of the affected row's DOM. A row with async render
 * work (DiffFileBlock token dispatch, Streamdown typesetting, etc.) then
 * settles at its measured height again — and the engine compensates
 * scrollTop for the size delta. Observed in plan-ready threads as a row
 * oscillating ±103 px every ~115 ms while the user is trying to scroll.
 *
 * Compare-by-value covers the fields the row renderers actually read.
 * `id`, `threadId`, `turnIndex`, `itemIndex`, `parentId`, `completionOf`,
 * `payloadId`, `inputPayloadId`, `kind`, `role`, `payloadKind`, `toolName`
 * are part of identity / structure and would change the renderer's branch
 * if they differed — included for completeness. `summary`, `status`,
 * `decision`, `payloadMeta`, `meta`, `createdAt`, `updatedAt`,
 * `isBackground` are the streaming / mutable surface. If every one of these
 * matches, the upsert is a true no-op and we can drop it.
 */
export function itemsAreEqual(a: Item, b: Item): boolean {
  return (
    itemsRenderEqual(a, b)
    && a.createdAt === b.createdAt
    && a.updatedAt === b.updatedAt
  );
}

/**
 * `itemsAreEqual` minus the `createdAt` / `updatedAt` timestamps: value
 * equality over every field a row renders or that positions it in the
 * timeline. The events fan-out's spring-latch predicate
 * (`providerUpsertAdvancesLiveContent`) uses THIS check so that an
 * applied upsert stamps the latch exactly when something the user can
 * see changed — a timestamp-only heartbeat bump must not hold the latch
 * open. Keep exhaustive over `Item`: `itemsAreEqual` builds on it, so a
 * field added here is automatically part of both the upsert dedupe and
 * the latch decision.
 */
export function itemsRenderEqual(a: Item, b: Item): boolean {
  return (
    a.id === b.id
    && a.threadId === b.threadId
    && a.turnIndex === b.turnIndex
    && a.itemIndex === b.itemIndex
    && a.kind === b.kind
    && a.role === b.role
    && a.status === b.status
    && a.summary === b.summary
    && a.payloadId === b.payloadId
    && a.inputPayloadId === b.inputPayloadId
    && a.payloadKind === b.payloadKind
    && a.payloadMeta === b.payloadMeta
    && a.parentId === b.parentId
    && a.completionOf === b.completionOf
    && a.toolName === b.toolName
    && a.decision === b.decision
    && a.meta === b.meta
    && a.isBackground === b.isBackground
  );
}

export function itemsForThread(
  nextItems: readonly Item[] | null | undefined,
  threadId: string,
): Item[] {
  return (nextItems ?? []).filter((item) => item.threadId === threadId);
}

/**
 * Merge `incoming` into `current` by id, returning a fresh array sorted by
 * (turnIndex, itemIndex). Used by paging paths where the backend can
 * legitimately re-return ancestor rows already in the window.
 *
 * Returns the original `current` reference when `incoming` is empty or every
 * incoming row is already present by the same object reference, so callers can
 * skip reactive writes and associated turn-diff rebuilds.
 */
export function mergeItemsById(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0) return current as Item[];
  const byId = new Map<string, Item>();
  for (const it of current) byId.set(it.id, it);
  let changed = false;
  for (const it of incoming) {
    const existing = byId.get(it.id);
    if (existing && itemsAreEqual(existing, it)) {
      continue;
    }
    if (existing !== it) {
      byId.set(it.id, it);
      changed = true;
    }
  }
  if (!changed) return current as Item[];
  const merged = Array.from(byId.values());
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}

export function reconcileItemWindow(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0 && current.length === 0) return current as Item[];

  const currentById = new Map<string, Item>();
  for (const item of current) currentById.set(item.id, item);

  const next: Item[] = [];
  let changed = incoming.length !== current.length;
  for (const item of incoming) {
    const existing = currentById.get(item.id);
    if (existing && itemsAreEqual(existing, item)) {
      next.push(existing);
    } else {
      next.push(item);
      if (existing !== item) changed = true;
    }
  }

  if (!changed) {
    for (let index = 0; index < current.length; index += 1) {
      if (current[index] !== next[index]) {
        changed = true;
        break;
      }
    }
  }

  return changed ? next : current as Item[];
}

/**
 * Like `mergeItemsById` but only adds rows not already present. Existing rows
 * keep their current reference so streamed events are not clobbered by a
 * slightly older SQLite row returned by a concurrent load.
 */
export function mergeMissingItemsById(incoming: readonly Item[], current: readonly Item[]): Item[] {
  if (incoming.length === 0) return current as Item[];
  const presentIds = new Set<string>();
  for (const it of current) presentIds.add(it.id);
  const additions: Item[] = [];
  for (const it of incoming) {
    if (presentIds.has(it.id)) continue;
    additions.push(it);
    presentIds.add(it.id);
  }
  if (additions.length === 0) return current as Item[];
  const merged = current.concat(additions);
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}

export interface ApplyItemUpsertsToWindowOptions {
  current: readonly Item[];
  incoming: readonly Item[];
  itemIndexById: ReadonlyMap<string, number>;
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
  indexesNeedRebuild: boolean;
  structureChanged: boolean;
  droppedNewerItems: boolean;
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
  const appendedIndexById = new Map<string, number>();
  const appendedItems: Item[] = [];
  const changedItems: Item[] = [];
  let changed = false;
  let needsSort = false;
  let structureChanged = false;
  let droppedNewerItems = false;
  // MIN_SAFE_INTEGER, not 0: head-healed prompts sit at NEGATIVE item
  // indexes, so 0 is not the start of a turn — a fallback floor at 0
  // would misclassify those rows as below the loaded window (mirror of
  // the ceiling's MAX_SAFE_INTEGER).
  const floorCursor = oldestLoadedCursor
    ?? (oldestLoadedTurnIndex === null || oldestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: oldestLoadedTurnIndex, itemIndex: Number.MIN_SAFE_INTEGER });
  const ceilingCursor = newestLoadedCursor
    ?? (newestLoadedTurnIndex === null || newestLoadedTurnIndex === undefined
      ? null
      : { turnIndex: newestLoadedTurnIndex, itemIndex: Number.MAX_SAFE_INTEGER });

  const workingItems = (): Item[] => {
    if (next === null) next = current.slice();
    return next;
  };

  for (const item of incoming) {
    if (currentThreadId !== null && item.threadId !== currentThreadId) continue;

    const existingIndex = itemIndexById.get(item.id) ?? appendedIndexById.get(item.id);
    if (existingIndex !== undefined) {
      const previous = (next ?? current)[existingIndex];
      if (!previous) continue;
      // No-op dedupe: if the backend re-emits an upsert with identical
      // content, skip the array replace. Otherwise every redundant
      // upsert produces a new `pane.items` reference, which cascades
      // through `groupedNodes`, the Virtualizer's `data` prop, and the
      // mounted row components — observed as a 103 px row oscillation
      // every ~115 ms in plan-ready threads. See `itemsAreEqual` for
      // the fields compared.
      if (itemsAreEqual(previous, item)) continue;
      workingItems()[existingIndex] = item;
      changed = true;
      if (itemTimelineStructureChanged(previous, item)) {
        structureChanged = true;
      }
      changedItems.push(item);
      if (appendedIndexById.has(item.id)) {
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

    const source = next ?? current;
    const previousTail = source.at(-1);
    if (previousTail && compareItemsByTimelinePosition(previousTail, item) > 0) {
      needsSort = true;
    }
    const target = workingItems();
    appendedIndexById.set(item.id, target.length);
    target.push(item);
    changed = true;
    structureChanged = true;
    appendedItems.push(item);
    changedItems.push(item);
  }

  if (!changed && !droppedNewerItems) return null;
  if (!changed) {
    return {
      items: current as Item[],
      appendedItems,
      changedItems,
      indexesNeedRebuild: false,
      structureChanged: false,
      droppedNewerItems,
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
    indexesNeedRebuild: needsSort,
    structureChanged,
    droppedNewerItems,
  };
}
