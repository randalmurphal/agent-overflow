import type { Thread } from '../types/models';
import { clearPayloadCacheForThread } from '../utils/payloadDataCache';
import { clearThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { clearTokensForThread } from '../utils/tokenCacheReactive.svelte';
import { ListThreads } from './bindings';
import { dropActivityRailUiPrefs, dropLiveTodoUiPrefs } from './thread.svelte';
import { threadItemCache } from './threadItemCache';
import { clearThreadStatus } from './threadStatuses.svelte';
import { addToast } from './toast.svelte';
import { releaseThreadTerminalState } from '../components/terminal/terminalStore.svelte';

type ThreadReadStatePatch = Partial<Pick<Thread, 'lastReadAt' | 'hasIncompleteTurn'>>;

let threads: Thread[] = $state([]);

export function getThreads(): Thread[] {
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    threads = await ListThreads() as Thread[];
  } catch (err) {
    console.error('Failed to load threads:', err);
    addToast('error', 'Failed to load threads');
  }
}

export function prependThread(thread: Thread): void {
  threads = [thread, ...threads.filter((t) => t.id !== thread.id)];
}

export function removeThread(id: string): void {
  threads = threads.filter((t) => t.id !== id);
  // Drop any live-status entry so the sidebar doesn't keep painting a
  // dot for a thread that no longer exists in the list.
  clearThreadStatus(id);
  // Drop the live-todo UI prefs entry too so the module-scoped map
  // doesn't accumulate dead-thread keys across long sessions.
  dropLiveTodoUiPrefs(id);
  dropActivityRailUiPrefs(id);
  // Symmetric eviction so a deleted thread doesn't leave a multi-MB
  // snapshot wedged in the LRU and so a fork-then-delete-then-fork
  // pattern can't surface stale items if a generated id ever recurs.
  threadItemCache.evict(id);
  clearThreadScrollSnapshot(id);
  clearTokensForThread(id);
  clearPayloadCacheForThread(id);
  releaseThreadTerminalState(id);
}

export function updateThreadTitle(id: string, title: string): void {
  threads = threads.map((t) => t.id === id ? { ...t, title } : t);
}

export function updateThreadModel(id: string, model: string): void {
  threads = threads.map((t) => t.id === id ? { ...t, model } : t);
}

export function replaceThread(thread: Thread): void {
  threads = threads.map((t) => t.id === thread.id ? thread : t);
}

/**
 * Bump a cached thread's activity timestamp when a live provider event
 * proves the backend touched it. Returns the current cached row so callers
 * can reconcile project-level projections without re-scanning the list.
 */
export function touchThreadActivity(id: string, updatedAt: number): Thread | undefined {
  if (!id || !Number.isFinite(updatedAt)) return undefined;
  const index = threads.findIndex((t) => t.id === id);
  if (index === -1) return undefined;

  const existing = threads[index];
  if ((existing.updatedAt ?? 0) >= updatedAt) {
    return existing;
  }

  const updated = { ...existing, updatedAt };
  const next = [...threads];
  next[index] = updated;
  threads = next;
  return updated;
}

/**
 * Patches local sidebar read state immediately after a MarkThreadRead /
 * MarkThreadUnread request. `hasIncompleteTurn` is included because
 * Interrupted is also unseen read-state; opening the thread clears it
 * before the next refreshThreads() round-trip.
 */
export function updateThreadReadState(
  id: string,
  patch: ThreadReadStatePatch,
): void {
  threads = threads.map((t) =>
    t.id === id ? { ...t, ...patch } : t,
  );
}

/**
 * Patches the thread's lastReadAt locally so the sidebar reflects a
 * Mark-read / Mark-unread action immediately, without waiting for the
 * next refreshThreads() round-trip. `undefined` encodes "never tracked";
 * explicit unread uses `0`.
 */
export function updateThreadLastRead(id: string, lastReadAt: number | undefined): void {
  updateThreadReadState(id, { lastReadAt });
}

/**
 * Patches the thread's pinnedAt locally so the sidebar reorders before
 * the next refreshThreads() round-trip. `undefined` clears the pin.
 */
export function updateThreadPinnedAt(id: string, pinnedAt: number | undefined): void {
  threads = threads.map((t) =>
    t.id === id ? { ...t, pinnedAt } : t,
  );
}

/**
 * Returns the thread with the given id, or undefined if the sidebar doesn't
 * currently track it (e.g. archived parent not in the filtered view).
 */
export function getThreadById(id: string): Thread | undefined {
  return threads.find((t) => t.id === id);
}
