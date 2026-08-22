import type { Settings } from "../types/settings";
import { GetSettings, UpdateSettings } from "./bindings";
import { addToast } from "./toast.svelte";

const DEFAULT_SETTINGS: Settings = {
  timestampFormat: "locale",
  sansFont: "geist",
  monoFont: "geist",
  fontSize: 13,
  recentWorkspaces: [],
  diffWordWrap: false,
  collapseDiffPreviews: false,
  streamingEnabled: true,
  lowPowerMode: false,
  // Spinner defaults mirror internal/settings.DefaultSettings: verbs on
  // (text-only), animations off (the LED chase is stock), compaction
  // slot "" = the built-in robo-papers default.
  spinnerVerbsEnabled: true,
  spinnerAnimationsEnabled: false,
  spinnerCustomVerbs: [],
  spinnerBuiltinVerbsDisabled: false,
  spinnerDisabledAnimations: [],
  spinnerCompactionAnimation: "",
  confirmArchive: true,
  confirmDelete: true,
  claudeBinaryPath: "claude",
  codexBinaryPath: "codex",
  claudeEnabled: true,
  codexEnabled: true,
  // Off by default, mirroring internal/settings.Settings.ClaudeTUIEnabled's
  // deliberate zero-value default: an unloaded store must not flash the TUI
  // into the pickers before GetSettings answers.
  claudeTuiEnabled: false,
  claudeHiddenModels: [],
  codexHiddenModels: [],
  defaultThreadEnvMode: "local",
  worktreeBranchPrefix: "ao-",
  paneDensity: "compact",
  activityRunDefault: "expanded",
  activityRunWindowRows: 30,
  // Text generation defaults mirror internal/settings.DefaultSettings.
  textGenerationProvider: "codex",
  textGenerationModel: "",
  textGenerationReasoningEffort: "low",
  commitMessageStyle: "conventional",
  commitMessageStyleCustom: "",
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
  // 30-day retention mirrors internal/settings.DefaultSettings so a
  // fresh frontend boot before GetSettings returns shows the right
  // default in the GeneralSettings input.
  retention: { days: 30 },
  // On by default, mirroring internal/settings.DefaultSettings: the
  // behind badge is only meaningful if something refreshes it.
  backgroundGitFetch: true,
  // Empty allowlist — only gitlab.com / github.com are recognised by
  // default. Users add self-hosted entries through the Settings UI.
  gitlabSelfHostedHosts: [],
  // No custom provider environment out of the box; the backend omits the
  // keys entirely until the user adds one.
  claudeCustomEnv: [],
  codexCustomEnv: [],
  projectSortMode: "lastActivity",
  usagePeriod: "month",
  workflowPaused: false,
};

function defaultSettings(): Settings {
  return {
    ...DEFAULT_SETTINGS,
    recentWorkspaces: [...DEFAULT_SETTINGS.recentWorkspaces],
    network: { ...DEFAULT_SETTINGS.network },
    retention: { ...DEFAULT_SETTINGS.retention },
    gitlabSelfHostedHosts: [...DEFAULT_SETTINGS.gitlabSelfHostedHosts],
    claudeHiddenModels: [...(DEFAULT_SETTINGS.claudeHiddenModels ?? [])],
    codexHiddenModels: [...(DEFAULT_SETTINGS.codexHiddenModels ?? [])],
    claudeCustomEnv: [...(DEFAULT_SETTINGS.claudeCustomEnv ?? [])],
    codexCustomEnv: [...(DEFAULT_SETTINGS.codexCustomEnv ?? [])],
    spinnerCustomVerbs: [...(DEFAULT_SETTINGS.spinnerCustomVerbs ?? [])],
    spinnerDisabledAnimations: [...(DEFAULT_SETTINGS.spinnerDisabledAnimations ?? [])],
  };
}

