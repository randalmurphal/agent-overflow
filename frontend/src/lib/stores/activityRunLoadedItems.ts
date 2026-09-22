import type { Item } from '../types/models';
import type { ActivityRunLookup } from './activityRunLookup';
import { compareItemsByTimelinePosition, itemsWithinLoadedWindow, type TimelineCursorLike } from './threadItems';

export interface RunWindowBounds {
  oldest: TimelineCursorLike | null;
  newest: TimelineCursorLike | null;
}

/** Scope/tray rows can be retained inside a run's unloaded interval as well
 * as outside the page. Neither kind of context belongs to its loaded span. */
export function activityRunLoadedItems(
  items: readonly Item[],
  bounds: RunWindowBounds,
  runs: ActivityRunLookup,
  previousMembers: ReadonlyMap<string, string>,
  includes: (item: Item) => boolean = item => !item.parentId,
): readonly Item[] {
  const window = itemsWithinLoadedWindow(items, bounds.oldest, bounds.newest);
  if (runs.size === 0) return window;
  const byId = new Map<string, Item>();
  for (const item of window) byId.set(item.id, item);
  let filtered: Item[] | undefined;
  for (let index = 0; index < window.length; index += 1) {
    const item = window[index];
    const stub = includes(item) ? runs.find(item) : undefined;
    let keep = true;
    if (stub && previousMembers.get(item.id) !== stub.firstItemId) {
      const first = byId.get(stub.loadedFirstItemId);
      const last = byId.get(stub.loadedLastItemId);
      keep = (!!first || !!last)
        && (!first || compareItemsByTimelinePosition(item, first) >= 0)
        && (!last || compareItemsByTimelinePosition(item, last) <= 0);
    }
    if (!keep) filtered ??= window.slice(0, index);
    else filtered?.push(item);
  }
  return filtered ?? window;
}
