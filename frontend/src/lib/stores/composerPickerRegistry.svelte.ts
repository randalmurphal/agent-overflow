// Tracks which composer-toolbar picker (if any) is currently open in
// which pane, and exposes imperative open/close/toggle helpers that
// the global keybinding chords route through.
//
// Each picker component publishes a small handle when it mounts via
// `registerComposerPicker`. The global chord (`composer.picker.model`,
// etc.) looks up the handle for the focused pane and calls toggle()
// on it. Re-pressing the chord while open closes the picker.
//
// We key by paneId so multi-pane mode works: each pane has its own
// composer with its own picker instances, and the same chord routes
// to whichever pane is currently focused.

export type ComposerPickerId = 'model' | 'effort' | 'access' | 'branch';

export interface ComposerPickerHandle {
  isOpen: () => boolean;
  open: () => void;
  close: () => void;
}

// Plain Map (no $state) — the chord handlers call these imperatively
// and don't subscribe to changes. Wrapping in $state would proxy the
// handles and trigger state_proxy_equality_mismatch when component
// code compares a stored handle with its original.
const entries = new Map<string, ComposerPickerHandle>();

function entryKey(paneId: string, pickerId: ComposerPickerId): string {
  return `${paneId}:${pickerId}`;
}

export function registerComposerPicker(
  paneId: string,
  pickerId: ComposerPickerId,
  handle: ComposerPickerHandle,
): () => void {
  const key = entryKey(paneId, pickerId);
  entries.set(key, handle);
  return () => {
    if (entries.get(key) === handle) entries.delete(key);
  };
}

export function toggleComposerPicker(paneId: string | null, pickerId: ComposerPickerId): boolean {
  if (!paneId) return false;
  const handle = entries.get(entryKey(paneId, pickerId));
  if (!handle) return false;
  if (handle.isOpen()) handle.close();
  else handle.open();
  return true;
}

export function isAnyComposerPickerOpen(): boolean {
  for (const handle of entries.values()) {
    if (handle.isOpen()) return true;
  }
  return false;
}

export function resetComposerPickerRegistryForTest(): void {
  entries.clear();
}
