import type { ThreadPane } from './thread.svelte';
import type { Item } from '../types/models';
import { GetThreadItem } from './bindings';
import { requireEntityBackend, withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { agentStateForPane } from './agentPane.svelte';
import { agentScopeRootId, subagentLaunchInfo } from '../utils/subagentLaunch';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';

const requests = new WeakMap<ThreadPane, symbol>();

/** Search navigation resolves the transcript before moving either viewport. */
export async function navigateToThreadItem(pane: ThreadPane, itemId: string): Promise<void> {
  const request = Symbol();
  requests.set(pane, request);
  const threadId = pane.threadId;
  if (!threadId) { requests.delete(pane); return; }
  const generation = pane.switchGeneration;
  const ownership = threadBackend(threadId);
  const current = () => requests.get(pane) === request
    && pane.threadId === threadId && pane.switchGeneration === generation
    && threadBackend(threadId) === ownership;
  try {
    const backend = requireEntityBackend(ownership);
    const read = async (id: string) => pane.getItemById(id)
      ?? await withBackendTarget(backend, () => GetThreadItem(threadId, id)) as Item;
    let item = await read(itemId);
    if (!current()) return;
    if (!item?.id || item.threadId !== threadId) throw new Error('The selected message is no longer available');
    if (!item.parentId) { pane.requestScrollToItem(itemId); return; }
    const trail: { itemId: string; label: string }[] = [];
    const seen = new Set<string>([item.id]);
    while (item.parentId) {
      const parent = await read(item.parentId);
      if (!current()) return;
      if (!parent?.id || parent.threadId !== threadId || seen.has(parent.id)) throw new Error('The agent ancestry is unavailable');
      seen.add(parent.id);
      const canonical = agentScopeRootId(parent);
      if (canonical !== parent.id && seen.has(canonical)) throw new Error('The agent ancestry contains a cycle');
      seen.add(canonical);
      item = canonical === parent.id ? parent : await read(canonical);
      if (!current()) return;
      if (!item?.id || item.threadId !== threadId) throw new Error('The agent transcript is unavailable');
      const info = subagentLaunchInfo(item, { hasChildren: () => true });
      trail.unshift({ itemId: item.id, label: info?.name ?? item.toolName ?? 'Agent' });
    }
    const target = trail.at(-1)!;
    pane.openAgentPane(target.itemId, target.label);
    agentStateForPane(pane.paneId, threadId).openAtItem(trail, itemId);
  } catch (error) {
    if (current()) addToast('error', `Could not open message: ${errString(error)}`);
  } finally {
    if (requests.get(pane) === request) requests.delete(pane);
  }
}
