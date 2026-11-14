// Whole-number settings bounded by their generated range, so a field and the
// backend's validator cannot disagree about what a save accepts.

import { FRONTEND_SETTING_RANGES } from '../generated/settingsDefaults';
import type { Settings } from '../types/settings';

export function settingRange(key: keyof Settings): readonly [number, number] {
  const range = FRONTEND_SETTING_RANGES[key];
  if (!range) throw new Error(`${key} has no generated range`);
  return range;
}

/**
 * Turns a typed entry into what the backend will store: a whole number inside
 * `range`. An empty or unreadable entry keeps `fallback`, the stored value,
 * rather than becoming the minimum.
 */
export function clampToRange(raw: string, fallback: number, [min, max]: readonly [number, number]): number {
  const value = raw.trim() === '' ? NaN : Number(raw);
  if (!Number.isFinite(value)) return fallback;
  return Math.min(Math.max(Math.round(value), min), max);
}
