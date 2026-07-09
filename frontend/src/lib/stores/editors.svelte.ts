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
  type EditorInfo,
} from './bindings';

let editors = $state<EditorInfo[]>([]);
let preference = $state<string>('');
let loaded = $state(false);
let loading = $state(false);
let error = $state<string | null>(null);
let inflight: Promise<void> | null = null;

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

async function load(): Promise<void> {
  loading = true;
  error = null;
  try {
    const [list, current] = await Promise.all([
      ListAvailableEditors(),
      GetEditorSettings(),
    ]);
    editors = (list as EditorInfo[]) ?? [];
    preference = (current as { preference?: string } | null)?.preference ?? '';
    loaded = true;
  } catch (err) {
    // Degrade gracefully: an empty catalog makes the control fall back
    // to a generic icon and no dropdown; the click still hits the
    // backend, which surfaces the real "no editor available" error.
    editors = [];
    error = err instanceof Error ? err.message : String(err);
  } finally {
    loading = false;
    inflight = null;
  }
}

/** Load the catalog once. Concurrent callers share the same in-flight
 *  promise; already-loaded is a no-op. */
export function ensureEditorsLoaded(): Promise<void> {
  if (loaded) return Promise.resolve();
  if (inflight) return inflight;
  inflight = load();
  return inflight;
}

/** Force a re-fetch (e.g. after the user changes editors in Settings and
 *  we want fresh availability, not just the new preference). */
export function refreshEditors(): Promise<void> {
  loaded = false;
  inflight = load();
  return inflight;
}

/** Available editors only, in catalog order. */
export function getAvailableEditors(): EditorInfo[] {
  return editors.filter((e) => e.available);
}

/** The editor an empty-editorID open will launch, or null if none. */
export function getResolvedEditor(): EditorInfo | null {
  return resolveDefault(editors, preference);
}

/** Non-null once at least one load attempt has finished. */
export function editorsLoaded(): boolean {
  return loaded || error !== null;
}

/** Last load error, or null. */
export function getEditorsError(): string | null {
  return error;
}

/** Optimistically record a new saved default so the header icon tracks a
 *  Settings change without a round-trip. No RPC — the Settings pane owns
 *  persistence; this only mirrors the value it already saved. */
export function applyEditorPreference(id: string): void {
  preference = id;
}

/** Test-only: reset the singleton so each test file starts clean. */
export function resetEditorsForTest(): void {
  editors = [];
  preference = '';
  loaded = false;
  loading = false;
  error = null;
  inflight = null;
}
