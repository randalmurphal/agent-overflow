import { THREAD_PREVIEW_LIMIT } from '../../lib/utils/sidebarThreadLimits';
import {
  flattenSidebarThreadTree,
  type FlattenSidebarThreadTreeInput,
  type SidebarTreeVisibleNode,
} from '../../lib/utils/sidebarTreeView';

type FlattenTreeNodesInput =
  Omit<FlattenSidebarThreadTreeInput, 'openThreadIds' | 'groupVisibleLimit'>
  & Partial<Pick<FlattenSidebarThreadTreeInput, 'openThreadIds' | 'groupVisibleLimit'>>;

/**
 * Flatten a tree no group cut reaches: no open threads and every group at
 * the default preview unless given. Fails when the flatten emits a group
 * show-more footer, so a test that meant to stay under the cut says so.
 */
export function flattenTreeNodes(input: FlattenTreeNodesInput): SidebarTreeVisibleNode[] {
  const rows = flattenSidebarThreadTree({
    openThreadIds: new Set(),
    groupVisibleLimit: () => THREAD_PREVIEW_LIMIT,
    ...input,
  });
  const nodes = rows.filter((row): row is SidebarTreeVisibleNode => row.kind !== 'group-more');
  if (nodes.length !== rows.length) throw new Error('flattenTreeNodes: a group cut emitted a show-more footer');
  return nodes;
}
