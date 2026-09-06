import { advanceCatalogRevision } from './computerCatalogRevision';
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
  kind: K, rows: () => readonly CatalogRows[K][], owner: (row: CatalogRows[K]) => BackendKey | undefined,
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

  return {
    changed(backend: BackendKey = HOME_BACKEND, mutation = true): void {
      if (!backendById(backend)) return;
      if (mutation) advanceCatalogRevision(backend, kind);
      pending.set(backend, replicaToken(backend));
      schedule();
    },
    reset(): void { ++epoch; queued = false; pending.clear(); running.clear(); },
  };
}
