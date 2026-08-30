import { SETTINGS_DEFAULTS } from '../../lib/generated/settingsDefaults';
import type { Settings } from '../../lib/types/settings';

/**
 * A full Settings object holding the SHIPPED defaults, generated from
 * internal/settings.DefaultSettings. A test that wants a non-default value
 * (claude-tui offered, spinner animations on, …) says so in `overrides`.
 *
 * No `theme` field: the light/dark mode moved out of settings into
 * `stores/appearance.svelte.ts` (docs/architecture/theme-system.md §6.2).
 */
export function makeSettings(overrides: Partial<Settings> = {}): Settings {
  return {
    ...SETTINGS_DEFAULTS,
    // Deep-copied: a test that pushes onto one of these lists must not
    // mutate the shared generated object for every case after it.
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
    ...overrides,
  };
}
