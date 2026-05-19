// Resolves the active diff theme by delegating to `themeMode.svelte.ts`
// and mapping the resolved light/dark to the Shiki theme name registered
// in the worker — currently `github-dark` and `github-light`. Changing
// the set requires updating the worker's loader too.
//
// The matchMedia subscription itself lives in `themeMode.svelte.ts` so
// the terminal, mermaid, and diff highlighting all share one listener.

import { getResolvedTheme, teardownThemeModeForTest } from './themeMode.svelte';

export type DiffThemeName = 'github-dark' | 'github-light';

/**
 * Returns the resolved Shiki theme name for the current settings.
 *
 * **Pure read** — safe to call from `$derived` blocks. The
 * "evict prior theme's cached tokens on transition" side-effect
 * is the caller's responsibility (DiffSidebarBody runs it from a
 * `$effect`).
 */
export function getDiffTheme(): DiffThemeName {
  return getResolvedTheme() === 'dark' ? 'github-dark' : 'github-light';
}

/** Used in tests to verify the listener is removed cleanly. */
export function teardownDiffThemeForTest(): void {
  teardownThemeModeForTest();
}
