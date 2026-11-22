// The one writer of a computer's kept catalogs, written at store mutation
// boundaries and when a round completes a tree, never from a render effect.
//
// Every write is the computer's whole tree: its three catalog records, in
// one IndexedDB transaction (../replica/session.ts#putReplicaTree), so the
// replica never holds catalogs that were not written together. Each computer
// has one queue here:
//   - A change to any of its catalogs, and a round that answered all three
//     (persistCatalogTree), ask for a write. The request stays waiting until
//     a write that started after it lands.
//   - A write starts only while the computer's three catalogs are one tree
//     (./catalogLoad.svelte.ts). A request made before they are one, such as
//     a change pushed while kept rows are read, waits, and the queue wakes
//     when they become one (onCatalogTree).
//   - A write carries the stamp this window holds for each catalog. When one
//     is not the current stamp, the write waits until this window holds
//     another, which it does once it has rows read or changed under the
//     current stamp (onCatalogStampObserved): another window's invalidation
//     must not bless rows this window read before it.
//   - A write IndexedDB failed is tried again after the spacing. A write for
//     a session that is gone is dropped with the session's requests.
//   - One write per computer runs at a time.
//
// A write encodes every row of the computer's catalogs, so its cost grows
// with them while changes arrive per row. A computer's writes therefore
// start at least CATALOG_WRITE_SPACING_MS apart: the first request after a
// quiet spell is written at once, and requests during the spacing are
// written together when it ends. A page going hidden writes what is waiting,
// so a window that closes does not leave the offline tree behind. Tokens keep
// a queued write from reaching a replaced or detached session.
//
// Windows are not ordered against each other: the backend gives catalogs no
// revision, so the last window to write a computer's tree wins.
import { untrack } from 'svelte';
import { backendById } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { putReplicaTree, replicaToken, type CatalogWriteOutcome, type TreeCatalog } from '../replica/session';
import { observedCatalogStamp, onCatalogStampObserved } from '../replica/catalogStamp';
import type { CatalogKind, CatalogRowCache, CatalogRows } from '../replica/catalog';
import { catalogTreeOf, noteCatalogChange, onCatalogTree } from './catalogLoad.svelte';
import { recordCatalogMutation, type CatalogMutation } from './computerCatalogReads';

export const CATALOG_WRITE_SPACING_MS = 2000;

export interface CatalogWriterOptions {
  /**
   * The store never writes into a row it holds: a change replaces the row.
   * Each write then encodes only the rows replaced since the last one.
   */
  rowsAreValues?: boolean;
}

// Where the queue reads each catalog's rows: the store that holds them.
interface CatalogSource<K extends CatalogKind> {
  rows(): readonly CatalogRows[K][];
  owner(row: CatalogRows[K]): BackendKey | undefined;
  cache: CatalogRowCache | undefined;
}
const sources: { [K in CatalogKind]?: CatalogSource<K> } = {};

// What one computer has waiting and running.
interface ComputerWrites {
  /** The replica session the requests are for. */
  token: number;
  /** Requests made, counted; the write that lands carries the count it started with. */
  requested: number;
  /** The request count the last landed write started with. */
  saved: number;
  running: boolean;
  /** Set while the last write started less than the spacing ago. */
  spacing: ReturnType<typeof setTimeout> | null;
  /** The stamps this window held when a write met a stale one, until it holds another. */
  staleAt: number | null;
}
const computers = new Map<BackendKey, ComputerWrites>();
// How many stamps this window has come to hold, per computer.
const stampsObserved = new Map<BackendKey, number>();
let queued = false;
let epoch = 0;

function clearSpacing(writes: ComputerWrites): void {
  if (writes.spacing !== null) clearTimeout(writes.spacing);
  writes.spacing = null;
}

function request(backend: BackendKey): void {
  if (!backendById(backend)) return;
  const token = replicaToken(backend);
  let writes = computers.get(backend);
  if (writes === undefined || writes.token !== token) {
    if (writes !== undefined) clearSpacing(writes);
    writes = { token, requested: 0, saved: 0, running: false, spacing: null, staleAt: null };
    computers.set(backend, writes);
  }
  writes.requested++;
  schedule();
}

function schedule(): void {
  if (queued) return;
  queued = true;
  const version = epoch;
  queueMicrotask(() => {
    if (version !== epoch) return;
    queued = false;
    for (const [backend, writes] of computers) drain(backend, writes);
  });
}

function sourceOf<K extends CatalogKind>(kind: K): CatalogSource<K> {
  const source = sources[kind] as CatalogSource<K> | undefined;
  if (source === undefined) throw new Error(`No store holds the ${kind} catalog the replica writes.`);
  return source;
}

