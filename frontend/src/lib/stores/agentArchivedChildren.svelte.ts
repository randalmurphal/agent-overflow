// The archived threads under an assistant, every container's, read when
// the user opens one of its rows' `N done` footer (docs/specs/
// standing-agents.md, "Sidebar"). Keyed by the total of the counts the
// assistant's row carries (agentArchivedTotal): it moves with every
// archive, unarchive and delete of a thread under it, so a list loaded for
// one total is stale once the row says another, and the footer asks again.

import { ListArchivedSpawnedThreads } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import type { Thread } from '../types/models';
import { addErrorToast } from './toast.svelte';

interface ArchivedChildren {
  /** The archived total the list was read for. */
  count: number;
  threads: readonly Thread[];
}

let lists = $state<Map<string, ArchivedChildren>>(new Map());
const pending = new Map<string, number>();

/**
 * The agent's archived children as last read, or null while no read for
 * the row's current count has landed. The footer shows a loading state on
 * null and the previous list is not shown for a count it no longer fits.
 */
export function getArchivedChildren(agentId: string, count: number): readonly Thread[] | null {
  const entry = lists.get(agentId);
  return entry && entry.count === count ? entry.threads : null;
}

/** Read the list for the row's current count unless that read is already out. */
export function loadArchivedChildren(agentId: string, count: number): void {
  if (count === 0) {
    if (lists.has(agentId)) {
      const next = new Map(lists);
      next.delete(agentId);
      lists = next;
    }
    return;
  }
  if (lists.get(agentId)?.count === count || pending.get(agentId) === count) return;
  // A thread the index does not know is local, as everywhere else.
  const backend = threadBackend(agentId) ?? HOME_BACKEND;
  pending.set(agentId, count);
  void withBackendTarget(backend, () => ListArchivedSpawnedThreads(agentId)).then((threads) => {
    if (pending.get(agentId) !== count) return;
    const next = new Map(lists);
    next.set(agentId, { count, threads: (threads ?? []) as Thread[] });
    lists = next;
  }).catch((err: unknown) => {
    if (isPassiveConnectionFailure(err)) return;
    console.error(`Failed to load the archived threads spawned by ${agentId}:`, err);
    addErrorToast('Failed to load finished threads', err);
  }).finally(() => {
    if (pending.get(agentId) === count) pending.delete(agentId);
  });
}

/**
 * Drop what was read for an agent, so the next load reads again: the
 * footer forgets its list when it opens, since the set may have changed
 * at an unchanged count while the agent was demoted or the footer shut.
 */
export function forgetArchivedChildren(agentId: string): void {
  pending.delete(agentId);
  if (!lists.has(agentId)) return;
  const next = new Map(lists);
  next.delete(agentId);
  lists = next;
}

/** Test helper. */
export function resetArchivedChildrenForTest(): void {
  lists = new Map();
  pending.clear();
}
