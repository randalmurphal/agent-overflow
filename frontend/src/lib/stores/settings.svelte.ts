import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';

const DEFAULT_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: '',
  defaultModelCodex: '',
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: false,
  confirmDelete: true,
  claudeBinaryPath: '',
  codexBinaryPath: '',
  claudeEnabled: true,
  codexEnabled: true,
};

let settings: Settings = $state({ ...DEFAULT_SETTINGS });
let loaded = $state(false);

export function getSettings(): Settings {
  return settings;
}

export function isSettingsLoaded(): boolean {
  return loaded;
}

export async function loadSettings(): Promise<void> {
  try {
    const result = await GetSettings();
    if (result) {
      settings = { ...DEFAULT_SETTINGS, ...result } as Settings;
    }
    loaded = true;
  } catch (err) {
    console.error('Failed to load settings:', err);
    loaded = true;
  }
}

export async function updateSetting<K extends keyof Settings>(
  key: K,
  value: Settings[K],
): Promise<void> {
  settings = { ...settings, [key]: value };
  try {
    const result = await UpdateSettings({ [key]: value });
    if (result) {
      settings = { ...DEFAULT_SETTINGS, ...result } as Settings;
    }
  } catch (err) {
    console.error('Failed to update setting:', err);
  }
}
