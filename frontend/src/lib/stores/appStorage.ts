// App-scoped UI-state storage: the durable replacement for raw
// localStorage. Values live in the backend's ui_state table, because
// webview localStorage silently resets every launch — the transport
// binds an ephemeral port, so the page origin (and its per-origin
// storage) changes each run.
//
// Layering:
//   - in-memory Map — source of truth after hydration; consumers'
//     $state syncs from it via their sync*FromAppStorage functions.
//   - localStorage blob — same-session cache so the pre-hydration
//     render (module-init reads) doesn't flash defaults.
//   - ui_state RPCs — the durable copy. Writes batch through a
//     debounce so a drag interaction is one RPC, not fifty.
//
// WHICH bucket is not this module's decision and is deliberately not
// something it can name. The backend derives the scope from the
// connection — the session the WebSocket upgrade presented, else the
// device id that upgrade declared (docs/specs/remote-access.md §6). A
// client id travelling as an RPC parameter would be a bearer string any
// client could spell, which is the identity hole that change closed.
//
// Consumers own their reactivity and parsing: this module stores
// opaque strings. Structured values are JSON-encoded by the caller.
//
// **One bucket per attached backend** (phase 7, spec §10). The bucket is
// the BACKEND's — it lives in that backend's ui_state table and is scoped
// there by the connection presenting it — so a client attached to two
// machines holds two, and each flushes to its own. `withBackendTarget` is
// how a flush names its backend: the ui_state methods are `home`-routed
// (they are this machine's settings for every other caller), and this is
// the one class of call whose backend is a property of the CONNECTION
// rather than of an entity or of the method.
//
// Every exported function defaults to the page's own backend, so every
// existing call site — which is all of them — keeps its shape and its
// behaviour. Cross-backend UI state (reading another machine's bucket to
// render its pane layout) is deliberately NOT built here: nothing asks for
// it yet, and a bucket that could answer for a backend it was not written
// against is a merge policy nobody has designed.

import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { onPurgeClientState, type PurgeScope } from '../transport/clientPurge';
import { DeleteUIState, GetUIState, SetUIState } from './bindings';
import { addToast } from './toast.svelte';

const BUCKET_CACHE_KEY = 'agent-overflow:uistate:bucket';
const WRITE_DEBOUNCE_MS = 300;

// The home bucket keeps TODAY'S cache key, unsuffixed: an already-running
// browser has a blob under that exact string, and a key that gained a
// suffix would flash defaults on the first paint after an update.
function bucketCacheKey(backend: BackendKey): string {
  return backend === HOME_BACKEND ? BUCKET_CACHE_KEY : `${BUCKET_CACHE_KEY}:${backend}`;
}

function readLocal(key: string): string | null {
  if (typeof localStorage === 'undefined') return null;
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeLocal(key: string, value: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(key, value);
  } catch {
    // Best-effort cache; the RPC layer is the durable copy.
  }
}

function readCachedBucket(backend: BackendKey): Map<string, string> {
  const raw = readLocal(bucketCacheKey(backend));
  if (!raw) return new Map();
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return new Map();
    const out = new Map<string, string>();
    for (const [key, value] of Object.entries(parsed)) {
      if (typeof value === 'string') out.set(key, value);
    }
    return out;
  } catch {
    return new Map();
  }
}

function writeCachedBucket(store: BackendStore): void {
  writeLocal(bucketCacheKey(store.backend), JSON.stringify(Object.fromEntries(store.bucket)));
}

// One backend's bucket and its write queue.
//
// `pendingSets` / `pendingDeletes` are the keys written locally but not
// yet confirmed by a SetUIState / DeleteUIState round-trip; local values
// win over the server for these during hydration, because the user
// interacted before hydration finished.
interface BackendStore {
  readonly backend: BackendKey;
  bucket: Map<string, string>;
  hydrated: boolean;
  readonly pendingSets: Map<string, string>;
  readonly pendingDeletes: Set<string>;
  flushTimer: ReturnType<typeof setTimeout> | null;
  flushInFlight: Promise<void> | null;
  saveFailureToastShown: boolean;
}

const stores = new Map<BackendKey, BackendStore>();

