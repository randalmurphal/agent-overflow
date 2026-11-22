// Catalog reads in flight, per computer and kind.
//
// A newer read supersedes an older one: only the newest read of a computer's
// catalog settles it. A local mutation supersedes nothing. Every read in
// flight records it and replays it over its answer, so the answer commits
// with each change made while it was out instead of being dropped. A dropped
// answer would leave a catalog that has not loaded with no read to load it.
//
// Mutations are pure transforms of a computer's rows, recorded only for the
// computers whose rows they touch. A read is released when it ends or a newer
// read of its catalog begins, whichever is first: it stops recording, holds
// no mutation (so none of the rows they capture) and runs its `onRelease`
// once. A superseded read can never commit, so nothing replays it. A read
// whose computer detaches keeps its journal until it ends.
import { onBackendDetached } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { CatalogKind } from '../replica/catalog';

export type CatalogMutation<T> = (rows: T[]) => T[];

export interface CatalogRead<T> {
  /** False once a newer read of the same computer's catalog has begun. */
  current(): boolean;
  /** `rows` with every mutation recorded since the read began applied in order. Throws once released. */
  replay(rows: T[]): T[];
  /** How many mutations the read has recorded; 0 once released. */
  changes(): number;
  /** Release the read, unless a newer read already did. Idempotent. */
  end(): void;
}

type Journal = CatalogMutation<unknown>[];
interface ComputerReads {
  revision: Partial<Record<CatalogKind, number>>;
  /** The reads recording, by their journal, with what releases each. */
  journals: Partial<Record<CatalogKind, Map<Journal, () => void>>>;
}

const computers = new Map<BackendKey, ComputerReads>();
let next = 0;

function readsOf(backend: BackendKey): ComputerReads {
  let reads = computers.get(backend);
  if (!reads) { reads = { revision: {}, journals: {} }; computers.set(backend, reads); }
  return reads;
}

/**
 * Begin a read of `backend`'s `kind` catalog, superseding and releasing every
 * older one. `onRelease` runs once, when this read ends or is superseded.
 */
export function beginCatalogRead<T>(backend: BackendKey, kind: CatalogKind, onRelease: () => void = () => {}): CatalogRead<T> {
  const reads = readsOf(backend);
  const open = reads.journals[kind] ??= new Map();
  const revision = reads.revision[kind] = ++next;
  for (const releaseOlder of open.values()) releaseOlder();
  let journal: Journal | null = [];
  const release = () => {
    if (journal === null) return;
    open.delete(journal);
    journal = null;
    onRelease();
  };
  open.set(journal, release);
  return {
    current: () => revision === catalogRevision(backend, kind),
    replay: (rows) => {
      if (journal === null) throw new Error(`A ${kind} read replayed its journal after it was released.`);
      return (journal as CatalogMutation<T>[]).reduce((acc, mutation) => mutation(acc), rows);
    },
    changes: () => journal?.length ?? 0,
    end: release,
  };
}

/** Record a local mutation for every read of `backend`'s catalog in flight. */
export function recordCatalogMutation<T>(backend: BackendKey, kind: CatalogKind, mutation: CatalogMutation<T>): void {
  for (const journal of computers.get(backend)?.journals[kind]?.keys() ?? []) journal.push(mutation as CatalogMutation<unknown>);
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

/** Test seam: forget every read, as if every computer had detached. */
export function resetCatalogReadsForTest(): void {
  computers.clear();
}
