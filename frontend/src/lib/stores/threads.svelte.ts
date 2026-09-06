import { reconcileThreadRows } from './eventsThreadRows';
import { computerCatalogWriter } from './computerCatalogWriter';
import { computerCatalog, readComputerRows, retainUnavailableComputerRows } from './computerRows';
import { currentThreadRow, noteThread, onThreadOwnershipChanged, threadBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { clearPayloadCacheForThread } from '../utils/payloadDataCache';
import { clearThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { clearThreadSizePriors } from '../utils/virtual/priors';
import { evictDiffSpansForThread } from '../utils/diffSpanCache.svelte';
import { clearItemProjectionSourcesForThread } from '../utils/itemProjectionSource.svelte';
import { ListThreads, MarkThreadRead, MarkThreadUnread } from './bindings';
import { dropActivityRailUiPrefs, dropLiveTodoUiPrefs } from './liveTodoState.svelte';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { invalidateReplicaCatalog } from '../replica/session';
import { dropThreadHistoryStamp } from './threadHistoryStamps';
import { clearLiveUsageSnapshot } from './threadContextWindow';
import { clearThreadStatus } from './threadStatuses.svelte';
import { addToast } from './toast.svelte';
import { releaseThreadTerminalState } from '../components/terminal/terminalStore.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { onBackendDetached } from '../transport/backends';
import { withLocalReadMarker } from './threadReadWrites';

type ThreadReadStatePatch = Partial<Pick<Thread, 'lastReadAt' | 'hasIncompleteTurn'>>;

let threads: Thread[] = $state([]);
const catalogWriter = computerCatalogWriter('threads', () => threads, (row) => threadBackend(row.id));
// Ownership changes before the destination's row is applied. Invalidate the
// former owner's pending reads and rewrite its catalog without the moved row;
// otherwise removing the destination could expose the old offline snapshot.
onThreadOwnershipChanged((_id, previousBackend) => {
  invalidateReplicaCatalog(previousBackend, 'threads');
  catalogWriter.changed(previousBackend);
});

// Live-activity bumps arrive on every streaming flush (tens per second
// while a turn runs). They are FIELD patches, not membership or order
// changes, so they live in a per-thread box and the `threads` array
// signal stays silent for them. Rewriting the array here was the
// per-beat trigger for the sidebar's animated each-blocks: every flush
// re-derived every project's thread tree and svelte's FLIP measure pass
// then forced synchronous layout twice per visible row (2026-08-26,
// perf-investigation REFERENCE.md). Readers that need the live value —
// tree sort, time labels, project ranking, the row-sync merge — read
// through getThreadLiveActivityAt; the durable row catches up on the
// next full row sync.
const liveActivityAt = createKeyedSignalRegistry<number>(0);

export function getThreads(): Thread[] {
  return threads;
}

// Only outstanding reads retain the latest promise; startup follows a newer
// reconnect snapshot before validating saved pane IDs.
let latestThreadRead: Promise<Thread[]> | null = null;
let pendingThreadReads = 0;

export async function readThreadRows(): Promise<Thread[]> {
  pendingThreadReads++;
  const request = readCurrentThreadRows();
  latestThreadRead = request;
  try { return await request; }
  finally { if (--pendingThreadReads === 0) latestThreadRead = null; }

  async function readCurrentThreadRows(): Promise<Thread[]> {
    const result = await readComputerRows<Thread>(
      async () => await ListThreads() as Thread[], (row, backend) => { noteThread(row.id, backend, row.ownershipEpoch ?? 0); }, computerCatalog('threads', () => threads, (row) => threadBackend(row.id), (late) => {
        reconcileThreadRows(currentRows(retainUnavailableComputerRows(threads, late, (row) => threadBackend(row.id))));
      }), currentThreadRow);
    // Startup must validate saved panes against the winning snapshot, even
    // when a first hello superseded its read. A local mutation with no newer
    // read already lives in `threads`. Retain the latest promise only while
    // reads are outstanding; no second persistent catalog is needed.
    if (!result) return latestThreadRead && latestThreadRead !== request ? latestThreadRead : currentRows(threads);
    return currentRows(retainUnavailableComputerRows(threads, result, (row) => threadBackend(row.id)));
  }
}

function currentRows(rows: Thread[]): Thread[] {
  const seen = new Set<string>();
  return rows.filter((row) => {
    if (!currentThreadRow(row) || seen.has(row.id)) return false;
    seen.add(row.id);
    return true;
  });
}

export async function loadThreads(): Promise<Thread[]> {
  threads = await readThreadRows();
  liveActivityAt.reset();
  return threads;
}

export async function refreshThreads(): Promise<void> {
  try {
    await loadThreads();
  } catch (err) {
    console.error('Failed to load threads:', err);
    addToast('error', 'Failed to load threads');
  }
}

/**
 * Wholesale registry replacement for resync paths. The caller owns the
 * merge policy — rows must already be reconciled against local state
 * (see eventsThreadRows.resyncThreadRows); this setter stays dumb so
 * that policy lives in one place.
 */
export function replaceAllThreads(rows: Thread[], mutation = true): void {
  for (const backend of new Set([...threads, ...rows].map((row) => threadBackend(row.id)))) catalogWriter.changed(backend, mutation);
  threads = rows;
  // Callers hand rows already reconciled against local state (including
  // the live-activity box, via mergeThreadRowWithLocal), so the boxes'
  // content is folded into the rows and stale entries can go.
  liveActivityAt.reset();
}

export function prependThread(thread: Thread): void {
  catalogWriter.changed(threadBackend(thread.id));
  threads = [thread, ...threads.filter((t) => t.id !== thread.id)];
}

export function removeThread(id: string): void {
  invalidateReplicaCatalog(threadBackend(id) ?? '', 'threads');
  catalogWriter.changed(threadBackend(id));
  threads = threads.filter((t) => t.id !== id);
  liveActivityAt.drop(id);
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
  // Durable counterpart of the same eviction: a deleted thread must not
  // leave a paintable window (or a stamp claiming one) behind in
  // IndexedDB, where it would outlive the process.
  void removeReplicaWindow(id);
  dropThreadHistoryStamp(id);
  clearThreadScrollSnapshot(id);
  clearThreadSizePriors(id);
  evictDiffSpansForThread(id);
  clearItemProjectionSourcesForThread(id);
  clearPayloadCacheForThread(id);
  clearLiveUsageSnapshot(id);
  releaseThreadTerminalState(id);
}

export function updateThreadTitle(id: string, title: string): void {
  catalogWriter.changed(threadBackend(id));
  threads = threads.map((t) => t.id === id ? { ...t, title } : t);
}

export function updateThreadModel(id: string, model: string): void {
  catalogWriter.changed(threadBackend(id));
  threads = threads.map((t) => t.id === id ? { ...t, model } : t);
}

export function replaceThread(thread: Thread): void {
  catalogWriter.changed(threadBackend(thread.id));
  threads = threads.map((t) => t.id === thread.id ? thread : t);
}

/**
 * Bump a cached thread's activity timestamp when a live provider event
 * proves the backend touched it. Returns the current cached row so callers
 * can reconcile project-level projections without re-scanning the list.
 */
export function touchThreadActivity(id: string, updatedAt: number): Thread | undefined {
  if (!id || !Number.isFinite(updatedAt)) return undefined;
  const existing = threads.find((t) => t.id === id);
  if (existing === undefined) return undefined;

  if (getThreadLiveActivityAt(existing) < updatedAt) {
    liveActivityAt.set(id, updatedAt);
  }
  return existing;
}

/**
 * The thread's newest activity timestamp: the durable row value or the
 * live streaming bump, whichever is ahead. Reactive on the per-thread
 * box, so a reader wakes only for its own thread's beats.
 */
export function getThreadLiveActivityAt(thread: Pick<Thread, 'id' | 'updatedAt'>): number {
  return Math.max(thread.updatedAt ?? 0, liveActivityAt.get(thread.id));
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
  catalogWriter.changed(threadBackend(id));
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
 * Persist the read stamp this client just applied locally.
 *
 * The RPC and the claim are one call because they have to be: the claim
 * is what tells `mergeThreadRowWithLocal` that a row arriving mid-flight
 * from another client's mark-unread is older than what this page did,
 * and a caller that could make the write without it would reintroduce
 * exactly the merge the claim exists to settle.
 *
 * Fire-and-forget by contract — a failed read stamp is a sidebar pill
 * that lingers, not something to interrupt anyone over — so the caller
 * gets the promise and the rejection is logged here.
 */
export function markThreadRead(id: string, lastReadAt: number): Promise<void> {
  return withLocalReadMarker(id, lastReadAt, () => MarkThreadRead(id))
    .catch((err) => {
      console.error('Failed to mark thread read:', err);
    });
}

/**
 * Mark a thread unread and patch the local row to match.
 *
 * Explicit unread is persisted as epoch 0; `undefined` means "never
 * tracked" and is deliberately read as read, for rows that predate the
 * column. The claim spans the RPC AND the local patch, because the
 * window a wire row can land in covers both halves — 0 is the smallest
 * value the field takes, so nothing downstream can tell it from a stale
 * one on the numbers alone.
 */
export async function markThreadUnread(id: string): Promise<void> {
  await withLocalReadMarker(id, 0, async () => {
    await MarkThreadUnread(id);
    updateThreadLastRead(id, 0);
  });
}

/**
 * Patches the complete pin state in one array replacement so a group move or
 * unpin cannot expose a transient mismatched pinnedAt / pinGroup pair.
 */
export function updateThreadPinState(
  id: string,
  pinnedAt: number | undefined,
  pinGroup: number | undefined,
): void {
  catalogWriter.changed(threadBackend(id));
  threads = threads.map((t) =>
    t.id === id ? { ...t, pinnedAt, pinGroup } : t,
  );
}

/**
 * Reconcile the rows SetThreadGroup returned — every thread the call
 * touched, discussion children included. Only the three fields that RPC
 * owns are patched: a full row swap would drag lastReadAt /
 * latestTurnCompletedAt backwards past a local read-mark the debounced
 * persist has not landed yet (the reason mergeThreadRowWithLocal exists).
 * The backend also emits thread:updated `replace` for each row, which is
 * what reaches the panes; this is the instant-feedback half.
 */
export function updateThreadGroupState(rows: readonly Thread[]): void {
  if (rows.length === 0) return;
  for (const backend of new Set(rows.map((row) => threadBackend(row.id)))) catalogWriter.changed(backend);
  const byId = new Map(rows.map((row) => [row.id, row] as const));
  threads = threads.map((t) => {
    const row = byId.get(t.id);
    if (row === undefined) return t;
    return { ...t, groupId: row.groupId, pinnedAt: row.pinnedAt, pinGroup: row.pinGroup };
  });
}

/**
 * Drop a deleted group's membership from every cached row. DeleteThreadGroup
 * nulls `group_id` in SQLite (ON DELETE SET NULL) without emitting a thread
 * row per member, so this is the local half of that write.
 */
export function clearThreadGroupMembership(groupId: string): void {
  if (!groupId) return;
  let changed = false;
  const next = threads.map((t) => {
    if (t.groupId !== groupId) return t;
    changed = true;
    catalogWriter.changed(threadBackend(t.id));
    return { ...t, groupId: undefined };
  });
  if (changed) threads = next;
}

/**
 * Drop every row a detached backend owned.
 *
 * NOT `removeThread`: these threads were not deleted, they merely stopped
 * being reachable from this client, so nothing durable is evicted here.
 * What has to go is the ROW, because the entity index has already forgotten
 * which machine it came from and every call about it would resolve to the
 * page's own backend from now on. A row nobody can route is worse than no
 * row: it looks live and answers wrong.
 *
 * The ids arrive in the detach payload rather than being read back from
 * the index, which is what makes this ordering-free
 * (`transport/backends.ts`, `BackendDetachment`).
 */
export function dropThreadsForDetachedBackend(ids: readonly string[]): void {
  if (ids.length === 0) return;
  const gone = new Set(ids);
  const kept = threads.filter((t) => !gone.has(t.id));
  if (kept.length === threads.length) return;
  threads = kept;
  for (const id of gone) {
    liveActivityAt.drop(id);
    clearThreadStatus(id);
    dropLiveTodoUiPrefs(id);
    dropActivityRailUiPrefs(id);
  }
}

onBackendDetached(({ threadIds }) => dropThreadsForDetachedBackend(threadIds));

/**
 * Returns the thread with the given id, or undefined if the sidebar doesn't
 * currently track it (e.g. archived parent not in the filtered view).
 */
export function getThreadById(id: string): Thread | undefined {
  return threads.find((t) => t.id === id);
}
