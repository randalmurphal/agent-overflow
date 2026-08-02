// Sidebar width + collapsed state — persisted per client through
// appStorage (ui_state table) so the preferred reading width survives
// restarts, not just reloads: raw localStorage resets every launch
// because the transport's ephemeral port changes the webview origin.
// Kept separate from `sidebar.svelte.ts` (which owns project expansion
// + sort) because the two slice state has no meaningful overlap and
// lives on different update cadences.
//
// Width and collapsed are separate facts on purpose: `width` is always
// the EXPANDED width, whether or not the sidebar is currently showing
// it. Collapsing never overwrites it, so expanding restores the width
// the user chose rather than the minimum.
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
const COLLAPSED_KEY = 'sidebar:collapsed';
const LEGACY_STORAGE_KEY = 'agent-overflow:sidebar:width';
const DEFAULT_WIDTH = 280;
export const SIDEBAR_MIN_WIDTH = 200;
/** Width of the collapsed rail — an IconButton (h-7/w-7) plus its
 *  symmetric gutter. The rail is the sidebar's only chrome while
 *  collapsed, so it is not resizable and has no independent bounds. */
export const SIDEBAR_RAIL_WIDTH = 36;
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

/**
 * Exported alongside `readPersistedWidth` for the same reason: the
 * module-init read cannot be re-driven from a test. Anything that is
 * not exactly the stored `'1'` reads as expanded, so a corrupt value
 * degrades to the visible state rather than a hidden sidebar the user
 * has no on-screen way to explain.
 */
export function readPersistedCollapsed(): boolean {
  return appStorageGet(COLLAPSED_KEY) === '1';
}

let width: number = $state(readPersistedWidth());
let collapsed: boolean = $state(readPersistedCollapsed());

export function getSidebarWidth(): number {
  return width;
}

export function isSidebarCollapsed(): boolean {
  return collapsed;
}

/**
 * Collapse to the rail or expand back to `width`.
 *
 * Both directions do work beyond flipping the flag, and both must run
 * even when the flag already holds the target value — a caller that
 * collapses twice is still asking for "the width on screen is safe"
 * both times:
 *
 *   - Collapsing FLUSHES the current width first. Collapsing while a
 *     resize drag is live tears the resizer down, and
 *     `createResizeGesture.destroy()` deliberately does not call
 *     `onResizeEnd` — without this flush the width the user just
 *     dragged to would be the one thing the collapse threw away.
 *   - Expanding RE-CLAMPS. Nothing re-clamps a width nobody is
 *     rendering, so a window shrunk while collapsed would otherwise
 *     bring the sidebar back over the main pane's reserve.
 */
export function setSidebarCollapsed(next: boolean): void {
  if (next) {
    persistSidebarWidth();
  } else {
    width = clamp(width);
    persistSidebarWidth();
  }
  collapsed = next;
  appStorageSet(COLLAPSED_KEY, next ? '1' : '0');
}

export function toggleSidebarCollapsed(): void {
  setSidebarCollapsed(!collapsed);
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
 * Re-read both slices after appStorage hydration lands the server-side
 * bucket. In-memory state seeded from the pre-hydration cache adopts
 * the durable value unless they already agree. Neither key is written
 * back here — hydration is an adoption, not a user action, so it must
 * not fire the flush/re-clamp `setSidebarCollapsed` owes a real toggle.
 */
export function syncSidebarLayoutFromAppStorage(): void {
  const raw = appStorageGet(WIDTH_KEY);
  if (raw !== null) {
    const parsed = Number.parseFloat(raw);
    if (Number.isFinite(parsed)) {
      const clamped = clamp(parsed);
      if (clamped !== width) width = clamped;
    }
  }
  const rawCollapsed = appStorageGet(COLLAPSED_KEY);
  if (rawCollapsed === null) return;
  const nextCollapsed = rawCollapsed === '1';
  if (nextCollapsed !== collapsed) collapsed = nextCollapsed;
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
  collapsed = false;
}
