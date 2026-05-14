import type { Item } from '../types/models';

export function compareItemsByTimelinePosition(a: Item, b: Item): number {
  if (a.turnIndex !== b.turnIndex) return a.turnIndex - b.turnIndex;
  if (a.itemIndex !== b.itemIndex) return a.itemIndex - b.itemIndex;
  return 0;
}

export function itemsForThread(
  nextItems: Item[] | null | undefined,
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
export function mergeItemsById(incoming: Item[], current: Item[]): Item[] {
  if (incoming.length === 0) return current;
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
  if (!changed) return current;
  const merged = Array.from(byId.values());
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}

/**
 * Like `mergeItemsById` but only adds rows not already present. Existing rows
 * keep their current reference so streamed events are not clobbered by a
 * slightly older SQLite row returned by a concurrent load.
 */
export function mergeMissingItemsById(incoming: Item[], current: Item[]): Item[] {
  if (incoming.length === 0) return current;
  if (current.length === 0) {
    const sorted = incoming.slice();
    sorted.sort(compareItemsByTimelinePosition);
    return sorted;
  }
  const presentIds = new Set<string>();
  for (const it of current) presentIds.add(it.id);
  const additions: Item[] = [];
  for (const it of incoming) {
    if (!presentIds.has(it.id)) additions.push(it);
  }
  if (additions.length === 0) return current;
  const merged = current.concat(additions);
  merged.sort(compareItemsByTimelinePosition);
  return merged;
}

export interface ApplyItemUpsertsToWindowOptions {
  current: Item[];
  incoming: Item[];
  currentThreadId: string | null;
  oldestLoadedTurnIndex: number | null;
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
  currentThreadId,
  oldestLoadedTurnIndex,
}: ApplyItemUpsertsToWindowOptions): Item[] {
  if (incoming.length === 0) return current;

  const itemIndexById = new Map<string, number>();
  for (let index = 0; index < current.length; index += 1) {
    itemIndexById.set(current[index].id, index);
  }

  const next = current.slice();
  let changed = false;
  let needsSort = false;

  for (const item of incoming) {
    if (currentThreadId !== null && item.threadId !== currentThreadId) continue;

    const existingIndex = itemIndexById.get(item.id);
    if (existingIndex !== undefined) {
      const previous = next[existingIndex];
      next[existingIndex] = item;
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
    itemIndexById.set(item.id, next.length);
    next.push(item);
    changed = true;
  }

  if (!changed) return current;

  if (needsSort) {
    next.sort(compareItemsByTimelinePosition);
  }
  return next;
}
