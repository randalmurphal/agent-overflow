// Shared, lazily-loaded snapshot of the open-in-editor catalog + the
// user's saved default. Every chat pane's header mounts an
// OpenInEditorControl; without a shared cache each would fire its own
// ListAvailableEditors / GetEditorSettings pair on mount (a wire RPC
// each in --connect mode, a PATH + /mnt/c walk behind the 60s backend
// cache otherwise). This store dedupes them to one fetch and lets the
// Settings picker push preference changes back so the header icon
// updates without a refetch.
//
// Read the values through the getter functions inside a `$derived` so
// consumers stay reactive, matching the getTransportStatus() idiom.

import {
  ListAvailableEditors,
  GetEditorSettings,
  SetEditorSettings,
  EditorSettings,
  type EditorInfo,
} from './bindings';
import { errString } from '../utils/errors';
import {
  isTransportClassError,
  onTransportStatusChange,
} from './transportStatus.svelte';

export type EditorsLoadStatus = 'idle' | 'loading' | 'loaded' | 'error';

// The backend detection cache uses the same lifetime. Revalidating on a
// later consumer entry or successful open keeps the header from retaining a
// catalog longer than the backend itself considers authoritative.
export const EDITORS_MAX_AGE_MS = 60_000;

let editors = $state<EditorInfo[]>([]);
let preference = $state<string>('');
let status = $state<EditorsLoadStatus>('idle');
let error = $state<string | null>(null);
let hasSnapshot = $state(false);
let loadedAt = 0;
let generation = 0;
let preferenceRevision = 0;
let preferenceMutationGeneration = 0;
let pendingPreferenceMutations = 0;
let storeEpoch = 0;
let persistedPreference = '';
let preferenceMutationTail: Promise<void> = Promise.resolve();
let retryAfterReconnect = false;
let inflight: { generation: number; promise: Promise<void> } | null = null;

/**
 * Pick the editor an empty-editorID OpenInEditor call will launch,
 * mirroring internal/editor/preference.go Resolve:
 *   1. saved preference, if that editor is available;
 *   2. else the first available *named* editor — the list arrives in
 *      catalog priority order with `env:*` appended, so "first named"
 *      respects that order;
 *   3. else the first available `env:*` ($EDITOR / $VISUAL) fallback;
 *   4. else null (nothing installed).
 * Kept deliberately in step with the Go resolver; if that decision tree
 * changes, change this too.
 */
export function resolveDefault(
  list: EditorInfo[],
  pref: string,
): EditorInfo | null {
  const available = list.filter((e) => e.available);
  if (pref) {
    const preferred = available.find((e) => e.id === pref);
    if (preferred) return preferred;
  }
  const named = available.find((e) => !e.envFallback);
  if (named) return named;
  return available.find((e) => e.envFallback) ?? null;
}

function startLoad(): Promise<void> {
  const loadGeneration = ++generation;
  const preferenceRevisionAtStart = preferenceRevision;
  status = 'loading';
  error = null;

  const promise = (async () => {
    try {
      const [list, current] = await Promise.all([
        ListAvailableEditors(),
        GetEditorSettings(),
      ]);
      if (generation !== loadGeneration) return;
      editors = (list as EditorInfo[]) ?? [];
      // A settings save may finish while catalog discovery is in flight. Its
      // returned preference is newer than the GetEditorSettings response this
      // load started with, so only install the response if no local preference
      // write crossed the request.
      if (
        preferenceRevision === preferenceRevisionAtStart
        && pendingPreferenceMutations === 0
      ) {
        preference = (current as { preference?: string } | null)?.preference ?? '';
        persistedPreference = preference;
      }
      hasSnapshot = true;
      loadedAt = Date.now();
      retryAfterReconnect = false;
      status = 'loaded';
    } catch (err) {
      if (generation !== loadGeneration) return;
      // Keep a prior successful snapshot visible during a refresh failure.
      // A first-load failure therefore has no catalog, while a transient
      // outage cannot erase editor choices the user was already looking at.
      error = errString(err);
      retryAfterReconnect = isTransportClassError(err);
      status = 'error';
      console.error('Failed to load editor catalog:', err);
    } finally {
      if (inflight?.generation === loadGeneration) inflight = null;
    }
  })();

  inflight = { generation: loadGeneration, promise };
  return promise;
}

