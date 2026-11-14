<script lang="ts">
  // The auto-switch settings (docs/specs/account-auto-switch.md). They are
  // global, so each provider page renders the same controls under its own
  // field ids.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import type { AccountAutoSwitchStrategy } from '../../types/settings';
  import { clampToRange, settingRange } from '../../utils/settingRange';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import { providerFieldId, type SettingsProvider } from './fields';
  import { NUMBER_CLASS, SELECT_CLASS } from './styles';

  let { provider }: { provider: SettingsProvider } = $props();

  const THRESHOLD_RANGE = settingRange('accountAutoSwitchThreshold');

  const STRATEGY_HINTS: Record<AccountAutoSwitchStrategy, string> = {
    'soonest-reset': 'Uses up the account whose weekly limit resets soonest, so less quota expires unused.',
    'account-order': 'The order of the cards above.',
  };

  let settings = $derived(getSettings());
  let threshold = $derived(settings.accountAutoSwitchThreshold);

  // An entry that clamps back to the stored value writes nothing, so the
  // field is repainted by hand.
  function commitThreshold(el: HTMLInputElement): void {
    const next = clampToRange(el.value, threshold, THRESHOLD_RANGE);
    el.value = String(next);
    if (next !== threshold) void updateSetting('accountAutoSwitchThreshold', next);
  }
</script>

<div class="mt-4 flex flex-col gap-1" data-testid="settings-auto-switch-{provider}">
  <p class="text-[0.71875rem] leading-snug text-fg-hint">
    Auto-switch settings apply to the accounts of every provider.
  </p>

  <SettingsField
    id={providerFieldId(provider, 'auto-switch-strategy')}
    label="Use accounts in order of"
    hint={STRATEGY_HINTS[settings.accountAutoSwitchStrategy]}
    htmlFor="{provider}-auto-switch-strategy"
  >
    <select
      id="{provider}-auto-switch-strategy"
      data-testid="settings-auto-switch-strategy"
      value={settings.accountAutoSwitchStrategy}
      onchange={(e) =>
        updateSetting(
          'accountAutoSwitchStrategy',
          (e.currentTarget as HTMLSelectElement).value as AccountAutoSwitchStrategy,
        )}
      class={SELECT_CLASS}
    >
      <option value="soonest-reset">Soonest weekly reset</option>
      <option value="account-order">Card order</option>
    </select>
  </SettingsField>

  <SettingsField
    id={providerFieldId(provider, 'auto-switch-threshold')}
    label="Switch at"
    hint="Leave an account once any limit that applies to a running model reaches this. When every account is past it, the current one is used until the provider refuses it."
    htmlFor="{provider}-auto-switch-threshold"
  >
    <span class="flex items-center gap-1.5 whitespace-nowrap text-[0.6875rem] text-fg-muted">
      <input
        id="{provider}-auto-switch-threshold"
        type="number"
        data-testid="settings-auto-switch-threshold"
        min={THRESHOLD_RANGE[0]}
        max={THRESHOLD_RANGE[1]}
        step="1"
        value={threshold}
        onchange={(e) => commitThreshold(e.currentTarget as HTMLInputElement)}
        class={NUMBER_CLASS}
      />
      %
    </span>
  </SettingsField>

  <SettingsField
    id={providerFieldId(provider, 'auto-continue')}
    label="Continue threads after a usage limit"
    hint={'When a usage limit stops a thread, send it "Continue" once another account takes over or the limit resets.'}
  >
    <ToggleSwitch
      checked={settings.accountAutoContinue}
      ariaLabel="Continue threads after a usage limit"
      onToggle={(value) => updateSetting('accountAutoContinue', value)}
    />
  </SettingsField>
  <p
    class="-mt-1 text-[0.71875rem] leading-snug text-warning"
    data-testid="settings-auto-continue-warning"
  >
    This also restarts threads that agents spawned.
  </p>
</div>
