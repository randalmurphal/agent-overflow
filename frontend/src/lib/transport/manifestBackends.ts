// The attached-backend list as the bootstrap manifest published it, and
// the per-backend manifest fetcher, held in a LEAF.
//
// Why a leaf and not a direct call: ./bootstrap.ts is imported by
// ./wsClient.ts, which is imported by ./backends.ts. A `bootstrap →
// backends` import would close that ring, and the ring has module-level
// side effects at both ends (the `wsClient` singleton, the home registry
// entry), so whichever module the bundler happened to enter first would
// decide whether the app booted. That is not a hypothetical: `backends.ts`
// reads `wsClient` during its own evaluation, which is a ReferenceError
// while `wsClient.ts` is still initialising.
//
// So the manifest PUBLISHES here, the same way it publishes grants,
// harness mode, passkey availability and backend identity into their own
// leaves, and ./backends.ts reads. One direction, no ring, and the
// registry's source stays the single injectable function it is meant to
// be — this module is simply the default one's backing store.

import type { Bootstrap } from './bootstrap';
import type { BackendDescriptor } from './backends';

let descriptors: readonly BackendDescriptor[] = [];
const listeners = new Set<() => void>();

/**
 * Read the `backends` array off a resolved bootstrap manifest.
 *
 * Every field is validated as a string rather than trusted, for the reason
 * ./bootstrap.ts validates `wsUrl`: an entry this build cannot read is
 * dropped, never coerced into a connection to somewhere unintended.
 * Unknown extra fields are ignored — frames evolve additively.
 */
export function readBackendDescriptors(value: unknown): BackendDescriptor[] {
  if (!Array.isArray(value)) return [];
  const out: BackendDescriptor[] = [];
  for (const raw of value) {
    if (!raw || typeof raw !== 'object') continue;
    const row = raw as Partial<BackendDescriptor>;
    if (typeof row.id !== 'string' || row.id === '') continue;
    if (typeof row.wsUrl !== 'string' || typeof row.bootstrapUrl !== 'string') continue;
    out.push({
      id: row.id,
      backendId: typeof row.backendId === 'string' ? row.backendId : '',
      name: typeof row.name === 'string' ? row.name : '',
      wsUrl: row.wsUrl,
      bootstrapUrl: row.bootstrapUrl,
    });
  }
  return out;
}

/** What the last resolved manifest named. Empty on every boot that has
 *  one backend, which is every boot today. */
export function manifestBackendDescriptors(): readonly BackendDescriptor[] {
  return descriptors;
}

/**
 * Publish the list a manifest just named. Called on every manifest
 * resolution — the first fetch and every reconnect refetch — so a backend
 * added or removed from Settings takes effect without a reload.
 *
 * Notifies only when the list actually MOVED: a reconnect repeats the same
 * array, and re-running the attach sweep on every reconnect would be pure
 * work for an answer that did not change.
 */
export function publishManifestBackends(next: readonly BackendDescriptor[]): void {
  if (sameDescriptors(descriptors, next)) return;
  descriptors = next;
  for (const listener of listeners) listener();
}

function sameDescriptors(
  a: readonly BackendDescriptor[],
  b: readonly BackendDescriptor[],
): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i += 1) {
    if (
      a[i].id !== b[i].id ||
      a[i].backendId !== b[i].backendId ||
      a[i].name !== b[i].name ||
      a[i].wsUrl !== b[i].wsUrl ||
      a[i].bootstrapUrl !== b[i].bootstrapUrl
    ) {
      return false;
    }
  }
  return true;
}

export function onManifestBackendsChanged(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * How an attached backend's manifest is fetched. Installed by
 * ./bootstrap.ts, which owns the exchange and its validation, and read by
 * ./backends.ts when it constructs a client — the same one-direction rule
 * as the list above.
 */
export type BackendManifestFetcher = (descriptor: BackendDescriptor) => Promise<Bootstrap>;

let fetcher: BackendManifestFetcher | null = null;

export function setBackendManifestFetcher(next: BackendManifestFetcher): void {
  fetcher = next;
}

export function fetchBackendManifest(descriptor: BackendDescriptor): Promise<Bootstrap> {
  if (fetcher === null) {
    return Promise.reject(new Error('backend manifest fetcher not installed'));
  }
  return fetcher(descriptor);
}

/** Test seam: forget the published list. The fetcher is module wiring and
 *  is deliberately kept. */
export function __resetManifestBackendsForTest(): void {
  descriptors = [];
}
