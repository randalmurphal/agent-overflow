// Plan sidebar width — persisted across sessions via localStorage. Mirrors
// `sidebarLayout.svelte.ts` (the threads sidebar) but with its own bounds
// because the right-side plan panel competes with the chat transcript +
// composer for horizontal space, not just the sidebar.

const STORAGE_KEY = 'agent-overflow:plan-sidebar:width';
const DEFAULT_WIDTH = 440;
export const PLAN_SIDEBAR_MIN_WIDTH = 320;
/** Reserve enough room for the left sidebar + a readable chat column when
 *  the plan panel is at its widest. Empirically picked to keep the
 *  composer + transcript usable on a 1280-wide window. */
const MAIN_RESERVE = 640;

function clamp(px: number): number {
  const viewportMax =
    typeof window === 'undefined'
      ? Number.POSITIVE_INFINITY
      : Math.max(PLAN_SIDEBAR_MIN_WIDTH, window.innerWidth - MAIN_RESERVE);
  return Math.max(PLAN_SIDEBAR_MIN_WIDTH, Math.min(viewportMax, Math.round(px)));
}

/**
 * Exported for direct unit testing of the "corrupt stored value falls
 * back to default" behaviour, which otherwise only runs at module
 * import and can't be re-driven from a test.
 */
export function readPersistedPlanSidebarWidth(): number {
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

let width: number = $state(readPersistedPlanSidebarWidth());

export function getPlanSidebarWidth(): number {
  return width;
}

/**
 * Live update for drag gestures — writes the in-memory state only.
 * Safe to call at pointer-event rate; no localStorage traffic.
 * Pair with `persistPlanSidebarWidth()` on pointer-up.
 */
export function setPlanSidebarWidthLive(next: number): void {
  const clamped = clamp(next);
  if (clamped === width) return;
  width = clamped;
}

/** Flush the current in-memory width to localStorage. Idempotent. */
export function persistPlanSidebarWidth(): void {
  write(width);
}

/** Convenience setter that updates and persists in one call. */
export function setPlanSidebarWidth(next: number): void {
  setPlanSidebarWidthLive(next);
  persistPlanSidebarWidth();
}

/** Viewport-derived maximum — refuse drags that would eat the chat pane. */
export function getPlanSidebarMaxWidth(): number {
  if (typeof window === 'undefined') return Number.POSITIVE_INFINITY;
  return Math.max(PLAN_SIDEBAR_MIN_WIDTH, window.innerWidth - MAIN_RESERVE);
}

/** Test helper — restore the default + wipe storage between unit tests. */
export function resetPlanSidebarLayoutForTest(): void {
  width = DEFAULT_WIDTH;
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
