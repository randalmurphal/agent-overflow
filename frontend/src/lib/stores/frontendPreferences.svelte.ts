// This frontend owns its preferences; hosts supply only the one-time migration
// seed. Remote settings echoes never change this screen's appearance or choices.
import { FRONTEND_SETTINGS_KEYS, SETTINGS_DEFAULTS, FRONTEND_SETTING_OPTIONS, FRONTEND_SETTING_RANGES } from '../generated/settingsDefaults';
import type { Settings } from '../types/settings';
import { readFrontendValue, writeFrontendValue, onFrontendValueChanged } from './frontendStorage';

const KEY = 'preferences';
const keys = new Set<string>(FRONTEND_SETTINGS_KEYS);

function read(): Partial<Settings> {
  const value = readFrontendValue(KEY);
  if (!value || typeof value !== 'object' || Array.isArray(value)) return {};
  return validatedFrontendPreferences(value);
}

// localStorage is untyped and may come from an interrupted/older write.
// Reject malformed shapes before they reach render-time array/number readers.
export function validatedFrontendPreferences(value: object): Partial<Settings> {
  return Object.fromEntries(Object.entries(value).filter(([key, entry]) => {
    if (!isFrontendPreference(key)) return false;
    const fallback = SETTINGS_DEFAULTS[key];
    const options = FRONTEND_SETTING_OPTIONS[key];
    if (options && (typeof entry !== 'string' || !options.includes(entry))) return false;
    const range = FRONTEND_SETTING_RANGES[key];
    if (range && (typeof entry !== 'number' || !Number.isInteger(entry) || entry < range[0] || entry > range[1])) return false;
    if (Array.isArray(fallback)) {
      return Array.isArray(entry) && entry.length <= 1024
        && entry.every((item) => typeof item === 'string' && item.length <= 4096);
    }
    if (typeof entry !== typeof fallback) return false;
    if (typeof entry === 'number') return Number.isFinite(entry) && entry > 0 && entry <= 4096;
    if (typeof entry === 'string') return entry.length <= 4096;
    return typeof entry === 'boolean';
  }));
}

let preferences = $state<Partial<Settings>>(read());

export function isFrontendPreference(key: string): key is typeof FRONTEND_SETTINGS_KEYS[number] {
  return keys.has(key);
}

export function frontendPreferences(): Partial<Settings> {
  return preferences;
}

export function seedFrontendPreferences(settings: Settings): void {
  const missing = Object.fromEntries(FRONTEND_SETTINGS_KEYS
    .filter((key) => !Object.hasOwn(preferences, key))
    .map((key) => [key, settings[key]]));
  if (Object.keys(missing).length === 0) return;
  preferences = { ...validatedFrontendPreferences(missing), ...preferences };
  writeFrontendValue(KEY, preferences);
}

export function updateFrontendPreferences(patch: Partial<Settings>): void {
  const local = validatedFrontendPreferences(patch);
  if (Object.keys(local).length === 0) return;
  preferences = { ...preferences, ...local };
  writeFrontendValue(KEY, preferences);
}

export function resetFrontendPreferencesForTest(seed: Partial<Settings> = read()): void {
  preferences = seed;
}

onFrontendValueChanged(KEY, () => { preferences = read(); });
