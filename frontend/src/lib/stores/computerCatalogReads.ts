// A bounded cancellation stamp for catalog reads. A later read or local
// mutation supersedes an older snapshot, including one finishing offline.
import { onBackendDetached } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { CatalogKind } from '../replica/catalog';
const revisions = new Map<BackendKey, Partial<Record<CatalogKind, number>>>();
let next = 0;
export function advanceCatalogRevision(backend: BackendKey, kind: CatalogKind): number {
  let held = revisions.get(backend);
  if (!held) { held = {}; revisions.set(backend, held); }
  return held[kind] = ++next;
}
export function catalogRevision(backend: BackendKey, kind: CatalogKind): number { return revisions.get(backend)?.[kind] ?? 0; }
onBackendDetached(({ backendId }) => { revisions.delete(backendId); });
