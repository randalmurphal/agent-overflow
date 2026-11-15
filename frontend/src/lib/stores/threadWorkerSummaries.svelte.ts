// How many workers each thread has open, and how many of them wait on the
// user, for the sidebar's rows (docs/specs/standing-agents.md, "Sidebar").
// The numbers are the backend's count of the open rows the thread's Agents
// list shows (ThreadWorkerSummary); nothing is derived here.
//
// One bulk read per computer at boot and on resync (ListThreadWorkerSummaries,
// read page by page until its last, which together list only threads with
// an open worker), and one GetThreadWorkerSummary for each thread a
// `thread:agent_rows_changed` frame names, which the backend sends for the
// caller of every changed request and for the threads above it. A burst of
// frames is one read per thread, through one rate-bounded scheduler. A lost
// frame on a computer reads that computer again in bulk. Nothing polls.
//
// Reads are ordered by the sequence number each takes when it starts. A
// thread's answer lands only when no read that started later has landed
// there. A computer's bulk read stands for every thread of that computer,
// those it lists and those it leaves out, but only once its last page has
// answered: it then lists what each thread it read holds, clears each
// thread of that computer it left out, and becomes the computer's
// watermark, so no answer of that computer from a read that started before
// it lands afterwards, a thread's absence included, an older bulk read's
// too. A frame's thread is read again whatever read is out, so a frame
// needs no replay over a bulk answer.
// Every answer is applied only while the computer that gave it is the one
// attached when its read started.
//
// A read that fails is read again, a computer's in bulk and a thread's on
// its own, through the scheduler once a backoff passes (DEFAULT_RETRY, one
// curve that resets once every failed read has answered), until one
// answers or a newer read covers it; meanwhile the row keeps what it
// showed, no badge before the first answer. The failure goes to the
// diagnostics log. A read the connection dropped is left to the reconnect,
// whose resync reads the computer again.
//
// Only threads with open workers are held. A thread's own sequence is kept
// only while an older read is still out; the watermark covers the rest.
// Each thread is its own key (keyedSignalRegistry), so a frame reruns only
// the row of the thread it names.

import { GetThreadWorkerSummary, ListThreadWorkerSummaries, type ThreadWorkerSummary } from './bindings';
import { DEFAULT_RETRY } from './entityStore.svelte';
import { onThreadAgentRowsChanged } from './eventsThreadAgentRows';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { errString } from '../utils/errors';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { attachedBackends, backendById, onBackendDetached, withBackendTarget } from '../transport/backends';
import { getBackendIdentity } from '../transport/backendIdentity';
import { isPassiveConnectionFailure } from '../transport/passiveReadFailure';
import type { BackendKey } from '../transport/backendKey';
import { createRefreshScheduler } from '../utils/refreshScheduler';

/** A thread's open workers and those of them waiting on the user. */
export interface ThreadWorkerCounts {
  readonly open: number;
  readonly needYou: number;
}

const NO_WORKERS: ThreadWorkerCounts = Object.freeze({ open: 0, needYou: 0 });
const REFRESH_DELAY_MS = 100;
const REFRESH_MAX_WAIT_MS = 400;

const counts = createKeyedSignalRegistry<ThreadWorkerCounts>(NO_WORKERS);
/** The computer that answered for each thread with open workers. */
const owners = new Map<string, BackendKey>();
/** The start sequence of the newest read that landed on each thread, while an older read is out. */
const landed = new Map<string, number>();
/** The start sequence of each computer's newest bulk read whose every page answered. */
const snapshots = new Map<BackendKey, number>();
/** The start sequences of the reads still out. */
const outstanding = new Set<number>();
let sequence = 0;
/** Threads a frame named since their last read, with the frame's computer. */
const due = new Map<string, BackendKey>();
/** Computers the next scheduler run reads in bulk. */
const bulkDue = new Set<BackendKey>();
/**
 * Threads, with their computer, and computers whose read failed, by that
 * read's start sequence. A failure stands until a read that started no
 * earlier answers: an older read's answer predates what the failed one
 * went to read.
 */
const failedThreads = new Map<string, { backend: BackendKey; seq: number }>();
const failedComputers = new Map<BackendKey, number>();
let retryDelayMs: number = DEFAULT_RETRY.initialMs;
let retryTimer: ReturnType<typeof setTimeout> | null = null;

/** The thread's counts; zero for a thread with no open worker. */
export function getThreadWorkerCounts(threadId: string): ThreadWorkerCounts {
  return counts.get(threadId);
}

