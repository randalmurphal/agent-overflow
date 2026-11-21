import { DisconnectedError } from '../transport/wsClient';
import { beginCatalogRead, type CatalogRead } from './computerCatalogReads';
// A failed list read is not an empty computer. Read each computer's share
// independently and retain only that computer's cached rows on failure.
import { attachedBackends, withBackendTarget, type BackendEntry } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { getReplicaCatalog, putReplicaCatalog, replicaCatalogStamp } from '../replica/session';
import type { CatalogKind, CatalogRows } from '../replica/catalog';
import { getBackendIdentity, type BackendIdentity } from '../transport/backendIdentity';
import { readBeforeDeadline, ReadDeadlineError } from '../utils/readBeforeDeadline';
import { untrack } from 'svelte';
import { settleCatalogRead } from './catalogLoad.svelte';

interface ComputerCatalog<T> {
  read(backend: BackendKey): Promise<T[] | null>;
  write(backend: BackendKey, rows: T[]): Promise<void>;
  hasRows(backend: BackendKey): boolean;
  /** Begin this computer's read; `onRelease` as beginCatalogRead's. */
  begin(backend: BackendKey, onRelease?: () => void): CatalogRead<T>;
  /**
   * One computer's current read failed with `error`; `cached` when its
   * kept or replica rows stand in for the answer. An answer, on time or
   * late, is settled by the read's commit (settleCatalogAnswers).
   */
  settle(backend: BackendKey, error: unknown, cached: boolean): void;
}

// The startup read budget: past it, a computer's answer commits late and
// the boot proceeds without it.
export const COMPUTER_READ_DEADLINE_MS = 2500;

/**
 * One read's share of a round that commits two catalogs of a computer
 * together (./threadCatalogs.ts). For a computer the round pairs, the read
 * hands its answer's commit to the round instead of running it; the
 * journal keeps recording until the round runs it.
 */
export interface CatalogRoundRead {
  /** Whether this round pairs `backend`'s commits. */
  pairs(backend: BackendKey): boolean;
  /** `backend`'s answer is in; `onTime` false for one past the deadline. */
  deliver(backend: BackendKey, commit: () => void, onTime: boolean): void;
  /** `backend`'s read can no longer deliver: ended, superseded or detached. */
  release(backend: BackendKey): void;
  /** Every computer of this read has answered, failed or passed the deadline. */
  windowEnded(): void;
}

export interface ComputerRowsOptions<T> {
  /** The catalog whose replica, load state and journal this read keeps. */
  catalog?: ComputerCatalog<T>;
  /** Whether a row of `backend`'s answer belongs in the committed rows. */
  admit?: (row: T, backend: BackendKey) => boolean;
  /** Read this computer alone; every other computer's rows are retained. */
  only?: BackendKey;
  /** Bound on each computer's answer, or null to wait for the RPC itself. */
  deadlineMs?: number | null;
  /** The round this read belongs to, when its commits pair with another catalog's. */
  round?: CatalogRoundRead;
}

export function computerCatalog<K extends CatalogKind>(
  kind: K, previous: () => readonly CatalogRows[K][], owner: (row: CatalogRows[K]) => BackendKey | undefined,
): ComputerCatalog<CatalogRows[K]> {
  // Reading the fallback must never subscribe a mount loader to its own
  // result. Accept a getter so callers cannot accidentally read it first.
  const populated = untrack(() => new Set(previous().map((row) => owner(row) ?? HOME_BACKEND)));
  const stamps = new Map<BackendKey, string | null>();
  return {
    begin(backend, onRelease) {
      // The stamp fences only the replica write: a structural invalidation
      // since the read began keeps this answer out of IndexedDB. The answer
      // itself still commits, with the invalidating mutation replayed.
      stamps.set(backend, replicaCatalogStamp(backend, kind));
      return beginCatalogRead<CatalogRows[K]>(backend, kind, onRelease);
    },
    read: (backend) => getReplicaCatalog(backend, kind),
    write: (backend, rows) => putReplicaCatalog(backend, kind, rows, stamps.get(backend) ?? null),
    hasRows: (backend) => populated.has(backend),
    settle: (backend, error, cached) => settleCatalogRead(kind, backend, { answered: false, error, cached }),
  };
}

// A read with no catalog behind it records nothing and is never superseded.
const UNTRACKED_READ: CatalogRead<never> = { current: () => true, replay: (rows) => rows, end: () => {} };

