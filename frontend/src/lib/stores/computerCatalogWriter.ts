// A computer's kept catalogs, written at store mutation boundaries and when a
// round completes a tree, never from a render effect.
//
// The replica keeps a computer's last complete tree and the changes pushed
// onto it, nothing else (./catalogLoad.svelte.ts has the provenance). Every
// write of the three catalog records goes through one queue per computer,
// here:
//   - A round that answered all three catalogs asks for the tree
//     (persistCatalogTree): the three records, written in one IndexedDB
//     transaction under the tree's id. It does not wait out the spacing.
//   - A change to a catalog that is `tree` asks for that one record, written
//     only while the record stored holds the same tree. A catalog that is
//     `none`, `unrecorded` or `partial` is never written: its rows are not a
//     tree the replica holds.
//   - One write per computer runs at a time, and a tree waiting to be written
//     goes before any single record and takes its place, so no record of an
//     older tree can land after a newer tree's transaction.
//   - A write IndexedDB fails stays waiting and is tried again after the
//     spacing; nothing counts as written until it is. A write that must not
//     land (another window replaced a stamp, the record holds another tree,
//     the session is gone) is dropped.
//
// A write encodes every row of the computer's catalog, so its cost grows
// with the catalog while changes arrive per row. A catalog's single-record
// writes therefore start at least CATALOG_WRITE_SPACING_MS apart: the first
// change after a quiet spell is written at once, and changes during the
// spacing are written together when it ends. A page going hidden writes what
// is waiting, so a window that closes does not leave the offline tree behind.
// Tokens keep a queued write from reaching a replaced or detached session.
import { untrack } from 'svelte';
import { backendById } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { putReplicaCatalog, putReplicaTree, replicaToken, type CatalogWriteOutcome, type TreeCatalog } from '../replica/session';
import { observedCatalogStamp } from '../replica/catalogStamp';
import type { CatalogKind, CatalogRowCache, CatalogRows } from '../replica/catalog';
import { catalogProvenance, catalogTreeOf, noteCatalogChange } from './catalogLoad.svelte';
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
  /** The replica session the waiting writes are for. */
  token: number;
  /** The tree is waiting to be written. */
  tree: boolean;
  /** Catalogs whose single record is waiting to be written. */
  kinds: Set<CatalogKind>;
  running: boolean;
  /** Catalogs whose last single-record write started less than the spacing ago. */
  spacing: Map<CatalogKind, ReturnType<typeof setTimeout>>;
  /** Set while a failed tree write waits to be tried again. */
  retry: ReturnType<typeof setTimeout> | null;
}
const computers = new Map<BackendKey, ComputerWrites>();
let queued = false;
let epoch = 0;

function clearTimers(writes: ComputerWrites): void {
  for (const timer of writes.spacing.values()) clearTimeout(timer);
  writes.spacing.clear();
  if (writes.retry !== null) clearTimeout(writes.retry);
  writes.retry = null;
}

function request(backend: BackendKey, kind: CatalogKind | null): void {
  if (!backendById(backend)) return;
  const token = replicaToken(backend);
  let writes = computers.get(backend);
  if (writes === undefined || writes.token !== token) {
    if (writes !== undefined) clearTimers(writes);
    writes = { token, tree: false, kinds: new Set(), running: false, spacing: new Map(), retry: null };
    computers.set(backend, writes);
  }
  if (kind === null) writes.tree = true;
  else writes.kinds.add(kind);
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
  if (writes.running) return;
  if (!backendById(backend) || replicaToken(backend) !== writes.token) {
    clearTimers(writes);
    computers.delete(backend);
    return;
  }
  if (writes.tree) {
    if (writes.retry !== null) return;
    writes.tree = false;
    const tree = catalogTreeOf(backend);
    if (tree !== null) {
      // The tree's snapshot holds every change waiting for a single record.
      writes.kinds.clear();
      const catalogs = {
        projects: catalogOf(backend, 'projects'),
        threads: catalogOf(backend, 'threads'),
        groups: catalogOf(backend, 'groups'),
      };
      run(backend, writes, putReplicaTree(backend, tree, catalogs, writes.token), () => {
        writes.tree = true;
        writes.retry = setTimeout(() => {
          writes.retry = null;
          schedule();
        }, CATALOG_WRITE_SPACING_MS);
      });
      return;
    }
  }
  for (const kind of writes.kinds) {
    if (writes.spacing.has(kind)) continue;
    writes.kinds.delete(kind);
    const provenance = catalogProvenance(backend, kind);
    if (provenance.kind !== 'tree') continue;
    const { rows, stamp, cache } = catalogOf(backend, kind);
    writes.spacing.set(kind, setTimeout(() => {
      writes.spacing.delete(kind);
      schedule();
    }, CATALOG_WRITE_SPACING_MS));
    run(backend, writes, putReplicaCatalog(backend, kind, rows, provenance, stamp, writes.token, cache), () => {
      writes.kinds.add(kind);
    });
    return;
  }
}

function run(backend: BackendKey, writes: ComputerWrites, write: Promise<CatalogWriteOutcome>, onFailed: () => void): void {
  writes.running = true;
  const version = epoch;
  void write.then((outcome) => {
    if (version !== epoch || computers.get(backend) !== writes) return;
    writes.running = false;
    if (outcome === 'failed') onFailed();
    schedule();
  });
}

if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState !== 'hidden') return;
    for (const writes of computers.values()) clearTimers(writes);
    if (computers.size > 0) schedule();
  });
}

/** Write `backend`'s tree: a round just answered and committed all three of its catalogs. */
export function persistCatalogTree(backend: BackendKey): void {
  request(backend, null);
}

/** Test seam: drop every waiting write and timer. */
export function resetCatalogWritesForTest(): void {
  ++epoch;
  queued = false;
  for (const writes of computers.values()) clearTimers(writes);
  computers.clear();
}

/**
 * The writer of one store's catalog: it applies the store's changes and asks
 * the computer's queue to write what they changed. Creating one registers the
 * store as where the queue reads that catalog's rows.
 */
export function computerCatalogWriter<K extends CatalogKind>(
  kind: K, rows: () => CatalogRows[K][], commit: (rows: CatalogRows[K][]) => void,
  owner: (row: CatalogRows[K]) => BackendKey | undefined,
  options: CatalogWriterOptions = {},
) {
  (sources as Record<K, CatalogSource<K>>)[kind] = { rows, owner, cache: options.rowsAreValues ? new WeakMap() : undefined };

  function changed(backend: BackendKey): void {
    noteCatalogChange(kind, backend);
    request(backend, kind);
  }

  return {
    /**
     * Apply a local mutation to the rows of `backends`: commit it, record it
     * for their reads in flight (./computerCatalogReads.ts) and ask the
     * computer's queue to write it.
     * `mutation` must be a pure transform that touches only those computers'
     * rows, because it is replayed over each one's answer.
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
