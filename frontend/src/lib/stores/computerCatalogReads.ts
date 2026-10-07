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
// detaches.
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
interface ComputerReads {
  revision: Partial<Record<CatalogKind, number>>;
  journals: Partial<Record<CatalogKind, Set<Journal>>>;
}

const computers = new Map<BackendKey, ComputerReads>();
let next = 0;

function readsOf(backend: BackendKey): ComputerReads {
  let reads = computers.get(backend);
  if (!reads) { reads = { revision: {}, journals: {} }; computers.set(backend, reads); }
  return reads;
}

export function beginCatalogRead<T>(backend: BackendKey, kind: CatalogKind): CatalogRead<T> {
  const reads = readsOf(backend);
  const revision = reads.revision[kind] = ++next;
  const journal: Journal = [];
  const open = reads.journals[kind] ??= new Set();
  open.add(journal);
  return {
    current: () => revision === catalogRevision(backend, kind),
    replay: (rows) => (journal as CatalogMutation<T>[]).reduce((acc, mutation) => mutation(acc), rows),
    end: () => { open.delete(journal); },
  };
}

/** Record a local mutation for every read of `backend`'s catalog in flight. */
export function recordCatalogMutation<T>(backend: BackendKey, kind: CatalogKind, mutation: CatalogMutation<T>): void {
  for (const journal of computers.get(backend)?.journals[kind] ?? []) journal.push(mutation as CatalogMutation<unknown>);
}

/** The newest read's revision; 0 before any. */
export function catalogRevision(backend: BackendKey, kind: CatalogKind): number {
  return computers.get(backend)?.revision[kind] ?? 0;
}

/** Test seam: the reads of `backend`'s catalog still recording. */
export function openCatalogReadsForTest(backend: BackendKey, kind: CatalogKind): number {
  return computers.get(backend)?.journals[kind]?.size ?? 0;
}

onBackendDetached(({ backendId }) => { computers.delete(backendId); });
