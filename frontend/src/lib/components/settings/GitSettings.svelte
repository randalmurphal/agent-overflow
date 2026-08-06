<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import GitLabHostsSection from './GitLabHostsSection.svelte';

  let settings = $derived(getSettings());
</script>

<div class="flex flex-col gap-6">
  <section data-testid="settings-git-sync">
    <SettingsHeader title="Repository Sync" />
    <div class="flex flex-col gap-1">
      <SettingsField
        label="Fetch remotes in the background"
        hint={settings.backgroundGitFetch
          ? 'Runs `git fetch origin` for each project every few minutes so ahead/behind counts stay accurate. Never prunes, and never asks for credentials — a remote that needs them is skipped.'
          : 'Ahead/behind counts will only update when you fetch, pull, or sync a branch yourself.'}
      >
        <ToggleSwitch
          checked={settings.backgroundGitFetch}
          ariaLabel="Toggle Background Git Fetch"
          onToggle={(value) => updateSetting('backgroundGitFetch', value)}
        />
      </SettingsField>
    </div>
  </section>

  <GitLabHostsSection />
</div>
