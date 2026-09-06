// Component tests use a settings snapshot to arrange BOTH the frontend's
// preferences and the computer's configuration. Production loadSettings only
// migrates preferences once; later server reads must never replace local UI.
import { loadSettings } from '../../lib/stores/settings.svelte';
import { resetFrontendPreferencesForTest } from '../../lib/stores/frontendPreferences.svelte';

export function loadSettingsFixture(): Promise<boolean> {
  resetFrontendPreferencesForTest({});
  return loadSettings();
}
