// The sidebar's thread list is two catalogs, threads and the groups that
// gather them, and this module is the only reader of either. Read apart, a
// computer's rows would show in one arrangement when the first answer
// commits and move when the second does. Every read of both (boot, hello,
// gap resync, retry, a history generation change) is one round here, and
// a round reads a computer's two catalogs as one unit:
//
//   - It begins both reads of a computer in one synchronous step, so a
//     newer round supersedes both at once. A superseded read publishes
//     nothing: an answer it holds is dropped and its journals end.
//   - Each catalog has its own on-time window: it ends once every
//     computer's read of that catalog answered, failed or passed the
//     deadline.
//   - A computer the sidebar does not list yet (catalogTreeListed) commits
//     each catalog's answer when that catalog's window ends, or as it lands
//     when late, and the sidebar holds its rows until neither read is out;
//     a read past its deadline is still out.
//   - A listed computer's two answers commit together, once neither read is
//     out and both windows have ended: in the round's batch, or in one block
//     of their own when the last late one lands. A failed read lets the other
//     commit alone; the failure shows and retries as the pair.
//   - Answers that commit at a window's end commit in one batch per catalog,
//     threads first, so a refresh reconciles the thread list once however
//     many computers answered.
//
// A round resolves when its thread window ends, with the thread rows as the
// boot's layout restore should see them: the store's rows with every listed
// computer's held thread answer applied, its journal replayed as of that
// moment. The owners of those rows are recorded then, so a restored pane
// routes to its computer before the rows commit. A round that newer rounds
// superseded follows them, bounded by its own on-time budget, so a retry
// that waits for its RPC cannot hold the boot.
import { untrack } from 'svelte';
import { attachedBackends, onBackendDetached, type BackendEntry } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { ReadDeadlineError } from '../utils/readBeforeDeadline';
import type { Thread, ThreadGroup } from '../types/models';
import { catalogFailureShown, registerCatalogReader } from './catalogLoad.svelte';
import {
  beginShare, commitShares, COMPUTER_READ_DEADLINE_MS, readShare,
  type CatalogPart, type ComputerShare, type ShareResult,
} from './computerRows';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { groupCatalogPart } from './threadGroups.svelte';
import { getThreads, threadCatalogPart, threadRowsWith } from './threads.svelte';

// reading: the RPC is out, on time so far. out: past the deadline, still
// out. answered / failed: in, not yet committed or settled. done:
// committed, settled or dropped.
type PartState = 'reading' | 'out' | 'answered' | 'failed' | 'done';

interface PartRead<T> {
  part: CatalogPart<T>;
  share: ComputerShare<T>;
  state: PartState;
  result: ShareResult<T> | null;
  /** This catalog's on-time window ended, or this read's answer landed late. */
  ready: boolean;
  /** How the read ended so far: answered, or the failure and whether rows stand in. */
  outcome: { answered: boolean; cached: boolean; error: unknown };
}

interface Round {
  /** Newer rounds that superseded a read of this round that could still commit. */
  supersededBy: Set<Round>;
  /** Settles when the round resolves or rejects; never rejects. */
  settled: Promise<{ ok: true } | { ok: false; error: unknown }>;
}

interface ComputerRead {
  backend: BackendKey;
  round: Round;
  /** Listed when the round began: its two answers commit together. */
  paired: boolean;
  ownersNoted: boolean;
  threads: PartRead<Thread>;
  groups: PartRead<ThreadGroup>;
}

// The newest round's read of each computer.
const reads = new Map<BackendKey, ComputerRead>();
// Whether each computer's thread list is listed. Boxless keys read false: a
// computer is held until its first round says otherwise.
const listed = createKeyedSignalRegistry<boolean>(false);
// Computers whose thread catalog is known: answered, cached rows committed,
// or failed with an error of its own.
const threadsKnown = new Set<BackendKey>();

/**
 * Whether the sidebar lists `backend`'s project threads and groups.
 * Reactive on that computer alone.
 */
export function catalogTreeListed(backend: BackendKey): boolean {
  return listed.get(backend);
}

function outstanding<T>(part: PartRead<T>): boolean {
  return part.state === 'reading' || part.state === 'out';
}

function currentRead(read: ComputerRead): boolean {
  return reads.get(read.backend) === read && read.threads.share.current() && read.groups.share.current();
}

function partRead<T>(part: CatalogPart<T>, target: BackendEntry): PartRead<T> {
  return {
    part, share: beginShare(part, target), state: 'reading', result: null, ready: false,
    outcome: { answered: false, cached: false, error: undefined },
  };
}

