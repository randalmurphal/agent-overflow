import type { Settings } from "../types/settings";
import { SETTINGS_DEFAULTS } from "../generated/settingsDefaults";

/**
 * The defaults are GENERATED from internal/settings.DefaultSettings
 * (`go generate ./internal/settings`), never hand-mirrored: the two used
 * to be kept in step by comments, which is a synchronization method with
 * no failure mode short of a user noticing the wrong value. Which fields
 * get a default and which stay undefined is the generator's deny-list.
 *
 * They are load-bearing at runtime, not just a pre-load placeholder: Go's
 * `omitempty` drops zero-valued fields on the wire, so every GetSettings
 * read comes back missing keys that mergeSettingsWithDefaults fills from
 * here.
 */
export function defaultSettings(): Settings {
  // Deep-copied per call: the store mutates what it hands out, and the
  // generated object is module-level shared state.
  return {
    ...SETTINGS_DEFAULTS,
    recentWorkspaces: [...SETTINGS_DEFAULTS.recentWorkspaces],
    network: { ...SETTINGS_DEFAULTS.network },
    retention: { ...SETTINGS_DEFAULTS.retention },
    gitlabSelfHostedHosts: [...SETTINGS_DEFAULTS.gitlabSelfHostedHosts],
    claudeHiddenModels: [...SETTINGS_DEFAULTS.claudeHiddenModels],
    codexHiddenModels: [...SETTINGS_DEFAULTS.codexHiddenModels],
    claudeCustomEnv: [...SETTINGS_DEFAULTS.claudeCustomEnv],
    codexCustomEnv: [...SETTINGS_DEFAULTS.codexCustomEnv],
    spinnerCustomVerbs: [...SETTINGS_DEFAULTS.spinnerCustomVerbs],
    spinnerDisabledAnimations: [...SETTINGS_DEFAULTS.spinnerDisabledAnimations],
  };
}

// The generated bindings return the wails model CLASS, whose field
// declarations materialize every wire-omitted optional key as an OWN
// property holding undefined. Spread copies own properties whether or
// not they hold a value, so without this strip an omitted key STOMPS
// its default with undefined instead of leaving it in place — which is
// how the untouched compaction-sprite slot ("" on our side, omitted by
// Go's omitempty) reached the resolver as undefined and fell through to
// the random pool (field bug 2026-08-22). Applies to the nested model
// classes too.
function withoutUndefined<T extends object>(value: T): T {
  const out: Record<string, unknown> = {};
  for (const [key, entry] of Object.entries(value)) {
    if (entry !== undefined) out[key] = entry;
  }
  return out as T;
}

export function mergeSettingsWithDefaults(raw: Partial<Settings>): Settings {
  const defaults = defaultSettings();
  const result = withoutUndefined(raw);
  if (result.network) result.network = withoutUndefined(result.network);
  if (result.retention) result.retention = withoutUndefined(result.retention);
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
