// Sidebar width — persisted per client through appStorage (ui_state
// table) so the preferred reading width survives restarts, not just
// reloads: raw localStorage resets every launch because the transport's
// ephemeral port changes the webview origin. Kept separate from
// `sidebar.svelte.ts` (which owns project expansion + sort) because
// the two slice state has no meaningful overlap and lives on different
// update cadences.
//
// The clamp is also enforced here so every caller (resize handle,
// palette command, hydration sync) converges on the same bounds.

import {
  appStorageAdoptLegacyKey,
  appStorageGet,
  appStorageSet,
} from './appStorage';
import { getAppShellWidth } from './layoutMetrics.svelte';

const WIDTH_KEY = 'sidebar:width';
const LEGACY_STORAGE_KEY = 'agent-overflow:sidebar:width';
const DEFAULT_WIDTH = 280;
export const SIDEBAR_MIN_WIDTH = 200;
/** Main pane reserve — keeps the composer + chat legible no matter how
 *  the user drags. Mirrors forge's 640px reserve scaled down a bit. */
const MAIN_RESERVE = 560;

function clamp(px: number): number {
  const viewportMax = Math.max(SIDEBAR_MIN_WIDTH, getAppShellWidth() - MAIN_RESERVE);
  return Math.max(SIDEBAR_MIN_WIDTH, Math.min(viewportMax, Math.round(px)));
}

function parseWidth(raw: string): string | null {
  const parsed = Number.parseFloat(raw);
  return Number.isFinite(parsed) ? String(parsed) : null;
}

/**
 * Exported for direct unit testing of the "corrupt stored value falls
 * back to default" behaviour, which otherwise only runs at module
 * import and can't be re-driven from a test.
 */
export function readPersistedWidth(): number {
  const raw =
    appStorageGet(WIDTH_KEY) ?? appStorageAdoptLegacyKey(WIDTH_KEY, LEGACY_STORAGE_KEY, parseWidth);
  if (raw === null) return DEFAULT_WIDTH;
  const parsed = Number.parseFloat(raw);
  if (!Number.isFinite(parsed)) return DEFAULT_WIDTH;
  return clamp(parsed);
}

let width: number = $state(readPersistedWidth());

export function getSidebarWidth(): number {
  return width;
}

/**
 * Live update for drag gestures — writes the in-memory state only.
 * Safe to call at pointer-event rate; no persistence traffic.
 * Pair with `persistSidebarWidth()` on pointer-up so the value survives
 * a reload without thrashing the write path during the drag.
 */
export function setSidebarWidthLive(next: number): void {
  const clamped = clamp(next);
  if (clamped === width) return;
  width = clamped;
}

/** Flush the current in-memory width to appStorage. Idempotent. */
export function persistSidebarWidth(): void {
  appStorageSet(WIDTH_KEY, String(width));
}

/**
 * Convenience setter that updates and persists in one call. Preferred
 * for one-shot width changes (hydration sync, palette command); the
 * pointer-driven resizer uses the live/persist pair above instead.
 */
export function setSidebarWidth(next: number): void {
  setSidebarWidthLive(next);
  persistSidebarWidth();
}

/**
 * Re-read the width after appStorage hydration lands the server-side
 * bucket. In-memory state seeded from the pre-hydration cache adopts
 * the durable value unless they already agree.
 */
export function syncSidebarWidthFromAppStorage(): void {
  const raw = appStorageGet(WIDTH_KEY);
  if (raw === null) return;
  const parsed = Number.parseFloat(raw);
  if (!Number.isFinite(parsed)) return;
  const clamped = clamp(parsed);
  if (clamped !== width) {
    width = clamped;
  }
}

/** Viewport-derived maximum — callers use this to render a resize-cap
 *  visual cue or to refuse a drag that would eat the main pane. */
export function getSidebarMaxWidth(): number {
  return Math.max(SIDEBAR_MIN_WIDTH, getAppShellWidth() - MAIN_RESERVE);
}

/** Test helper — restore the default between unit tests. Callers reset
 *  appStorage separately (resetAppStorageForTest). */
export function resetSidebarLayoutForTest(): void {
  width = DEFAULT_WIDTH;
}
