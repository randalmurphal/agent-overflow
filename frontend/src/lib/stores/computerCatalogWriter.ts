import { recordCatalogMutation, type CatalogMutation } from './computerCatalogReads';
// Persist metadata at store mutation boundaries, never from a render effect.
// One write per computer may run at once; bursts collapse into the latest
// snapshot. Tokens prevent a queued write reaching a replaced/detached store.
import { untrack } from 'svelte';
import { backendById } from '../transport/backends';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { putReplicaCatalog, replicaToken } from '../replica/session';
import { observedCatalogStamp } from '../replica/catalogStamp';
import type { CatalogKind, CatalogRows } from '../replica/catalog';

export function computerCatalogWriter<K extends CatalogKind>(
  kind: K, rows: () => CatalogRows[K][], commit: (rows: CatalogRows[K][]) => void,
  owner: (row: CatalogRows[K]) => BackendKey | undefined,
) {
  const pending = new Map<BackendKey, number>();
  const running = new Set<BackendKey>();
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
        if (running.has(backend)) continue;
        pending.delete(backend);
        if (!backendById(backend) || replicaToken(backend) !== token) continue;
        running.add(backend);
        const selected = snapshot.filter((row) => (owner(row) ?? HOME_BACKEND) === backend);
        void putReplicaCatalog(backend, kind, selected, observedCatalogStamp(backend, kind), token).finally(() => {
          if (version !== epoch) return;
          running.delete(backend);
          if (pending.has(backend)) schedule();
        });
      }
    });
  }

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
    reset(): void { ++epoch; queued = false; pending.clear(); running.clear(); },
  };
}

function sameRows<T>(a: readonly T[], b: readonly T[]): boolean {
  return a === b || (a.length === b.length && a.every((row, index) => row === b[index]));
}
