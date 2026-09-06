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
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';
import { withBackendTarget, attachedBackends, onBackendsChanged } from '../transport/backends';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { errString } from '../utils/errors';
import {
  isTransportClassError,
  onBackendHelloChange,
} from './transportStatus.svelte';

export type EditorsLoadStatus = 'idle' | 'loading' | 'loaded' | 'error';

// The backend detection cache uses the same lifetime. Revalidating on a
// later consumer entry or successful open keeps the header from retaining a
// catalog longer than the backend itself considers authoritative.
export const EDITORS_MAX_AGE_MS = 60_000;

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

function createEditors(backend: BackendKey) {
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

  function startLoad(): Promise<void> {
    const loadGeneration = ++generation;
    const preferenceRevisionAtStart = preferenceRevision;
    status = 'loading';
    error = null;

    const promise = (async () => {
      try {
        const [list, current] = await Promise.all([
          withBackendTarget(backend, () => ListAvailableEditors()),
          withBackendTarget(backend, () => GetEditorSettings()),
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
  function ensureEditorsLoaded(): Promise<void> {
    if (inflight) return inflight.promise;
    if (status === 'error') return Promise.resolve();
    if (hasSnapshot && Date.now() - loadedAt < EDITORS_MAX_AGE_MS) {
      return Promise.resolve();
    }
    return startLoad();
  }

  /** Force a re-fetch. A newer request supersedes any in-flight request;
   * stale completions are generation-fenced and cannot overwrite it. */
  function refreshEditors(): Promise<void> {
    return startLoad();
  }

  /** Every catalog row, including unavailable editors, in backend order. */
  function getEditors(): readonly EditorInfo[] {
    return editors;
  }

  /** Available editors only, in catalog order. */
  function getAvailableEditors(): EditorInfo[] {
    return editors.filter((e) => e.available);
  }

  /** The editor an empty-editorID open will launch, or null if none. */
  function getResolvedEditor(): EditorInfo | null {
    return resolveDefault(editors, preference);
  }

  function getEditorPreference(): string {
    return preference;
  }

  function getEditorsLoadStatus(): EditorsLoadStatus {
    return status;
  }

  /** Whether at least one authoritative response has populated the store. */
  function hasEditorsSnapshot(): boolean {
    return hasSnapshot;
  }

  /** Last load error, or null. */
  function getEditorsError(): string | null {
    return error;
  }

  /** Persist a new default through the editor entity's single mutation path.
   *
   * Calls are serialized in invocation order so out-of-order RPC completions
   * cannot leave the backend on an older preference. The newest requested value
   * remains visible while earlier writes drain. If that newest write fails, the
   * UI rolls back to the last value the backend confirmed. */
  function setEditorPreference(id: string): Promise<void> {
    const mutationGeneration = ++preferenceMutationGeneration;
    const mutationEpoch = storeEpoch;
    pendingPreferenceMutations += 1;
    preferenceRevision += 1;
    preference = id;

    const mutation = preferenceMutationTail.then(async () => {
      if (storeEpoch !== mutationEpoch) throw new Error('Computer was removed.');
      try {
        const updated = (await withBackendTarget(backend, () => SetEditorSettings(
          new EditorSettings({ preference: id }),
        ))) as EditorSettings;
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

  /**
   * Re-read the saved default after a settings write moved the `editor` key —
   * on this client or any other.
   *
   * The preference is a settings value with its own RPC, and this store held it
   * behind a 60-second catalog TTL with nothing invalidating it, so a change
   * made anywhere else left every open header icon pointing at the previous
   * editor for up to a minute (and forever, on a store whose TTL kept being
   * refreshed by an unrelated load).
   *
   * Only the PREFERENCE is re-read. The catalog is a PATH and /mnt/c walk and
   * nothing about a settings write changes what is installed, so refetching it
   * here would spend the expensive half of the load for the cheap half's
   * answer.
   *
   * Three skips, each of which is a real bug without it: no snapshot yet (the
   * first consumer's own load will read it), a local write still in flight (the
   * pending value is newer than anything the backend can answer with), and a
   * preference revision that moved across the await (same reason, one lap
   * later).
   */
  async function resyncEditorPreference(): Promise<void> {
    if (!hasSnapshot || pendingPreferenceMutations > 0) return;
    const epoch = storeEpoch;
    const revisionAtStart = preferenceRevision;
    try {
      const current = await withBackendTarget(backend, () => GetEditorSettings());
      if (storeEpoch !== epoch) return;
      if (preferenceRevision !== revisionAtStart || pendingPreferenceMutations > 0) return;
      preference = (current as { preference?: string } | null)?.preference ?? '';
      persistedPreference = preference;
    } catch (err) {
      // The catalog on screen is still valid; a failed re-read leaves the
      // previous preference rather than blanking a working picker.
      console.error('Failed to re-read the editor preference:', err);
    }
  }

  function reconnect(): void {
    if (!retryAfterReconnect || inflight) return;
    retryAfterReconnect = false;
    void startLoad();
  }

  /** Test-only: reset the singleton so each test file starts clean. */
  function dispose(): void {
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

  return { ensureEditorsLoaded, refreshEditors, getEditors, getAvailableEditors, getResolvedEditor, getEditorPreference, getEditorsLoadStatus, hasEditorsSnapshot, getEditorsError, setEditorPreference, resyncEditorPreference, dispose, reconnect };
}

type ComputerEditors = ReturnType<typeof createEditors>;
const computers = new Map<BackendKey, ComputerEditors>();
const slots = createKeyedSignalRegistry<ComputerEditors | null>(null);
const EMPTY = createEditors(HOME_BACKEND);
// Allocate reactive state at the transport's attach edge, never inside a
// reader's $derived (Svelte cannot subscribe that reader to its own new state).
function syncComputers(): void {
  const live = new Set(attachedBackends().map((entry) => entry.id));
  for (const backend of live) {
    if (computers.has(backend)) continue;
    const state = createEditors(backend);
    computers.set(backend, state);
    slots.set(backend, state);
  }
  for (const [backend, state] of computers) {
    if (live.has(backend)) continue;
    state.dispose();
    computers.delete(backend);
    slots.drop(backend);
  }
}
onBackendsChanged(syncComputers);
syncComputers();
function readComputer(backend: BackendKey): ComputerEditors { return slots.get(backend) ?? EMPTY; }
function editComputer(backend: BackendKey): ComputerEditors {
  const state = computers.get(backend);
  if (!state) throw new Error('This computer is no longer connected.');
  return state;
}
onBackendHelloChange((backend, hello) => {
  if (hello) computers.get(backend)?.reconnect();
});
export function ensureEditorsLoaded(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["ensureEditorsLoaded"]> { return editComputer(backend).ensureEditorsLoaded(); }
export function refreshEditors(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["refreshEditors"]> { return editComputer(backend).refreshEditors(); }
export function getEditors(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getEditors"]> { return readComputer(backend).getEditors(); }
export function getAvailableEditors(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getAvailableEditors"]> { return readComputer(backend).getAvailableEditors(); }
export function getResolvedEditor(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getResolvedEditor"]> { return readComputer(backend).getResolvedEditor(); }
export function getEditorPreference(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getEditorPreference"]> { return readComputer(backend).getEditorPreference(); }
export function getEditorsLoadStatus(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getEditorsLoadStatus"]> { return readComputer(backend).getEditorsLoadStatus(); }
export function hasEditorsSnapshot(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["hasEditorsSnapshot"]> { return readComputer(backend).hasEditorsSnapshot(); }
export function getEditorsError(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["getEditorsError"]> { return readComputer(backend).getEditorsError(); }
export function setEditorPreference(id: string, backend: BackendKey = HOME_BACKEND): Promise<void> { return editComputer(backend).setEditorPreference(id); }
export function resyncEditorPreference(backend: BackendKey = HOME_BACKEND): ReturnType<ComputerEditors["resyncEditorPreference"]> { return readComputer(backend).resyncEditorPreference(); }
export function resetEditorsForTest(): void {
  for (const state of computers.values()) state.dispose();
  computers.clear();
  slots.reset();
  syncComputers();
}
