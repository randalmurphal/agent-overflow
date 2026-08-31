// App-scoped UI-state storage: the durable replacement for raw
// localStorage. Values live in the backend's ui_state table under this
// client's bucket (Go settings-dir identity when the shell provides
// one), because webview localStorage silently resets every launch —
// the transport binds an ephemeral port, so the page origin (and its
// per-origin storage) changes each run.
//
// Layering:
//   - in-memory Map — source of truth after hydration; consumers'
//     $state syncs from it via their sync*FromAppStorage functions.
//   - localStorage blob — same-session cache so the pre-hydration
//     render (module-init reads) doesn't flash defaults, plus the
//     best-effort identity cache for plain-browser clients.
//   - ui_state RPCs — the durable copy. Writes batch through a
//     debounce so a drag interaction is one RPC, not fifty.
//
// Client identity resolution (module init, synchronous — must run
// before the bootstrap fetch scrubs the URL's ticket): the ?cid= URL
// param stamped by the native shell, the WSL launcher and the --connect
// stub, else the localStorage-cached id, else a freshly minted UUID. The
// first is durable (persisted in the Go config dir); the latter two are
// best-effort and reset with the origin, degrading to fresh defaults —
// exactly today's behavior.
//
// Consumers own their reactivity and parsing: this module stores
// opaque strings. Structured values are JSON-encoded by the caller.

import { DeleteUIState, GetUIState, SetUIState } from './bindings';
import { addToast } from './toast.svelte';
// The device id is transport identity, not a UI-state detail: the WebSocket
// client puts it on the upgrade URL so bound methods can attribute a write.
// Both read it from the same leaf so the bucket this store scopes and the
// identity the backend sees can never be two different strings.
import {
  clearCachedDeviceIdForTest,
  getDeviceId,
  reresolveDeviceIdForTest,
} from '../transport/clientIdentity';

const BUCKET_CACHE_KEY = 'agent-overflow:uistate:bucket';
const WRITE_DEBOUNCE_MS = 300;

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

