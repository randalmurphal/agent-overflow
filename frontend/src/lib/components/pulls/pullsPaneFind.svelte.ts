// The Pull requests page's find (stores/paneFind.svelte.ts): the pane holds
// two regions, the list column and the detail beside it. The list region's
// provider filters the shown tab's rows through the page stores
// (`setPullsQuery`, `setPipelinesQuery`), so every count the tab shows
// follows it; a tab switch registers the other tab's provider, which the
// store hands the open query. The detail region's find is the detail's own
// (`paneFindDetailKey`); with none registered, mod+f there falls through.
// Under compact the region on screen is the target. Call from the pane's
// script after the Pipelines tab's attach effect, so a tab switch attaches
// the tab before its provider searches it.

import { untrack } from 'svelte';
import {
  PANE_FIND_DETAIL_REGION,
  PANE_FIND_LIST_REGION,
  declarePaneFindRegions,
  paneFindListKey,
  registerPaneFindProvider,
  type PaneFindProvider,
} from '../../stores/paneFind.svelte';
import { pullsListedCount, setPullsQuery } from '../../stores/pullsPage.svelte';
import { pipelinesRows, setPipelinesQuery } from '../../stores/pipelinesPage.svelte';

export type PullsTab = 'mrs' | 'pipelines';

/** What the list find needs of the shown list component. */
export interface PullsFindList {
  focusFirstRow(): Promise<boolean>;
  scroller(): HTMLElement | null;
}

export interface PullsPaneFindHost {
  paneId: string;
  tab(): PullsTab;
  /** Under compact only one region shows: the detail while it is open. */
  compact(): boolean;
  detailShown(): boolean;
  list(): PullsFindList | undefined;
}

export function listFindProvider(paneId: string, tab: PullsTab, list: () => PullsFindList | undefined): PaneFindProvider {
  const setQuery = tab === 'mrs' ? setPullsQuery : setPipelinesQuery;
  return {
    noun: tab === 'mrs' ? 'merge requests' : 'pipelines',
    result: {
      get count() {
        return tab === 'mrs' ? pullsListedCount(paneId) : pipelinesRows(paneId).length;
      },
      current: -1,
      pending: false,
    },
    search: (query) => setQuery(paneId, query),
    focusFirst: () => list()?.focusFirstRow() ?? false,
    clear: () => setQuery(paneId, ''),
  };
}

/** Wires the page's find for the pane's life; returns the list region's
 * find key, for its bar. */
export function attachPullsPaneFind(host: PullsPaneFindHost): string {
  const { paneId } = host;
  $effect(() =>
    untrack(() =>
      declarePaneFindRegions(paneId, {
        initial: PANE_FIND_LIST_REGION,
        shown: () => (host.compact() ? (host.detailShown() ? PANE_FIND_DETAIL_REGION : PANE_FIND_LIST_REGION) : null),
      }),
    ),
  );
  const listKey = paneFindListKey(paneId);
  $effect(() => {
    const tab = host.tab();
    return untrack(() => registerPaneFindProvider(listKey, listFindProvider(paneId, tab, host.list)));
  });
  return listKey;
}
