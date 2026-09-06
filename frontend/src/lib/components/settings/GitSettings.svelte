<script lang="ts">
  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting } = settingsComputer();
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import GitLabHostsSection from './GitLabHostsSection.svelte';
  import { INPUT_CLASS } from './styles';

  let settings = $derived(getSettings());
</script>

<div class="settings-sections">
  <section data-testid="settings-git-sync">
    <SettingsHeader title="Repository sync" />
    <div class="flex flex-col gap-1">
      <SettingsField
        id="git.background-fetch"
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

  <section data-testid="settings-git-worktrees">
    <SettingsHeader title="Worktrees" />
    <div class="flex flex-col gap-1">
      <SettingsField
        id="git.worktree-branch-prefix"
        label="Worktree branch prefix"
        hint="Prefix for generated worktree branches."
        htmlFor="worktree-branch-prefix"
      >
        <input
          id="worktree-branch-prefix"
          data-testid="settings-worktree-branch-prefix"
          type="text"
          value={settings.worktreeBranchPrefix}
          onblur={(e) =>
            updateSetting(
              'worktreeBranchPrefix',
              (e.target as HTMLInputElement).value,
            )}
          class="{INPUT_CLASS} max-w-[12rem]"
        />
      </SettingsField>
    </div>
  </section>

  <GitLabHostsSection />
</div>
