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

import { SvelteMap } from 'svelte/reactivity';

export type ComposerPickerId = 'model' | 'effort' | 'access' | 'mcp' | 'branch';

export interface ComposerPickerHandle {
  isOpen: () => boolean;
  /** Current selection, derived by the picker for compact menu summaries. */
  selectionLabel?: () => string;
  /**
   * `anchor` is where the menu hangs instead of the picker's own trigger.
   * The roll-up passes its button; a chord or slash command passes
   * nothing and the picker resolves the anchor itself
   * (`resolvePickerAnchor`).
   */
  open: (anchor?: HTMLElement) => void;
  close: () => void;
}

// The toolbar's minimal rung hides every picker but the model behind one
// roll-up button, and a menu anchored to a hidden trigger has no geometry
// to sit on. The roll-up registers its button per pane so a picker opened
// while its trigger is hidden can hang from the one control that is on
// screen, whichever path opened it (roll-up row, chord, slash command).
const fallbackAnchors = new Map<string, HTMLElement>();

export function registerComposerPickerFallbackAnchor(paneId: string, el: HTMLElement): () => void {
  fallbackAnchors.set(paneId, el);
  return () => {
    if (fallbackAnchors.get(paneId) === el) fallbackAnchors.delete(paneId);
  };
}

/**
 * The element a picker's menu hangs from: the explicit anchor when the
 * opener gave one, else the trigger when it is rendered, else the pane's
 * fallback (the roll-up button). A trigger with no client rects is not on
 * screen (`display: none` up its chain); `offsetParent` is unreliable for
 * fixed ancestors, `getClientRects` is not.
 */
export function resolvePickerAnchor(
  paneId: string,
  explicit: HTMLElement | undefined,
  trigger: HTMLElement | undefined,
): HTMLElement | undefined {
  if (explicit) return explicit;
  if (trigger && trigger.getClientRects().length > 0) return trigger;
  return fallbackAnchors.get(paneId) ?? trigger;
}

// SvelteMap tracks registration changes without proxying handles. Selection
// getters read the picker's existing derived state, so summaries stay reactive.
const entries = new SvelteMap<string, ComposerPickerHandle>();

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

export function composerPickerSelectionLabel(paneId: string, pickerId: ComposerPickerId): string {
  return entries.get(entryKey(paneId, pickerId))?.selectionLabel?.() ?? '';
}

export function toggleComposerPicker(paneId: string | null, pickerId: ComposerPickerId): boolean {
  if (!paneId) return false;
  const handle = entries.get(entryKey(paneId, pickerId));
  if (!handle) return false;
  if (handle.isOpen()) handle.close();
  else handle.open();
  return true;
}

/**
 * Open a picker without the toggle's close-if-open branch. The composer's
 * `/model` and `/effort` commands mean "show me the picker", so re-running one
 * while its menu is already up must not dismiss it. Returns false when the
 * pane has no such picker mounted (a placeholder with nothing to choose).
 */
export function openComposerPicker(
  paneId: string | null,
  pickerId: ComposerPickerId,
  anchor?: HTMLElement,
): boolean {
  if (!paneId) return false;
  const handle = entries.get(entryKey(paneId, pickerId));
  if (!handle) return false;
  handle.open(anchor);
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
  fallbackAnchors.clear();
}
