<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';

  let settings = $derived(getSettings());
</script>

<div class="settings-sections" data-testid="settings-agent-guide">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="agent-guide.enabled"
        label="Append the app guide"
        hint="Tell Claude and Codex what Agent Overflow renders and which built-in tools are on. Applies to sessions started after a change."
      >
        <ToggleSwitch
          checked={settings.agentGuideEnabled}
          ariaLabel="Toggle app guide"
          onToggle={(value) => updateSetting('agentGuideEnabled', value)}
        />
      </SettingsField>
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      The guide names only the tool servers that are on for the conversation. A running session keeps
      the prompt it started with until it is restarted.
    </p>
  </section>
</div>
