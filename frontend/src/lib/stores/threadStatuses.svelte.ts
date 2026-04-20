import type { Item } from '../types/models';

// Global per-thread live-status projection for the sidebar. Chat state is
// authoritative in the unified item stream; this store keeps the minimal
// derived signal the thread list needs for off-pane rows (running, pending
// approval, error). No persistence: an empty map at boot is acceptable and
// rehydrates from the next live event.

export type ThreadLiveStatus = 'idle' | 'running' | 'pending-approval' | 'error';

let statuses: Map<string, ThreadLiveStatus> = $state(new Map());
const activeItemIDsByThread = new Map<string, Set<string>>();
const approvalIDsByThread = new Map<string, Set<string>>();
const approvalThreadByID = new Map<string, string>();
const errorThreads = new Set<string>();

function trackedIDsFor(map: Map<string, Set<string>>, threadId: string): Set<string> {
  let ids = map.get(threadId);
  if (!ids) {
    ids = new Set<string>();
    map.set(threadId, ids);
  }
  return ids;
}

function removeTrackedID(map: Map<string, Set<string>>, threadId: string, id: string): void {
  const ids = map.get(threadId);
  if (!ids) return;
  ids.delete(id);
  if (ids.size === 0) {
    map.delete(threadId);
  }
}

function isActiveTimelineItem(item: Item): boolean {
  return (
    ((item.kind === 'assistant_text' || item.kind === 'thinking') && item.status === 'streaming')
    || (item.kind === 'tool_call' && item.status === 'running' && !item.isBackground)
  );
}

function recalculateThreadStatus(threadId: string): void {
  if ((approvalIDsByThread.get(threadId)?.size ?? 0) > 0) {
    setThreadStatus(threadId, 'pending-approval');
    return;
  }
  if ((activeItemIDsByThread.get(threadId)?.size ?? 0) > 0) {
    setThreadStatus(threadId, 'running');
    return;
  }
  if (errorThreads.has(threadId)) {
    setThreadStatus(threadId, 'error');
    return;
  }
  setThreadStatus(threadId, 'idle');
}

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
  activeItemIDsByThread.delete(threadId);
  const approvalIDs = approvalIDsByThread.get(threadId);
  if (approvalIDs) {
    for (const requestId of approvalIDs) {
      approvalThreadByID.delete(requestId);
    }
    approvalIDsByThread.delete(threadId);
  }
  errorThreads.delete(threadId);
  if (!statuses.has(threadId)) return;
  const next = new Map(statuses);
  next.delete(threadId);
  statuses = next;
}

/**
 * Feed a live item upsert into the sidebar-status projection. This is the
 * canonical chat-state path: running rows keep the dot hot, terminal rows
 * clear their own ids, and inline error rows leave the thread in an error
 * state until a new active item or user turn supersedes it.
 */
export function projectThreadItem(item: Item): void {
  if (!item?.threadId || !item.id) return;

  if (item.kind === 'error') {
    errorThreads.add(item.threadId);
  } else if (item.kind === 'user_text' || isActiveTimelineItem(item)) {
    errorThreads.delete(item.threadId);
  }

  if (isActiveTimelineItem(item)) {
    trackedIDsFor(activeItemIDsByThread, item.threadId).add(item.id);
  } else {
    removeTrackedID(activeItemIDsByThread, item.threadId, item.id);
  }

  recalculateThreadStatus(item.threadId);
}

/**
 * Feed an approval request into the projection. Pending approval dominates
 * every other status because the thread is blocked on the user.
 */
export function projectApprovalRequest(threadId: string, requestId: string): void {
  if (!threadId || !requestId) return;
  approvalThreadByID.set(requestId, threadId);
  trackedIDsFor(approvalIDsByThread, threadId).add(requestId);
  errorThreads.delete(threadId);
  recalculateThreadStatus(threadId);
}

/**
 * Feed an approval resolution into the projection. Resolve payloads should
 * include threadId, but we also track requestId -> threadId so late or
 * partial events still clean up correctly.
 */
export function projectApprovalResolution(
  threadId: string | null | undefined,
  requestId: string,
): void {
  if (!requestId) return;
  const ownerThreadId = threadId ?? approvalThreadByID.get(requestId);
  if (!ownerThreadId) return;
  approvalThreadByID.delete(requestId);
  removeTrackedID(approvalIDsByThread, ownerThreadId, requestId);
  recalculateThreadStatus(ownerThreadId);
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
  activeItemIDsByThread.clear();
  approvalIDsByThread.clear();
  approvalThreadByID.clear();
  errorThreads.clear();
  if (statuses.size === 0) return;
  statuses = new Map();
}
