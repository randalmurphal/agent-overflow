// Backend history identity carried by the bootstrap manifest
// (`backendId` / `replicaGeneration`, internal/transport/server.go).
//
// `backendId` keys the client-side replica database; `replicaGeneration`
// is re-minted whenever the backend's rev/epoch continuity breaks for a
// reason the counters cannot express (a restored database rewinds them).
// Both are refetched on every reconnect, which is the only moment a
// mid-session generation change can be observed — hence the subscription
// rather than a one-shot read.
//
// Either field empty means "this backend does not identify its history"
// (the `--connect` stub injects its own manifest and carries neither);
// consumers must treat that as replica-disabled, not as a wildcard.
//
// **One identity per ATTACHED BACKEND, keyed by registry id**
// (./backendKey.ts). Each backend fetches its own manifest over its own
// socket, so each has its own identity and its own generation re-mints;
// a single map with `HOME_BACKEND` as the default key is what lets every
// existing call site keep its shape while a second backend gets a slot of
// its own. Subscribers are told WHICH backend moved, because "the
// identity changed" answers nothing once there is more than one.

import { HOME_BACKEND, type BackendKey } from './backendKey';

export interface BackendIdentity {
  backendId: string;
  generation: string;
  /**
   * What a person calls this machine — its hostname, the same string the
   * pairing payload showed (docs/specs/remote-access.md §10, "Machine
   * name"). DISPLAY ONLY: nothing is keyed on it, two backends may
   * legitimately answer the same one, and `backendId` stays the
   * identity. Empty means the backend published none, and a surface
   * renders the id or "unknown machine" rather than guessing.
   *
   * Deliberately alongside the replica fields rather than in a store of
   * its own: it arrives on the same manifest, on the same schedule, and
   * a second subscription would be a second moment for the two to
   * disagree about which backend is being described.
   */
  name: string;
}

export const UNKNOWN_BACKEND_IDENTITY: BackendIdentity = {
  backendId: '',
  generation: '',
  name: '',
};

type IdentityListener = (identity: BackendIdentity, backendId: BackendKey) => void;

// Per-backend identity. Home has an entry from module load so a reader
// before the first manifest gets the same "unknown" answer it always did.
const identities = new Map<BackendKey, BackendIdentity>([
  [HOME_BACKEND, UNKNOWN_BACKEND_IDENTITY],
]);
const listeners = new Set<IdentityListener>();

export function getBackendIdentity(backendId: BackendKey = HOME_BACKEND): BackendIdentity {
  return identities.get(backendId) ?? UNKNOWN_BACKEND_IDENTITY;
}

/**
 * Subscribe to identity changes on every backend. The callback fires
 * immediately once per KNOWN backend, so a late subscriber (any module
 * imported after the first manifest resolved) is not left waiting for the
 * next reconnect. Returns an unsubscribe.
 *
 * The second argument names the backend whose identity this is. A
 * subscriber that keeps per-backend state keys on it; one that only ever
 * cared about the page's own backend compares it to `HOME_BACKEND`.
 */
export function onBackendIdentity(listener: IdentityListener): () => void {
  listeners.add(listener);
  for (const [backendId, identity] of identities) listener(identity, backendId);
  return () => {
    listeners.delete(listener);
  };
}

function publish(backendId: BackendKey, next: BackendIdentity): void {
  identities.set(backendId, next);
  for (const listener of listeners) listener(next, backendId);
}

/**
 * Called only from a bootstrap manifest path, once per resolved manifest —
 * ./bootstrap.ts for the page's own backend, ./backends.ts for each
 * attached one. Non-string wire values collapse to empty (replica
 * disabled) rather than being coerced: an identity we cannot trust must
 * not key a database.
 */
export function setBackendIdentityFromBootstrap(
  backendId: unknown,
  generation: unknown,
  name: unknown = undefined,
  backend: BackendKey = HOME_BACKEND,
): void {
  const current = getBackendIdentity(backend);
  const next: BackendIdentity = {
    backendId: typeof backendId === 'string' ? backendId : '',
    generation: typeof generation === 'string' ? generation : '',
    name: typeof name === 'string' ? name : '',
  };
  if (
    next.backendId === current.backendId &&
    next.generation === current.generation &&
    next.name === current.name
  ) {
    return;
  }
  publish(backend, next);
}

/**
 * Observe a generation carried by a `SyncThreadWindow` response. The
 * manifest is only refetched on reconnect, so a backend whose database
 * was restored mid-session (harness bundle restore) would otherwise keep
 * serving clients whose invalidation circuit never closes — every sync
 * response therefore carries the live generation, and this is the consumer
 * the manifest path cannot replace.
 *
 * Returns true when the observation CHANGED the identity (subscribers —
 * replica wipe, stamp registry, L1 cache — have then already run,
 * synchronously). Empty and unknown-backend observations are ignored: a
 * response generation is only meaningful against a backend we have already
 * identified.
 */
export function observeBackendGeneration(
  generation: unknown,
  backend: BackendKey = HOME_BACKEND,
): boolean {
  if (typeof generation !== 'string' || generation === '') return false;
  const current = getBackendIdentity(backend);
  if (current.backendId === '') return false;
  if (generation === current.generation) return false;
  publish(backend, { backendId: current.backendId, generation, name: current.name });
  return true;
}

/** Forget an attached backend's identity. Called when it detaches; the
 *  home slot is never dropped, only reset. */
export function forgetBackendIdentity(backend: BackendKey): void {
  if (backend === HOME_BACKEND) return;
  if (!identities.has(backend)) return;
  identities.delete(backend);
  for (const listener of listeners) listener(UNKNOWN_BACKEND_IDENTITY, backend);
}

/**
 * Test-only: forget every identity. Subscribers are deliberately KEPT and
 * notified — they are module-init wiring (the replica session, the stamp
 * registry), not per-test state, and dropping them would silently
 * unsubscribe them for every later test in the run.
 */
export function __resetBackendIdentityForTest(): void {
  for (const backend of [...identities.keys()]) forgetBackendIdentity(backend);
  if (getBackendIdentity() === UNKNOWN_BACKEND_IDENTITY) return;
  publish(HOME_BACKEND, UNKNOWN_BACKEND_IDENTITY);
}