function storeFor(backend: BackendKey): BackendStore {
  let held = stores.get(backend);
  if (held === undefined) {
    held = {
      backend,
      // Pre-hydration reads serve the same-session cache; hydration
      // reconciles it against the server bucket.
      bucket: readCachedBucket(backend),
      hydrated: false,
      pendingSets: new Map(),
      pendingDeletes: new Set(),
      flushTimer: null,
      flushInFlight: null,
      saveFailureToastShown: false,
    };
    stores.set(backend, held);
  }
  return held;
}

/** Sync read. Null means "no persisted value" — callers apply their own default. */
export function appStorageGet(key: string, backend: BackendKey = HOME_BACKEND): string | null {
  return storeFor(backend).bucket.get(key) ?? null;
}

export function appStorageSet(
  key: string,
  value: string,
  backend: BackendKey = HOME_BACKEND,
): void {
  const store = storeFor(backend);
  store.bucket.set(key, value);
  store.pendingDeletes.delete(key);
  store.pendingSets.set(key, value);
  writeCachedBucket(store);
  scheduleFlush(store);
}

export function appStorageDelete(key: string, backend: BackendKey = HOME_BACKEND): void {
  const store = storeFor(backend);
  store.bucket.delete(key);
  store.pendingSets.delete(key);
  store.pendingDeletes.add(key);
  writeCachedBucket(store);
  scheduleFlush(store);
}

/**
 * Adopt a value from a pre-appStorage localStorage key: if the bucket
 * has no persisted value for `key` but the legacy key holds one, move
 * it in (write-through included) and delete the legacy key. Returns
 * the value now effective for `key`, or null. Callers pass a `parse`
 * that returns null to reject corrupt legacy content.
 */
export function appStorageAdoptLegacyKey(
  key: string,
  legacyStorageKey: string,
  parse: (raw: string) => string | null,
): string | null {
  const existing = appStorageGet(key);
  // Legacy adoption is the HOME backend's alone: the keys it migrates were
  // written by a build with one connection, so there is exactly one bucket
  // they can have belonged to.
  if (existing !== null) return existing;
  const raw = readLocal(legacyStorageKey);
  if (raw === null) return null;
  removeLegacyKey(legacyStorageKey);
  const value = parse(raw);
  if (value === null) return null;
  appStorageSet(key, value);
  return value;
}

function removeLegacyKey(legacyStorageKey: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(legacyStorageKey);
  } catch {
    // Removal is cosmetic; a lingering legacy key is re-adopted as a
    // no-op next boot (bucket now holds the key).
  }
}

/**
 * Fetch this connection's server-side bucket and reconcile:
 *   - server value wins for every key without a pending local write
 *     (the durable copy is authoritative on boot);
 *   - keys that exist only locally are pushed up (first run after a
 *     key moved into appStorage, or writes made while offline);
 *   - pending writes stay pending — the debounced flush delivers them.
 * Returns false when the fetch failed; the session then runs on the
 * localStorage cache and queued writes retry on the next mutation.
 */
export async function hydrateAppStorage(backend: BackendKey = HOME_BACKEND): Promise<boolean> {
  const store = storeFor(backend);
  let server: { [key: string]: string | undefined };
  try {
    server = (await withBackendTarget(backend, () => GetUIState())) ?? {};
  } catch (err) {
    console.error('appStorage: hydrate failed:', err);
    return false;
  }
  const push = new Map<string, string>();
  for (const [key, value] of store.bucket) {
    if (
      server[key] === undefined &&
      !store.pendingSets.has(key) &&
      !store.pendingDeletes.has(key)
    ) {
      push.set(key, value);
    }
  }
  for (const [key, value] of Object.entries(server)) {
    if (typeof value !== 'string') continue;
    if (store.pendingSets.has(key) || store.pendingDeletes.has(key)) continue;
    store.bucket.set(key, value);
  }
  for (const [key, value] of push) {
    store.pendingSets.set(key, value);
  }
  store.hydrated = true;
  writeCachedBucket(store);
  if (store.pendingSets.size > 0 || store.pendingDeletes.size > 0) {
    scheduleFlush(store);
  }
  return true;
}

export function isAppStorageHydrated(backend: BackendKey = HOME_BACKEND): boolean {
  return storeFor(backend).hydrated;
}

function scheduleFlush(store: BackendStore): void {
  if (store.flushTimer !== null) return;
  store.flushTimer = setTimeout(() => {
    store.flushTimer = null;
    void flushAppStorage(store.backend);
  }, WRITE_DEBOUNCE_MS);
}

