import type { Item } from '../types/models';
import type { PagedItems, TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { ListItemsBeforeCursor, ListItemsAfterCursor } from './bindings';
import { SLICE_AROUND_ITEM_BUDGET, type TimelinePageShape } from './threadPaneShared';
import { compareCursors, mergeItemsById } from './threadItems';
import { groupActivityRunSpans } from '../utils/activityRunSpans';
import { refreshRetainedRunWindows } from './threadRetainedRunRefresh';
import { isWindowedTimelineRow } from './threadWindowDigest';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { ThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';

/** Capture before reading so pagination cannot move the requested window mid-read. */
export function captureRetainedTimelineWindow(
  items: readonly Item[],
  window: Pick<ThreadTimelineWindow, 'oldestLoadedCursor' | 'newestLoadedCursor'>,
  runs: Pick<ThreadActivityRuns, 'loadedItems' | 'isLoadedMember'>,
  followingTail: boolean,
  selection: TimelineSelection = {},
) {
  const loaded = runs.loadedItems(items);
  const includes = (item: Item) => isWindowedTimelineRow(item, selection);
  return {
    oldest: window.oldestLoadedCursor ? { ...window.oldestLoadedCursor } : null,
    newest: window.newestLoadedCursor ? { ...window.newestLoadedCursor } : null,
    count: loaded.filter(includes).length,
    runs: groupActivityRunSpans(loaded, item => runs.isLoadedMember(item.id), includes),
    followingTail,
  };
}

/** Assemble fresh bounded pages before replacing an already visible window. */
export async function refreshRetainedTimelineWindow(options: {
  threadId: string;
  page: PagedItems;
  retained: ReturnType<typeof captureRetainedTimelineWindow>;
  shape: TimelinePageShape;
  isCurrent(): boolean;
  selection?: TimelineSelection;
}): Promise<PagedItems> {
  const { threadId, retained, shape, selection = {} } = options;
  const ownership = threadBackend(threadId);
  const backend = requireEntityBackend(ownership);
  const current = () => options.isCurrent() && threadBackend(threadId) === ownership;
  let page = options.page;
  const floor = retained.oldest;
  const chunks: Item[][] = [];
  const ids = new Set<string>();
  const runs = new Map((page.runs ?? []).map(run => [run.firstItemId, run]));
  const addItems = (items: Item[]) => {
    chunks.push(items);
    for (const item of items) if (isWindowedTimelineRow(item, selection)) ids.add(item.id);
  };
  addItems(page.items as Item[]);
  while (current() && floor && page.hasMoreOlder && compareCursors(page.oldestCursor, floor) > 0
    && (!retained.followingTail || ids.size < Math.max(retained.count, SLICE_AROUND_ITEM_BUDGET))) {
    const before = page.oldestCursor;
    const older = await withBackendTarget(backend, () => ListItemsBeforeCursor(threadId, before,
      SLICE_AROUND_ITEM_BUDGET, shape, selection));
    if (!current()) return options.page;
    if (older.items.length === 0) {
      if (older.hasMoreOlder) throw new Error('History refresh returned no older rows with more history remaining');
      page = { ...page, hasMore: false, hasMoreOlder: false };
      break;
    }
    if (compareCursors(older.oldestCursor, before) >= 0) throw new Error('History refresh did not advance');
    addItems(older.items as Item[]);
    for (const run of older.runs ?? []) if (!runs.has(run.firstItemId)) runs.set(run.firstItemId, run);
    page = { ...page, oldestCursor: older.oldestCursor,
      oldestTurnIndex: older.oldestTurnIndex, hasMore: older.hasMoreOlder, hasMoreOlder: older.hasMoreOlder };
  }
  const ceiling = retained.newest;
  while (current() && !retained.followingTail && ceiling && page.hasMoreNewer && compareCursors(page.newestCursor, ceiling) < 0) {
    const after = page.newestCursor;
    const newer = await withBackendTarget(backend, () => ListItemsAfterCursor(threadId, after,
      SLICE_AROUND_ITEM_BUDGET, shape, selection));
    if (!current()) return options.page;
    if (newer.items.length === 0) {
      if (newer.hasMoreNewer) throw new Error('History refresh returned no newer rows with more history remaining');
      page = { ...page, hasMoreNewer: false };
      break;
    }
    if (compareCursors(newer.newestCursor, after) <= 0) throw new Error('History refresh did not advance');
    addItems(newer.items as Item[]);
    for (const run of newer.runs ?? []) runs.set(run.firstItemId, run);
    page = { ...page, newestCursor: newer.newestCursor,
      newestTurnIndex: newer.newestTurnIndex, hasMoreNewer: newer.hasMoreNewer };
  }
  if (!current()) return options.page;
  if (chunks.length > 1) page = { ...page, items: mergeItemsById(chunks.flat(), []), runs: [...runs.values()] };
  const retainedRuns = retained.runs.filter(span => !retained.followingTail || span.items.some(item =>
    compareCursors(item, page.oldestCursor) >= 0 && compareCursors(item, page.newestCursor) <= 0));
  return refreshRetainedRunWindows(threadId, page, retainedRuns, shape, current, selection);
}
