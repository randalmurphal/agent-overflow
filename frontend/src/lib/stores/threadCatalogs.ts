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
//     of their own when the last late one lands.
//   - The pair is atomic in the sidebar without exception. A failed read
//     settles its failure, which shows with Retry and retries as the pair;
//     the other half, answered, is never published to the sidebar's list,
//     on any attempt and for any failure. A listed computer keeps its last
//     complete pair, an unlisted one its loading or error row, and only a
//     complete pair replaces them. An unlisted computer's thread answer
//     still reaches the thread store, for its open panes.
//   - Answers that commit at a window's end commit in one batch per catalog,
//     threads first, so a refresh reconciles the thread list once however
//     many computers answered.
//
// A round resolves when its thread window ends, with the thread rows as the
// boot's layout restore should see them: the store's rows with every listed
// computer's held thread answer applied, its journal replayed as of that
// moment. A thread answer withheld for its partner's failure counts for the
// round that withheld it and the rounds following that round, and lives as
// long as one of them is unresolved. The owners of those rows are recorded
// then, so a restored pane routes to its computer before the rows commit. A
// round that newer rounds superseded follows every one of them, those that
// supersede it while it waits included, bounded by its own on-time budget,
// so a retry that waits for its RPC cannot hold the boot. Only then does it
// decide whether any of its computers' thread rows are known.
//
// Each catalog read is an attempt of the pair's retry (./catalogLoad.svelte.ts)
// until it is released, ended or superseded, so a computer's read holds the
// retry timer off while either part is still out. Once both parts are done
// or dropped, nothing here holds it, its journals or its answers.
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
// committed, settled, withheld or dropped.
type PartState = 'reading' | 'out' | 'answered' | 'failed' | 'done';

interface PartOutcome {
  answered: boolean;
  cached: boolean;
  error: unknown;
}

interface PartRead<T> {
  part: CatalogPart<T>;
  share: ComputerShare<T>;
  state: PartState;
  result: ShareResult<T> | null;
  /** This catalog's on-time window ended, or this read's answer landed late. */
  ready: boolean;
  /** Superseded or detached before it was done: it publishes nothing. */
  dropped: boolean;
  /** How the read ended so far: answered, or the failure and whether rows stand in. */
  outcome: PartOutcome;
}

// Where a part stands, whichever catalog it reads.
type PartStatus = Pick<PartRead<unknown>, 'state' | 'ready' | 'dropped' | 'outcome'>;

type RoundOutcome = { ok: true; known: ReadonlySet<BackendKey> } | { ok: false; error: unknown };

