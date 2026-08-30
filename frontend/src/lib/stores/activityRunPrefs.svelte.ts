// Domain accessors for the activity-run user settings, mirroring
// `paneDensity.svelte.ts`. Consumers read through here rather than
// touching the raw settings store so a corrupt or out-of-range persisted
// value cannot reach layout code — Go sanitizes on load, but a remote
// client can be handed settings from an older or newer peer.

import { getSettings, updateSetting } from './settings.svelte';
import { SETTINGS_DEFAULTS } from '../generated/settingsDefaults';
import {
  ACTIVITY_RUN_WINDOW_ROWS_DEFAULT,
  ACTIVITY_RUN_WINDOW_ROWS_MAX,
  ACTIVITY_RUN_WINDOW_ROWS_MIN,
} from '../utils/activityRunWindow';
import type { ActivityRunDefaultMode } from '../types/settings';

export type { ActivityRunDefaultMode };

// Generated from internal/settings.DefaultSettings.ActivityRunDefault, so
// the fallback for an out-of-range persisted value is the shipped default
// rather than a second opinion about it.
const DEFAULT_MODE: ActivityRunDefaultMode = SETTINGS_DEFAULTS.activityRunDefault;

export function getActivityRunDefaultMode(): ActivityRunDefaultMode {
  const mode = getSettings().activityRunDefault;
  return mode === 'expanded' || mode === 'collapsed' ? mode : DEFAULT_MODE;
}

/** Collapse state for a run the user has never explicitly toggled. */
export function activityRunDefaultCollapsed(): boolean {
  return getActivityRunDefaultMode() === 'collapsed';
}

export async function setActivityRunDefaultMode(mode: ActivityRunDefaultMode): Promise<void> {
  if (mode === getActivityRunDefaultMode()) return;
  await updateSetting('activityRunDefault', mode);
}

/**
 * Mirrors internal/settings.{Min,Max}ActivityRunWindowRows — below the
 * floor the clip would show blank space under the last mounted row; above
 * the ceiling the window stops bounding DOM, which is its whole purpose.
 * Exported so an input can echo the clamp it just applied: clamping to the
 * already-stored value writes nothing, so the field has to correct itself.
 */
export function clampActivityRunWindowRows(rows: number): number {
  if (!Number.isFinite(rows)) return ACTIVITY_RUN_WINDOW_ROWS_DEFAULT;
  return Math.min(
    ACTIVITY_RUN_WINDOW_ROWS_MAX,
    Math.max(ACTIVITY_RUN_WINDOW_ROWS_MIN, Math.round(rows)),
  );
}

/** Mount-window size, clamped. */
export function activityRunWindowRows(): number {
  return clampActivityRunWindowRows(getSettings().activityRunWindowRows);
}

export async function setActivityRunWindowRows(rows: number): Promise<void> {
  const next = clampActivityRunWindowRows(rows);
  if (next === activityRunWindowRows()) return;
  await updateSetting('activityRunWindowRows', next);
}