function beginRead(): number {
  const seq = ++sequence;
  outstanding.add(seq);
  return seq;
}

/** Ends a read, and forgets each thread's sequence that no read still out could undercut. */
function endRead(seq: number): void {
  outstanding.delete(seq);
  let oldest = Infinity;
  for (const out of outstanding) oldest = Math.min(oldest, out);
  for (const [threadId, at] of landed) if (at <= oldest) landed.delete(threadId);
}

/**
 * Whether the computer that answers is still the one attached when the read
 * started: not detached and attached again, and not another backend behind
 * the same key.
 */
function backendLifetime(backend: BackendKey): () => boolean {
  const entry = backendById(backend);
  const identity = getBackendIdentity(backend);
  return () => {
    if (entry === undefined || backendById(backend) !== entry) return false;
    const current = getBackendIdentity(backend);
    return !identity.backendId || (identity.backendId === current.backendId && identity.generation === current.generation);
  };
}

/** Whether a read of the thread that started after `seq` has landed, its computer's bulk read included. */
function covered(threadId: string, backend: BackendKey, seq: number): boolean {
  return seq < (snapshots.get(backend) ?? 0) || (landed.get(threadId) ?? 0) > seq;
}

/** Reports a failed read and arms the retry of every failed read, once the backoff passes. */
function failed(what: string, backend: BackendKey, err: unknown): void {
  const detail = `${what} on ${backend}: ${errString(err)}`;
  reportFrontendDiagnostic('threadWorkerSummaries: read failed', detail);
  // A session that is not the host's cannot persist the report.
  console.warn(`threadWorkerSummaries: read failed: ${detail}`);
  if (retryTimer !== null || (failedThreads.size === 0 && failedComputers.size === 0)) return;
  retryTimer = setTimeout(() => {
    retryTimer = null;
    for (const [threadId, read] of failedThreads) due.set(threadId, read.backend);
    for (const owner of failedComputers.keys()) bulkDue.add(owner);
    scheduler.request({ immediate: true });
  }, retryDelayMs);
  retryDelayMs = Math.min(retryDelayMs * 2, DEFAULT_RETRY.maxMs);
}

/** Resets the backoff once no failed read is left to read again. */
function recovered(): void {
  if (failedThreads.size > 0 || failedComputers.size > 0) return;
  if (retryTimer !== null) clearTimeout(retryTimer);
  retryTimer = null;
  retryDelayMs = DEFAULT_RETRY.initialMs;
}

/** Clears the thread's failed read once a read of that computer that started no earlier answered for it. */
function answeredFor(threadId: string, backend: BackendKey, seq: number): void {
  const read = failedThreads.get(threadId);
  if (read && read.backend === backend && read.seq <= seq) failedThreads.delete(threadId);
}

/** Lands one thread's answer from the read that started at `seq`, unless a later read has. */
function land(threadId: string, backend: BackendKey, seq: number, open: number, needYou: number): void {
  if (covered(threadId, backend, seq)) return;
  landed.set(threadId, seq);
  answeredFor(threadId, backend, seq);
  if (open <= 0) {
    owners.delete(threadId);
    counts.drop(threadId);
    return;
  }
  owners.set(threadId, backend);
  const current = counts.get(threadId);
  if (current.open === open && current.needYou === needYou) return;
  counts.set(threadId, { open, needYou });
}

/**
 * Boot-time and resync load: every attached computer, or `only`. Each
 * computer's read stands alone and settles when that computer answers,
 * however late. A failure is read again on the backoff; the counts are
 * decoration on rows that work without them, so a row carries no error.
 */
export async function refreshThreadWorkerSummaries(only?: BackendKey): Promise<void> {
  const targets = attachedBackends().filter((entry) => only === undefined || entry.id === only);
  await Promise.all(targets.map((entry) => readComputer(entry.id)));
}

