import type { Item } from '../types/models';

export function compareItemsByTimelinePosition(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
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
 * `<Virtualizer data={groupedNodes}>` to re-iterate, which can land as
 * a remount of the affected row's DOM. A row with async render work
 * (DiffFileBlock token dispatch, Streamdown typesetting, etc.) then
 * settles at its measured height again — and virtua applies a scrollTop
 * jump for the size delta. Observed in plan-ready threads as a row
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
    && a.createdAt === b.createdAt
    && a.updatedAt === b.updatedAt
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
  oldestLoadedTurnIndex: number | null;
}

export interface ApplyItemUpsertsToWindowResult {
  items: Item[];
  appendedItems: readonly Item[];
  indexesNeedRebuild: boolean;
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
  oldestLoadedTurnIndex,
}: ApplyItemUpsertsToWindowOptions): ApplyItemUpsertsToWindowResult | null {
  if (incoming.length === 0) return null;

  const next = current.slice();
  const appendedIndexById = new Map<string, number>();
  const appendedItems: Item[] = [];
  let changed = false;
  let needsSort = false;

  for (const item of incoming) {
    if (currentThreadId !== null && item.threadId !== currentThreadId) continue;

    const existingIndex = itemIndexById.get(item.id) ?? appendedIndexById.get(item.id);
    if (existingIndex !== undefined) {
      const previous = next[existingIndex];
      // No-op dedupe: if the backend re-emits an upsert with identical
      // content, skip the array replace. Otherwise every redundant
      // upsert produces a new `pane.items` reference, which cascades
      // through `groupedNodes`, the Virtualizer's `data` prop, and the
      // mounted row components — observed as a 103 px row oscillation
      // every ~115 ms in plan-ready threads. See `itemsAreEqual` for
      // the fields compared.
      if (itemsAreEqual(previous, item)) continue;
      next[existingIndex] = item;
      if (appendedIndexById.has(item.id)) {
        const appendedOffset = existingIndex - current.length;
        appendedItems[appendedOffset] = item;
      }
      if (compareItemsByTimelinePosition(previous, item) !== 0) {
        needsSort = true;
      }
      changed = true;
      continue;
    }

    if (oldestLoadedTurnIndex !== null && item.turnIndex < oldestLoadedTurnIndex) {
      continue;
    }

    const previousTail = next.at(-1);
    if (previousTail && compareItemsByTimelinePosition(previousTail, item) > 0) {
      needsSort = true;
    }
    appendedIndexById.set(item.id, next.length);
    next.push(item);
    appendedItems.push(item);
    changed = true;
  }

  if (!changed) return null;

  if (needsSort) {
    next.sort(compareItemsByTimelinePosition);
  }
  return {
    items: next,
    appendedItems,
    indexesNeedRebuild: needsSort,
  };
}
