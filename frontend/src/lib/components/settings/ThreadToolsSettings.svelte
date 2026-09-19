<script lang="ts">
  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting, hostTierWritable } = settingsComputer();
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';

  let settings = $derived(getSettings());
  // Host tier, for the reason the browser keys are: the switch grants a
  // provider session on this machine authority over this computer's other
  // threads (internal/settings/tier.go).
  let hostWritable = $derived(hostTierWritable());
  let hostReason = $derived(hostWritable ? undefined : HOST_TIER_REASON);
</script>

<div class="settings-sections" data-testid="settings-thread-tools">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="thread-tools.enabled"
        label="Built-in thread tools"
        hint="Let Claude and Codex read, start and answer this computer's other conversations."
      >
        <ToggleSwitch
          checked={settings.threadToolsEnabled}
          disabled={!hostWritable}
          title={hostReason}
          ariaLabel="Toggle Built-in Thread Tools"
          onToggle={(value) => updateSetting('threadToolsEnabled', value)}
        />
      </SettingsField>
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      This says nothing about who may reach this computer's threads. Pairing already says which
      computers belong to you, and a paired computer's agents reach this one whether the switch is on
      or off. A single conversation can be opted out in its MCP menu.
    </p>
  </section>
</div>
