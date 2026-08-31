<script lang="ts">
  // Per-project settings. Today that is one thing — the worktree setup recipe
  // — so the section is a project picker over a single editor rather than a
  // nested tab set.
  //
  // The binding behind the editor carries //ao:stepup (it configures argv
  // commands that run unattended on every worktree this project cuts), so no
  // standing grant reaches it from a remote session. The picker says so instead of
  // rendering an editor whose every save would fail.

  import { onMount } from 'svelte';
  import { getProjectLabelText, getProjects, isLoaded, refreshProjects } from '../../stores/projects.svelte';
  import { hasScope } from '../../transport/scopes';
  import SettingsHeader from './SettingsHeader.svelte';
  import SettingsField from './SettingsField.svelte';
  import WorktreeSetupEditor from './WorktreeSetupEditor.svelte';
  import { SELECT_CLASS } from './styles';

  let projects = $derived(getProjects());
  let loaded = $derived(isLoaded());
  // The editable half is the worktree setup command, which runs in a PTY.
  let ungranted = $derived(!hasScope('terminal:operate'));
  let selectedId = $state('');

  onMount(() => {
    if (!isLoaded()) void refreshProjects();
  });

  // Keep the selection valid without stealing it: only fall back to the first
  // project when the current pick is gone (or was never made).
  $effect(() => {
    if (projects.length === 0) {
      selectedId = '';
      return;
    }
    if (!projects.some((p) => p.project.id === selectedId)) {
      selectedId = projects[0].project.id;
    }
  });
</script>

<div class="space-y-6">
  <SettingsHeader
    title="Projects"
    description="Per-project configuration. Settings here apply to every thread and workflow run in the selected project."
  />

  {#if ungranted}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-projects-local-only">
      Project configuration is local only. Open Agent Overflow on the host machine to edit it.
    </p>
  {:else if loaded && projects.length === 0}
    <p class="text-[0.75rem] text-fg-muted" data-testid="settings-projects-empty">
      No projects yet. Add one from the sidebar first.
    </p>
  {:else}
    <SettingsField
      label="Project"
      hint="Choose which project these settings apply to."
      htmlFor="settings-projects-select"
    >
      <select
        id="settings-projects-select"
        data-testid="settings-projects-select"
        class={SELECT_CLASS}
        value={selectedId}
        onchange={(e) => (selectedId = (e.target as HTMLSelectElement).value)}
      >
        {#each projects as entry (entry.project.id)}
          <option value={entry.project.id}>{getProjectLabelText(entry.project.id)}</option>
        {/each}
      </select>
    </SettingsField>

    {#if selectedId}
      {#key selectedId}
        <WorktreeSetupEditor projectId={selectedId} />
      {/key}
    {/if}
  {/if}
</div>
