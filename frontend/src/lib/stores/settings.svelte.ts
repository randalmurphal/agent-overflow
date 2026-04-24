import type { Settings } from '../types/settings';
import { GetSettings, UpdateSettings } from './bindings';
import { addToast } from './toast.svelte';

const DEFAULT_SETTINGS: Settings = {
  theme: 'system',
  timestampFormat: 'locale',
  defaultProvider: 'claude',
  defaultModelClaude: 'claude-opus-4-7',
  defaultModelCodex: 'gpt-5.5',
  modelContextWindows: {},
  recentWorkspaces: [],
  diffWordWrap: false,
  backgroundTrayExpanded: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: 'claude',
  codexBinaryPath: 'codex',
  claudeEnabled: true,
  codexEnabled: true,
  // Thread defaults mirror internal/settings.DefaultSettings so a fresh
  // frontend seeing no backend response still picks the correct seed.
  // defaultMode is legacy; CreateThread starts in chat unless explicitly
  // overridden by a caller.
  defaultMode: 'chat',
  defaultRuntimeMode: 'full-access',
  defaultThreadEnvMode: 'local',
  worktreeBranchPrefix: 'ao-',
  defaultReasoningEffort: 'high',
  defaultFastMode: false,
  defaultContextWindow: 1000000,
  // Text generation defaults mirror internal/settings.DefaultSettings.
  textGenerationProvider: 'codex',
  textGenerationModel: '',
  textGenerationReasoningEffort: 'low',
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
      settings = {
        ...DEFAULT_SETTINGS,
        ...result,
        modelContextWindows: result.modelContextWindows ?? {},
      } as Settings;
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
      settings = {
        ...DEFAULT_SETTINGS,
        ...result,
        modelContextWindows: result.modelContextWindows ?? {},
      } as Settings;
    }
  } catch (err) {
    console.error('Failed to update setting:', err);
    settings = previous;
    addToast('error', 'Failed to save setting');
  }
}
