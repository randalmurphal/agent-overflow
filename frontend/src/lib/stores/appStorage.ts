// View layout belongs to this frontend. The old server bucket is read once
// for migration; subsequent boots, edits and host removals are local only.
// Callers own value schemas and reactivity. No credentials or history here.
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { withBackendTarget } from '../transport/backends';
import { onPurgeClientState } from '../transport/clientPurge';
import { readBeforeDeadline } from '../utils/readBeforeDeadline';
import { GetUIState } from './bindings';
import { addToast } from './toast.svelte';

const BUCKET_KEY = 'agent-overflow:uistate:bucket';
const MIGRATED_KEY = 'agent-overflow:uistate:frontend';
const stores = new Map<BackendKey, ViewStore>();
interface ViewStore {
  backend: BackendKey;
  bucket: Map<string, string>;
  hydrated: boolean;
  touched: Set<string>;
  loading: Promise<boolean> | null;
  generation: number;
}
let saveFailureShown = false;
const keyFor = (key: string, backend: BackendKey) => backend ? `${key}:${backend}` : key;
function readLocal(key: string): string | null {
  try { return localStorage.getItem(key); } catch { return null; }
}
function readBucket(backend: BackendKey): Map<string, string> {
  try {
    const parsed: unknown = JSON.parse(readLocal(keyFor(BUCKET_KEY, backend)) ?? '{}');
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return new Map();
    return new Map(Object.entries(parsed).filter((entry): entry is [string, string] => typeof entry[1] === 'string'));
  } catch { return new Map(); }
}
function storeFor(backend: BackendKey): ViewStore {
  let store = stores.get(backend);
  if (!store) {
    store = { backend, bucket: readBucket(backend), hydrated: readLocal(keyFor(MIGRATED_KEY, backend)) === '1',
      touched: new Set(), loading: null, generation: 0 };
    stores.set(backend, store);
  }
  return store;
}
function persist(store: ViewStore): void {
  try {
    localStorage.setItem(keyFor(BUCKET_KEY, store.backend), JSON.stringify(Object.fromEntries(store.bucket)));
    localStorage.setItem(keyFor(MIGRATED_KEY, store.backend), '1');
    saveFailureShown = false;
  } catch {
    if (!saveFailureShown) {
      saveFailureShown = true;
      addToast('error', 'This device could not save its layout. Changes may be lost when the app closes.');
    }
  }
}
export function appStorageGet(key: string, backend: BackendKey = HOME_BACKEND): string | null {
  return storeFor(backend).bucket.get(key) ?? null;
}
export function appStorageSet(key: string, value: string, backend: BackendKey = HOME_BACKEND): void {
  const store = storeFor(backend);
  store.bucket.set(key, value);
  if (!store.hydrated) store.touched.add(key);
  persist(store);
}
export function appStorageDelete(key: string, backend: BackendKey = HOME_BACKEND): void {
  const store = storeFor(backend);
  store.bucket.delete(key);
  if (!store.hydrated) store.touched.add(key);
  persist(store);
}
export function appStorageAdoptLegacyKey(key: string, legacyKey: string, parse: (raw: string) => string | null): string | null {
  const existing = appStorageGet(key);
  if (existing !== null) return existing;
  const raw = readLocal(legacyKey);
  if (raw === null) return null;
  try { localStorage.removeItem(legacyKey); } catch { /* adoption remains idempotent */ }
  const value = parse(raw);
  if (value !== null) appStorageSet(key, value);
  return value;
}
export function hydrateAppStorage(backend: BackendKey = HOME_BACKEND): Promise<boolean> {
  const store = storeFor(backend);
  if (store.hydrated) return Promise.resolve(true);
  if (store.loading) return store.loading;
  const generation = store.generation;
  store.loading = (async () => {
    try {
      const server = await readBeforeDeadline(withBackendTarget(backend, () => GetUIState()), 2500);
      if (stores.get(backend) !== store || store.generation !== generation) return false;
      for (const [key, value] of Object.entries(server ?? {})) {
        // Existing local state and a deletion made during migration win.
        if (typeof value === 'string' && !store.bucket.has(key) && !store.touched.has(key)) store.bucket.set(key, value);
      }
      store.hydrated = true;
      store.touched.clear();
      persist(store);
      return true;
    } catch { return false; } finally { store.loading = null; }
  })();
  return store.loading;
}
export function isAppStorageHydrated(backend: BackendKey = HOME_BACKEND): boolean { return storeFor(backend).hydrated; }
/** Writes are synchronous; retained as the caller's explicit durability boundary. */
export async function flushAppStorage(_backend: BackendKey = HOME_BACKEND): Promise<void> {}

// Invalidate a legacy migration from the departed host, retaining this
// frontend's layout. Entity detach closes that host's panes separately.
onPurgeClientState((scope) => {
  for (const store of stores.values()) if (scope === null || scope === store.backend) ++store.generation;
});
export function reinitAppStorageForTest(): void { stores.clear(); saveFailureShown = false; }
export function resetAppStorageForTest(): void {
  for (const backend of new Set([HOME_BACKEND, ...stores.keys()])) {
    try {
      localStorage.removeItem(keyFor(BUCKET_KEY, backend));
      localStorage.removeItem(keyFor(MIGRATED_KEY, backend));
    } catch { /* absent storage */ }
  }
  reinitAppStorageForTest();
}
