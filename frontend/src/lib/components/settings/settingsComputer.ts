// A settings page captures its computer in context. Switching a thread or
// opening another pane cannot redirect an edit; switching the selector remounts
// the page, including its forms and any in-flight operation's captured target.
import { getContext, setContext } from 'svelte';
import type { Settings } from '../../types/settings';
import * as settings from '../../stores/settings.svelte';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import { withBackendTarget } from '../../transport/backends';
import { hasScope, type Scope } from '../../transport/scopes';

const COMPUTER = Symbol('settings-computer');
export function provideSettingsComputer(backend: BackendKey): void {
  setContext(COMPUTER, backend);
}

export function settingsComputer() {
  const backend = getContext<BackendKey | undefined>(COMPUTER) ?? HOME_BACKEND;
  return {
    backend,
    call: <T>(operation: () => T): T => withBackendTarget(backend, operation),
    hasScope: (scope: Scope) => hasScope(scope, backend),
    getSettings: () => settings.getSettings(backend),
    updateSetting: <K extends keyof Settings>(key: K, value: Settings[K]) =>
      settings.updateSetting(key, value, backend),
    updateSettingsPatch: (patch: Partial<Settings>) => settings.updateSettingsPatch(patch, backend),
    applySettingsSnapshot: (value: Partial<Settings>) => settings.applySettingsSnapshot(value, backend),
  };
}
