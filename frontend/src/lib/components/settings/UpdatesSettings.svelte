<script lang="ts">
  // Settings → Updates. Renders the reactive updater store and drives the
  // check / download / restart flow. Every state-changing action is an explicit
  // button press — nothing downloads or installs on its own. The "Advanced"
  // version picker (rollback included) lives in VersionPicker.
  import SettingsHeader from './SettingsHeader.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import VersionPicker from './VersionPicker.svelte';
  import { PRIMARY_BUTTON_CLASS, SECONDARY_BUTTON_CLASS } from './styles';
  import {
    getUpdateState,
    isDownloadInFlight,
    runUpdateCheck,
    startUpdateDownload,
    restartForUpdate,
  } from '../../stores/updates.svelte';

  const s = getUpdateState();

  const checking = $derived(s.phase === 'checking');
  const downloading = $derived(isDownloadInFlight(s.phase));
  // The progress / restart block renders for ANY install in flight — the latest
  // flow and a by-tag rollback alike — so it can't hang off the latestVersion
  // card (a rollback while up-to-date has no latestVersion).
  const showActive = $derived(downloading || s.phase === 'ready');
  const progressPercent = $derived(
    s.total > 0 ? Math.min(100, Math.round((s.written / s.total) * 100)) : 0,
  );

  const phaseLabel = $derived.by(() => {
    switch (s.phase) {
      case 'downloading':
        return 'Downloading update…';
      case 'verifying':
        return 'Verifying…';
      case 'installing':
        return 'Installing…';
      default:
        return '';
    }
  });

  function formatMB(bytes: number): string {
    return (bytes / (1024 * 1024)).toFixed(1);
  }
</script>

<div class="flex flex-col gap-6">
  <SettingsHeader
    eyebrow="Software"
    title="Updates"
    description="Check for and install new versions of Agent Overflow. Nothing is downloaded or installed without your confirmation."
  />

  {#if !s.supported}
    <p class="text-[0.75rem] text-fg-muted">
      In-app updates aren’t available for this build.{#if s.currentVersion}
        You’re running version {s.currentVersion}.{/if}
    </p>
  {:else}
    <div
      class="flex items-center justify-between gap-4 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3"
    >
      <div class="flex flex-col gap-0.5">
        <span class="text-[0.8125rem] font-medium text-fg">Current version</span>
        <span class="text-[0.75rem] tabular-nums text-fg-muted">{s.currentVersion || 'unknown'}</span>
      </div>
      <button
        class={SECONDARY_BUTTON_CLASS}
        onclick={() => void runUpdateCheck()}
        disabled={checking || downloading || s.phase === 'ready'}
      >
        {checking ? 'Checking…' : 'Check for Updates'}
      </button>
    </div>

    {#if s.phase === 'up-to-date'}
      <p class="text-[0.75rem] text-fg-muted">You’re on the latest version.</p>
    {/if}

    {#if s.phase === 'error' && s.error}
      <SettingsCallout tone="error">{s.error}</SettingsCallout>
    {/if}

    {#if s.latestVersion}
      <section
        class="flex flex-col gap-3 rounded-[var(--radius-field)] border border-accent/30 bg-accent/5 px-4 py-3"
      >
        <div class="flex items-center justify-between gap-3">
          <div class="flex flex-col gap-0.5">
            <span class="text-[0.8125rem] font-semibold text-fg">
              Version {s.latestVersion} available
            </span>
            {#if s.releaseName && s.releaseName !== s.latestVersion}
              <span class="text-[0.71875rem] text-fg-muted">{s.releaseName}</span>
            {/if}
          </div>

          {#if s.phase === 'available'}
            <button class={PRIMARY_BUTTON_CLASS} onclick={() => void startUpdateDownload()}>
              Download
            </button>
          {/if}
        </div>

        {#if s.releaseNotes}
          <details class="text-[0.71875rem] text-fg-muted">
            <summary class="cursor-pointer text-fg-subtle hover:text-fg">Release notes</summary>
            <pre
              class="mt-2 max-h-60 overflow-y-auto whitespace-pre-wrap font-sans leading-relaxed">{s.releaseNotes}</pre>
          </details>
        {/if}
      </section>
    {/if}

    {#if showActive}
      <section
        class="flex flex-col gap-3 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3"
      >
        {#if downloading}
          <div class="flex flex-col gap-1.5">
            <div class="flex items-center justify-between text-[0.6875rem] text-fg-muted">
              <span>{phaseLabel}</span>
              {#if s.phase === 'downloading' && s.total > 0}
                <span class="tabular-nums">{formatMB(s.written)} / {formatMB(s.total)} MB</span>
              {/if}
            </div>
            <div class="h-1.5 w-full overflow-hidden rounded-full bg-surface-2">
              <div
                class="h-full rounded-full bg-accent transition-[width] duration-150"
                style="width: {s.phase === 'downloading' ? progressPercent : 100}%"
              ></div>
            </div>
          </div>
        {/if}

        {#if s.phase === 'ready'}
          <div class="flex items-center justify-between gap-3">
            <p class="text-[0.6875rem] text-fg-muted">
              The update is ready. Agent Overflow will restart to finish installing.
            </p>
            <button class={PRIMARY_BUTTON_CLASS} onclick={() => void restartForUpdate()}>
              Restart to update
            </button>
          </div>
        {/if}
      </section>
    {/if}

    <!-- Hidden during an active install so a second, conflicting install can't
         be started; canInstallSelected() is the in-flight guard otherwise. -->
    {#if !showActive}
      <VersionPicker />
    {/if}
  {/if}
</div>
