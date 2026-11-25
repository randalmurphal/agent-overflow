// Coordinates cached app-shell blur for full-viewport overlays and modals.
//
// Applying `backdrop-filter: blur(...)` to a full-viewport scrim forces
// Chromium/Skia/WebKit compositors to re-run the Gaussian blur pass across
// the viewport whenever content INSIDE the overlay or modal repaints
// (tab switches, hover transitions, typing, cursor blinks). Delegating the
// base overlay's blur to `filter: blur(...)` on the inert app-shell layer
// behind it lets the compositor cache one blurred texture across frames
// while preserving identical visuals.
//
// Stacking rule:
// - Only the bottom-most open overlay/modal (index 0) delegates to the app
//   shell when a shell host is mounted (`App.svelte`).
// - Any stacked dialog on top (index >= 1) keeps its native `backdrop-filter`
//   so it blurs the underlying overlay/modal as well.
// - Standalone renders without an app-shell host (e.g. isolated unit tests)
//   always fall back to native `backdrop-filter`.

import { untrack } from 'svelte';

export type BackdropBlurLevel = 'sm' | 'md';

interface BlurEntry {
  readonly id: number;
  readonly level: BackdropBlurLevel;
}

let nextId = 1;
let hostCount = $state(0);
let stack = $state<BlurEntry[]>([]);

export function registerAppShellBlurHost(): () => void {
  untrack(() => {
    hostCount += 1;
  });
  let released = false;
  return () => {
    if (released) return;
    released = true;
    untrack(() => {
      hostCount = Math.max(0, hostCount - 1);
    });
  };
}

export function getAppShellBlur(): BackdropBlurLevel | null {
  if (hostCount <= 0 || stack.length === 0) return null;
  return stack[0].level;
}

export interface BackdropBlurLease {
  /** True when the app shell is carrying this surface's blur, so this
   * surface's scrim should suppress its own `backdrop-filter`. */
  readonly delegatedToShell: boolean;
  release(): void;
}

export function acquireBackdropBlur(level: BackdropBlurLevel): BackdropBlurLease {
  const id = nextId++;
  // Read and update `stack` inside untrack so calling acquire/release from an
  // $effect never subscribes that effect to `stack` and triggers a loop.
  untrack(() => {
    stack = [...stack, { id, level }];
  });
  let released = false;

  return {
    get delegatedToShell(): boolean {
      return !released && hostCount > 0 && stack.length > 0 && stack[0].id === id;
    },
    release(): void {
      if (released) return;
      released = true;
      untrack(() => {
        stack = stack.filter((entry) => entry.id !== id);
      });
    },
  };
}

export function resetBackdropBlurForTest(): void {
  hostCount = 0;
  stack = [];
}
