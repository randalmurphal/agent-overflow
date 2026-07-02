import type { Settings } from '../types/settings';

// Hidden-model lookups for the picker surfaces. Hiding is display-only
// and mirrors internal/settings.HiddenModelsForProvider: the Claude
// list covers both `claude` and `claude-tui` (one binary, one catalog),
// unknown providers hide nothing. Capability/effort lookups must keep
// using the full catalog — existing threads can ride a hidden model.

export type HiddenModelsSettingsKey = 'claudeHiddenModels' | 'codexHiddenModels';

/**
 * Settings key holding the hide-list for a provider, or null for
 * providers without one. Single owner of the provider→key routing —
 * both the read path (hiddenModelSlugs) and the settings-page write
 * path go through here.
 */
export function hiddenModelsSettingsKey(
  provider: string,
): HiddenModelsSettingsKey | null {
  switch (provider) {
    case 'claude':
    case 'claude-tui':
      return 'claudeHiddenModels';
    case 'codex':
      return 'codexHiddenModels';
    default:
      return null;
  }
}

/** Hidden-model slugs for a provider as a lookup set. */
export function hiddenModelSlugs(
  settings: Settings,
  provider: string,
): ReadonlySet<string> {
  const key = hiddenModelsSettingsKey(provider);
  return new Set(key ? (settings[key] ?? []) : []);
}

/** Whether the user hid the model from pickers in settings. */
export function isModelHidden(
  settings: Settings,
  provider: string,
  slug: string,
): boolean {
  return hiddenModelSlugs(settings, provider).has(slug);
}
