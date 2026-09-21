import type { Item } from '../types/models';
import type { ActivityRunStub } from '../../../bindings/agent-overflow/internal/store/models';
import type { ActivityRunRecords } from './activityRunStubs';
import { compareItemToCursor, compareItemsByTimelinePosition, itemsWithinLoadedWindow, type TimelineCursorLike } from './threadItems';

export interface RunWindowBounds {
  oldest: TimelineCursorLike | null;
  newest: TimelineCursorLike | null;
}

/** Scope/tray rows can be retained inside a run's unloaded interval as well
 * as outside the page. Neither kind of context belongs to its loaded span. */
export function activityRunLoadedItems(
  items: readonly Item[],
  bounds: RunWindowBounds,
  records: ActivityRunRecords,
  stubs: readonly ActivityRunStub[],
  previousMembers: ReadonlyMap<string, string>,
  includes: (item: Item) => boolean = item => !item.parentId,
): readonly Item[] {
  const window = itemsWithinLoadedWindow(items, bounds.oldest, bounds.newest);
  if (records.size === 0 && stubs.length === 0) return window;
  const byId = new Map(window.map(item => [item.id, item]));
  const descriptions = new Map([...records.values()].map(record => [record.runFirstItemId, record.stub]));
  for (const stub of stubs) descriptions.set(stub.firstItemId, stub);
  const spans = [...descriptions.values()].map(stub => ({
    stub,
    first: byId.get(stub.loadedFirstItemId),
    last: byId.get(stub.loadedLastItemId),
  }));
  const keep = (item: Item): boolean => {
    if (!includes(item)) return true;
    for (const { stub, first, last } of spans) {
      if (compareItemToCursor(item, { turnIndex: stub.firstTurnIndex, itemIndex: stub.firstItemIndex, itemId: stub.firstItemId }) < 0
        || compareItemToCursor(item, { turnIndex: stub.lastTurnIndex, itemIndex: stub.lastItemIndex, itemId: stub.lastItemId }) > 0) continue;
      if (previousMembers.get(item.id) === stub.firstItemId) return true;
      return (!!first || !!last)
        && (!first || compareItemsByTimelinePosition(item, first) >= 0)
        && (!last || compareItemsByTimelinePosition(item, last) <= 0);
    }
    return true;
  };
  return window.every(keep) ? window : window.filter(keep);
}