/**
 * Send pending writes now. Safe to call at any time (pagehide,
 * tests); coalesces with an in-flight send instead of racing it.
 */
export async function flushAppStorage(backend: BackendKey = HOME_BACKEND): Promise<void> {
  const store = storeFor(backend);
  if (store.flushInFlight) {
    await store.flushInFlight;
  }
  if (store.pendingSets.size === 0 && store.pendingDeletes.size === 0) return;
  if (store.flushTimer !== null) {
    clearTimeout(store.flushTimer);
    store.flushTimer = null;
  }
  const sets = new Map(store.pendingSets);
  const deletes = new Set(store.pendingDeletes);
  store.pendingSets.clear();
  store.pendingDeletes.clear();

  const run = (async () => {
    try {
      if (sets.size > 0) {
        await withBackendTarget(backend, () => SetUIState(Object.fromEntries(sets)));
      }
      if (deletes.size > 0) {
        await withBackendTarget(backend, () => DeleteUIState([...deletes]));
      }
    } catch (err) {
      console.error('appStorage: flush failed:', err);
      if (!store.saveFailureToastShown) {
        store.saveFailureToastShown = true;
        addToast('error', 'Failed to save UI state');
      }
      // Re-queue what didn't land, but never clobber a newer write
      // that arrived while this flush was in flight.
      for (const [key, value] of sets) {
        if (!store.pendingSets.has(key) && !store.pendingDeletes.has(key)) {
          store.pendingSets.set(key, value);
        }
      }
      for (const key of deletes) {
        if (!store.pendingSets.has(key)) {
          store.pendingDeletes.add(key);
        }
      }
    } finally {
      store.flushInFlight = null;
    }
  })();
  store.flushInFlight = run;
  await run;
}

/**
 * Drop one backend's bucket, in memory and in the same-session cache.
 *
 * The localStorage blob is the half that outlives the tab, so a sign-out
 * that cleared only the Map would leave the pane layout, the pinned rows
 * and every other persisted preference of a backend this device no longer
 * has a credential for readable by whoever opens the page next.
 *
 * Pending writes are dropped rather than flushed: they are addressed to a
 * backend this client is in the middle of letting go of, and a flush would
 * either fail or write state back into a bucket that was just emptied.
 */
function dropBucket(store: BackendStore): void {
  if (store.flushTimer !== null) {
    clearTimeout(store.flushTimer);
    store.flushTimer = null;
  }
  store.pendingSets.clear();
  store.pendingDeletes.clear();
  store.bucket = new Map();
  store.hydrated = false;
  store.saveFailureToastShown = false;
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(bucketCacheKey(store.backend));
  } catch {
    // Best-effort, exactly as the write side is.
  }
}

// The bucket's half of a sign-out, a detach and a refused credential
// (`transport/clientPurge.ts` owns the three moments). A null scope is
// every backend; anything else is one, and a scope this client holds no
// bucket for is a no-op rather than an error.
onPurgeClientState((scope: PurgeScope) => {
  if (scope === null) {
    for (const store of stores.values()) dropBucket(store);
    stores.clear();
    return;
  }
  const held = stores.get(scope);
  if (held === undefined) return;
  dropBucket(held);
  stores.delete(scope);
});

/**
 * Test helper — re-run module-init resolution against the CURRENT
 * localStorage contents (a simulated same-origin reload: state resets,
 * caches survive).
 */
export function reinitAppStorageForTest(): void {
  for (const store of stores.values()) {
    if (store.flushTimer !== null) {
      clearTimeout(store.flushTimer);
      store.flushTimer = null;
    }
    store.flushInFlight = null;
    store.pendingSets.clear();
    store.pendingDeletes.clear();
    store.hydrated = false;
    store.saveFailureToastShown = false;
    store.bucket = readCachedBucket(store.backend);
  }
  storeFor(HOME_BACKEND);
}

/** Test helper — reset the bucket and queues; wipe the cache. */
export function resetAppStorageForTest(): void {
  for (const store of stores.values()) {
    if (store.flushTimer !== null) {
      clearTimeout(store.flushTimer);
      store.flushTimer = null;
    }
    if (typeof localStorage !== 'undefined') {
      try {
        localStorage.removeItem(bucketCacheKey(store.backend));
      } catch {
        // ignore
      }
    }
  }
  stores.clear();
  storeFor(HOME_BACKEND);
}