function readCachedBucket(): Map<string, string> {
  const raw = readLocal(BUCKET_CACHE_KEY);
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

function writeCachedBucket(): void {
  writeLocal(BUCKET_CACHE_KEY, JSON.stringify(Object.fromEntries(bucket)));
}

let clientId = getDeviceId();
// Pre-hydration reads serve the same-session cache; hydration
// reconciles it against the server bucket.
let bucket: Map<string, string> = readCachedBucket();
let hydrated = false;

// Keys written locally but not yet confirmed by a SetUIState /
// DeleteUIState round-trip. Local values win over the server for these
// during hydration — the user interacted before hydration finished.
const pendingSets = new Map<string, string>();
const pendingDeletes = new Set<string>();
let flushTimer: ReturnType<typeof setTimeout> | null = null;
let flushInFlight: Promise<void> | null = null;
let saveFailureToastShown = false;

export function getAppStorageClientId(): string {
  return clientId;
}

/** Sync read. Null means "no persisted value" — callers apply their own default. */
export function appStorageGet(key: string): string | null {
  return bucket.get(key) ?? null;
}

export function appStorageSet(key: string, value: string): void {
  bucket.set(key, value);
  pendingDeletes.delete(key);
  pendingSets.set(key, value);
  writeCachedBucket();
  scheduleFlush();
}

export function appStorageDelete(key: string): void {
  bucket.delete(key);
  pendingSets.delete(key);
  pendingDeletes.add(key);
  writeCachedBucket();
  scheduleFlush();
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
 * Fetch this client's server-side bucket and reconcile:
 *   - server value wins for every key without a pending local write
 *     (the durable copy is authoritative on boot);
 *   - keys that exist only locally are pushed up (first run after a
 *     key moved into appStorage, or writes made while offline);
 *   - pending writes stay pending — the debounced flush delivers them.
 * Returns false when the fetch failed; the session then runs on the
 * localStorage cache and queued writes retry on the next mutation.
 */
export async function hydrateAppStorage(): Promise<boolean> {
  let server: { [key: string]: string | undefined };
  try {
    server = (await GetUIState(clientId)) ?? {};
  } catch (err) {
    console.error('appStorage: hydrate failed:', err);
    return false;
  }
  const push = new Map<string, string>();
  for (const [key, value] of bucket) {
    if (server[key] === undefined && !pendingSets.has(key) && !pendingDeletes.has(key)) {
      push.set(key, value);
    }
  }
  for (const [key, value] of Object.entries(server)) {
    if (typeof value !== 'string') continue;
    if (pendingSets.has(key) || pendingDeletes.has(key)) continue;
    bucket.set(key, value);
  }
  for (const [key, value] of push) {
    pendingSets.set(key, value);
  }
  hydrated = true;
  writeCachedBucket();
  if (pendingSets.size > 0 || pendingDeletes.size > 0) {
    scheduleFlush();
  }
  return true;
}

export function isAppStorageHydrated(): boolean {
  return hydrated;
}

function scheduleFlush(): void {
  if (flushTimer !== null) return;
  flushTimer = setTimeout(() => {
    flushTimer = null;
    void flushAppStorage();
  }, WRITE_DEBOUNCE_MS);
}

/**
 * Send pending writes now. Safe to call at any time (pagehide,
 * tests); coalesces with an in-flight send instead of racing it.
 */
export async function flushAppStorage(): Promise<void> {
  if (flushInFlight) {
    await flushInFlight;
  }
  if (pendingSets.size === 0 && pendingDeletes.size === 0) return;
  if (flushTimer !== null) {
    clearTimeout(flushTimer);
    flushTimer = null;
  }
  const sets = new Map(pendingSets);
  const deletes = new Set(pendingDeletes);
  pendingSets.clear();
  pendingDeletes.clear();

  flushInFlight = (async () => {
    try {
      if (sets.size > 0) {
        await SetUIState(clientId, Object.fromEntries(sets));
      }
      if (deletes.size > 0) {
        await DeleteUIState(clientId, [...deletes]);
      }
    } catch (err) {
      console.error('appStorage: flush failed:', err);
      if (!saveFailureToastShown) {
        saveFailureToastShown = true;
        addToast('error', 'Failed to save UI state');
      }
      // Re-queue what didn't land, but never clobber a newer write
      // that arrived while this flush was in flight.
      for (const [key, value] of sets) {
        if (!pendingSets.has(key) && !pendingDeletes.has(key)) {
          pendingSets.set(key, value);
        }
      }
      for (const key of deletes) {
        if (!pendingSets.has(key)) {
          pendingDeletes.add(key);
        }
      }
    } finally {
      flushInFlight = null;
    }
  })();
  await flushInFlight;
}

/**
 * Test helper — re-run module-init resolution against the CURRENT
 * localStorage contents (a simulated same-origin reload: state resets,
 * caches survive).
 */
export function reinitAppStorageForTest(): void {
  if (flushTimer !== null) {
    clearTimeout(flushTimer);
    flushTimer = null;
  }
  flushInFlight = null;
  pendingSets.clear();
  pendingDeletes.clear();
  hydrated = false;
  saveFailureToastShown = false;
  clientId = reresolveDeviceIdForTest();
  bucket = readCachedBucket();
}

/** Test helper — reset identity, bucket, and queues; wipe the caches. */
export function resetAppStorageForTest(): void {
  if (flushTimer !== null) {
    clearTimeout(flushTimer);
    flushTimer = null;
  }
  flushInFlight = null;
  pendingSets.clear();
  pendingDeletes.clear();
  hydrated = false;
  saveFailureToastShown = false;
  if (typeof localStorage !== 'undefined') {
    try {
      localStorage.removeItem(BUCKET_CACHE_KEY);
    } catch {
      // ignore
    }
  }
  clearCachedDeviceIdForTest();
  clientId = getDeviceId();
  bucket = new Map();
}
