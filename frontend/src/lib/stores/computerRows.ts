import { DisconnectedError } from '../transport/wsClient';
import { beginCatalogRead, type CatalogRead } from './computerCatalogReads';
// A failed list read is not an empty computer. Read each computer's share
// independently and retain only that computer's cached rows on failure.
import { attachedBackends, withBackendTarget, type BackendEntry } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { getReplicaCatalog, putReplicaCatalog, replicaCatalogStamp } from '../replica/session';
import type { CatalogKind, CatalogRows } from '../replica/catalog';
import { getBackendIdentity } from '../transport/backendIdentity';
import { readBeforeDeadline, ReadDeadlineError } from '../utils/readBeforeDeadline';
import { untrack } from 'svelte';
import { settleCatalogRead } from './catalogLoad.svelte';

interface ComputerCatalog<T> {
  read(backend: BackendKey): Promise<T[] | null>;
  write(backend: BackendKey, rows: T[]): Promise<void>;
  hasRows(backend: BackendKey): boolean;
  begin(backend: BackendKey): CatalogRead<T>;
  /**
   * One computer's current read failed with `error`. An answer, on time or
   * late, is settled by the read's commit (settleCatalogAnswers).
   */
  settle?(backend: BackendKey, error: unknown): void;
}

// The startup read budget: past it, a computer's answer commits late and
// the boot proceeds without it.
export const COMPUTER_READ_DEADLINE_MS = 2500;

export interface ComputerRowsOptions<T> {
  /** The catalog whose replica, load state and journal this read keeps. */
  catalog?: ComputerCatalog<T>;
  /** Whether a row of `backend`'s answer belongs in the committed rows. */
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
    settle: kind === 'threads' || kind === 'projects'
      ? (backend, error) => settleCatalogRead(kind, backend, false, error)
      : undefined,
  };
}

// A read with no catalog behind it records nothing and is never superseded.
const UNTRACKED_READ: CatalogRead<never> = { current: () => true, replay: (rows) => rows, end: () => {} };

export interface ComputerRows<T> {
  rows: T[];
  answered: ReadonlySet<BackendKey>;
  attached: ReadonlySet<BackendKey>;
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
  const { catalog: cache, admit } = options;
  const targets = attachedBackends().filter((entry) => options.only === undefined || entry.id === options.only);
  const deadlineMs = options.deadlineMs === undefined ? COMPUTER_READ_DEADLINE_MS : options.deadlineMs;
  const results = await Promise.all(targets.map(async (target) => {
    const identity = getBackendIdentity(target.id);
    const pending: CatalogRead<T> = cache?.begin(target.id) ?? UNTRACKED_READ;
    function stillCurrent(): boolean {
      const current = getBackendIdentity(target.id);
      return pending.current() && attachedBackends().includes(target)
        && (!identity.backendId || (identity.backendId === current.backendId && identity.generation === current.generation));
    }
    let error: unknown;
    // A read past its deadline keeps recording: its late answer or late
    // failure ends it, not this call.
    let late = false;
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
      const rows = (deadlineMs === null ? await request : await readBeforeDeadline(request, deadlineMs, (answer) => {
        try {
          if (!stillCurrent()) return;
          const arrived = pending.replay(answer ?? []);
          for (const row of arrived) note(row, target.id);
          void cache?.write(target.id, arrived);
          commit({ rows: admit ? arrived.filter((row) => admit(row, target.id)) : arrived, answered: new Set([target.id]),
            attached: new Set(attachedBackends().map((entry) => entry.id)) });
        } finally { pending.end(); }
      }, () => pending.end())) ?? [];
      const current = getBackendIdentity(target.id);
      if (identity.backendId && (identity.backendId !== current.backendId || identity.generation !== current.generation)) {
        throw new Error('Computer history changed during the read.');
      }
      return { target, rows, answered: true, identity: current, pending, late, cached: false };
    } catch (reason) {
      error = reason;
      late = reason instanceof ReadDeadlineError;
    }
    const cachedIdentity = getBackendIdentity(target.id);
    const previous = cache?.hasRows(target.id) ?? false;
    const rows = cache && !previous ? await cache.read(target.id) : null;
    return { target, rows: rows ?? [], answered: false, error, identity: cachedIdentity, pending, late, cached: previous || rows !== null };
  }));
  // Replay once every computer has settled, so a mutation made while a slower
  // computer was still out is not lost. An unanswered computer keeps its
  // local rows, which already hold its mutations.
  for (const result of results) {
    if (result.answered) result.rows = result.pending.replay(result.rows);
    if (!result.late) result.pending.end();
  }
  const live = new Set<BackendEntry>(attachedBackends());
  const answered = new Set<BackendKey>();
  const rows: T[] = [];
  let error: unknown;
  let hasCache = false;
  let currentResults = 0;
  for (const result of results) {
    const target = result.target;
    // One final membership check covers both RPC and IndexedDB awaits.
    if (!live.has(target) || !result.pending.current()) continue;
    const current = getBackendIdentity(target.id);
    if (result.identity.backendId !== current.backendId || result.identity.generation !== current.generation) continue;
    currentResults++;
    // A failure settles here. An answer settles in `commit` with its rows
    // (settleCatalogAnswers), so no render sees a catalog marked loaded
    // before its rows land.
    if (!result.answered) cache?.settle?.(target.id, result.error);
    error ??= result.error;
    hasCache ||= result.cached;
    if (result.answered) {
      answered.add(target.id);
      if (cache) void cache.write(target.id, result.rows);
    }
    for (const row of result.rows) note(row, target.id);
  }
  // All origins must be indexed before admission: arrival/attachment order
  // cannot decide ownership when an offline catalog predates a move.
  for (const result of results) {
    const current = getBackendIdentity(result.target.id);
    if (!live.has(result.target) || !result.pending.current()
      || result.identity.backendId !== current.backendId || result.identity.generation !== current.generation) continue;
    for (const row of result.rows) if (!admit || admit(row, result.target.id)) rows.push(row);
  }
  // Superseded reads have no authority and no failure to report. In
  // particular, boot and a connection's first hello can overlap. Keep
  // cancellation distinct from both an authoritative empty list and an
  // actual failed dial, so callers cannot mark an unknown catalog loaded.
  if (currentResults === 0) return false;
  // An asleep saved computer is ordinary state. Its connection banner
  // owns the explanation; cached catalogs remain usable.
  // No answer and no cache is unknown, never an authoritative empty list:
  // treating it as empty would erase saved panes on a cold offline startup.
  if (answered.size === 0 && !hasCache) throw error ?? new Error('No computer could be reached.');
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
