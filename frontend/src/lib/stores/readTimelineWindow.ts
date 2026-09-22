import type { Item } from '../types/models';
import type { TimelineSelection } from '../../../bindings/agent-overflow/internal/store/models';
import { SyncThreadWindow } from './bindings';
import { timelinePageShape, SLICE_AROUND_ITEM_BUDGET } from './threadPaneShared';
import { captureRetainedTimelineWindow, refreshRetainedTimelineWindow } from './threadRetainedTimelineRefresh';
import { heldWindowOf } from './threadWindowDigest';
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
  const retained = captureRetainedTimelineWindow(items, window, runs, followingTail, selection);
  const held = heldWindowOf(runs.loadedItems(items), window.hasMoreHistory, window.hasMoreNewer, runs.heldRunFold(), selection);
  const response = await withBackendTarget(backend, () => SyncThreadWindow(threadId, {
    anchorItemId: followingTail ? '' : anchor || window.newestLoadedCursor?.itemId || '',
    itemBudget: SLICE_AROUND_ITEM_BUDGET, haveEpoch: -1, haveRev: -1,
    ...shape, selection, haveWindow: held ?? undefined,
  }));
  if (!current()) return response;
  observeBackendGeneration(response.generation, backend);
  if (!current() || !response.page) return response;
  const page = await refreshRetainedTimelineWindow({ threadId, page: response.page, retained, shape, isCurrent: current, selection });
  return { ...response, page };
}
