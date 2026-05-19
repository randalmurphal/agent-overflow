// Resolves the active light/dark mode from `settings.theme` plus the
// system color-scheme preference for `'system'`. Exposes a pure read
// so consumers can call it from `$derived` blocks without triggering
// Svelte 5's `state_unsafe_mutation` guard.
//
// The matchMedia listener is attached lazily on first `'system'` read
// and survives until `teardownThemeModeForTest()` is called. Same
// pattern as `diffTheme.svelte.ts` — and `getDiffTheme()` is now a
// one-line wrapper over this store, so we don't double-subscribe.

import { getSettings } from './settings.svelte';

export type ResolvedTheme = 'light' | 'dark';

type SystemSubscription = {
  query: MediaQueryList;
  handler: (e: MediaQueryListEvent) => void;
} | null;

let systemSubscription: SystemSubscription = null;
let systemMode: ResolvedTheme = $state(detectSystemModeOnce());

function detectSystemModeOnce(): ResolvedTheme {
  if (typeof window === 'undefined') return 'dark';
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches
      ? 'dark'
      : 'light';
  } catch {
    return 'dark';
  }
}

function ensureSystemSubscription(): void {
  if (systemSubscription) return;
  if (typeof window === 'undefined') return;
  try {
    const query = window.matchMedia('(prefers-color-scheme: dark)');
    const handler = (e: MediaQueryListEvent) => {
      systemMode = e.matches ? 'dark' : 'light';
    };
    query.addEventListener('change', handler);
    systemSubscription = { query, handler };
  } catch {
    // Some test environments don't implement matchMedia. Treat as
    // dark fallback; the cached `systemMode` already holds that.
  }
}

/**
 * Returns the resolved light/dark mode for the current settings.
 * `'system'` reads `prefers-color-scheme` and re-evaluates when the
 * media query fires (subscriber attached lazily on first read).
 *
 * **Pure read** — safe to call from `$derived` blocks.
 */
export function getResolvedTheme(): ResolvedTheme {
  const setting = getSettings().theme;
  if (setting === 'light') return 'light';
  if (setting === 'dark') return 'dark';
  // 'system' or unset
  ensureSystemSubscription();
  return systemMode;
}

/** Used in tests to verify the listener is removed cleanly. */
export function teardownThemeModeForTest(): void {
  if (systemSubscription) {
    try {
      systemSubscription.query.removeEventListener('change', systemSubscription.handler);
    } catch {
      // ignore
    }
    systemSubscription = null;
  }
  systemMode = detectSystemModeOnce();
}
