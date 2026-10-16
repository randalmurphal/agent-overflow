// The Claude command timeout ceiling (settings.claudeMaxCommandTimeoutHours).
// The bounds come from the generated range, so the field and the backend's
// validator cannot disagree about what a save accepts.

import { FRONTEND_SETTING_RANGES } from '../generated/settingsDefaults';

/** The variable the backend exports; a custom-env entry with this name wins. */
export const COMMAND_TIMEOUT_ENV = 'BASH_MAX_TIMEOUT_MS';

const range = FRONTEND_SETTING_RANGES.claudeMaxCommandTimeoutHours;
if (!range) throw new Error('claudeMaxCommandTimeoutHours has no generated range');

export const COMMAND_TIMEOUT_HOURS_MIN = range[0];
export const COMMAND_TIMEOUT_HOURS_MAX = range[1];

/**
 * Turns a typed entry into what the backend will store: whole hours inside
 * the range. An empty or unreadable entry keeps `fallback`, the stored value,
 * rather than becoming the minimum.
 */
export function clampCommandTimeoutHours(raw: string, fallback: number): number {
  const value = raw.trim() === '' ? NaN : Number(raw);
  if (!Number.isFinite(value)) return fallback;
  return Math.min(Math.max(Math.round(value), COMMAND_TIMEOUT_HOURS_MIN), COMMAND_TIMEOUT_HOURS_MAX);
}
