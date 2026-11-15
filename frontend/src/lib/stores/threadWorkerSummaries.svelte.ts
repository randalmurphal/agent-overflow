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
// Only threads with open workers are held. A thread's own sequence is kept
// only while an older read is still out; the watermark covers the rest.
// Each thread is its own key (keyedSignalRegistry), so a frame reruns only
// the row of the thread it names.

import { GetThreadWorkerSummary, ListThreadWorkerSummaries, type ThreadWorkerSummary } from './bindings';
import { onThreadAgentRowsChanged } from './eventsThreadAgentRows';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
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

/** Lands one thread's answer from the read that started at `seq`, unless a later read has. */
function land(threadId: string, backend: BackendKey, seq: number, open: number, needYou: number): void {
  if (seq < (snapshots.get(backend) ?? 0)) return;
  if ((landed.get(threadId) ?? 0) > seq) return;
  landed.set(threadId, seq);
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
 * however late. Errors reach the console only: the counts are decoration
 * on rows that work without them.
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
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load thread worker summaries:', err);
  } finally {
    endRead(seq);
  }
}

const scheduler = createRefreshScheduler({
  name: 'threadWorkerSummaries',
  delayMs: REFRESH_DELAY_MS,
  maxWaitMs: REFRESH_MAX_WAIT_MS,
  run: async (token) => {
    const reads = [...due];
    due.clear();
    await Promise.all(reads.map(async ([threadId, backend]) => {
      const seq = beginRead();
      const current = backendLifetime(backend);
      try {
        const summary = await withBackendTarget(backend, () => GetThreadWorkerSummary(threadId));
        if (!token.isCurrent() || !current()) return;
        land(threadId, backend, seq, summary.open, summary.needYou);
      } catch (err) {
        // A failed read says nothing about the workers: the row keeps
        // what it showed until a read succeeds.
        if (!isPassiveConnectionFailure(err)) console.error(`Failed to load the worker summary of ${threadId}:`, err);
      } finally {
        endRead(seq);
      }
    }));
  },
});

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
  snapshots.delete(backendId as BackendKey);
  for (const [threadId, owner] of [...owners]) {
    if (owner !== backendId) continue;
    owners.delete(threadId);
    landed.delete(threadId);
    counts.drop(threadId);
  }
});

/** Test helper: what the store retains, by kind. */
export function threadWorkerSummaryRetentionForTest(): { counts: number; owners: number; landed: number; snapshots: number } {
  return { counts: [...counts.keys()].length, owners: owners.size, landed: landed.size, snapshots: snapshots.size };
}

/** Test helper. */
export function resetThreadWorkerSummariesForTest(): void {
  scheduler.reset();
  due.clear();
  owners.clear();
  landed.clear();
  snapshots.clear();
  outstanding.clear();
  counts.reset();
  sequence = 0;
}
