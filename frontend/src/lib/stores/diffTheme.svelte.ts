// Resolves the active diff theme from `settings.theme` plus the
// system color-scheme preference for `'system'`. Exposes a pure
// read so consumers can call it from `$derived` blocks without
// triggering Svelte 5's `state_unsafe_mutation` guard. The
// "evict prior theme's tokens on transition" side-effect is
// driven separately by callers (see DiffSidebarBody) — keeping
// the read path clean of writes.
//
// Theme name maps directly to the Shiki theme registered in the
// worker — currently `github-dark` and `github-light`. Changing
// the set requires updating the worker's loader too.

import { getSettings } from './settings.svelte';

export type DiffThemeName = 'github-dark' | 'github-light';

type SystemSubscription = {
  query: MediaQueryList;
  handler: (e: MediaQueryListEvent) => void;
} | null;

let systemSubscription: SystemSubscription = null;
let systemTheme: DiffThemeName = $state(detectSystemThemeOnce());

function detectSystemThemeOnce(): DiffThemeName {
  if (typeof window === 'undefined') return 'github-dark';
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches
      ? 'github-dark'
      : 'github-light';
  } catch {
    return 'github-dark';
  }
}

function ensureSystemSubscription(): void {
  if (systemSubscription) return;
  if (typeof window === 'undefined') return;
  try {
    const query = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => {
      systemTheme = e.matches ? 'github-dark' : 'github-light';
    };
    query.addEventListener('change', handler);
    systemSubscription = { query, handler };
  } catch {
    // Some test environments don't implement matchMedia. Treat as
    // dark fallback; the cached `systemTheme` already holds that.
  }
}

/**
 * Returns the resolved Shiki theme name for the current settings.
 * `'system'` reads `prefers-color-scheme` and re-evaluates when the
 * media query fires (subscriber attached lazily on first read).
 *
 * **Pure read** — safe to call from `$derived` blocks. The
 * "evict prior theme's cached tokens on transition" side-effect
 * is the caller's responsibility (DiffSidebarBody runs it from a
 * `$effect`).
 */
export function getDiffTheme(): DiffThemeName {
  const setting = getSettings().theme;
  if (setting === 'light') return 'github-light';
  if (setting === 'dark') return 'github-dark';
  // 'system' or unset
  ensureSystemSubscription();
  return systemTheme;
}

/** Used in tests to verify the listener is removed cleanly. */
export function teardownDiffThemeForTest(): void {
  if (systemSubscription) {
    try {
      systemSubscription.query.removeEventListener('change', systemSubscription.handler);
    } catch {
      // ignore
    }
    systemSubscription = null;
  }
  systemTheme = detectSystemThemeOnce();
}
