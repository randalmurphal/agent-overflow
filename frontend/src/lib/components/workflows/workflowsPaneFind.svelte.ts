// The Workflows pane's find (stores/paneFind.svelte.ts). The pane's two
// regions never share the screen: home is the list region, a run detail or
// all-clear the detail region, so the level shown is the target. The list
// region's provider, registered while home lists, filters home's rows
// through `setWorkflowsHomeQuery`; the detail region has no find, so mod+f
// there falls through. Enter focuses the first row carrying
// `data-find-row` inside the pane body.

import { untrack } from 'svelte';
import {
  PANE_FIND_DETAIL_REGION,
  PANE_FIND_LIST_REGION,
  declarePaneFindRegions,
  paneFindListKey,
  registerPaneFindProvider,
  type PaneFindProvider,
} from '../../stores/paneFind.svelte';
import { getWorkflowsHomeRowCount, setWorkflowsHomeQuery } from '../../stores/workflowsHome.svelte';

/** Marks a home row the list find's Enter may focus. */
export const WORKFLOWS_FIND_ROW_ATTR = 'data-find-row';

export interface WorkflowsPaneFindHost {
  paneId: string;
  /** Home is on screen and listing. */
  homeListed(): boolean;
  /** The pane body, which holds home's rows. */
  body(): HTMLElement | null;
}

export function workflowsListFindProvider(body: () => HTMLElement | null): PaneFindProvider {
  return {
    noun: 'workflows',
    result: {
      get count() {
        return getWorkflowsHomeRowCount();
      },
      current: -1,
      pending: false,
    },
    search: (query) => setWorkflowsHomeQuery(query),
    focusFirst: () => {
      const row = body()?.querySelector<HTMLElement>(`[${WORKFLOWS_FIND_ROW_ATTR}]`);
      if (!row) return false;
      row.focus();
      return document.activeElement === row;
    },
    clear: () => setWorkflowsHomeQuery(''),
  };
}

/** Wires the pane's find for its life; returns the list region's key. */
export function attachWorkflowsPaneFind(host: WorkflowsPaneFindHost): string {
  const { paneId } = host;
  $effect(() =>
    untrack(() =>
      declarePaneFindRegions(paneId, {
        initial: PANE_FIND_LIST_REGION,
        shown: () => (host.homeListed() ? PANE_FIND_LIST_REGION : PANE_FIND_DETAIL_REGION),
      }),
    ),
  );
  const listKey = paneFindListKey(paneId);
  $effect(() => {
    if (!host.homeListed()) return;
    return untrack(() => registerPaneFindProvider(listKey, workflowsListFindProvider(host.body)));
  });
  return listKey;
}
