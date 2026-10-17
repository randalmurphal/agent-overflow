import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import { getAllPanes } from './panes.svelte';
import { reconcileThreadRows } from './eventsThreadRows';
import { computerCatalogWriter } from './computerCatalogWriter';
import { computerCatalog, readComputerRows, retainUnavailableComputerRows, type ComputerRows } from './computerRows';
import { currentThreadRow, noteThread, onThreadOwnershipChanged, threadBackend, threadGroupBackend } from '../transport/entityIndex';
import type { Thread } from '../types/models';
import { clearPayloadCacheForThread } from '../utils/payloadDataCache';
import { hasUnread } from '../utils/threadStatusPill';
import { clearThreadScrollSnapshot } from '../utils/threadScrollSnapshots';
import { clearThreadSizePriors } from '../utils/virtual/priors';
import { evictDiffSpansForThread } from '../utils/diffSpanCache.svelte';
import { clearItemProjectionSourcesForThread } from '../utils/itemProjectionSource.svelte';
import { ListThreads, MarkThreadRead, MarkThreadsRead, MarkThreadUnread } from './bindings';
import { dropActivityRailUiPrefs, dropLiveTodoUiPrefs } from './liveTodoState.svelte';
import { threadItemCache } from './threadItemCache';
import { removeReplicaWindow } from '../replica';
import { invalidateReplicaCatalog } from '../replica/session';

import { clearLiveUsageSnapshot } from './threadContextWindow';
import { clearThreadStatus } from './threadStatuses.svelte';
import { addErrorToast } from './toast.svelte';
import { forgetAgentSidebarState } from './sidebar.svelte';
import { forgetArchivedChildren } from './agentArchivedChildren.svelte';
import { notifyStandingAgentEnded } from './standingAgentEnded';
import { releaseThreadTerminalState } from '../components/terminal/terminalStore.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { onBackendDetached, withBackendTarget } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { claimLocalReadMarker, withLocalReadMarker } from './threadReadWrites';
import { threadReplyRow, threadRowFrameMark, threadRowFramedSince } from './threadRowFrames';
import { registerCatalogReader, settleCatalogAnswers } from './catalogLoad.svelte';

type ThreadReadStatePatch = Partial<Pick<Thread, 'lastReadAt' | 'hasIncompleteTurn' | 'hasFailedTurn'>>;

let threads: Thread[] = $state([]);
const catalogWriter = computerCatalogWriter('threads', () => threads, setThreads, (row) => threadBackend(row.id));

// Every write to the list passes here except dropThreadsForDetachedBackend,
// so a thread that stops being an assistant's current thread in this list
// (handed off, archived, deleted, or replaced by a row without the mark)
// takes its agent-only state with it, and an assistant with no current
// thread left in the list takes its sidebar state (keyed by the assistant,
// so a handoff keeps it). A computer detaching keeps that state: its
// assistants are still assistants, and the persisted fold must survive.
function setThreads(rows: Thread[]): void {
  const before = threads;
  rows = oneCurrentThreadPerAssistant(before, rows);
  threads = rows;
  let currentThreadOf: Map<string, string> | null = null;
  for (const row of before) {
    if (!row.agentId) continue;
    currentThreadOf ??= new Map(rows.flatMap((t) => (t.agentId ? [[t.agentId, t.id] as const] : [])));
    const current = currentThreadOf.get(row.agentId);
    if (current === row.id) continue;
    forgetArchivedChildren(row.id);
    // The wakes store reads this one, so it listens rather than being called.
    notifyStandingAgentEnded(row.id);
    if (current === undefined) forgetAgentSidebarState(row.agentId);
  }
}

/**
 * An assistant has one current thread. A handoff lists the new thread
 * before the old one leaves, so a write that holds two current threads of
 * one assistant keeps the one that was not current before (the newest when
 * both are new) and holds the other as the past thread it now is.
 */
