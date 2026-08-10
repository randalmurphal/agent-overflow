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

export interface BackendIdentity {
  backendId: string;
  generation: string;
}

export const UNKNOWN_BACKEND_IDENTITY: BackendIdentity = { backendId: '', generation: '' };

let current: BackendIdentity = UNKNOWN_BACKEND_IDENTITY;
const listeners = new Set<(identity: BackendIdentity) => void>();

export function getBackendIdentity(): BackendIdentity {
  return current;
}

/**
 * Subscribe to identity changes. The callback fires immediately with
 * the current value so a late subscriber (any module imported after the
 * first manifest resolved) is not left waiting for the next reconnect.
 * Returns an unsubscribe.
 */
export function onBackendIdentity(listener: (identity: BackendIdentity) => void): () => void {
  listeners.add(listener);
  listener(current);
  return () => {
    listeners.delete(listener);
  };
}

/**
 * Called only from the bootstrap manifest path, once per resolved
 * manifest. Non-string wire values collapse to empty (replica
 * disabled) rather than being coerced — an identity we cannot trust
 * must not key a database.
 */
export function setBackendIdentityFromBootstrap(
  backendId: unknown,
  generation: unknown,
): void {
  const next: BackendIdentity = {
    backendId: typeof backendId === 'string' ? backendId : '',
    generation: typeof generation === 'string' ? generation : '',
  };
  if (next.backendId === current.backendId && next.generation === current.generation) return;
  current = next;
  for (const listener of listeners) listener(next);
}

/**
 * Observe a generation carried by a `SyncThreadWindow` response. The
 * manifest is only refetched on reconnect, so a backend whose database
 * was restored mid-session (harness bundle restore) would otherwise
 * keep serving clients whose invalidation circuit never closes — every
 * sync response therefore carries the live generation, and this is the
 * consumer the manifest path cannot replace.
 *
 * Returns true when the observation CHANGED the identity (subscribers
 * — replica wipe, stamp registry, L1 cache — have then already run,
 * synchronously). Empty and unknown-backend observations are ignored:
 * a response generation is only meaningful against a backend we have
 * already identified.
 */
export function observeBackendGeneration(generation: unknown): boolean {
  if (typeof generation !== 'string' || generation === '') return false;
  if (current.backendId === '') return false;
  if (generation === current.generation) return false;
  current = { backendId: current.backendId, generation };
  for (const listener of listeners) listener(current);
  return true;
}

/**
 * Test-only: forget the identity. Subscribers are deliberately KEPT and
 * notified — they are module-init wiring (the replica session, the stamp
 * registry), not per-test state, and dropping them would silently
 * unsubscribe them for every later test in the run.
 */
export function __resetBackendIdentityForTest(): void {
  if (current === UNKNOWN_BACKEND_IDENTITY) return;
  current = UNKNOWN_BACKEND_IDENTITY;
  for (const listener of listeners) listener(current);
}