/** Load the catalog once. Concurrent callers share the same in-flight
 *  promise. A successful snapshot is revalidated after the backend's own
 *  catalog TTL; an error waits for an explicit retry or reconnect edge. */
export function ensureEditorsLoaded(): Promise<void> {
  if (inflight) return inflight.promise;
  if (status === 'error') return Promise.resolve();
  if (hasSnapshot && Date.now() - loadedAt < EDITORS_MAX_AGE_MS) {
    return Promise.resolve();
  }
  return startLoad();
}

/** Force a re-fetch. A newer request supersedes any in-flight request;
 * stale completions are generation-fenced and cannot overwrite it. */
export function refreshEditors(): Promise<void> {
  return startLoad();
}

/** Every catalog row, including unavailable editors, in backend order. */
export function getEditors(): readonly EditorInfo[] {
  return editors;
}

/** Available editors only, in catalog order. */
export function getAvailableEditors(): EditorInfo[] {
  return editors.filter((e) => e.available);
}

/** The editor an empty-editorID open will launch, or null if none. */
export function getResolvedEditor(): EditorInfo | null {
  return resolveDefault(editors, preference);
}

export function getEditorPreference(): string {
  return preference;
}

export function getEditorsLoadStatus(): EditorsLoadStatus {
  return status;
}

/** Whether at least one authoritative response has populated the store. */
export function hasEditorsSnapshot(): boolean {
  return hasSnapshot;
}

/** Last load error, or null. */
export function getEditorsError(): string | null {
  return error;
}

/** Persist a new default through the editor entity's single mutation path.
 *
 * Calls are serialized in invocation order so out-of-order RPC completions
 * cannot leave the backend on an older preference. The newest requested value
 * remains visible while earlier writes drain. If that newest write fails, the
 * UI rolls back to the last value the backend confirmed. */
export function setEditorPreference(id: string): Promise<void> {
  const mutationGeneration = ++preferenceMutationGeneration;
  const mutationEpoch = storeEpoch;
  pendingPreferenceMutations += 1;
  preferenceRevision += 1;
  preference = id;

  const mutation = preferenceMutationTail.then(async () => {
    try {
      const updated = (await SetEditorSettings(
        new EditorSettings({ preference: id }),
      )) as EditorSettings;
      if (storeEpoch !== mutationEpoch) return;

      persistedPreference = updated.preference;
      preferenceRevision += 1;
      if (preferenceMutationGeneration === mutationGeneration) {
        preference = updated.preference;
      }
    } catch (err) {
      if (storeEpoch === mutationEpoch) {
        preferenceRevision += 1;
        if (preferenceMutationGeneration === mutationGeneration) {
          preference = persistedPreference;
        }
      }
      throw err;
    } finally {
      if (storeEpoch === mutationEpoch) pendingPreferenceMutations -= 1;
    }
  });

  // Keep the queue usable after a failed write. The returned mutation still
  // rejects so the caller can surface the failure.
  preferenceMutationTail = mutation.then(
    () => undefined,
    () => undefined,
  );
  return mutation;
}

onTransportStatusChange((next) => {
  if (next.status !== 'connected' || !retryAfterReconnect || inflight) return;
  retryAfterReconnect = false;
  void startLoad();
});

/** Test-only: reset the singleton so each test file starts clean. */
export function resetEditorsForTest(): void {
  storeEpoch += 1;
  generation += 1;
  editors = [];
  preference = '';
  status = 'idle';
  error = null;
  hasSnapshot = false;
  loadedAt = 0;
  preferenceRevision = 0;
  preferenceMutationGeneration = 0;
  pendingPreferenceMutations = 0;
  persistedPreference = '';
  preferenceMutationTail = Promise.resolve();
  retryAfterReconnect = false;
  inflight = null;
}
