import type { Settings } from "../types/settings";
import { GetSettings, UpdateSettings } from "./bindings";
import { addToast } from "./toast.svelte";

const DEFAULT_SETTINGS: Settings = {
  theme: "system",
  timestampFormat: "locale",
  sansFont: "geist",
  monoFont: "geist",
  recentWorkspaces: [],
  diffWordWrap: false,
  streamingEnabled: true,
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: "claude",
  codexBinaryPath: "codex",
  claudeEnabled: true,
  codexEnabled: true,
  defaultThreadEnvMode: "local",
  worktreeBranchPrefix: "ao-",
  paneDensity: "compact",
  // Text generation defaults mirror internal/settings.DefaultSettings.
  textGenerationProvider: "codex",
  textGenerationModel: "",
  textGenerationReasoningEffort: "low",
  // Auto-compact thresholds default to 90% per provider per tier — same
  // value as the Go DefaultSettings so an unloaded settings store doesn't
  // disagree with what the backend would send back on first GetSettings.
  claudeAutoCompactStandardPercent: 90,
  claudeAutoCompactExtendedPercent: 90,
  codexAutoCompactStandardPercent: 90,
  codexAutoCompactExtendedPercent: 90,
  observabilityTracingEnabled: false,
  observabilityOtlpEndpoint: "",
  observabilityEventLogEnabled: false,
  // Phase E LAN-bind preference defaults to false — loopback is the
  // safe out-of-the-box behaviour. Toggling on through Settings →
  // Network rebinds the transport without restarting the app.
  network: { bindAll: false },
};

function defaultSettings(): Settings {
  return {
    ...DEFAULT_SETTINGS,
    recentWorkspaces: [...DEFAULT_SETTINGS.recentWorkspaces],
    network: { ...DEFAULT_SETTINGS.network },
  };
}

let settings: Settings = $state(defaultSettings());

export function getSettings(): Settings {
  return settings;
}

export async function loadSettings(): Promise<boolean> {
  try {
    const result = await GetSettings();
    if (result) {
      settings = {
        ...defaultSettings(),
        ...result,
      } as Settings;
    }
    return true;
  } catch (err) {
    console.error("Failed to load settings:", err);
    addToast("error", "Failed to load settings");
    return false;
  }
}

export async function updateSetting<K extends keyof Settings>(
  key: K,
  value: Settings[K],
): Promise<void> {
  await updateSettingsPatch({ [key]: value } as Partial<Settings>);
}

export async function updateSettingsPatch(
  patch: Partial<Settings>,
): Promise<void> {
  const previous = { ...settings };
  settings = { ...settings, ...patch };
  try {
    const result = await UpdateSettings(patch);
    if (result) {
      settings = {
        ...defaultSettings(),
        ...result,
      } as Settings;
    }
  } catch (err) {
    console.error("Failed to update setting:", err);
    settings = previous;
    addToast("error", "Failed to save setting");
  }
}

export function resetSettingsForTest(): void {
  settings = defaultSettings();
}
