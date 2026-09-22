import type { Item } from '../types/models';
import type { TimelinePageShape } from './threadPaneShared';
import { ACTIVITY_RUN_WINDOW_ROWS_MAX } from '../utils/activityRunWindow';
import type { ActivityRunStub, PagedItems, TimelineCursor, TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import type { ActivityRunSpan } from '../utils/activityRunSpans';
import { ListActivityRunMembers, ListItemsBeforeCursor } from './bindings';
import { compareCursors, compareItemToCursor, mergeItemsById } from './threadItems';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';

function cursor(item: Pick<Item, 'id' | 'turnIndex' | 'itemIndex'>): TimelineCursor {
  return { turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id };
}

function bounds(run: ActivityRunStub): { first: TimelineCursor; last: TimelineCursor } {
  return {
    first: { turnIndex: run.firstTurnIndex, itemIndex: run.firstItemIndex, itemId: run.firstItemId },
    last: { turnIndex: run.lastTurnIndex, itemIndex: run.lastItemIndex, itemId: run.lastItemId },
  };
}

/** Re-read expanded run windows before replacing history. Cursor reads also
 * handle deleted members and runs split or joined since the previous paint. */
export async function refreshRetainedRunWindows(
  threadId: string,
  page: PagedItems,
  previous: readonly (ActivityRunSpan & { readerPinned?: boolean })[],
  shape: TimelinePageShape,
  isCurrent: () => boolean,
  selection: TimelineSelection = {},
  followingTail = false,
): Promise<PagedItems> {
  const pageItems = page.items;
  const present = new Set(pageItems.map(item => item.id));
  const retained = previous.filter(span => {
    if ((!span.readerPinned && span.items.length <= shape.runWindowRows)
      || !span.items.some(item => !present.has(item.id))) return false;
    if (!followingTail || span.readerPinned || span.items.some(item => present.has(item.id))) return true;
    const oldLast = cursor(span.items[span.items.length - 1]);
    // One run can have only one contiguous shipped span. If the thread's
    // tail has advanced beyond this entire old span, keep the fresh tail
    // window; restoring the old span would hide the newest activity behind
    // a "later" boundary and reread history the reader did not ask for.
    return !page.runs.some(run => {
      const range = bounds(run);
      return compareCursors(oldLast, range.first) >= 0
        && compareCursors(oldLast, range.last) <= 0
        && pageItems.some(item => item.id === run.loadedFirstItemId && compareItemToCursor(item, oldLast) > 0);
    });
  });
  if (retained.length === 0) return page;
  const backend = requireEntityBackend(threadBackend(threadId));
  const readShape = { ...shape, runWindowRows: ACTIVITY_RUN_WINDOW_ROWS_MAX };
  let result = page;
  const pending = [...retained];
  while (pending.length > 0) {
    const span = pending.pop()!;
    let floor = cursor(span.items[0]);
    let ceiling = cursor(span.items[span.items.length - 1]);
    // A live run can gain a member while this pane is away. When the new
    // page's shipped span overlaps the old one, keep its new edge in the
    // contiguous window we restate. Otherwise the old span's stub counts
    // the arrival as "later" even though the reader was following the run.
    if (!span.readerPinned) {
      for (const run of page.runs) {
        const range = bounds(run);
        if (compareCursors(ceiling, range.first) < 0 || compareCursors(ceiling, range.last) > 0) continue;
        const freshFirst = pageItems.find(item => item.id === run.loadedFirstItemId);
        const freshLast = pageItems.find(item => item.id === run.loadedLastItemId);
        if (freshFirst && freshLast && compareItemToCursor(freshFirst, ceiling) <= 0
          && compareItemToCursor(freshLast, ceiling) > 0) {
          ceiling = cursor(freshLast);
        }
        break;
      }
    }
    let before = { ...ceiling, itemIndex: ceiling.itemIndex + 1, itemId: '' };
    const incomingRows: Item[] = [];
    const runs = new Map<string, ActivityRunStub>();
    const replaced: { first: TimelineCursor; last: TimelineCursor }[] = [];
    let oldest = result.oldestCursor;
    let newest = result.newestCursor;
    let hasMoreOlder = result.hasMoreOlder;
    let hasMoreNewer = result.hasMoreNewer;
    while (isCurrent()) {
      // One logical unit per response; advance by shipped rows, since a
      // logical run's cursor includes all of its unshipped members.
      const chunk = await withBackendTarget(backend, () => ListItemsBeforeCursor(threadId, before, 1, readShape, selection));
      if (!isCurrent()) return page;
      const incoming = chunk.items as Item[];
      if (incoming.length === 0) break;
      const first = cursor(incoming[0]);
      if (compareCursors(first, before) >= 0) throw new Error('Activity history refresh did not advance');
      for (const run of chunk.runs) {
        const range = bounds(run);
        for (let index = pending.length - 1; index >= 0; index -= 1) {
          const other = pending[index];
          if (compareItemToCursor(other.items[other.items.length - 1], range.first) < 0
            || compareItemToCursor(other.items[0], range.last) > 0) continue;
          const otherFloor = cursor(other.items[0]);
          if (compareCursors(otherFloor, floor) < 0) floor = otherFloor;
          pending.splice(index, 1);
        }
      }
      const selected = incoming.filter(item => compareItemToCursor(item, floor) >= 0 && compareItemToCursor(item, ceiling) <= 0);
      if (selected.length > 0) {
        for (const item of selected) incomingRows.push(item);
        replaced.push({ first: chunk.oldestCursor, last: chunk.newestCursor });
        for (const run of chunk.runs) runs.set(run.firstItemId, run);
        if (compareCursors(chunk.oldestCursor, oldest) <= 0) {
          oldest = chunk.oldestCursor;
          hasMoreOlder = chunk.hasMoreOlder;
        }
        if (compareCursors(chunk.newestCursor, newest) >= 0) {
          newest = chunk.newestCursor;
          hasMoreNewer = chunk.hasMoreNewer;
        }
      }
      if (compareCursors(first, floor) <= 0) break;
      before = first;
    }
    if (!isCurrent()) return page;
    const rows = mergeItemsById(incomingRows, []);
    const refreshed: ActivityRunStub[] = [];
    for (const run of runs.values()) {
      const { first, last } = bounds(run);
      const members = rows.filter(item => compareItemToCursor(item, first) >= 0 && compareItemToCursor(item, last) <= 0);
      if (members.length === 0) continue;
      const answer = await withBackendTarget(backend, () => ListActivityRunMembers(threadId,
        { selection, runFirstItemId: run.firstItemId, loadedFirstItemId: members[0].id,
          loadedLastItemId: members[members.length - 1].id, direction: 'before', limit: 0, shape }));
      if (!isCurrent()) return page;
      if (answer.stub.memberCount - answer.stub.unshippedBefore - answer.stub.unshippedAfter !== members.length) {
        throw new Error('Activity history changed during refresh');
      }
      refreshed.push(answer.stub);
    }
    const overlaps = (first: TimelineCursor, last: TimelineCursor) => replaced.some(range =>
      compareCursors(first, range.last) <= 0 && compareCursors(last, range.first) >= 0);
    result = { ...result,
      items: mergeItemsById(rows, result.items.filter(item => !overlaps(cursor(item), cursor(item))) as Item[]),
      runs: [...result.runs.filter(run => { const range = bounds(run); return !overlaps(range.first, range.last); }), ...refreshed],
      oldestCursor: oldest, newestCursor: newest,
      oldestTurnIndex: oldest.turnIndex, newestTurnIndex: newest.turnIndex,
      hasMore: hasMoreOlder, hasMoreOlder, hasMoreNewer,
    };
  }
  return result;
}
