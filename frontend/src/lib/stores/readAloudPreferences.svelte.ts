// How this device reads replies aloud: whether it does, in which voice, how
// fast and how loud. Preferences of the screen, kept in frontend storage:
// nothing on a computer reads them.

import { onFrontendValueChanged, readFrontendValue, writeFrontendValue } from './frontendStorage';

export interface ReadAloudPreferences {
  enabled: boolean;
  /** A voice id of this device's engine; '' is its default voice. */
  voiceId: string;
  rate: number;
  volume: number;
}

export const READ_ALOUD_RATE = { min: 0.5, max: 2, step: 0.1 } as const;
export const READ_ALOUD_VOLUME = { min: 0, max: 1, step: 0.05 } as const;

const STORAGE_KEY = 'readAloud';

const DEFAULTS: ReadAloudPreferences = { enabled: false, voiceId: '', rate: 1, volume: 1 };

function inRange(value: unknown, range: { min: number; max: number }, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) && value >= range.min && value <= range.max ? value : fallback;
}

/** What storage holds, with anything missing or out of range at its default. */
function stored(): ReadAloudPreferences {
  const raw = readFrontendValue(STORAGE_KEY);
  const value = raw !== null && typeof raw === 'object' ? (raw as Record<string, unknown>) : {};
  return {
    enabled: typeof value.enabled === 'boolean' ? value.enabled : DEFAULTS.enabled,
    voiceId: typeof value.voiceId === 'string' ? value.voiceId : DEFAULTS.voiceId,
    rate: inRange(value.rate, READ_ALOUD_RATE, DEFAULTS.rate),
    volume: inRange(value.volume, READ_ALOUD_VOLUME, DEFAULTS.volume),
  };
}

const preferences = $state<ReadAloudPreferences>(stored());

if (typeof window !== 'undefined') {
  onFrontendValueChanged(STORAGE_KEY, () => Object.assign(preferences, stored()));
}

export function readAloudPreferences(): Readonly<ReadAloudPreferences> {
  return preferences;
}

/** Change one preference; a value out of range is refused. */
export function setReadAloudPreference<K extends keyof ReadAloudPreferences>(key: K, value: ReadAloudPreferences[K]): void {
  const next = { ...preferences, [key]: value };
  if (next.rate !== inRange(next.rate, READ_ALOUD_RATE, NaN) || next.volume !== inRange(next.volume, READ_ALOUD_VOLUME, NaN)) return;
  Object.assign(preferences, next);
  writeFrontendValue(STORAGE_KEY, { ...preferences });
}

/** Reload from storage. Tests only. */
export function reloadReadAloudPreferencesForTest(): void {
  Object.assign(preferences, stored());
}
