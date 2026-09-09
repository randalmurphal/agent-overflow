<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import {
    getProjectSortMode,
    setProjectSortMode,
    getShowProviderIcons,
    setShowProviderIcons,
    PROJECT_SORT_OPTIONS,
    type ProjectSortMode,
  } from '../../stores/sidebar.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import { SELECT_CLASS } from './styles';

  let iconSaveError = $state('');
</script>

<div class="settings-sections">
  <section class="flex flex-col gap-1">
    <SettingsField
      id="sidebar.provider-icons"
      label="Show provider icons"
      hint="Show the provider beside each thread title."
    >
      <ToggleSwitch
        checked={getShowProviderIcons()}
        ariaLabel="Toggle Provider Icons"
        onToggle={(value) => {
          iconSaveError = setShowProviderIcons(value)
            ? '' : 'Could not save this preference. It may be lost when the app closes.';
        }}
      />
    </SettingsField>
    {#if iconSaveError}
      <p role="alert" class="text-xs text-error">{iconSaveError}</p>
    {/if}

    <SettingsField
      id="sidebar.auto-pin"
      label="Auto-pin new threads"
      hint="Put a new thread on the front burner after its first message is sent."
    >
      <ToggleSwitch
        checked={getSettings().autoPinNewThreads}
        ariaLabel="Toggle Auto-Pin New Threads"
        onToggle={(value) => updateSetting('autoPinNewThreads', value)}
      />
    </SettingsField>

    <SettingsField
      id="sidebar.project-order"
      label="Project order"
      hint="Order projects by activity, creation time, or manual drag order."
      htmlFor="sidebar-project-order"
    >
      <select
        id="sidebar-project-order"
        value={getProjectSortMode()}
        onchange={(event) => setProjectSortMode(event.currentTarget.value as ProjectSortMode)}
        class={SELECT_CLASS}
      >
        {#each PROJECT_SORT_OPTIONS as option (option.value)}
          <option value={option.value}>{option.label}</option>
        {/each}
      </select>
    </SettingsField>
  </section>
</div>
