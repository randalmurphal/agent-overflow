import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';
import { addToast } from './toast.svelte';

const DEFAULT_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-sonnet-4-6',
  defaultModelCodex: 'gpt-5.4',
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: '',
  observabilityEventLogEnabled: false,
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
  const previous = { ...settings };
  settings = { ...settings, [key]: value };
  try {
    const result = await UpdateSettings({ [key]: value });
    if (result) {
      settings = { ...DEFAULT_SETTINGS, ...result } as Settings;
    }
  } catch (err) {
    console.error('Failed to update setting:', err);
    settings = previous;
    addToast('error', 'Failed to save setting');
  }
}
