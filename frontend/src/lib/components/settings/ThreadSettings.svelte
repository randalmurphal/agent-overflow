<script lang="ts">
  // Settings → Threads: what a new thread starts as, and which destructive
  // sidebar actions stop for a confirmation first.

  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import type { ThreadEnvMode } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { SELECT_CLASS } from './styles';

  let settings = $derived(getSettings());

  const ENV_OPTIONS: Array<{ value: ThreadEnvMode; label: string }> = [
    { value: 'local', label: 'Current checkout' },
    { value: 'worktree', label: 'New worktree' },
  ];
</script>

<div class="settings-sections">
  <section data-testid="settings-thread-defaults">
    <SettingsHeader
      title="New threads"
      description="New threads start in chat mode. Provider, model, effort, permissions, and context come from the composer controls."
    />
    <div class="flex flex-col gap-1">
      <SettingsField
        id="threads.default-environment"
        label="Default environment"
        hint="Workspace mode seeded on new draft threads."
        htmlFor="default-thread-env-mode"
      >
        <select
          id="default-thread-env-mode"
          data-testid="settings-default-thread-env-mode"
          value={settings.defaultThreadEnvMode}
          onchange={(e) =>
            updateSetting(
              'defaultThreadEnvMode',
              (e.target as HTMLSelectElement).value as ThreadEnvMode,
            )}
          class={SELECT_CLASS}
        >
          {#each ENV_OPTIONS as opt (opt.value)}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </SettingsField>

      <SettingsField
        id="threads.auto-pin"
        label="Auto-pin new threads"
        hint="Put a new thread on the front burner after its first message is sent."
      >
        <ToggleSwitch
          checked={settings.autoPinNewThreads}
          ariaLabel="Toggle Auto-Pin New Threads"
          onToggle={(value) => updateSetting('autoPinNewThreads', value)}
        />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader
      title="Safety checks"
      description="Which destructive sidebar actions stop for confirmation."
    />
    <div class="flex flex-col gap-1">
      <SettingsField
        id="threads.confirm-archive"
        label="Confirm before archive"
        hint="Show a confirmation dialog when archiving threads."
      >
        <ToggleSwitch
          checked={settings.confirmArchive}
          ariaLabel="Toggle Archive Confirmation"
          onToggle={(value) => updateSetting('confirmArchive', value)}
        />
      </SettingsField>

      <SettingsField
        id="threads.confirm-delete"
        label="Confirm before delete"
        hint="Show a confirmation dialog when deleting threads."
      >
        <ToggleSwitch
          checked={settings.confirmDelete}
          ariaLabel="Toggle Delete Confirmation"
          onToggle={(value) => updateSetting('confirmDelete', value)}
        />
      </SettingsField>
    </div>
  </section>
</div>