function oneCurrentThreadPerAssistant(before: readonly Thread[], rows: Thread[]): Thread[] {
  let current: Map<string, Thread> | null = null;
  let duplicated = false;
  for (const row of rows) {
    if (!row.agentId) continue;
    current ??= new Map();
    const held = current.get(row.agentId);
    if (held === undefined) {
      current.set(row.agentId, row);
      continue;
    }
    duplicated = true;
    const wasCurrent = (t: Thread) => before.some((b) => b.id === t.id && b.agentId === t.agentId);
    const heldWas = wasCurrent(held);
    if (heldWas !== wasCurrent(row) ? heldWas : row.createdAt > held.createdAt) current.set(row.agentId, row);
  }
  if (!duplicated) return rows;
  return rows.map((row) => (row.agentId && current!.get(row.agentId) !== row
    ? { ...row, agentId: undefined, pastAgentId: row.agentId }
    : row));
}
// Ownership changes before the destination's row is applied. Fence the
// former owner's offline snapshot and rewrite it without the moved row;
// otherwise removing the destination could expose the old offline snapshot.
// A read of the former owner still in flight needs nothing: admission
// (currentThreadRow) drops the moved row from its answer.
onThreadOwnershipChanged((_id, previousBackend) => {
  invalidateReplicaCatalog(previousBackend, 'threads');
  catalogWriter.persist(previousBackend);
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
let latestThreadRead: Promise<void> | null = null;
let pendingThreadReads = 0;

async function listThreadRows(): Promise<Thread[]> {
  return await ListThreads() as Thread[];
}

function noteThreadRow(row: Thread, backend: BackendKey): void {
  noteThread(row.id, backend, row.ownershipEpoch ?? 0);
}

function threadRowOwner(row: Thread): BackendKey | undefined {
  return threadBackend(row.id);
}

// Every thread answer, on time, late or retried, merges with live local
// state: a read mark or completion newer than the snapshot survives it.
function commitThreadRows(result: ComputerRows<Thread>): void {
  reconcileThreadRows(currentRows(retainUnavailableComputerRows(threads, result, threadRowOwner)));
  settleCatalogAnswers('threads', result.answered);
}

function threadReadOptions(only?: BackendKey) {
  const catalog = computerCatalog('threads', () => threads, threadRowOwner);
  return only === undefined
    ? { catalog, admit: currentThreadRow }
    : { catalog, admit: currentThreadRow, only, deadlineMs: null };
}

/**
 * Read every computer's threads and commit them. Resolves with the catalog
 * once this read, or a newer one that superseded it, has committed:
 * startup validates saved panes against the winning snapshot, even when a
 * first hello superseded its read.
 */
export async function loadThreads(): Promise<Thread[]> {
  pendingThreadReads++;
  const request = readCurrentThreadRows();
  latestThreadRead = request;
  try { await request; }
  finally { if (--pendingThreadReads === 0) latestThreadRead = null; }
  return threads;

  async function readCurrentThreadRows(): Promise<void> {
    if (await readComputerRows<Thread>(listThreadRows, noteThreadRow, commitThreadRows, threadReadOptions())) return;
    if (latestThreadRead && latestThreadRead !== request) await latestThreadRead;
  }
}

// The catalog store's retry for a computer whose threads have not loaded:
// its rows alone, waiting for the answer rather than the startup deadline.
async function retryThreadCatalog(backend: BackendKey): Promise<void> {
  try {
    await readComputerRows<Thread>(listThreadRows, noteThreadRow, commitThreadRows, threadReadOptions(backend));
  } catch {
    // readComputerRows settled this computer's failure into the catalog
    // state, which is where it is shown and retried.
  }
}

registerCatalogReader('threads', retryThreadCatalog);

function currentRows(rows: Thread[]): Thread[] {
  const seen = new Set<string>();
  return rows.filter((row) => {
    if (!currentThreadRow(row) || seen.has(row.id)) return false;
    seen.add(row.id);
    return true;
  });
}

export async function refreshThreads(): Promise<void> {
  try {
    await loadThreads();
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load threads:', err);
    addErrorToast('Failed to load threads', err);
  }
}

/**
 * Wholesale registry replacement for catalog answers. The caller owns the
 * merge policy — rows must already be reconciled against local state
 * (see eventsThreadRows.reconcileThreadRows); this setter stays dumb so
 * that policy lives in one place.
 */
export function replaceAllThreads(rows: Thread[]): void {
  for (const backend of new Set([...threads, ...rows].map((row) => threadBackend(row.id)))) catalogWriter.persist(backend);
  setThreads(rows);
  // Callers hand rows already reconciled against local state (including
  // the live-activity box, via mergeThreadRowWithLocal), so the boxes'
  // content is folded into the rows and stale entries can go.
  liveActivityAt.reset();
}

/** Test-only: an empty catalog with no live-activity bumps, without the
 *  catalog write a replacement records. */
export function resetThreadsForTest(): void {
  setThreads([]);
  liveActivityAt.reset();
}

export function prependThread(thread: Thread): void {
  catalogWriter.mutate(threadBackend(thread.id), (rows) => [thread, ...rows.filter((t) => t.id !== thread.id)]);
}

/**
 * `prependThread` for a row an RPC returned, from a call started at `mark`
 * (threadRowFrameMark): a thread:updated frame that arrived during the call
 * (the new row's `listed` echo, a later write) is kept over the reply
 * (threadRowFrames.ts). Returns the row the sidebar now holds.
 */
export function prependThreadReply(reply: Thread, mark: number): Thread {
  const held = getThreadById(reply.id);
  const row = threadReplyRow(reply, mark, held) ?? held ?? reply;
  prependThread(row);
  return row;
}

let threadRemovedObservers: Array<(id: string) => void> = [];

/**
 * Run `observer` after every `removeThread`: the thread was deleted or
 * archived, so per-thread state kept outside this store must go with it.
 */
export function addThreadRemovedObserver(observer: (id: string) => void): () => void {
  threadRemovedObservers = [...threadRemovedObservers, observer];
  return () => {
    threadRemovedObservers = threadRemovedObservers.filter((existing) => existing !== observer);
  };
}

export function removeThread(id: string): void {
  invalidateReplicaCatalog(threadBackend(id) ?? '', 'threads');
  catalogWriter.mutate(threadBackend(id), (rows) => rows.filter((t) => t.id !== id));
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
  clearThreadScrollSnapshot(id);
  clearThreadSizePriors(id);
  evictDiffSpansForThread(id);
  clearItemProjectionSourcesForThread(id);
  clearPayloadCacheForThread(id);
  clearLiveUsageSnapshot(id);
  releaseThreadTerminalState(id);
  for (const observer of threadRemovedObservers) observer(id);
}

export function updateThreadTitle(id: string, title: string): void {
  catalogWriter.mutate(threadBackend(id), (rows) => rows.map((t) => t.id === id ? { ...t, title } : t));
}

export function updateThreadModel(id: string, model: string): void {
  catalogWriter.mutate(threadBackend(id), (rows) => rows.map((t) => t.id === id ? { ...t, model } : t));
}

export function replaceThread(thread: Thread): void {
  catalogWriter.mutate(threadBackend(thread.id), (rows) => rows.map((t) => t.id === thread.id ? thread : t));
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
 * MarkThreadUnread request. `hasIncompleteTurn` and `hasFailedTurn` are
 * included because Interrupted and Failed are also unseen read-state;
 * opening the thread clears them before the next refreshThreads()
 * round-trip.
 */
export function updateThreadReadState(
  id: string,
  patch: ThreadReadStatePatch,
): void {
  catalogWriter.mutate(threadBackend(id), (rows) => rows.map((t) =>
    t.id === id ? { ...t, ...patch } : t,
  ));
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

/** The most threads one MarkThreadsRead call takes (store.MarkThreadsReadLimit). */
export const MARK_THREADS_READ_LIMIT = 500;

/**
 * Mark All Read on an agent row or a group (docs/specs/standing-agents.md
 * ruling 31): MarkThreadsRead on the computer that holds the threads, for
 * every one with something to read, in calls of at most
 * MARK_THREADS_READ_LIMIT. Each read marker is claimed at the stamp this
 * client expects for the length of the calls, and the rows each call
 * changed are reconciled through threadReplyRow, so a frame that arrived
 * meanwhile is not undone. Throws the first call's failure after the rest
 * have run.
 */
export async function markThreadsRead(threads: readonly Thread[]): Promise<void> {
  const owed = threads.filter((thread) => hasUnread(thread) || thread.hasFailedTurn || thread.hasIncompleteTurn);
  if (owed.length === 0) return;
  const now = Date.now();
  const releases = owed.map((thread) => claimLocalReadMarker(thread.id, Math.max(now, thread.latestTurnCompletedAt ?? 0)));
  try {
    const mark = threadRowFrameMark();
    const backend = threadBackend(owed[0].id) ?? HOME_BACKEND;
    const rows: Thread[] = [];
    let failure: unknown = null;
    for (let start = 0; start < owed.length; start += MARK_THREADS_READ_LIMIT) {
      const ids = owed.slice(start, start + MARK_THREADS_READ_LIMIT).map((thread) => thread.id);
      try {
        rows.push(...((await withBackendTarget(backend, () => MarkThreadsRead(ids))) ?? []) as Thread[]);
      } catch (err) {
        failure ??= err;
      }
    }
    for (const reply of rows) {
      const row = threadReplyRow(reply, mark, getThreadById(reply.id));
      if (row === null) continue;
      updateThreadReadState(row.id, {
        lastReadAt: row.lastReadAt,
        hasIncompleteTurn: row.hasIncompleteTurn,
        hasFailedTurn: row.hasFailedTurn,
      });
    }
    if (failure !== null) throw failure;
  } finally {
    for (const release of releases) release();
  }
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
  // threadRowFrameMark() taken before the pin RPC this answers: a whole-row
  // frame since then already carries a pin at least as new (threadRowFrames.ts).
  mark: number,
): void {
  if (threadRowFramedSince(id, mark)) return;
  catalogWriter.mutate(threadBackend(id), (rows) => rows.map((t) =>
    t.id === id ? { ...t, pinnedAt, pinGroup } : t,
  ));
}

/**
 * Reconcile the rows SetThreadGroup or SetThreadPlacement returned: every
 * thread the call touched. Only the four fields those RPCs
 * own are patched: a full row swap would drag lastReadAt /
 * latestTurnCompletedAt backwards past a local read-mark the debounced
 * persist has not landed yet (the reason mergeThreadRowWithLocal exists).
 * The backend also emits thread:updated `replace` for each row, which is
 * what reaches the panes; this is the instant-feedback half.
 */
export function updateThreadGroupState(
  replied: readonly Thread[],
  // threadRowFrameMark() taken before the SetThreadGroup call: a row a
  // whole-row frame has carried since is newer than the reply's.
  mark: number,
): void {
  const rows = replied.filter((row) => !threadRowFramedSince(row.id, mark));
  if (rows.length === 0) return;
  const byId = new Map(rows.map((row) => [row.id, row] as const));
  catalogWriter.mutate(rows.map((row) => threadBackend(row.id)), (current) => current.map((t) => {
    const row = byId.get(t.id);
    if (row === undefined) return t;
    return { ...t, groupId: row.groupId, placement: row.placement, pinnedAt: row.pinnedAt, pinGroup: row.pinGroup };
  }));
}

/**
 * Drop a deleted group's membership from every cached row. A thread leaving
 * a group leaves its pin behind, so the pin goes with it, matching what
 * DeleteThreadGroup writes. The backend's per-member thread:updated frames
 * carry the same values; this is the instant-feedback half.
 */
export function clearThreadGroupMembership(groupId: string): void {
  if (!groupId) return;
  // Empty placeholders have no catalog row but still carry the group intent.
  for (const pane of getAllPanes().values()) {
    if (pane.thread?.groupId === groupId) {
      pane.replaceThread({ ...pane.thread, groupId: undefined, placement: 'standalone', pinnedAt: undefined, pinGroup: undefined });
    }
  }
  // A group's members live on the group's computer; a read of it in flight
  // may carry members this client has not listed yet.
  const backends = [threadGroupBackend(groupId), ...threads.filter((t) => t.groupId === groupId).map((t) => threadBackend(t.id))];
  catalogWriter.mutate(backends, (rows) => rows.map((t) => t.groupId === groupId
    ? { ...t, groupId: undefined, placement: 'standalone', pinnedAt: undefined, pinGroup: undefined }
    : t));
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
  // Every indexed thread, listed or not: live state can be held for a
  // thread that only a pane, a search or a snapshot named.
  for (const id of ids) {
    liveActivityAt.drop(id);
    clearThreadStatus(id);
    dropLiveTodoUiPrefs(id);
    dropActivityRailUiPrefs(id);
  }
  const gone = new Set(ids);
  const kept = threads.filter((t) => !gone.has(t.id));
  if (kept.length !== threads.length) threads = kept;
}

onBackendDetached(({ threadIds }) => dropThreadsForDetachedBackend(threadIds));

/**
 * Returns the thread with the given id, or undefined if the sidebar doesn't
 * currently track it (e.g. archived parent not in the filtered view).
 */
export function getThreadById(id: string): Thread | undefined {
  return threadIndex().byId.get(id);
}

/** The listed current thread of the assistant with this id, if any. */
export function assistantCurrentThread(agentId: string): Thread | undefined {
  return threadIndex().currentByAgent.get(agentId);
}

// The list by thread id and by assistant, for lookups made per event.
// Every write replaces the list's array, so the index is rebuilt on the
// first lookup after one. Reading `threads` keeps a reactive caller
// subscribed to the list, as the scan it replaces did.
let indexedRows: Thread[] | null = null;
let indexes = { byId: new Map<string, Thread>(), currentByAgent: new Map<string, Thread>() };
function threadIndex(): typeof indexes {
  const rows = threads;
  if (rows === indexedRows) return indexes;
  const byId = new Map<string, Thread>();
  const currentByAgent = new Map<string, Thread>();
  for (const row of rows) {
    byId.set(row.id, row);
    if (row.agentId && !currentByAgent.has(row.agentId)) currentByAgent.set(row.agentId, row);
  }
  indexedRows = rows;
  indexes = { byId, currentByAgent };
  return indexes;
}
