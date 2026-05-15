// Sidebar width — persisted across sessions via localStorage so the
// user's preferred reading width survives a reload. Kept separate from
// `sidebar.svelte.ts` (which owns project expansion + sort) because
// the two slice state has no meaningful overlap and lives on different
// update cadences.
//
// The clamp is also enforced here so every caller (resize handle,
// palette command, settings sync) converges on the same bounds.

import { getAppShellWidth } from './layoutMetrics.svelte';

const STORAGE_KEY = 'agent-overflow:sidebar:width';
const DEFAULT_WIDTH = 280;
export const SIDEBAR_MIN_WIDTH = 200;
/** Main pane reserve — keeps the composer + chat legible no matter how
 *  the user drags. Mirrors forge's 640px reserve scaled down a bit. */
const MAIN_RESERVE = 560;

function clamp(px: number): number {
  const viewportMax = Math.max(SIDEBAR_MIN_WIDTH, getAppShellWidth() - MAIN_RESERVE);
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(viewportMax, Math.round(px)));
}

/**
 * Exported for direct unit testing of the "corrupt stored value falls
 * back to default" behaviour, which otherwise only runs at module
 * import and can't be re-driven from a test.
 */
export function readPersistedWidth(): number {
  if (typeof localStorage === 'undefined') return DEFAULT_WIDTH;
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return DEFAULT_WIDTH;
    const parsed = Number.parseFloat(raw);
    if (!Number.isFinite(parsed)) return DEFAULT_WIDTH;
    return clamp(parsed);
  } catch {
    return DEFAULT_WIDTH;
  }
}

function write(value: number): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(STORAGE_KEY, String(value));
  } catch {
    // Best-effort persistence; in-memory width still updates.
  }
}

let width: number = $state(readPersistedWidth());

export function getSidebarWidth(): number {
  return width;
}

/**
 * Live update for drag gestures — writes the in-memory state only.
 * Safe to call at pointer-event rate; no localStorage traffic.
 * Pair with `persistSidebarWidth()` on pointer-up so the value survives
 * a reload without thrashing the disk during the drag.
 */
export function setSidebarWidthLive(next: number): void {
  const clamped = clamp(next);
  if (clamped === width) return;
  width = clamped;
}

/** Flush the current in-memory width to localStorage. Idempotent. */
export function persistSidebarWidth(): void {
  write(width);
}

/**
 * Convenience setter that updates and persists in one call. Preferred
 * for one-shot width changes (settings sync, palette command); the
 * pointer-driven resizer uses the live/persist pair above instead.
 */
export function setSidebarWidth(next: number): void {
  setSidebarWidthLive(next);
  persistSidebarWidth();
}

/** Viewport-derived maximum — callers use this to render a resize-cap
 *  visual cue or to refuse a drag that would eat the main pane. */
export function getSidebarMaxWidth(): number {
  return Math.max(SIDEBAR_MIN_WIDTH, getAppShellWidth() - MAIN_RESERVE);
}

/** Test helper — restore the default + wipe storage between unit tests. */
export function resetSidebarLayoutForTest(): void {
  width = DEFAULT_WIDTH;
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
