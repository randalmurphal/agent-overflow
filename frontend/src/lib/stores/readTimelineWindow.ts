import type { Item } from '../types/models';
import type { PagedItems, TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { ListItemsBeforeCursor, ListItemsAfterCursor, SyncThreadWindow } from './bindings';
import { timelinePageShape, SLICE_AROUND_ITEM_BUDGET } from './threadPaneShared';
import { compareCursors, mergeItemsById } from './threadItems';
import { mergeRunStubs } from './activityRunStubs';
import { groupActivityRunSpans } from '../utils/activityRunSpans';
import { refreshRetainedRunWindows } from './threadRetainedRunRefresh';
import { isWindowedTimelineRow, heldWindowOf } from './threadWindowDigest';
import type { ThreadActivityRuns } from './threadActivityRuns.svelte';
import type { ThreadTimelineWindow } from './threadTimelineWindow.svelte';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { observeBackendGeneration } from '../transport/backendIdentity';

/** Revalidate all retained coordinates and explicitly loaded run members. */
export async function readTimelineWindow(
  threadId: string, selection: TimelineSelection, items: readonly Item[],
  window: ThreadTimelineWindow, runs: ThreadActivityRuns, anchor: string,
  current: () => boolean,
) {
  const backend = requireEntityBackend(threadBackend(threadId));
  const shape = timelinePageShape();
  const followingTail = !anchor && !window.hasMoreNewer;
  const held = heldWindowOf(items, window.hasMoreHistory, window.hasMoreNewer, runs.heldRunFold(), selection);
  const response = await withBackendTarget(backend, () => SyncThreadWindow(threadId, {
    anchorItemId: followingTail ? '' : anchor || window.newestLoadedCursor?.itemId || '',
    itemBudget: SLICE_AROUND_ITEM_BUDGET, haveEpoch: -1, haveRev: -1,
    ...shape, selection, haveWindow: held ?? undefined,
  }));
  if (!current()) return response;
  observeBackendGeneration(response.generation, backend);
  if (!current() || !response.page) return response;
  let page: PagedItems = response.page;
  const floor = window.oldestLoadedCursor;
  while (current() && floor && page.hasMoreOlder && compareCursors(page.oldestCursor, floor) > 0
    && (!followingTail || page.items.length < Math.max(items.length, SLICE_AROUND_ITEM_BUDGET))) {
    const before = page.oldestCursor;
    const older = await withBackendTarget(backend, () => ListItemsBeforeCursor(threadId, before,
      SLICE_AROUND_ITEM_BUDGET, shape, selection));
    if (!current()) return response;
    if (older.items.length === 0) break;
    if (compareCursors(older.oldestCursor, before) >= 0) throw new Error('History refresh did not advance');
    page = { ...page, items: mergeItemsById(older.items as Item[], page.items as Item[]),
      runs: mergeRunStubs(older.runs, page.runs), oldestCursor: older.oldestCursor,
      oldestTurnIndex: older.oldestTurnIndex, hasMore: older.hasMoreOlder, hasMoreOlder: older.hasMoreOlder };
  }
  const ceiling = window.newestLoadedCursor;
  while (current() && !followingTail && ceiling && page.hasMoreNewer && compareCursors(page.newestCursor, ceiling) < 0) {
    const after = page.newestCursor;
    const newer = await withBackendTarget(backend, () => ListItemsAfterCursor(threadId, after,
      SLICE_AROUND_ITEM_BUDGET, shape, selection));
    if (!current()) return response;
    if (newer.items.length === 0) break;
    if (compareCursors(newer.newestCursor, after) <= 0) throw new Error('History refresh did not advance');
    page = { ...page, items: mergeItemsById(page.items as Item[], newer.items as Item[]),
      runs: mergeRunStubs(page.runs, newer.runs), newestCursor: newer.newestCursor,
      newestTurnIndex: newer.newestTurnIndex, hasMoreNewer: newer.hasMoreNewer };
  }
  if (!current()) return response;
  const retained = groupActivityRunSpans(items, item => runs.isLoadedMember(item.id), item => isWindowedTimelineRow(item, selection))
    .filter(span => !followingTail || span.items.some(item =>
      compareCursors(item, page.oldestCursor) >= 0 && compareCursors(item, page.newestCursor) <= 0));
  page = await refreshRetainedRunWindows(threadId, page,
    retained,
    shape, current, selection);
  return { ...response, page };
}
