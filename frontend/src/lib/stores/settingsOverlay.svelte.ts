// Settings overlay open-state (UI-SPEC §2.1). Settings mounts as a SIBLING of
// PaneHost, layered over it through `primitives/OverlayShell.svelte` — so its
// open state belongs in a store next to every other overlay's, not in
// App.svelte. Distant surfaces (the context-window meter, the
// `/config` composer command, the account switcher) deep-link straight in by
// calling `openSettingsOverlay`; none of them needs a window event to reach
// App's local state any more.
//
// Nothing here is persisted. `section` survives a close within a session (the
// same tab comes back), and a restart starts on the pane tree with settings
// closed.

import { setToolPaneOpenExclusion } from './toolPanes.svelte';
import { isCompactLayout } from './layoutMode.svelte';
import type { BackendKey } from '../transport/backendKey';
import {
  DEFAULT_SETTINGS_SECTION,
  settingsSectionAvailable,
  type SettingsSection,
} from '../components/settings/sections';

let open = $state(false);
let section = $state<SettingsSection>(DEFAULT_SETTINGS_SECTION);
let computer = $state<BackendKey | null>(null);

export function getSettingsComputer(): BackendKey | null { return computer; }
export function setSettingsComputer(backend: BackendKey): void { computer = backend; }
// Compact renders Settings as stacked screens (docs/specs/remote-access.md
// § The phone client): the rail is one screen, a section's page is the
// next, and "back" from the page is the rail. Which of the two is showing
// lives here rather than in the view so the `settings.close` command — the
// path Esc and the phone's hardware back both take — can answer rail-first.
// Desktop ignores it: both columns stay visible.
let railOpen = $state(true);

export function isSettingsOpen(): boolean {
  return open;
}

export function getSettingsSection(): SettingsSection {
  return section;
}

export function isSettingsRailOpen(): boolean {
  return railOpen;
}

export function showSettingsRail(): void {
  railOpen = true;
}

export function hideSettingsRail(): void {
  railOpen = false;
}

/**
 * The one settings-open path. Omitting `nextSection` keeps whichever tab was
 * last shown. A page this build does not offer opens Settings on the default
 * page instead.
 */
export function openSettingsOverlay(nextSection: SettingsSection = section, backend?: BackendKey): void {
  const target = settingsSectionAvailable(nextSection) ? nextSection : DEFAULT_SETTINGS_SECTION;
  section = target;
  computer = backend ?? null;
  // A deep link to a specific section lands on that page directly.
  railOpen = target === DEFAULT_SETTINGS_SECTION;
  open = true;
}

/**
 * The one settings-close path — X button, scrim, `settings.close`, and a tool
 * pane opening all route here.
 *
 * Settings fields commit on blur, so focus has to leave the surface BEFORE it
 * unmounts: a click-close moves focus by itself, but an Esc-close would
 * otherwise unmount a focused input and silently drop the edit in it. The
 * already-closed guard comes FIRST precisely because of that blur — this runs
 * on every tool open, and blurring there would steal focus from whatever the
 * user was typing in.
 */
export function closeSettingsOverlay(): void {
  if (!open) return;
  const active = document.activeElement;
  if (active instanceof HTMLElement) active.blur();
  open = false;
}

/**
 * The Esc / hardware-back answer: on the compact page screen it is one step
 * back to the rail, everywhere else it is the close above.
 */
export function escapeSettingsOverlay(): void {
  if (!open) return;
  if (isCompactLayout() && !railOpen) {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
    railOpen = true;
    return;
  }
  closeSettingsOverlay();
}

// A tool pane opened under settings would be invisible, so opening one closes
// settings. Armed at module init: importing this module is what wires it, so
// no registration order and no test's reset can disarm it. The import runs
// one way (this store → toolPanes) so the pair never forms a cycle.
setToolPaneOpenExclusion(closeSettingsOverlay);

export function resetSettingsOverlayForTest(): void {
  open = false;
  section = DEFAULT_SETTINGS_SECTION;
  railOpen = true;
  computer = null;
}
