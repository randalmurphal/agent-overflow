// Settings overlay open-state (UI-SPEC §2.1). Settings mounts as a SIBLING of
// PaneHost, layered over it through the same frame primitive the workflows
// overlay uses — so its open state belongs in a store next to every other
// overlay's, not in App.svelte. Distant surfaces (the context-window meter, the
// `/config` composer command, the account switcher) deep-link straight in by
// calling `openSettingsOverlay`; none of them needs a window event to reach
// App's local state any more.
//
// Nothing here is persisted. `section` survives a close within a session (the
// same tab comes back), and a restart starts on the pane tree with settings
// closed.

import {
  closeWorkflowsOverlay,
  setWorkflowsOverlayExclusion,
} from './workflowsOverlay.svelte';
import type { SettingsSection } from '../components/settings/sections';

let open = $state(false);
let section = $state<SettingsSection>('general');

export function isSettingsOpen(): boolean {
  return open;
}

export function getSettingsSection(): SettingsSection {
  return section;
}

/**
 * The one settings-open path. Omitting `nextSection` keeps whichever tab was
 * last shown.
 */
export function openSettingsOverlay(nextSection: SettingsSection = section): void {
  // Settings and the workflows overlay are both full-height layers over the
  // pane strip, each with its own focus trap; stacking them has no coherent
  // Esc. The reverse direction runs off `openWorkflowsOverlay`, the one writer
  // of that store's `open` (armed at the bottom of this module).
  closeWorkflowsOverlay();
  section = nextSection;
  open = true;
}

/**
 * The one settings-close path — X button, scrim, `settings.close`, and the
 * workflows overlay opening all route here.
 *
 * Settings fields commit on blur, so focus has to leave the surface BEFORE it
 * unmounts: a click-close moves focus by itself, but an Esc-close would
 * otherwise unmount a focused input and silently drop the edit in it. The
 * already-closed guard comes FIRST precisely because of that blur — this runs
 * on every workflows-overlay open, and blurring there would steal focus from
 * whatever the user was typing in.
 */
export function closeSettingsOverlay(): void {
  if (!open) return;
  const active = document.activeElement;
  if (active instanceof HTMLElement) active.blur();
  open = false;
}

// Arm the mutual exclusion at module init. Importing this module is what wires
// it, so no registration order — and no test's reset — can leave the workflows
// overlay able to open on top of settings. The import direction is one-way
// (this store → workflowsOverlay) so the pair never forms a cycle.
setWorkflowsOverlayExclusion(closeSettingsOverlay);

export function resetSettingsOverlayForTest(): void {
  open = false;
  section = 'general';
}