export interface ComputerRows<T> {
  rows: T[];
  answered: ReadonlySet<BackendKey>;
  attached: ReadonlySet<BackendKey>;
}

interface RowsRead<T> {
  read: () => PromiseLike<T[] | null>;
  note: (row: T, backend: BackendKey) => void;
  commit: (result: ComputerRows<T>) => void;
  options: ComputerRowsOptions<T>;
  deadlineMs: number | null;
}

/** One computer's share of a read, once it answered, failed or passed the deadline. */
interface TargetResult<T> {
  target: BackendEntry;
  rows: T[];
  answered: boolean;
  error: unknown;
  identity: BackendIdentity;
  pending: CatalogRead<T>;
  /** Past the deadline: its late answer or late failure ends the journal. */
  late: boolean;
  cached: boolean;
  /** Commit an answer of this computer alone: replay, end, note, commit. */
  commitAnswer(answer: T[] | null): void;
}

/**
 * Read every attached computer's rows and hand them to `commit`, in the same
 * synchronous block that replays the local mutations made while the read was
 * out, so no later change can land between the replay and the commit. A
 * computer that misses the deadline commits alone when it answers. A
 * computer the round pairs commits when the round runs its commit. Resolves
 * true once this read committed, false when a newer read superseded it, and
 * rejects when no computer answered and none has rows to keep.
 */
export async function readComputerRows<T>(
  read: () => PromiseLike<T[] | null>,
  note: (row: T, backend: BackendKey) => void,
  commit: (result: ComputerRows<T>) => void,
  options: ComputerRowsOptions<T> = {},
): Promise<boolean> {
  const deadlineMs = options.deadlineMs === undefined ? COMPUTER_READ_DEADLINE_MS : options.deadlineMs;
  const rowsRead: RowsRead<T> = { read, note, commit, options, deadlineMs };
  const targets = attachedBackends().filter((entry) => options.only === undefined || entry.id === options.only);
  try {
    const results = await Promise.all(targets.map((target) => readTarget(rowsRead, target)));
    return commitResults(rowsRead, results);
  } finally {
    options.round?.windowEnded();
  }
}

function sameIdentity(a: BackendIdentity, b: BackendIdentity): boolean {
  return a.backendId === b.backendId && a.generation === b.generation;
}

async function readTarget<T>(rowsRead: RowsRead<T>, target: BackendEntry): Promise<TargetResult<T>> {
  const { read, note, commit, options: { catalog: cache, admit, round }, deadlineMs } = rowsRead;
  const identity = getBackendIdentity(target.id);
  const paired = round?.pairs(target.id) ?? false;
  const pending: CatalogRead<T> = cache?.begin(target.id, paired ? () => round!.release(target.id) : undefined) ?? UNTRACKED_READ;
  function stillCurrent(): boolean {
    return pending.current() && attachedBackends().includes(target)
      && (!identity.backendId || sameIdentity(identity, getBackendIdentity(target.id)));
  }
  function commitAnswer(answer: T[] | null): void {
    const current = stillCurrent();
    const arrived = current ? pending.replay(answer ?? []) : [];
    pending.end();
    if (!current) return;
    for (const row of arrived) note(row, target.id);
    void cache?.write(target.id, arrived);
    commit({ rows: admit ? arrived.filter((row) => admit(row, target.id)) : arrived, answered: new Set([target.id]),
      attached: new Set(attachedBackends().map((entry) => entry.id)) });
  }
  function onLate(answer: T[] | null): void {
    if (paired) round!.deliver(target.id, () => commitAnswer(answer), false);
    else commitAnswer(answer);
  }
  function onLateFailure(error: unknown): void {
    const current = stillCurrent();
    pending.end();
    if (current) cache?.settle(target.id, error, false);
  }
  const result = { target, pending, commitAnswer, late: false, cached: false };
  let error: unknown;
  try {
    // Every saved computer gets its first dial, independently and under
    // the same deadline: 'disconnected' is the status until an attempt
    // has failed, and a phone need not have a legacy HOME slot at all.
    // Once that attempt has failed the reconnect ladder owns retries and
    // this read treats the computer as offline like any other; its first
    // hello refreshes every list (computerHydration). Issuing the read
    // regardless would fire one dial per list per refresh at a computer
    // that is off.
    const status = target.status.status;
    if (status !== 'connected' && status !== 'disconnected') throw new DisconnectedError('Computer is offline.');
    const request = withBackendTarget(target.id, read);
    const rows = (deadlineMs === null ? await request : await readBeforeDeadline(request, deadlineMs, onLate, onLateFailure)) ?? [];
    const current = getBackendIdentity(target.id);
    if (identity.backendId && !sameIdentity(identity, current)) throw new Error('Computer history changed during the read.');
    return { ...result, rows, answered: true, error: undefined, identity: current };
  } catch (reason) {
    error = reason;
  }
  // A read past its deadline keeps recording: its late answer or late
  // failure ends it, not this call.
  const late = error instanceof ReadDeadlineError;
  const cachedIdentity = getBackendIdentity(target.id);
  const previous = cache?.hasRows(target.id) ?? false;
  const rows = cache && !previous ? await cache.read(target.id) : null;
  return { ...result, rows: rows ?? [], answered: false, error, identity: cachedIdentity, late, cached: previous || rows !== null };
}