async function readComputer(backend: BackendKey): Promise<void> {
  const status = backendById(backend)?.status.status;
  // An offline computer's reconnect ladder owns its retries, and its first
  // hello reads it again.
  if (status !== 'connected' && status !== 'disconnected') return;
  const seq = beginRead();
  const current = backendLifetime(backend);
  try {
    const listed = new Map<string, ThreadWorkerSummary>();
    let after = '';
    for (;;) {
      const page = await withBackendTarget(backend, () => ListThreadWorkerSummaries(after));
      if (!current()) return;
      for (const row of page?.summaries ?? []) listed.set(row.threadId, row);
      const next = page?.next ?? '';
      if (next === '') break;
      if (next === after) throw new Error(`the worker summaries after ${after} repeat their cursor`);
      after = next;
    }
    if (seq < (snapshots.get(backend) ?? 0)) return;
    for (const row of listed.values()) land(row.threadId, backend, seq, row.open, row.needYou);
    // The computer listed every thread with an open worker: one it left
    // out has none.
    for (const [threadId, owner] of [...owners]) {
      if (owner === backend && !listed.has(threadId)) land(threadId, backend, seq, 0, 0);
    }
    snapshots.set(backend, seq);
    // The computer's answer stands for every thread of it.
    if ((failedComputers.get(backend) ?? 0) <= seq) failedComputers.delete(backend);
    for (const threadId of [...failedThreads.keys()]) answeredFor(threadId, backend, seq);
    recovered();
  } catch (err) {
    if (isPassiveConnectionFailure(err) || !current()) return;
    if (seq >= (snapshots.get(backend) ?? 0)) failedComputers.set(backend, Math.max(seq, failedComputers.get(backend) ?? 0));
    failed('the bulk read', backend, err);
  } finally {
    endRead(seq);
  }
}

const scheduler = createRefreshScheduler({
  name: 'threadWorkerSummaries',
  delayMs: REFRESH_DELAY_MS,
  maxWaitMs: REFRESH_MAX_WAIT_MS,
  run: async (token) => {
    const computers = [...bulkDue];
    bulkDue.clear();
    const reads = [...due];
    due.clear();
    await Promise.all([...computers.map(readComputer), ...reads.map(async ([threadId, backend]) => {
      const seq = beginRead();
      const current = backendLifetime(backend);
      try {
        const summary = await withBackendTarget(backend, () => GetThreadWorkerSummary(threadId));
        if (!token.isCurrent() || !current()) return;
        land(threadId, backend, seq, summary.open, summary.needYou);
        recovered();
      } catch (err) {
        // A failed read says nothing about the workers: the row keeps
        // what it showed until a read succeeds.
        // Nor does one a newer read covered, a deleted thread's included.
        if (isPassiveConnectionFailure(err) || !token.isCurrent() || !current() || covered(threadId, backend, seq)) return;
        failedThreads.set(threadId, { backend, seq: Math.max(seq, failedThreads.get(threadId)?.seq ?? 0) });
        failed(`the read of ${threadId}`, backend, err);
      } finally {
        endRead(seq);
      }
    })]);
  },
});

/**
 * A thread was deleted: its counts go, a read queued for it never starts,
 * and no read of it still out lands or is retried. The forget is a read of
 * its own that found nothing, so every read that started before it is
 * older than it.
 */
export function forgetThreadWorkerSummary(threadId: string): void {
  due.delete(threadId);
  failedThreads.delete(threadId);
  recovered();
  const seq = beginRead();
  landed.set(threadId, seq);
  owners.delete(threadId);
  counts.drop(threadId);
  endRead(seq);
}

onThreadAgentRowsChanged((change, backend) => {
  if (change === null) {
    void refreshThreadWorkerSummaries(backend as BackendKey);
    return;
  }
  due.set(change.threadId, backend as BackendKey);
  scheduler.request();
});

onBackendDetached(({ backendId }) => {
  for (const [threadId, owner] of [...due]) if (owner === backendId) due.delete(threadId);
  for (const [threadId, read] of [...failedThreads]) if (read.backend === backendId) failedThreads.delete(threadId);
  failedComputers.delete(backendId as BackendKey);
  bulkDue.delete(backendId as BackendKey);
  recovered();
  snapshots.delete(backendId as BackendKey);
  for (const [threadId, owner] of [...owners]) {
    if (owner !== backendId) continue;
    owners.delete(threadId);
    landed.delete(threadId);
    counts.drop(threadId);
  }
});

/** Test helper: what the store retains, by kind. */
export function threadWorkerSummaryRetentionForTest(): { counts: number; owners: number; landed: number; snapshots: number; failed: number } {
  return {
    counts: [...counts.keys()].length,
    owners: owners.size,
    landed: landed.size,
    snapshots: snapshots.size,
    failed: failedThreads.size + failedComputers.size,
  };
}

/** Test helper. */
export function resetThreadWorkerSummariesForTest(): void {
  scheduler.reset();
  due.clear();
  bulkDue.clear();
  failedThreads.clear();
  failedComputers.clear();
  recovered();
  owners.clear();
  landed.clear();
  snapshots.clear();
  outstanding.clear();
  counts.reset();
  sequence = 0;
}
