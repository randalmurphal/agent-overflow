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
// - Only the lowest-tier (z-index) open overlay/modal delegates to the app
//   shell when a shell host is mounted (`App.svelte`), breaking ties in FIFO
//   order. This ensures an OverlayShell (z-40) mounted beneath an already-open
//   Modal (z-60) owns the shell blur while the higher-tier Modal keeps native
//   `backdrop-filter` to blur the OverlayShell beneath it.
// - Any stacked surface above that base keeps its native `backdrop-filter`
//   so it blurs the underlying overlay/modal as well.
// - Standalone renders without an app-shell host (e.g. isolated unit tests)
//   always fall back to native `backdrop-filter`.

import { untrack } from 'svelte';

export type BackdropBlurLevel = 'sm' | 'md';

interface BlurEntry {
  readonly id: number;
  readonly level: BackdropBlurLevel;
  readonly tier: number;
}

let nextId = 1;
let hostCount = $state(0);
let stack = $state<BlurEntry[]>([]);

function baseEntry(): BlurEntry | null {
  if (stack.length === 0) return null;
  let winner = stack[0];
  for (let i = 1; i < stack.length; i += 1) {
    const candidate = stack[i];
    if (candidate.tier < winner.tier) winner = candidate;
  }
  return winner;
}

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
  if (hostCount <= 0) return null;
  return baseEntry()?.level ?? null;
}

export interface BackdropBlurLease {
  /** True when the app shell is carrying this surface's blur, so this
   * surface's scrim should suppress its own `backdrop-filter`. */
  readonly delegatedToShell: boolean;
  release(): void;
}

export function acquireBackdropBlur(level: BackdropBlurLevel, tier = 0): BackdropBlurLease {
  const id = nextId++;
  // Read and update `stack` inside untrack so calling acquire/release from an
  // $effect never subscribes that effect to `stack` and triggers a loop.
  untrack(() => {
    stack = [...stack, { id, level, tier }];
  });
  let released = false;

  return {
    get delegatedToShell(): boolean {
      return !released && hostCount > 0 && baseEntry()?.id === id;
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
