// Global per-thread "live status" map. The sidebar renders a colored
// dot next to each thread so the user can see which threads are busy
// without focusing them. The pane store (thread.svelte.ts) keeps the
// authoritative per-pane `sessionStatus`; this store is a parallel
// lightweight projection so off-pane threads still show a signal.
//
// Deliberately simple — routing logic lives in stores/events.ts. This
// file owns the map, nothing else. Any event kind → status mapping is
// the caller's job.
//
// Persistence: none. An idle map at boot is fine — the first provider
// event after reconnect seeds whatever is actually running.

export type ThreadLiveStatus = 'idle' | 'running' | 'pending-approval' | 'error';

let statuses: Map<string, ThreadLiveStatus> = $state(new Map());

/**
 * Returns the live status for a thread, or 'idle' when nothing has been
 * recorded for this id yet. 'idle' is the same signal the sidebar uses
 * to render no dot, so callers don't need to special-case undefined.
 */
export function getThreadStatus(threadId: string): ThreadLiveStatus {
  return statuses.get(threadId) ?? 'idle';
}

/**
 * Set or replace a thread's live status. Writing 'idle' is equivalent
 * to clearing — we drop the entry so the map doesn't grow with stale
 * idle-only rows across long sessions.
 */
export function setThreadStatus(threadId: string, status: ThreadLiveStatus): void {
  if (status === 'idle') {
    if (!statuses.has(threadId)) return;
    const next = new Map(statuses);
    next.delete(threadId);
    statuses = next;
    return;
  }
  const current = statuses.get(threadId);
  if (current === status) return;
  statuses = new Map(statuses).set(threadId, status);
}

/**
 * Drop any status for this thread. Called when a thread is
 * archived/deleted so its dot doesn't linger in the sidebar.
 */
export function clearThreadStatus(threadId: string): void {
  if (!statuses.has(threadId)) return;
  const next = new Map(statuses);
  next.delete(threadId);
  statuses = next;
}

/**
 * Count of threads currently in a non-idle state. Reactive via $state,
 * so any future sidebar section-header badge ("3 active") can bind to
 * this without subscribing to the whole map.
 */
export function getNonIdleThreadCount(): number {
  return statuses.size;
}

/**
 * Read-only view onto the status map. Primarily for tests that want to
 * assert "nothing other than these threads has a status".
 */
export function getAllThreadStatuses(): Map<string, ThreadLiveStatus> {
  return statuses;
}

/**
 * Wipe the entire map. Only intended for test isolation — production
 * code should use clearThreadStatus per id.
 */
export function resetForTest(): void {
  if (statuses.size === 0) return;
  statuses = new Map();
}
