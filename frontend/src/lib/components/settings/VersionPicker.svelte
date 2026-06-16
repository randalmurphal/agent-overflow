<script lang="ts">
  // Settings → Updates → Advanced: pick and install a specific version,
  // including an older one (rollback). The release list lazy-loads the first
  // time the disclosure opens; everything else is driven by the updates store.
  import SettingsCallout from './SettingsCallout.svelte';
  import { PRIMARY_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import {
    getUpdateState,
    loadVersions,
    selectVersion,
    selectedVersion,
    canInstallSelected,
    startUpdateDownload,
    type ReleaseSummary,
  } from '../../stores/updates.svelte';

  const s = getUpdateState();

  const selected = $derived(selectedVersion());
  const canInstall = $derived(canInstallSelected());

  function onToggle(e: Event & { currentTarget: EventTarget & HTMLDetailsElement }): void {
    // Lazy-load the first time the disclosure opens; cached for later opens.
    if (e.currentTarget.open && !s.versionsLoaded && !s.versionsLoading) {
      void loadVersions();
    }
  }

  function versionLabel(v: ReleaseSummary): string {
    const marks: string[] = [];
    if (v.isLatest) marks.push('latest');
    if (v.isCurrent) marks.push('current');
    else if (v.isOlder) marks.push('older');
    if (v.prerelease) marks.push('pre-release');
    return marks.length ? `${v.tag} — ${marks.join(', ')}` : v.tag;
  }
</script>

<details
  class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3"
  ontoggle={onToggle}
>
  <summary class="cursor-pointer text-[0.75rem] font-medium text-fg-subtle hover:text-fg">
    Install a specific version
  </summary>

  <div class="mt-3 flex flex-col gap-3">
    {#if s.versionsLoading}
      <p class="text-[0.75rem] text-fg-muted">Loading available versions…</p>
    {:else if s.versionsError}
      <SettingsCallout tone="error">{s.versionsError}</SettingsCallout>
    {:else if s.versionsLoaded && s.availableVersions.length === 0}
      <p class="text-[0.75rem] text-fg-muted">
        No installable versions were found for this platform.
      </p>
    {:else if s.versionsLoaded}
      <div class="flex items-end justify-between gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-[0.6875rem] uppercase tracking-wide text-fg-hint">Version</span>
          <select
            class={SELECT_CLASS}
            value={s.selectedTag}
            onchange={(e) => selectVersion(e.currentTarget.value)}
          >
            {#each s.availableVersions as v (v.tag)}
              <option value={v.tag}>{versionLabel(v)}</option>
            {/each}
          </select>
        </label>

        <button
          class={PRIMARY_BUTTON_CLASS}
          disabled={!canInstall}
          onclick={() => void startUpdateDownload(s.selectedTag)}
        >
          Install this version
        </button>
      </div>

      {#if selected?.isOlder}
        <SettingsCallout tone="warn">
          Version {selected.version} is older than the build you’re running. Downgrading can lose
          data or settings written by the newer version. Proceed only if you understand the risk.
        </SettingsCallout>
      {:else if selected?.isCurrent}
        <p class="text-[0.71875rem] text-fg-muted">This is the version you’re currently running.</p>
      {/if}
    {/if}
  </div>
</details>