function catalogOf<K extends CatalogKind>(backend: BackendKey, kind: K): TreeCatalog<K> {
  const source = sourceOf(kind);
  const rows = untrack(source.rows).filter((row) => (source.owner(row) ?? HOME_BACKEND) === backend);
  return { rows, stamp: observedCatalogStamp(backend, kind), cache: source.cache };
}

function drain(backend: BackendKey, writes: ComputerWrites): void {
  if (!backendById(backend) || replicaToken(backend) !== writes.token) {
    drop(backend, writes);
    return;
  }
  if (writes.running || writes.spacing !== null || writes.requested === writes.saved) return;
  const observed = stampsObserved.get(backend) ?? 0;
  if (writes.staleAt === observed || catalogTreeOf(backend) === null) return;
  writes.staleAt = null;
  const started = writes.requested;
  const catalogs = {
    projects: catalogOf(backend, 'projects'),
    threads: catalogOf(backend, 'threads'),
    groups: catalogOf(backend, 'groups'),
  };
  writes.running = true;
  writes.spacing = setTimeout(() => {
    writes.spacing = null;
    schedule();
  }, CATALOG_WRITE_SPACING_MS);
  const version = epoch;
  void putReplicaTree(backend, catalogs, writes.token).then((outcome) => {
    if (version !== epoch || computers.get(backend) !== writes) return;
    writes.running = false;
    settle(backend, writes, outcome, started, observed);
    schedule();
  });
}

function settle(backend: BackendKey, writes: ComputerWrites, outcome: CatalogWriteOutcome, started: number, observed: number): void {
  switch (outcome) {
    case 'written':
      writes.saved = started;
      return;
    case 'failed':
      // The spacing's end tries it again.
      return;
    case 'stale-stamp':
      writes.staleAt = observed;
      return;
    case 'gone':
      drop(backend, writes);
      return;
  }
  outcome satisfies never;
}

function drop(backend: BackendKey, writes: ComputerWrites): void {
  clearSpacing(writes);
  if (computers.get(backend) === writes) computers.delete(backend);
}

onCatalogTree(() => { if (computers.size > 0) schedule(); });
onCatalogStampObserved((backend) => {
  stampsObserved.set(backend, (stampsObserved.get(backend) ?? 0) + 1);
  if (computers.has(backend)) schedule();
});

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'hidden') return;
    for (const writes of computers.values()) clearSpacing(writes);
    if (computers.size > 0) schedule();
  });
}

/** Write `backend`'s tree: a round just answered and committed all three of its catalogs. */
export function persistCatalogTree(backend: BackendKey): void {
  request(backend);
}

/** Test seam: drop every waiting write and timer. */
export function resetCatalogWritesForTest(): void {
  ++epoch;
  queued = false;
  for (const writes of computers.values()) clearSpacing(writes);
  computers.clear();
  stampsObserved.clear();
}

/**
 * The writer of one store's catalog: it applies the store's changes and asks
 * the computer's queue to write them. Creating one registers the store as
 * where the queue reads that catalog's rows.
 */
export function computerCatalogWriter<K extends CatalogKind>(
  kind: K, rows: () => CatalogRows[K][], commit: (rows: CatalogRows[K][]) => void,
  owner: (row: CatalogRows[K]) => BackendKey | undefined,
  options: CatalogWriterOptions = {},
) {
  (sources as Record<K, CatalogSource<K>>)[kind] = { rows, owner, cache: options.rowsAreValues ? new WeakMap() : undefined };

  function changed(backend: BackendKey): void {
    noteCatalogChange(kind, backend);
    request(backend);
  }

  return {
    /**
     * Apply a local mutation to the rows of `backends`: commit it, record it
     * for their reads in flight (./computerCatalogReads.ts) and ask the
     * computer's queue to write it. `mutation` must be a pure transform that
     * touches only those computers' rows, because it is replayed over each
     * one's answer.
     */
    mutate(backends: BackendKey | undefined | Iterable<BackendKey | undefined>, mutation: CatalogMutation<CatalogRows[K]>): void {
      const current = untrack(rows);
      const next = mutation(current);
      if (!sameRows(current, next)) commit(next);
      const targets = typeof backends === 'string' || backends === undefined ? [backends] : backends;
      for (const backend of new Set(Array.from(targets, (entry) => entry ?? HOME_BACKEND))) {
        recordCatalogMutation(backend, kind, mutation);
        changed(backend);
      }
    },
    /**
     * Note a change the store's rows of `backend` already show, such as a
     * row whose owner moved away, and ask the computer's queue to write it.
     */
    changed,
  };
}

function sameRows<T>(a: readonly T[], b: readonly T[]): boolean {
  return a === b || (a.length === b.length && a.every((row, index) => row === b[index]));
}
