<script lang="ts">
  // The ceiling on a Claude command's timeout, in hours. The backend exports
  // it as BASH_MAX_TIMEOUT_MS at spawn on both Claude transports, unless the
  // provider's custom environment names that variable, which then wins; the
  // row says so rather than letting the number read as applied.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSettingsPatch } = settingsComputer();
  import {
    clampCommandTimeoutHours,
    COMMAND_TIMEOUT_ENV,
    COMMAND_TIMEOUT_HOURS_MAX,
    COMMAND_TIMEOUT_HOURS_MIN,
  } from '../../utils/claudeCommandTimeout';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS } from './styles';

  let settings = $derived(getSettings());
  let hours = $derived(settings.claudeMaxCommandTimeoutHours);
  let overridden = $derived(
    (settings.claudeCustomEnv ?? []).some((entry) => entry.name === COMMAND_TIMEOUT_ENV),
  );

  function commit(el: HTMLInputElement): void {
    const next = clampCommandTimeoutHours(el.value, hours);
    // An entry that clamps back to the stored value writes nothing, so the
    // field is repainted by hand.
    el.value = String(next);
    if (next === hours) return;
    void updateSettingsPatch({ claudeMaxCommandTimeoutHours: next });
  }
</script>

<div class="flex flex-col gap-1" data-testid="settings-claude-command-timeout-row">
  <SettingsField
    id="claude.command-timeout"
    label="Command timeout"
    hint="The longest a command may run when Claude asks for a long timeout, including background commands, which otherwise stop after 30 minutes."
    htmlFor="claude-command-timeout"
  >
    <span class="flex items-center gap-1.5 whitespace-nowrap text-[0.6875rem] text-fg-muted">
      <input
        id="claude-command-timeout"
        type="number"
        data-testid="settings-claude-command-timeout"
        min={COMMAND_TIMEOUT_HOURS_MIN}
        max={COMMAND_TIMEOUT_HOURS_MAX}
        step="1"
        value={hours}
        onchange={(e) => commit(e.currentTarget as HTMLInputElement)}
        class="{INPUT_CLASS} w-16 text-right tabular-nums"
      />
      hours
    </span>
  </SettingsField>
  {#if overridden}
    <p
      class="-mt-1 text-[0.6875rem] leading-snug text-fg-hint"
      data-testid="settings-claude-command-timeout-overridden"
    >
      {COMMAND_TIMEOUT_ENV} in Environment overrides this.
    </p>
  {/if}
</div>
