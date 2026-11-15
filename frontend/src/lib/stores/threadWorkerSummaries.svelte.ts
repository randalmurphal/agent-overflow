// How many workers each thread has open, and how many of them wait on the
// user, for the sidebar's rows (docs/specs/standing-agents.md, "Sidebar").
// The numbers are the backend's count of the open rows the thread's Agents
// list shows (ThreadWorkerSummary); nothing is derived here.
//
// One bulk read per computer at boot and on resync (ListThreadWorkerSummaries,
// read page by page until its last, which together list only threads with
// an open worker), and one
// GetThreadWorkerSummary for each thread a `thread:agent_rows_changed` frame
// names, which the backend sends for the caller of every changed request
// and for the threads above it. A burst of frames is one read per thread,
// through one rate-bounded scheduler. A lost frame on a computer reads that
// computer again in bulk. Nothing polls.
//
// A newer read supersedes an older one per thread: each read takes a
// sequence number when it starts, and an answer lands on a thread only when
// no read that started later has landed there, so a bulk read that answers
// after a frame's own read never undoes it. A frame's thread is read again
// whatever read is out, so a frame needs no replay over a bulk answer.
//
// Each thread is its own key (keyedSignalRegistry), so a frame reruns only
// the row of the thread it names.

import { GetThreadWorkerSummary, ListThreadWorkerSummaries, type ThreadWorkerSummary } from './bindings';
import { onThreadAgentRowsChanged } from './eventsThreadAgentRows';
import { readComputerRows } from './computerRows';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { attachedBackends, onBackendDetached, withBackendTarget } from '../transport/backends';
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
/** The start sequence of the newest read that landed on each thread. */
const landed = new Map<string, number>();
let sequence = 0;
/** Threads a frame named since their last read, with the frame's computer. */
const due = new Map<string, BackendKey>();

/** The thread's counts; zero for a thread with no open worker. */
export function getThreadWorkerCounts(threadId: string): ThreadWorkerCounts {
  return counts.get(threadId);
}

/** Lands one thread's answer from the read that started at `seq`, unless a later read has. */
function land(threadId: string, backend: BackendKey, seq: number, open: number, needYou: number): void {
  if ((landed.get(threadId) ?? 0) > seq) return;
  landed.set(threadId, seq);
  if (open <= 0) {
    owners.delete(threadId);
    counts.set(threadId, NO_WORKERS);
    return;
  }
  owners.set(threadId, backend);
  const current = counts.get(threadId);
  if (current.open === open && current.needYou === needYou) return;
  counts.set(threadId, { open, needYou });
}

/**
 * Every page of one computer's ListThreadWorkerSummaries, each asked of
 * that computer: only the pages together say which threads have none.
 */
async function readPages(backend: BackendKey): Promise<ThreadWorkerSummary[]> {
  const rows: ThreadWorkerSummary[] = [];
  let after = '';
  for (;;) {
    const page = await withBackendTarget(backend, () => ListThreadWorkerSummaries(after));
    rows.push(...(page?.summaries ?? []));
    const next = page?.next ?? '';
    if (next === '') return rows;
    if (next === after) throw new Error(`the worker summaries after ${after} repeat their cursor`);
    after = next;
  }
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
  const seq = ++sequence;
  try {
    await readComputerRows<ThreadWorkerSummary>(
      async () => await readPages(backend),
      () => {},
      (result) => {
        if (!result.answered.has(backend)) return;
        const listed = new Set<string>();
        for (const row of result.rows) {
          listed.add(row.threadId);
          land(row.threadId, backend, seq, row.open, row.needYou);
        }
        // The computer lists every thread with an open worker: one it
        // left out has none.
        for (const [threadId, owner] of [...owners]) {
          if (owner === backend && !listed.has(threadId)) land(threadId, backend, seq, 0, 0);
        }
      },
      { only: backend, deadlineMs: null },
    );
  } catch (err) {
    if (isPassiveConnectionFailure(err)) return;
    console.error('Failed to load thread worker summaries:', err);
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
      const seq = ++sequence;
      try {
        const summary = await withBackendTarget(backend, () => GetThreadWorkerSummary(threadId));
        if (!token.isCurrent()) return;
        land(threadId, backend, seq, summary.open, summary.needYou);
      } catch (err) {
        // A failed read says nothing about the workers: the row keeps
        // what it showed until a read succeeds.
        if (!isPassiveConnectionFailure(err)) console.error(`Failed to load the worker summary of ${threadId}:`, err);
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
  for (const [threadId, owner] of [...owners]) {
    if (owner !== backendId) continue;
    owners.delete(threadId);
    landed.delete(threadId);
    counts.drop(threadId);
  }
});

/** Test helper. */
export function resetThreadWorkerSummariesForTest(): void {
  scheduler.reset();
  due.clear();
  owners.clear();
  landed.clear();
  counts.reset();
  sequence = 0;
}