function beginComputer(round: Round, target: BackendEntry, threads: CatalogPart<Thread>, groups: CatalogPart<ThreadGroup>): ComputerRead {
  const previous = reads.get(target.id);
  const read: ComputerRead = {
    backend: target.id,
    round,
    paired: untrack(() => listed.get(target.id)),
    ownersNoted: false,
    threads: partRead(threads, target),
    groups: partRead(groups, target),
  };
  reads.set(target.id, read);
  if (previous !== undefined && (previous.threads.state !== 'done' || previous.groups.state !== 'done')) {
    previous.round.supersededBy.add(round);
    drop(previous);
  }
  return read;
}

// A read that can no longer commit ends every journal it holds; an RPC
// still out ends its own when it lands.
function drop(read: ComputerRead): void {
  dropPart(read.threads);
  dropPart(read.groups);
}

function dropPart<T>(part: PartRead<T>): void {
  if (part.result && !part.result.late) part.result.share.pending.end();
  part.result = null;
  if (!outstanding(part)) part.state = 'done';
}

// The read's on-time outcome: an answer, a failure, or a deadline.
function arrive<T>(part: PartRead<T>, result: ShareResult<T>): void {
  // A late answer that landed before the window closed already arrived.
  if (part.state !== 'reading') return;
  part.result = result;
  part.state = result.answered ? 'answered' : result.late ? 'out' : 'failed';
  part.outcome = { answered: result.answered, cached: result.cached, error: result.error };
}

function arriveLate<T>(read: ComputerRead, part: PartRead<T>, result: ShareResult<T>): void {
  if (part.state === 'done') {
    result.share.pending.end();
    return;
  }
  part.result = result;
  part.state = result.answered ? 'answered' : 'failed';
  part.outcome = { answered: result.answered, cached: part.outcome.cached, error: result.error };
  part.ready = true;
  commitReads([read]);
}

// The result `part` hands to this block's commit, if any. A read past its
// deadline settles as one here, once, and stays out.
function take<T>(part: PartRead<T>, commitAnswer: boolean): ShareResult<T> | null {
  const { result } = part;
  if (result === null || !part.ready) return null;
  if (part.state === 'out') {
    part.result = null;
    return result;
  }
  if (part.state === 'failed' || (part.state === 'answered' && commitAnswer)) {
    part.state = 'done';
    part.result = null;
    return result;
  }
  return null;
}

function addTo<T>(batches: Map<CatalogPart<T>, ShareResult<T>[]>, part: CatalogPart<T>, result: ShareResult<T> | null): void {
  if (result === null) return;
  const batch = batches.get(part);
  if (batch) batch.push(result);
  else batches.set(part, [result]);
}

// Commit, in one synchronous block, everything the reads may commit now:
// one commit per catalog, threads first.
function commitReads(list: readonly ComputerRead[]): void {
  const threadBatches = new Map<CatalogPart<Thread>, ShareResult<Thread>[]>();
  const groupBatches = new Map<CatalogPart<ThreadGroup>, ShareResult<ThreadGroup>[]>();
  const concluded: ComputerRead[] = [];
  for (const read of list) {
    if (!currentRead(read)) {
      drop(read);
      continue;
    }
    const { threads, groups } = read;
    const commitAnswer = !read.paired
      || (!outstanding(threads) && !outstanding(groups) && threads.ready && groups.ready);
    const thread = take(threads, commitAnswer);
    if (thread && (thread.answered || thread.cached || catalogFailureShown(thread.error))) threadsKnown.add(read.backend);
    addTo(threadBatches, threads.part, thread);
    addTo(groupBatches, groups.part, take(groups, commitAnswer));
    concluded.push(read);
  }
  try {
    for (const [part, results] of threadBatches) commitShares(part, results);
  } finally {
    for (const [part, results] of groupBatches) commitShares(part, results);
  }
  for (const read of concluded) {
    if (read.threads.state === 'answered' && read.threads.ready) noteHeldOwners(read);
    updateListing(read);
  }
}

// A held thread answer's owners are recorded when its window ends, so a
// pane the restore opens on one of its threads routes to that computer.
function noteHeldOwners(read: ComputerRead): void {
  if (read.ownersNoted || read.threads.result === null) return;
  read.ownersNoted = true;
  for (const row of read.threads.result.rows) read.threads.part.note(row, read.backend);
}

// The listing's one writer.
function updateListing(read: ComputerRead): void {
  if (untrack(() => listed.get(read.backend)) || !threadsKnown.has(read.backend)) return;
  if (read.threads.state !== 'done' || read.groups.state !== 'done') return;
  listed.set(read.backend, true);
}

/** The thread rows a caller validating saved panes should see now. */
function threadRowsNow(): readonly Thread[] {
  const held = [...reads.values()].filter((read) => read.threads.state === 'answered' && read.threads.ready && currentRead(read));
  if (held.length === 0) return getThreads();
  const rows = held.flatMap((read) => {
    const { part, result } = read.threads;
    return result!.share.pending.replay(result!.rows.slice()).filter((row) => !part.admit || part.admit(row, read.backend));
  });
  return threadRowsWith({
    rows,
    answered: new Set(held.map((read) => read.backend)),
    attached: new Set(attachedBackends().map((entry) => entry.id)),
  });
}