function commitResults<T>(rowsRead: RowsRead<T>, results: TargetResult<T>[]): boolean {
  const { note, commit, options: { catalog: cache, admit, round } } = rowsRead;
  // One final membership check covers both RPC and IndexedDB awaits.
  // Superseded, detached and re-identified computers' results have no
  // authority: a newer read, the detach or the identity change's own
  // reads (a hello, or ./threadCatalogs.ts for a generation) follow them.
  const live = new Set<BackendEntry>(attachedBackends());
  const current = results.filter((result) => live.has(result.target) && result.pending.current()
    && sameIdentity(result.identity, getBackendIdentity(result.target.id)));
  // A paired computer's answer goes to the round with its journal still
  // recording. Every other computer replays now, once every computer has
  // settled, so a mutation made while a slower computer was still out is
  // not lost. An unanswered computer keeps its local rows, which already
  // hold its mutations.
  const delivered = new Set<TargetResult<T>>();
  for (const result of results) {
    if (result.answered && round?.pairs(result.target.id) && current.includes(result)) {
      delivered.add(result);
      continue;
    }
    if (result.answered) result.rows = result.pending.replay(result.rows);
    if (!result.late) result.pending.end();
  }
  const committed = current.filter((result) => !delivered.has(result));
  const answered = new Set<BackendKey>();
  const rows: T[] = [];
  let error: unknown;
  let hasCache = false;
  for (const result of committed) {
    // A failure settles here. An answer settles in `commit` with its rows
    // (settleCatalogAnswers), so no render sees a catalog marked loaded
    // before its rows land.
    if (!result.answered) cache?.settle(result.target.id, result.error, result.cached);
    error ??= result.error;
    hasCache ||= result.cached;
    if (result.answered) {
      answered.add(result.target.id);
      if (cache) void cache.write(result.target.id, result.rows);
    }
    for (const row of result.rows) note(row, result.target.id);
  }
  // All origins must be indexed before admission: arrival/attachment order
  // cannot decide ownership when an offline catalog predates a move.
  for (const result of committed) {
    for (const row of result.rows) if (!admit || admit(row, result.target.id)) rows.push(row);
  }
  for (const result of delivered) round!.deliver(result.target.id, () => result.commitAnswer(result.rows), true);
  // Superseded reads have no authority and no failure to report. In
  // particular, boot and a connection's first hello can overlap. Keep
  // cancellation distinct from both an authoritative empty list and an
  // actual failed dial, so callers cannot mark an unknown catalog loaded.
  if (current.length === 0) return false;
  if (answered.size === 0 && !hasCache) {
    // The round commits what was delivered.
    if (delivered.size > 0) return true;
    // An asleep saved computer is ordinary state. Its connection banner
    // owns the explanation; cached catalogs remain usable.
    // No answer and no cache is unknown, never an authoritative empty list:
    // treating it as empty would erase saved panes on a cold offline startup.
    throw error ?? new Error('No computer could be reached.');
  }
  commit({ rows, answered, attached: new Set([...live].map((entry) => entry.id)) });
  return true;
}

export function retainUnavailableComputerRows<T>(
  previous: readonly T[], result: ComputerRows<T>, owner: (row: T) => BackendKey | undefined,
): T[] {
  const retained = previous.filter((row) => {
    const backend = owner(row) ?? HOME_BACKEND;
    return result.attached.has(backend) && !result.answered.has(backend);
  });
  return retained.length ? [...result.rows, ...retained] : result.rows;
}
