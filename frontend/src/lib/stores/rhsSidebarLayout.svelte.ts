// Shared right-side sidebar layout factory. Every RHS panel that
// supports drag-to-resize composes this — diff sidebar, plan sidebar,
// checkpoint diff panel, and any future addition. Keeps the
// width-bound + persistence logic in one place so they all behave
// identically; per-panel files just supply a storage key, default,
// and minimum width.
//
// Width state is reactive ($state) and lives in the closure of the
// returned handle. Multiple calls produce independent stores.
//
// Live setter for drag gestures (`setWidthLive`) writes the in-memory
// state only — safe at pointer-event rate, no localStorage traffic.
// Pair with `persistWidth` on pointerup to flush.

export interface RhsSidebarLayoutOptions {
  storageKey: string;
  defaultWidth: number;
  minWidth: number;
  /** Pixels reserved for the rest of the chrome (left sidebar +
   *  chat column + composer) when the panel is at its widest.
   *  Defaults to 640 — empirically picked to keep the composer +
   *  transcript usable on a 1280-wide window. */
  mainReserve?: number;
}

export interface RhsSidebarLayout {
  readonly minWidth: number;
  getWidth(): number;
  getMaxWidth(): number;
  setWidthLive(next: number): void;
  persistWidth(): void;
  setWidth(next: number): void;
  readPersistedWidth(): number;
  resetForTest(): void;
}

const DEFAULT_MAIN_RESERVE = 640;

export function createRhsSidebarLayout(
  opts: RhsSidebarLayoutOptions,
): RhsSidebarLayout {
  const { storageKey, defaultWidth, minWidth } = opts;
  const mainReserve = opts.mainReserve ?? DEFAULT_MAIN_RESERVE;

  function getMaxWidth(): number {
    if (typeof window === 'undefined') return Number.POSITIVE_INFINITY;
    return Math.max(minWidth, window.innerWidth - mainReserve);
  }

  function clamp(px: number): number {
    return Math.max(minWidth, Math.min(getMaxWidth(), Math.round(px)));
  }

  function readPersistedWidth(): number {
    if (typeof localStorage === 'undefined') return defaultWidth;
    try {
      const raw = localStorage.getItem(storageKey);
      if (!raw) return defaultWidth;
      const parsed = Number.parseFloat(raw);
      if (!Number.isFinite(parsed)) return defaultWidth;
      return clamp(parsed);
    } catch {
      return defaultWidth;
    }
  }

  function write(value: number): void {
    if (typeof localStorage === 'undefined') return;
    try {
      localStorage.setItem(storageKey, String(value));
    } catch {
      // Best-effort persistence; in-memory width still updates.
    }
  }

  let width: number = $state(readPersistedWidth());

  function setWidthLive(next: number): void {
    const clamped = clamp(next);
    if (clamped === width) return;
    width = clamped;
  }

  function persistWidth(): void {
    write(width);
  }

  function setWidth(next: number): void {
    setWidthLive(next);
    persistWidth();
  }

  function resetForTest(): void {
    width = defaultWidth;
    if (typeof localStorage === 'undefined') return;
    try {
      localStorage.removeItem(storageKey);
    } catch {
      // ignore
    }
  }

  return {
    get minWidth() {
      return minWidth;
    },
    getWidth: () => width,
    getMaxWidth,
    setWidthLive,
    persistWidth,
    setWidth,
    readPersistedWidth,
    resetForTest,
  };
}
