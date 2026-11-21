// Catalog reads in flight, per computer and kind.
//
// A newer read supersedes an older one: only the newest read of a computer's
// catalog settles it. A local mutation supersedes nothing. Every read in
// flight records it and replays it over its answer, so the answer commits
// with each change made while it was out instead of being dropped. A dropped
// answer would leave a catalog that has not loaded with no read to load it.
//
// Mutations are pure transforms of a computer's rows, recorded only for the
// computers whose rows they touch. A journal lives as long as its RPC, which
// the transport bounds, and is dropped when the read ends or its computer
// detaches. Every read ends: its answer, its failure, or the late answer or
// late failure of a read past its deadline ends it.
import { onBackendDetached } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { CatalogKind } from '../replica/catalog';

export type CatalogMutation<T> = (rows: T[]) => T[];

export interface CatalogRead<T> {
  /** False once a newer read of the same computer's catalog has begun. */
  current(): boolean;
  /** `rows` with every mutation recorded since the read began applied in order. */
  replay(rows: T[]): T[];
  /** Stop recording. Idempotent. */
  end(): void;
}

type Journal = CatalogMutation<unknown>[];
interface OpenRead {
  journal: Journal;
  /** Called once, when the read can no longer commit. */
  release: () => void;
}
interface ComputerReads {
  revision: Partial<Record<CatalogKind, number>>;
  open: Partial<Record<CatalogKind, Map<number, OpenRead>>>;
}

const computers = new Map<BackendKey, ComputerReads>();
let next = 0;

function readsOf(backend: BackendKey): ComputerReads {
  let reads = computers.get(backend);
  if (!reads) { reads = { revision: {}, open: {} }; computers.set(backend, reads); }
  return reads;
}

/**
 * Begin a read of `backend`'s catalog, superseding the one before it.
 * `onRelease` runs once, at the first of: the read ends, or a newer read of
 * the same catalog begins. A detached computer's read still ends, and its
 * answer fails the attached check where it would commit.
 */
export function beginCatalogRead<T>(backend: BackendKey, kind: CatalogKind, onRelease?: () => void): CatalogRead<T> {
  const reads = readsOf(backend);
  const open = reads.open[kind] ??= new Map();
  const superseded = open.get(reads.revision[kind] ?? 0);
  const revision = reads.revision[kind] = ++next;
  let released = false;
  const read: OpenRead = {
    journal: [],
    release: () => {
      if (released) return;
      released = true;
      onRelease?.();
    },
  };
  open.set(revision, read);
  superseded?.release();
  return {
    current: () => revision === catalogRevision(backend, kind),
    replay: (rows) => (read.journal as CatalogMutation<T>[]).reduce((acc, mutation) => mutation(acc), rows),
    end: () => {
      open.delete(revision);
      read.release();
    },
  };
}

/** Record a local mutation for every read of `backend`'s catalog in flight. */
export function recordCatalogMutation<T>(backend: BackendKey, kind: CatalogKind, mutation: CatalogMutation<T>): void {
  for (const read of computers.get(backend)?.open[kind]?.values() ?? []) read.journal.push(mutation as CatalogMutation<unknown>);
}

/** The newest read's revision; 0 before any. */
export function catalogRevision(backend: BackendKey, kind: CatalogKind): number {
  return computers.get(backend)?.revision[kind] ?? 0;
}

/**
 * Whether the newest read of `backend`'s catalog is still out: begun and not
 * ended. A read past its startup deadline is still out until its late
 * answer or late failure ends it.
 */
export function catalogReadInFlight(backend: BackendKey, kind: CatalogKind): boolean {
  const reads = computers.get(backend);
  return reads?.open[kind]?.has(reads.revision[kind] ?? 0) ?? false;
}

/** Test seam: the reads of `backend`'s catalog still recording. */
export function openCatalogReadsForTest(backend: BackendKey, kind: CatalogKind): number {
  return computers.get(backend)?.open[kind]?.size ?? 0;
}

onBackendDetached(({ backendId }) => { computers.delete(backendId); });
