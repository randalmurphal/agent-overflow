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

/**
 * The models a picker for `provider` offers: the catalog minus the hidden
 * slugs, except `keepSlug` (a thread already riding a hidden model keeps its
 * checkmark row, so the picker never shows "nothing selected").
 *
 * The settings UI refuses to hide the last visible model, but a hand-edited
 * settings.json — or a second connected client — can still hide everything;
 * that falls back to the full catalog rather than presenting an empty picker,
 * the same backstop as the Go seed path's firstVisibleModel.
 *
 * Shared by the model submenu and the composer's `/model <arg>` resolver so
 * the two cannot disagree about which models exist.
 */
export function pickerVisibleModels<T extends { slug: string }>(
  settings: Settings,
  provider: string,
  models: readonly T[],
  keepSlug?: string,
): T[] {
  const hidden = hiddenModelSlugs(settings, provider);
  if (hidden.size === 0) return [...models];
  const visible = models.filter(
    (model) => !hidden.has(model.slug) || (keepSlug !== undefined && model.slug === keepSlug),
  );
  return visible.length === 0 ? [...models] : visible;
}

/** Whether the user hid the model from pickers in settings. */
export function isModelHidden(
  settings: Settings,
  provider: string,
  slug: string,
): boolean {
  return hiddenModelSlugs(settings, provider).has(slug);
}
