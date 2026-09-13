// A settings page captures its computer in context. Switching a thread or
// opening another pane cannot redirect an edit; switching the selector remounts
// the page, including its forms and any in-flight operation's captured target.
import { getContext, setContext } from 'svelte';
import type { Settings } from '../../types/settings';
import * as settings from '../../stores/settings.svelte';
import { HOME_BACKEND, type BackendKey } from '../../transport/backendKey';
import { withBackendTarget } from '../../transport/backends';
import { passkeysUsable } from '../../transport/passkey';
import { STEP_UP_REFUSAL } from '../../transport/scopeRefusal';
import { hasScope, type Scope } from '../../transport/scopes';

/**
 * The reason a host-tier control shows while it is inert. The step-up
 * sentence without the passkey remedy, because `hostTierWritable` is true
 * whenever a passkey could satisfy the write.
 */
export const HOST_TIER_REASON = STEP_UP_REFUSAL.title;

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
    /**
     * Whether a host-tier key (internal/settings/tier.go) can be written
     * from this page. The backend takes such a write only under a step-up
     * proof (internal/app/app_authz.go requireSettingsTier): host presence,
     * or a passkey assertion the transport runs on refusal
     * (transport/stepUp.ts). A page with neither disables the control with
     * `HOST_TIER_REASON` instead of offering a write that is refused after
     * the fact. Reactive on the scope subscription when read from a
     * `$derived`.
     */
    hostTierWritable: () => hasScope('host', backend) || passkeysUsable(),
    getSettings: () => settings.getSettings(backend),
    updateSetting: <K extends keyof Settings>(key: K, value: Settings[K]) =>
      settings.updateSetting(key, value, backend),
    updateSettingsPatch: (patch: Partial<Settings>) => settings.updateSettingsPatch(patch, backend),
    applySettingsSnapshot: (value: Partial<Settings>) => settings.applySettingsSnapshot(value, backend),
  };
}
