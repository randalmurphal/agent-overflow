import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';
import { addToast } from './toast.svelte';

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

export function getSettings(): Settings {
  return settings;
}

export async function loadSettings(): Promise<void> {
  try {
    const result = await GetSettings();
    if (result) {
      settings = { ...DEFAULT_SETTINGS, ...result } as Settings;
    }
  } catch (err) {
    console.error('Failed to load settings:', err);
    addToast('error', 'Failed to load settings');
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
    addToast('error', 'Failed to save setting');
  }
}