function mergeSettingsWithDefaults(result: Partial<Settings>): Settings {
  const defaults = defaultSettings();
  return {
    ...defaults,
    ...result,
    recentWorkspaces: result.recentWorkspaces
      ? [...result.recentWorkspaces]
      : defaults.recentWorkspaces,
    network: {
      ...defaults.network,
      ...result.network,
    },
    retention: {
      ...defaults.retention,
      ...result.retention,
    },
    gitlabSelfHostedHosts: result.gitlabSelfHostedHosts
      ? [...result.gitlabSelfHostedHosts]
      : defaults.gitlabSelfHostedHosts,
    claudeHiddenModels: result.claudeHiddenModels
      ? [...result.claudeHiddenModels]
      : defaults.claudeHiddenModels,
    codexHiddenModels: result.codexHiddenModels
      ? [...result.codexHiddenModels]
      : defaults.codexHiddenModels,
    claudeCustomEnv: result.claudeCustomEnv
      ? [...result.claudeCustomEnv]
      : defaults.claudeCustomEnv,
    codexCustomEnv: result.codexCustomEnv
      ? [...result.codexCustomEnv]
      : defaults.codexCustomEnv,
    spinnerCustomVerbs: result.spinnerCustomVerbs
      ? [...result.spinnerCustomVerbs]
      : defaults.spinnerCustomVerbs,
    spinnerDisabledAnimations: result.spinnerDisabledAnimations
      ? [...result.spinnerDisabledAnimations]
      : defaults.spinnerDisabledAnimations,
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
      settings = mergeSettingsWithDefaults(result as Partial<Settings>);
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

/**
 * Serializes the UpdateSettings round trips. The transport dispatches RPCs
 * concurrently per connection, and a single gesture can issue two DEPENDENT
 * whole-list writes — Settings → Prompts & Tools commits a textarea on blur
 * and then handles the click of the button that took the focus, each sending
 * the entire override list. Landed out of order, the earlier list is what
 * persists AND its full-snapshot answer overwrites the store, so the typing
 * disappears with no error anywhere. Only the RPC and the merge of its answer
 * are ordered; the optimistic write stays synchronous at call time.
 *
 * Null means idle, and an idle queue dispatches STRAIGHT AWAY rather than off
 * a microtask: a lone settings write is the overwhelmingly common case and
 * has nothing to be ordered against, so it should not start a turn later than
 * it used to.
 */
let updateQueue: Promise<void> | null = null;

function pickKeys(
  source: Settings,
  keys: readonly (keyof Settings)[],
): Partial<Settings> {
  const picked: Record<string, unknown> = {};
  // An absent optional key is picked as undefined ON PURPOSE: restoring it
  // has to re-erase the field, not leave the failed patch's value behind.
  for (const key of keys) picked[key] = source[key];
  return picked as Partial<Settings>;
}

export async function updateSettingsPatch(
  patch: Partial<Settings>,
): Promise<void> {
  // Captured against what this gesture actually replaced, before the RPC is
  // queued — the store's state when the answer arrives may include another
  // gesture's optimistic write, which is not ours to undo.
  const replaced = pickKeys(settings, Object.keys(patch) as (keyof Settings)[]);
  settings = { ...settings, ...patch };

  // sendSettingsPatch handles its own failure and never rejects, so a failed
  // write cannot poison the queue for every later one — which is also why the
  // idle reset below can be a plain `then`.
  const ahead = updateQueue;
  const run: Promise<void> = ahead
    ? ahead.then(() => sendSettingsPatch(patch, replaced))
    : sendSettingsPatch(patch, replaced);
  updateQueue = run;
  void run.then(() => {
    if (updateQueue === run) updateQueue = null;
  });
  return run;
}

async function sendSettingsPatch(
  patch: Partial<Settings>,
  replaced: Partial<Settings>,
): Promise<void> {
  try {
    const result = await UpdateSettings(patch);
    if (result) {
      settings = mergeSettingsWithDefaults(result as Partial<Settings>);
    }
  } catch (err) {
    console.error("Failed to update setting:", err);
    // Only the keys this patch wrote. Restoring the whole pre-patch snapshot
    // reverts every optimistic write made since, including ones whose own
    // RPC is still queued behind this one.
    settings = { ...settings, ...replaced };
    addToast("error", "Failed to save setting");
  }
}

/**
 * Re-seeds the store from a full Settings snapshot returned by a dedicated
 * mutator (the custom-environment CRUD). Those bindings return the same
 * redacted shape GetSettings does, so the store stays consistent without a
 * second round trip — and unlike updateSettingsPatch there is no optimistic
 * pre-write, because the backend is the only side that knows what the
 * validated, deduped list looks like.
 */
export function applySettingsSnapshot(result: Partial<Settings>): void {
  settings = mergeSettingsWithDefaults(result);
}

export function resetSettingsForTest(): void {
  settings = defaultSettings();
  // The queue is module state too: a test whose RPC never settles would
  // otherwise wedge every write in every test that follows it.
  updateQueue = null;
}
