// A structural catalog invalidation must survive a crash before IndexedDB's
// asynchronous rewrite. Small synchronous stamps fence the old envelope.
// Stamps describe catalogs, never individual threads; eviction fails closed.
import type { BackendKey } from '../transport/backendKey';
import type { CatalogKind } from './catalog';
import { randomId } from '../utils/randomId';

const PREFIX = 'agent-overflow:catalog-stamp:';
const LIMIT = 192;
const observed = new Map<string, string>();
const keyFor = (backend: BackendKey, kind: CatalogKind) => PREFIX + JSON.stringify([backend, kind]);

/** Read afresh so another window's invalidation fences this window's writes. */
export function currentCatalogStamp(backend: BackendKey, kind: CatalogKind): string {
  const key = keyFor(backend, kind);
  const stamp = localStorage.getItem(key);
  return stamp && /^[a-f\d-]{36}$/.test(stamp) ? stamp : replace(key);
}

function replace(key: string): string {
  const stamp = randomId();
  // Separate keys avoid a read/modify/write of a shared map: two windows
  // invalidating different computers must never restore each other's stamp.
  localStorage.setItem(key, stamp);
  const older: string[] = [];
  for (let i = 0; i < localStorage.length; i++) {
    const candidate = localStorage.key(i);
    if (candidate?.startsWith(PREFIX) && candidate !== key) older.push(candidate);
  }
  for (let i = 0; i < older.length - LIMIT + 1; i++) localStorage.removeItem(older[i]);
  return stamp;
}

export function observeCatalogStamp(backend: BackendKey, kind: CatalogKind, stamp: string): void {
  const key = keyFor(backend, kind);
  observed.delete(key);
  observed.set(key, stamp);
  while (observed.size > LIMIT) observed.delete(observed.keys().next().value!);
}

/** Ordinary local mutations keep the stamp under which their rows were read. */
export function observedCatalogStamp(backend: BackendKey, kind: CatalogKind): string | null {
  return observed.get(keyFor(backend, kind)) ?? null;
}

export function invalidateCatalogStamp(backend: BackendKey, kind: CatalogKind): void {
  observeCatalogStamp(backend, kind, replace(keyFor(backend, kind)));
}

export function resetCatalogStampsForTest(): void { observed.clear(); }
