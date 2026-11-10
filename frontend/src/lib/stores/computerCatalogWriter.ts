import { recordCatalogMutation, type CatalogMutation } from './computerCatalogReads';
// Persist metadata at store mutation boundaries, never from a render effect.
// One write per computer may run at once; bursts collapse into the latest
// snapshot. Tokens prevent a queued write reaching a replaced/detached store.
//
// A write encodes every row of the computer's catalog, so its cost grows
// with the catalog while changes arrive per row. A computer's writes
// therefore start at least CATALOG_WRITE_SPACING_MS apart: the first change
// after a quiet spell is written at once, and changes during the spacing are
// written together when it ends. A page going hidden writes what is waiting,
// so a window that closes does not leave the offline snapshot behind.
import { untrack } from 'svelte';
import { backendById } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { putReplicaCatalog, replicaToken } from '../replica/session';
import { observedCatalogStamp } from '../replica/catalogStamp';
import type { CatalogKind, CatalogRows } from '../replica/catalog';

export const CATALOG_WRITE_SPACING_MS = 2000;

const flushers = new Set<() => void>();
if (typeof document !== 'undefined') {
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') for (const flush of flushers) flush();
  });
}

export function computerCatalogWriter<K extends CatalogKind>(
  kind: K, rows: () => CatalogRows[K][], commit: (rows: CatalogRows[K][]) => void,
  owner: (row: CatalogRows[K]) => BackendKey | undefined,
) {
  const pending = new Map<BackendKey, number>();
  const running = new Set<BackendKey>();
  // Computers whose last write started less than the spacing ago.
  const spacing = new Map<BackendKey, ReturnType<typeof setTimeout>>();
  let queued = false;
  let epoch = 0;

  function schedule(): void {
    if (queued) return;
    queued = true;
    const version = epoch;
    queueMicrotask(() => {
      if (version !== epoch) return;
      queued = false;
      const snapshot = untrack(rows);
      for (const [backend, token] of pending) {
        if (running.has(backend) || spacing.has(backend)) continue;
        pending.delete(backend);
        if (!backendById(backend) || replicaToken(backend) !== token) continue;
        running.add(backend);
        spacing.set(backend, setTimeout(() => {
          spacing.delete(backend);
          if (pending.has(backend)) schedule();
        }, CATALOG_WRITE_SPACING_MS));
        const selected = snapshot.filter((row) => (owner(row) ?? HOME_BACKEND) === backend);
        void putReplicaCatalog(backend, kind, selected, observedCatalogStamp(backend, kind), token).finally(() => {
          if (version !== epoch) return;
          running.delete(backend);
          if (pending.has(backend)) schedule();
        });
      }
    });
  }

  function endSpacing(): void {
    for (const timer of spacing.values()) clearTimeout(timer);
    spacing.clear();
  }
  flushers.add(() => {
    endSpacing();
    if (pending.size > 0) schedule();
  });

  function persist(backend: BackendKey = HOME_BACKEND): void {
    if (!backendById(backend)) return;
    pending.set(backend, replicaToken(backend));
    schedule();
  }

  return {
    /**
     * Apply a local mutation to the rows of `backends`: commit it, record it
     * for their reads in flight (./computerCatalogReads.ts) and persist it.
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
        persist(backend);
      }
    },
    /** Persist rows a read committed. Nothing is recorded: they are an answer, not a change. */
    persist,
    reset(): void {
      ++epoch; queued = false; pending.clear(); running.clear();
      endSpacing();
    },
  };
}

function sameRows<T>(a: readonly T[], b: readonly T[]): boolean {
  return a === b || (a.length === b.length && a.every((row, index) => row === b[index]));
}
