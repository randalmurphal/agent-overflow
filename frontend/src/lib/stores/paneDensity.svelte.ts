import { getSettings, updateSetting } from './settings.svelte';
import { PANE_DENSITY_MIN_WIDTHS } from '../utils/paneWidths';
import type { PaneDensityMode } from '../types/settings';

export type { PaneDensityMode };
export { PANE_DENSITY_MIN_WIDTHS };

const DEFAULT_PANE_DENSITY: PaneDensityMode = 'compact';

export function getPaneDensityMode(): PaneDensityMode {
  // Validate against the known modes, not just null/undefined: a corrupt
  // persisted value would otherwise flow through as an undefined min width
  // ("min-width:undefinedpx", silently dropped) instead of falling back.
  const mode = getSettings().paneDensity;
  return mode != null && mode in PANE_DENSITY_MIN_WIDTHS ? mode : DEFAULT_PANE_DENSITY;
}

export function getMinPaneWidth(): number {
  return PANE_DENSITY_MIN_WIDTHS[getPaneDensityMode()];
}

export async function setPaneDensityMode(mode: PaneDensityMode): Promise<void> {
  if (mode === getPaneDensityMode()) return;
  await updateSetting('paneDensity', mode);
}
