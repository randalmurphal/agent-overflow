import { getSettings, updateSetting } from './settings.svelte';
import type { PaneDensityMode } from '../types/settings';

export type { PaneDensityMode };

export const PANE_DENSITY_MIN_WIDTHS: Record<PaneDensityMode, number> = {
  compact: 560,
  comfortable: 880,
  spacious: 1400,
};

const DEFAULT_PANE_DENSITY: PaneDensityMode = 'compact';

export function getPaneDensityMode(): PaneDensityMode {
  return getSettings().paneDensity ?? DEFAULT_PANE_DENSITY;
}

export function getMinPaneWidth(): number {
  return PANE_DENSITY_MIN_WIDTHS[getPaneDensityMode()];
}

export async function setPaneDensityMode(mode: PaneDensityMode): Promise<void> {
  if (mode === getPaneDensityMode()) return;
  await updateSetting('paneDensity', mode);
}
