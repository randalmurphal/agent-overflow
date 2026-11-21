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

export interface ComputerCatalog<T> {
  read(backend: BackendKey): Promise<T[] | null>;
  write(backend: BackendKey, rows: T[]): Promise<void>;
  hasRows(backend: BackendKey): boolean;
  begin(backend: BackendKey): CatalogRead<T>;
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

/** How one catalog is read from each computer and committed. */
export interface CatalogPart<T> {
  read: () => PromiseLike<T[] | null>;
  note: (row: T, backend: BackendKey) => void;
  commit: (result: ComputerRows<T>) => void;
  /** The catalog whose replica, load state and journal this read keeps. */
  catalog?: ComputerCatalog<T>;
  /** Whether a row of `backend`'s answer belongs in the committed rows. */
  admit?: (row: T, backend: BackendKey) => boolean;
}

export interface ComputerRowsOptions<T> {
  catalog?: ComputerCatalog<T>;
  admit?: (row: T, backend: BackendKey) => boolean;
  /** Read this computer alone; every other computer's rows are retained. */
  only?: BackendKey;
  /** Bound on each computer's answer, or null to wait for the RPC itself. */
  deadlineMs?: number | null;
}

export function computerCatalog<K extends CatalogKind>(
  kind: K, previous: () => readonly CatalogRows[K][], owner: (row: CatalogRows[K]) => BackendKey | undefined,
): ComputerCatalog<CatalogRows[K]> {
  // Reading the fallback must never subscribe a mount loader to its own
  // result. Accept a getter so callers cannot accidentally read it first.
  const populated = untrack(() => new Set(previous().map((row) => owner(row) ?? HOME_BACKEND)));
  const stamps = new Map<BackendKey, string | null>();
  return {
    begin(backend) {
      // The stamp fences only the replica write: a structural invalidation
      // since the read began keeps this answer out of IndexedDB. The answer
      // itself still commits, with the invalidating mutation replayed.
      stamps.set(backend, replicaCatalogStamp(backend, kind));
      return beginCatalogRead<CatalogRows[K]>(backend, kind);
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

/** One computer's read of one catalog, begun. */
export interface ComputerShare<T> {
  readonly target: BackendEntry;
  readonly pending: CatalogRead<T>;
  /**
   * Whether this read may still commit: the newest read of that computer's
   * catalog, the computer still attached, its history unchanged.
   */
  current(): boolean;
}

/** One computer's share once it answered, failed or passed the deadline. */
export interface ShareResult<T> {
  share: ComputerShare<T>;
  rows: T[];
  answered: boolean;
  error: unknown;
  identity: BackendIdentity;
  /** Past the deadline: its late answer or late failure ends the journal. */
  late: boolean;
  cached: boolean;
}

/** Where a share past its deadline delivers its late answer or late failure. */
export interface LateShare<T> {
  onLate(result: ShareResult<T>): void;
}

/** What committing one catalog's results did. */
export interface CommitOutcome {
  /** Results that were still current. */
  current: number;
  /** Whether rows (answered or cached) were committed. */
  committed: boolean;
  error: unknown;
}

function sameIdentity(a: BackendIdentity, b: BackendIdentity): boolean {
  return a.backendId === b.backendId && a.generation === b.generation;
}

/**
 * Begin `target`'s read of `part`'s catalog: supersede the read before it
 * and start recording local mutations. Synchronous, so a caller can begin
 * several catalogs of one computer as one step.
 */
export function beginShare<T>(part: CatalogPart<T>, target: BackendEntry): ComputerShare<T> {
  const identity = getBackendIdentity(target.id);
  const pending: CatalogRead<T> = part.catalog?.begin(target.id) ?? UNTRACKED_READ;
  return {
    target,
    pending,
    current: () => pending.current() && attachedBackends().includes(target)
      && (!identity.backendId || sameIdentity(identity, getBackendIdentity(target.id))),
  };
}

/**
 * Read one computer's rows. Resolves on time with the answer, the failure,
 * or a late result once `deadlineMs` passed; the late answer or late
 * failure then goes to `late`.
 */
export async function readShare<T>(
  part: CatalogPart<T>, share: ComputerShare<T>, deadlineMs: number | null, late: LateShare<T>,
): Promise<ShareResult<T>> {
  const { target } = share;
  const startIdentity = getBackendIdentity(target.id);
  const lateResult = (fields: Pick<ShareResult<T>, 'rows' | 'answered' | 'error'>): ShareResult<T> => (
    { share, ...fields, identity: getBackendIdentity(target.id), late: false, cached: false });
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
    const request = withBackendTarget(target.id, part.read);
    const rows = (deadlineMs === null ? await request : await readBeforeDeadline(
      request, deadlineMs,
      (answer) => late.onLate(lateResult({ rows: answer ?? [], answered: true, error: undefined })),
      (failure) => late.onLate(lateResult({ rows: [], answered: false, error: failure })),
    )) ?? [];
    const identity = getBackendIdentity(target.id);
    if (startIdentity.backendId && !sameIdentity(startIdentity, identity)) throw new Error('Computer history changed during the read.');
    return { share, rows, answered: true, error: undefined, identity, late: false, cached: false };
  } catch (reason) {
    error = reason;
  }
  // A read past its deadline keeps recording: its late answer or late
  // failure ends it, not this call.
  const isLate = error instanceof ReadDeadlineError;
  const identity = getBackendIdentity(target.id);
  const previous = part.catalog?.hasRows(target.id) ?? false;
  const rows = part.catalog && !previous ? await part.catalog.read(target.id) : null;
  return { share, rows: rows ?? [], answered: false, error, identity, late: isLate, cached: previous || rows !== null };
}

/**
 * Commit one catalog's results in one synchronous block: replay each
 * answer's journal and end it, settle each failure, write the replicas,
 * note every row's owner before admission, and call `commit` once with the
 * rows of every computer that answered or kept rows. A result past its
 * deadline settles as one but keeps recording.
 */
export function commitShares<T>(part: CatalogPart<T>, results: readonly ShareResult<T>[]): CommitOutcome {
  const { note, commit, catalog, admit } = part;
  // One final membership check covers both RPC and IndexedDB awaits.
  // Superseded, detached and re-identified computers' results have no
  // authority: a newer read, the detach or the identity change's own
  // reads (a hello, or a generation change's re-read) follow them.
  const live = new Set<BackendEntry>(attachedBackends());
  const current = results.filter((result) => result.share.current()
    && sameIdentity(result.identity, getBackendIdentity(result.share.target.id)));
  // Every computer replays now, once every computer in the block has
  // settled, so a mutation made while a slower computer was still out is
  // not lost. An unanswered computer keeps its local rows, which already
  // hold its mutations.
  for (const result of results) {
    if (result.answered) result.rows = result.share.pending.replay(result.rows);
    if (!result.late) result.share.pending.end();
  }
  const answered = new Set<BackendKey>();
  const rows: T[] = [];
  let error: unknown;
  let hasCache = false;
  for (const result of current) {
    const backend = result.share.target.id;
    // A failure settles here. An answer settles in `commit` with its rows
    // (settleCatalogAnswers), so no render sees a catalog marked loaded
    // before its rows land.
    if (!result.answered) catalog?.settle(backend, result.error, result.cached);
    error ??= result.error;
    hasCache ||= result.cached;
    if (result.answered) {
      answered.add(backend);
      if (catalog) void catalog.write(backend, result.rows);
    }
    for (const row of result.rows) note(row, backend);
  }
  // All origins must be indexed before admission: arrival/attachment order
  // cannot decide ownership when an offline catalog predates a move.
  for (const result of current) {
    const backend = result.share.target.id;
    for (const row of result.rows) if (!admit || admit(row, backend)) rows.push(row);
  }
  const committed = current.length > 0 && (answered.size > 0 || hasCache);
  if (committed) commit({ rows, answered, attached: new Set([...live].map((entry) => entry.id)) });
  return { current: current.length, committed, error };
}

/**
 * Read every attached computer's rows and hand them to `commit`, in the same
 * synchronous block that replays the local mutations made while the read was
 * out, so no later change can land between the replay and the commit. A
 * computer that misses the deadline commits alone when it answers. Resolves
 * true once this read committed, false when a newer read superseded it, and
 * rejects when no computer answered and none has rows to keep.
 */
export async function readComputerRows<T>(
  read: () => PromiseLike<T[] | null>,
  note: (row: T, backend: BackendKey) => void,
  commit: (result: ComputerRows<T>) => void,
  options: ComputerRowsOptions<T> = {},
): Promise<boolean> {
  const part: CatalogPart<T> = { read, note, commit, catalog: options.catalog, admit: options.admit };
  const deadlineMs = options.deadlineMs === undefined ? COMPUTER_READ_DEADLINE_MS : options.deadlineMs;
  const targets = attachedBackends().filter((entry) => options.only === undefined || entry.id === options.only);
  const shares = targets.map((target) => beginShare(part, target));
  const late: LateShare<T> = { onLate: (result) => { commitShares(part, [result]); } };
  const results = await Promise.all(shares.map((share) => readShare(part, share, deadlineMs, late)));
  const outcome = commitShares(part, results);
  // Superseded reads have no authority and no failure to report. In
  // particular, boot and a connection's first hello can overlap. Keep
  // cancellation distinct from both an authoritative empty list and an
  // actual failed dial, so callers cannot mark an unknown catalog loaded.
  if (outcome.current === 0) return false;
  // An asleep saved computer is ordinary state. Its connection banner
  // owns the explanation; cached catalogs remain usable.
  // No answer and no cache is unknown, never an authoritative empty list:
  // treating it as empty would erase saved panes on a cold offline startup.
  if (!outcome.committed) throw outcome.error ?? new Error('No computer could be reached.');
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