interface Round {
  /** Newer rounds that superseded a read of this round that could still count. */
  supersededBy: Set<Round>;
  /** Older rounds, unresolved, that follow this one: a read of theirs this one superseded. */
  followers: Set<Round>;
  resolved: boolean;
  /** Withheld thread answers this round resolves with: its own and those of rounds it followed. */
  withheld: Set<Withheld>;
  /**
   * Settles when the round resolves, with the computers whose thread rows
   * it knew, or rejects; never rejects.
   */
  settled: Promise<RoundOutcome>;
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

// A thread answer withheld for its partner's failure, kept for the rows the
// unresolved rounds holding it resolve with. `result` is null once released.
interface Withheld {
  backend: BackendKey;
  part: CatalogPart<Thread>;
  result: ShareResult<Thread> | null;
  holders: Set<Round>;
}

// The newest round's read of each computer, while it is in flight.
const reads = new Map<BackendKey, ComputerRead>();
// Each computer's withheld thread answer while a round holds it.
const withheld = new Map<BackendKey, Withheld>();
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

function outstanding(part: PartStatus): boolean {
  return part.state === 'reading' || part.state === 'out';
}

function currentRead(read: ComputerRead): boolean {
  return reads.get(read.backend) === read && read.threads.share.current() && read.groups.share.current();
}

function partRead<T>(part: CatalogPart<T>, target: BackendEntry): PartRead<T> {
  return {
    part, share: beginShare(part, target), state: 'reading', result: null, ready: false, dropped: false,
    outcome: { answered: false, cached: false, error: undefined },
  };
}

// `older` follows `newer`: it waits for it, and holds what it withholds.
// Every follower joins as `newer` begins its reads, before it can withhold.
function supersede(older: Round, newer: Round): void {
  older.supersededBy.add(newer);
  if (!older.resolved) newer.followers.add(older);
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
  if (previous !== undefined) {
    supersede(previous.round, round);
    drop(previous);
  }
  const kept = withheld.get(target.id);
  if (kept !== undefined) {
    for (const holder of kept.holders) supersede(holder, round);
    release(kept);
  }
  return read;
}

// A read that can no longer commit publishes nothing. Its journals end: a
// newer read released them already; a detach or identity change ends them
// here, or, for an RPC still out, when it lands.
function drop(read: ComputerRead): void {
  dropPart(read.threads);
  dropPart(read.groups);
  concludeIfDone(read);
}

function dropPart<T>(part: PartRead<T>): void {
  if (part.state === 'done') return;
  part.dropped = true;
  if (part.result && !part.result.late) part.result.share.pending.end();
  part.result = null;
  if (!outstanding(part)) part.state = 'done';
}

// A read whose parts are both done or dropped is no longer in flight, and
// nothing here holds it.
function concludeIfDone(read: ComputerRead): void {
  const concluded = (part: PartStatus) => part.state === 'done' || part.dropped;
  if (!concluded(read.threads) || !concluded(read.groups)) return;
  if (reads.get(read.backend) === read) reads.delete(read.backend);
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
// deadline settles as one here, once, and stays out. A listed computer's
// answer waits for its partner and commits with it; when the partner
// failed it is withheld instead, handed to `withhold`.
function take<T>(
  read: ComputerRead, part: PartRead<T>, partner: PartStatus, withhold: (result: ShareResult<T>) => void,
): ShareResult<T> | null {
  const { result } = part;
  if (result === null || !part.ready) return null;
  if (part.state === 'out') {
    part.result = null;
    return result;
  }
  if (part.state === 'answered' && read.paired) {
    if (outstanding(partner) || !partner.ready) return null;
    if (!partner.outcome.answered) {
      part.state = 'done';
      part.result = null;
      withhold(result);
      return null;
    }
  }
  part.state = 'done';
  part.result = null;
  return result;
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
    const thread = take(read, threads, groups, (result) => withholdThreads(read, result));
    if (thread && (thread.answered || thread.cached || catalogFailureShown(thread.error))) threadsKnown.add(read.backend);
    addTo(threadBatches, threads.part, thread);
    addTo(groupBatches, groups.part, take(read, groups, threads, (result) => result.share.pending.end()));
    concluded.push(read);
  }
  try {
    for (const [part, results] of threadBatches) commitShares(part, results);
  } finally {
    for (const [part, results] of groupBatches) commitShares(part, results);
  }
  for (const read of concluded) {
    if (read.threads.state === 'answered' && read.threads.ready) noteHeldOwners(read, read.threads.result!);
    updateListing(read);
    concludeIfDone(read);
  }
}

// A held thread answer's owners are recorded when its window ends, so a
// pane the restore opens on one of its threads routes to that computer.
function noteHeldOwners(read: ComputerRead, result: ShareResult<Thread>): void {
  if (read.ownersNoted) return;
  read.ownersNoted = true;
  for (const row of result.rows) read.threads.part.note(row, read.backend);
}

// The unresolved rounds that can still read what `round` withholds: itself
// and every round following it, directly or through another follower.
function liveHolders(round: Round): Set<Round> {
  const live = new Set<Round>();
  const seen = new Set<Round>();
  const visit = (next: Round) => {
    if (seen.has(next)) return;
    seen.add(next);
    if (!next.resolved) live.add(next);
    for (const follower of next.followers) visit(follower);
  };
  visit(round);
  return live;
}

function hold(entry: Withheld, round: Round): void {
  entry.holders.add(round);
  round.withheld.add(entry);
}

// A withheld thread answer is never committed; the rounds that can still
// read it resolve with it.
function withholdThreads(read: ComputerRead, result: ShareResult<Thread>): void {
  const holders = liveHolders(read.round);
  if (holders.size === 0) {
    result.share.pending.end();
    return;
  }
  noteHeldOwners(read, result);
  // A computer's earlier withheld answer was released when this read began.
  const entry: Withheld = { backend: read.backend, part: read.threads.part, result, holders: new Set() };
  for (const holder of holders) hold(entry, holder);
  withheld.set(read.backend, entry);
}

function release(entry: Withheld): void {
  entry.result?.share.pending.end();
  entry.result = null;
  for (const holder of entry.holders) holder.withheld.delete(entry);
  entry.holders.clear();
  if (withheld.get(entry.backend) === entry) withheld.delete(entry.backend);
}

// A round resolved: what no unresolved round holds any more is released.
function finishRound(round: Round): void {
  round.resolved = true;
  for (const newer of round.supersededBy) newer.followers.delete(round);
  for (const entry of round.withheld) {
    entry.holders.delete(round);
    round.withheld.delete(entry);
    if (entry.holders.size === 0) release(entry);
  }
}

// The listing's one writer. A computer lists once both reads are done and
// its threads are known, unless one half answered and the other failed:
// then its loading or error row stays until a complete pair.
function updateListing(read: ComputerRead): void {
  if (untrack(() => listed.get(read.backend)) || !threadsKnown.has(read.backend)) return;
  const { threads, groups } = read;
  if (threads.state !== 'done' || groups.state !== 'done') return;
  if (threads.outcome.answered !== groups.outcome.answered) return;
  listed.set(read.backend, true);
}

/** The thread rows a caller validating saved panes, `round`'s, should see now. */
function threadRowsNow(round: Round): readonly Thread[] {
  const held: { backend: BackendKey; part: CatalogPart<Thread>; result: ShareResult<Thread> }[] = [];
  for (const read of reads.values()) {
    if (read.threads.state === 'answered' && read.threads.ready && currentRead(read)) {
      held.push({ backend: read.backend, part: read.threads.part, result: read.threads.result! });
    }
  }
  for (const { backend, part, result } of round.withheld) {
    if (result !== null && result.share.current()) held.push({ backend, part, result });
  }
  if (held.length === 0) return getThreads();
  const rows = held.flatMap(({ backend, part, result }) => (
    result.share.pending.replay(result.rows.slice()).filter((row) => !part.admit || part.admit(row, backend))));
  return threadRowsWith({
    rows,
    answered: new Set(held.map(({ backend }) => backend)),
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

async function runRound(
  round: Round, begun: readonly ComputerRead[], threadPart: CatalogPart<Thread>, groupPart: CatalogPart<ThreadGroup>,
  deadlineMs: number | null,
): Promise<{ rows: readonly Thread[]; known: ReadonlySet<BackendKey> }> {
  let budgetTimer: ReturnType<typeof setTimeout> | undefined;
  const budget = deadlineMs === null ? null : new Promise<'budget'>((resolve) => {
    budgetTimer = setTimeout(() => resolve('budget'), deadlineMs);
  });
  try {
    void readCatalog(begun, groupPart, (read) => read.groups, deadlineMs).catch((error: unknown) => {
      console.error('Failed to read the thread groups:', error);
    });
    await readCatalog(begun, threadPart, (read) => read.threads, deadlineMs);
    // Follow every newer round that superseded a read of this one, those
    // that supersede one while this waits included, within the budget.
    const followed = new Set<Round>();
    const outcomes: RoundOutcome[] = [];
    let expired = false;
    for (;;) {
      const next = [...round.supersededBy].filter((newer) => !followed.has(newer));
      if (next.length === 0) break;
      for (const newer of next) followed.add(newer);
      const settled = await Promise.race([Promise.all(next.map((newer) => newer.settled)), ...(budget ? [budget] : [])]);
      if (settled === 'budget') {
        expired = true;
        break;
      }
      outcomes.push(...settled);
    }
    // A computer's threads are known to this caller when its own read
    // answered or its kept rows stand in, or a newer round that took the
    // read over knew them. None known is unknown, never an authoritative
    // empty list: treating it as empty would erase saved panes on a cold
    // offline startup.
    const targets = new Set(begun.map((read) => read.backend));
    const known = new Set<BackendKey>();
    for (const read of begun) {
      const { dropped, outcome } = read.threads;
      if (!dropped && (outcome.answered || outcome.cached)) known.add(read.backend);
    }
    for (const outcome of outcomes) {
      if (outcome.ok) for (const backend of outcome.known) if (targets.has(backend)) known.add(backend);
    }
    const attached = new Set(untrack(() => attachedBackends()).map((entry) => entry.id));
    if (known.size === 0 && [...targets].some((backend) => attached.has(backend))) {
      // Past the budget, the rows this caller has are its answer when its
      // computers' threads are known from an earlier read.
      if (expired) {
        if (![...targets].some((backend) => threadsKnown.has(backend))) throw new ReadDeadlineError();
      } else {
        const errors = [
          ...begun.map((read) => read.threads.outcome.error),
          ...outcomes.map((outcome) => (outcome.ok ? undefined : outcome.error)),
        ];
        throw errors.find((error) => error !== undefined) ?? new Error('No computer could be reached.');
      }
    }
    return { rows: threadRowsNow(round), known };
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
  let settle!: (outcome: RoundOutcome) => void;
  const round: Round = {
    supersededBy: new Set(), followers: new Set(), resolved: false, withheld: new Set(),
    settled: new Promise((resolve) => { settle = resolve; }),
  };
  const threadPart = threadCatalogPart();
  const groupPart = groupCatalogPart();
  const targets = untrack(() => attachedBackends()).filter((entry) => only === undefined || entry.id === only);
  const begun = targets.map((target) => beginComputer(round, target, threadPart, groupPart));
  const result = runRound(round, begun, threadPart, groupPart, only === undefined ? COMPUTER_READ_DEADLINE_MS : null);
  result.then(({ known }) => {
    finishRound(round);
    settle({ ok: true, known });
  }, (error: unknown) => {
    finishRound(round);
    settle({ ok: false, error });
  });
  return result.then(({ rows }) => rows);
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
  const read = reads.get(backendId);
  if (read !== undefined) drop(read);
  reads.delete(backendId);
  const kept = withheld.get(backendId);
  if (kept !== undefined) release(kept);
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
  for (const entry of withheld.values()) release(entry);
  threadsKnown.clear();
  listed.reset();
}
