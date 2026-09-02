<script lang="ts">
  // Settings → Updates → Advanced: pick and install a specific version,
  // including an older one (rollback). The release list lazy-loads the first
  // time the disclosure opens.
  //
  // Prop-driven, because two things install a picked version: the in-app
  // updater for the build this page runs inside (`UpdatesSettings`), and a
  // supervised machine's update flow over the wire (`MachineUpdateCard`),
  // one per attached machine. The list, the pick and the install go to
  // whichever store the host wired; the disclosure and the copy are shared.
  import SettingsCallout from './SettingsCallout.svelte';
  import { PRIMARY_BUTTON_CLASS, SELECT_CLASS } from './styles';
  import type { ReleaseSummary } from '../../stores/bindings';

  interface Props {
    versions: readonly ReleaseSummary[];
    loaded: boolean;
    loading: boolean;
    error: string;
    selectedTag: string;
    canInstall: boolean;
    /** Fired when the disclosure opens; the host loads the list here, once. */
    onOpen: () => void;
    onSelect: (tag: string) => void;
    onInstall: (tag: string) => void;
  }

  const {
    versions,
    loaded,
    loading,
    error,
    selectedTag,
    canInstall,
    onOpen,
    onSelect,
    onInstall,
  }: Props = $props();

  const selected = $derived(versions.find((v) => v.tag === selectedTag));

  function onToggle(e: Event & { currentTarget: EventTarget & HTMLDetailsElement }): void {
    // Lazy-load the first time the disclosure opens; cached for later opens.
    if (e.currentTarget.open && !loaded && !loading) onOpen();
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
    {#if loading}
      <p class="text-[0.75rem] text-fg-muted">Loading available versions…</p>
    {:else if error}
      <SettingsCallout tone="error">{error}</SettingsCallout>
    {:else if loaded && versions.length === 0}
      <p class="text-[0.75rem] text-fg-muted">
        No installable versions were found for this platform.
      </p>
    {:else if loaded}
      <div class="flex items-end justify-between gap-3">
        <label class="flex flex-col gap-1">
          <span class="text-[0.6875rem] uppercase tracking-wide text-fg-hint">Version</span>
          <select
            class={SELECT_CLASS}
            value={selectedTag}
            onchange={(e) => onSelect(e.currentTarget.value)}
          >
            {#each versions as v (v.tag)}
              <option value={v.tag}>{versionLabel(v)}</option>
            {/each}
          </select>
        </label>

        <button
          class={PRIMARY_BUTTON_CLASS}
          disabled={!canInstall}
          onclick={() => onInstall(selectedTag)}
        >
          Install this version
        </button>
      </div>

      {#if selected?.isOlder}
        <SettingsCallout tone="warn">
          Version {selected.version} is older than the one installed. Downgrading can lose data or
          settings written by the newer version. Proceed only if you understand the risk.
        </SettingsCallout>
      {:else if selected?.isCurrent}
        <p class="text-[0.71875rem] text-fg-muted">This is the version currently installed.</p>
      {/if}
    {/if}
  </div>
</details>
