export type PaneDensityMode = 'compact' | 'comfortable' | 'spacious';

export const PANE_DENSITY_STORAGE_KEY = 'agentOverflowPaneDensity';

export const PANE_DENSITY_MIN_WIDTHS: Record<PaneDensityMode, number> = {
  compact: 560,
  comfortable: 880,
  spacious: 1400,
};

const DEFAULT_PANE_DENSITY: PaneDensityMode = 'compact';

const VALID_MODES = new Set<PaneDensityMode>(['compact', 'comfortable', 'spacious']);

function isPaneDensityMode(value: string | null): value is PaneDensityMode {
  return value !== null && VALID_MODES.has(value as PaneDensityMode);
}

export function readPersistedPaneDensity(): PaneDensityMode {
  try {
    const storage = globalThis.localStorage;
    if (!storage) return DEFAULT_PANE_DENSITY;
    const raw = storage.getItem(PANE_DENSITY_STORAGE_KEY);
    return isPaneDensityMode(raw) ? raw : DEFAULT_PANE_DENSITY;
  } catch (err) {
    console.warn('Failed to read pane density persistence:', err);
    return DEFAULT_PANE_DENSITY;
  }
}

function writePaneDensity(mode: PaneDensityMode): void {
  try {
    globalThis.localStorage?.setItem(PANE_DENSITY_STORAGE_KEY, mode);
  } catch (err) {
    console.warn('Failed to write pane density persistence:', err);
  }
}

let currentMode: PaneDensityMode = $state(readPersistedPaneDensity());
let minPaneWidth = $derived(PANE_DENSITY_MIN_WIDTHS[currentMode]);

export function getPaneDensityMode(): PaneDensityMode {
  return currentMode;
}

export function getMinPaneWidth(): number {
  return minPaneWidth;
}

export function setPaneDensityMode(mode: PaneDensityMode): void {
  if (mode === currentMode) return;
  currentMode = mode;
  writePaneDensity(mode);
}

export function resetPaneDensityForTest(): void {
  currentMode = DEFAULT_PANE_DENSITY;
  try {
    globalThis.localStorage?.removeItem(PANE_DENSITY_STORAGE_KEY);
  } catch (err) {
    console.warn('Failed to clear pane density persistence:', err);
  }
}
