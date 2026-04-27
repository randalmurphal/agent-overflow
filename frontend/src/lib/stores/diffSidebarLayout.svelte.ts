// Diff sidebar width — persisted across sessions via localStorage.
// Mirrors `planSidebarLayout.svelte.ts` but with its own bounds and
// storage key so per-tool diff inspection can be sized independently
// from plan review (the panels share the right-side slot via the
// pane mutex; their content density is different — diffs need wider).

const STORAGE_KEY = 'agent-overflow:diff-sidebar:width';
const DEFAULT_WIDTH = 480;
export const DIFF_SIDEBAR_MIN_WIDTH = 360;
/** Same reservation as PlanSidebar — the chat composer + transcript
 *  must remain usable on a 1280-wide window when the diff sidebar is
 *  at its widest. */
const MAIN_RESERVE = 640;

function clamp(px: number): number {
  const viewportMax =
    typeof window === 'undefined'
      ? Number.POSITIVE_INFINITY
      : Math.max(DIFF_SIDEBAR_MIN_WIDTH, window.innerWidth - MAIN_RESERVE);
  return Math.max(DIFF_SIDEBAR_MIN_WIDTH, Math.min(viewportMax, Math.round(px)));
}

export function readPersistedDiffSidebarWidth(): number {
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
    // best effort
  }
}

let width: number = $state(readPersistedDiffSidebarWidth());

export function getDiffSidebarWidth(): number {
  return width;
}

/** Live update for drag gestures; pair with `persistDiffSidebarWidth()`
 *  on pointer-up. */
export function setDiffSidebarWidthLive(next: number): void {
  const clamped = clamp(next);
  if (clamped === width) return;
  width = clamped;
}

export function persistDiffSidebarWidth(): void {
  write(width);
}

export function setDiffSidebarWidth(next: number): void {
  setDiffSidebarWidthLive(next);
  persistDiffSidebarWidth();
}

export function getDiffSidebarMaxWidth(): number {
  if (typeof window === 'undefined') return Number.POSITIVE_INFINITY;
  return Math.max(DIFF_SIDEBAR_MIN_WIDTH, window.innerWidth - MAIN_RESERVE);
}

export function resetDiffSidebarLayoutForTest(): void {
  width = DEFAULT_WIDTH;
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.removeItem(STORAGE_KEY);
  } catch {
    // ignore
  }
}