// Read one catalog of every computer in the round. Resolves when that
// catalog's window ends, after the commit it allows.
async function readCatalog<T>(
  begun: readonly ComputerRead[], part: CatalogPart<T>, of: (read: ComputerRead) => PartRead<T>, deadlineMs: number | null,
): Promise<void> {
  await Promise.all(begun.map(async (read) => {
    const share = of(read);
    arrive(share, await readShare(part, share.share, deadlineMs, { onLate: (result) => arriveLate(read, share, result) }));
  }));
  for (const read of begun) of(read).ready = true;
  commitReads(begun);
}

async function runRound(round: Round, only: BackendKey | undefined): Promise<readonly Thread[]> {
  const deadlineMs = only === undefined ? COMPUTER_READ_DEADLINE_MS : null;
  let budgetTimer: ReturnType<typeof setTimeout> | undefined;
  const budget = deadlineMs === null ? null : new Promise<'budget'>((resolve) => {
    budgetTimer = setTimeout(() => resolve('budget'), deadlineMs);
  });
  try {
    const threadPart = threadCatalogPart();
    const groupPart = groupCatalogPart();
    const targets = untrack(() => attachedBackends()).filter((entry) => only === undefined || entry.id === only);
    const begun = targets.map((target) => beginComputer(round, target, threadPart, groupPart));
    void readCatalog(begun, groupPart, (read) => read.groups, deadlineMs).catch((error: unknown) => {
      console.error('Failed to read the thread groups:', error);
    });
    await readCatalog(begun, threadPart, (read) => read.threads, deadlineMs);
    const current = begun.filter(currentRead);
    // A computer's threads are known to this caller when they answered, held
    // or committed, or its kept rows stand in. None known is unknown, never
    // an authoritative empty list: treating it as empty would erase saved
    // panes on a cold offline startup.
    const outcomes = current.map((read) => read.threads.outcome);
    if (current.length > 0 && !outcomes.some((outcome) => outcome.answered || outcome.cached)) {
      throw outcomes.map((outcome) => outcome.error).find((error) => error !== undefined) ?? new Error('No computer could be reached.');
    }
    const superseders = [...round.supersededBy];
    if (superseders.length > 0) {
      const followed = await Promise.race([
        Promise.all(superseders.map((newer) => newer.settled)),
        ...(budget ? [budget] : []),
      ]);
      if (current.length === 0) {
        // Past the budget, the rows this caller has are its answer when its
        // computers' threads are known; unknown is never an empty list.
        if (followed === 'budget') {
          if (!begun.some((read) => threadsKnown.has(read.backend))) throw new ReadDeadlineError();
        } else {
          const failed = followed.find((outcome) => !outcome.ok);
          if (!followed.some((outcome) => outcome.ok) && failed && !failed.ok) throw failed.error;
        }
      }
    }
    return threadRowsNow();
  } finally {
    clearTimeout(budgetTimer);
  }
}

/**
 * Read the computers' threads and groups as one round, every computer's or
 * `only`'s, waiting for `only`'s answers rather than the startup deadline.
 * Resolves with the thread rows once the round's thread window has ended
 * (see the module comment); rejects when no computer's threads are known.
 * Each computer's failure is in the catalog state, which shows and retries
 * it.
 */
export function loadThreadCatalogs(only?: BackendKey): Promise<readonly Thread[]> {
  let settle!: (outcome: { ok: true } | { ok: false; error: unknown }) => void;
  const round: Round = { supersededBy: new Set(), settled: new Promise((resolve) => { settle = resolve; }) };
  const result = runRound(round, only);
  result.then(() => settle({ ok: true }), (error: unknown) => settle({ ok: false, error }));
  return result;
}

registerCatalogReader(['threads', 'groups'], async (backend) => {
  try {
    await loadThreadCatalogs(backend);
  } catch {
    // The round settled this computer's failure into the catalog state,
    // which is where it is shown and retried.
  }
});

onBackendDetached(({ backendId }) => {
  reads.delete(backendId);
  threadsKnown.delete(backendId);
  listed.drop(backendId);
});

/**
 * Test seam: list every attached computer's threads, for a test that seeds
 * the row stores directly.
 */
export function listCatalogTreesForTest(): void {
  for (const entry of attachedBackends()) listed.set(entry.id, true);
}

/** Test seam: forget every read and listing. */
export function resetThreadCatalogsForTest(): void {
  for (const read of reads.values()) drop(read);
  reads.clear();
  threadsKnown.clear();
  listed.reset();
}
