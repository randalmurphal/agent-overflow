// The Claude command timeout ceiling (settings.claudeMaxCommandTimeoutHours).

import { clampToRange, settingRange } from './settingRange';

/** The variable the backend exports; a custom-env entry with this name wins. */
export const COMMAND_TIMEOUT_ENV = 'BASH_MAX_TIMEOUT_MS';

const range = settingRange('claudeMaxCommandTimeoutHours');

export const COMMAND_TIMEOUT_HOURS_MIN = range[0];
export const COMMAND_TIMEOUT_HOURS_MAX = range[1];

/** Whole hours inside the range; see clampToRange. */
export function clampCommandTimeoutHours(raw: string, fallback: number): number {
  return clampToRange(raw, fallback, range);
}
