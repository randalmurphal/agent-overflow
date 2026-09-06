<script lang="ts">
  // One supervised machine on Settings → Updates: what it runs, what is
  // waiting, the flow while it runs, and how the last one ended. Every
  // state-changing action is a button press; the backend owns the flow and
  // this card mirrors its status frames (`stores/serviceUpdate.svelte.ts`).
  import SettingsCallout from './SettingsCallout.svelte';
  import VersionPicker from './VersionPicker.svelte';
  import Button from '../primitives/Button.svelte';
  import { PRIMARY_BUTTON_CLASS } from './styles';
  import { attachedBackendEntry, backendDisplayName } from '../../stores/attachedBackends.svelte';
  import {
    canInstallSelectedRelease,
    isServiceUpdateInFlight,
    loadServiceReleases,
    machineUpdate,
    requestServiceUpdate,
    cancelServiceUpdate,
    selectServiceRelease,
  } from '../../stores/serviceUpdate.svelte';
  import type { BackendKey } from '../../transport/backendKey';

  interface Props {
    key: BackendKey;
  }

  const { key }: Props = $props();

  const m = $derived(machineUpdate(key));
  const entry = $derived(attachedBackendEntry(key));
  const name = $derived(entry ? backendDisplayName(entry) : '');

  // Folded here so every template read below is total: the card renders
  // only for a machine whose status answered, but the store's box is
  // nullable and the guard rule (frontend/AGENTS.md) wants the null
  // handled in one place rather than optional-chained in each branch.
  const currentVersion = $derived(m.status?.currentVersion ?? '');
  const unavailable = $derived(m.status?.unavailable ?? '');
  const latestVersion = $derived(m.status?.latestVersion ?? '');
  const latestTag = $derived(m.status?.latestTag ?? '');
  const phase = $derived(m.status?.phase ?? 'idle');
  const flowError = $derived(phase === 'error' ? (m.status?.error ?? '') : '');
  const target = $derived(m.status?.targetVersion || m.status?.targetTag || '');
  const written = $derived(m.status?.written ?? 0);
  const total = $derived(m.status?.total ?? 0);

  const inFlight = $derived(m.requesting || isServiceUpdateInFlight(phase));
  const progressPercent = $derived(
    total > 0 ? Math.min(100, Math.round((written / total) * 100)) : 0,
  );
  // Resolving has nothing to measure yet and reads as empty; the steps
  // after the download are the in-app updater's convention, full.
  const barPercent = $derived(
    phase === 'downloading' ? progressPercent : phase === 'resolving' || m.requesting ? 0 : 100,
  );

  const phaseLabel = $derived.by(() => {
    if (m.requesting) return 'Starting…';
    switch (phase) {
      case 'resolving':
        return 'Resolving the release…';
      case 'downloading':
        return 'Downloading…';
      case 'verifying':
        return 'Verifying…';
      case 'staging':
        return 'Staging…';
      case 'waiting':
        return m.status?.waitingFor || 'Waiting for this computer to finish its work…';
      case 'requested':
        if (m.status?.error) return 'Restarting to check the update result…';
        return target ? `Restarting into version ${target}…` : 'Restarting…';
      default:
        return '';
    }
  });

  // Two versions, because a rollback is the case where they differ: `target`
  // is what the update was aiming at and `version` is what came back, and
  // naming only the second told the person their old version was the one
  // that failed. `target` is absent on a backend older than this bundle, and
  // there the running version is the closest true answer.
  const outcomeCopy = $derived.by(() => {
    const o = m.outcome;
    if (o === null) return null;
    if (o.outcome === 'committed') {
      return { tone: 'info' as const, text: `Updated to version ${o.version}.` };
    }
    const reason = o.reason ? ` ${o.reason}` : '';
    const target = o.target || o.version;
    return {
      tone: 'warn' as const,
      text: `The update to version ${target} was rolled back. Running ${o.version}.${reason}`,
    };
  });

  const canInstall = $derived(canInstallSelectedRelease(key));

  function formatMB(bytes: number): string {
    return (bytes / (1024 * 1024)).toFixed(1);
  }
</script>

<section
  class="flex flex-col gap-3 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0/40 px-4 py-3"
  data-testid="machine-update"
  data-backend={key}
>
  <div class="flex items-center justify-between gap-4">
    <div class="flex flex-col gap-0.5">
      <span class="text-[0.8125rem] font-medium text-fg">{name}</span>
      <span class="text-[0.75rem] tabular-nums text-fg-muted">
        {currentVersion ? `Running ${currentVersion}` : 'Version unknown'}
      </span>
    </div>
    {#if latestVersion && !inFlight && !unavailable}
      <button
        class={PRIMARY_BUTTON_CLASS}
        onclick={() => void requestServiceUpdate(key, latestTag)}
      >
        Update to {latestVersion}
      </button>
    {/if}
  </div>

  {#if m.loadError}
    <SettingsCallout tone="error">{m.loadError}</SettingsCallout>
  {/if}

  {#if unavailable}
    <p class="text-[0.75rem] text-fg-muted">{unavailable}</p>
  {:else}
    {#if m.requestError}
      <SettingsCallout tone="error">{m.requestError}</SettingsCallout>
    {/if}

    {#if flowError && !m.requesting}
      <SettingsCallout tone="error">{flowError}</SettingsCallout>
    {/if}

    {#if outcomeCopy !== null}
      <SettingsCallout tone={outcomeCopy.tone}>{outcomeCopy.text}</SettingsCallout>
    {/if}

    {#if phase === 'canceled'}
      <p class="text-sm text-fg-muted">Update canceled. This computer is still running {currentVersion}.</p>
    {/if}

    {#if inFlight}
      <div class="flex flex-col gap-1.5" data-testid="machine-update-progress">
        <div class="flex items-center justify-between text-[0.6875rem] text-fg-muted">
          <span>{phaseLabel}</span>
          {#if phase === 'downloading' && total > 0}
            <span class="tabular-nums">{formatMB(written)} / {formatMB(total)} MB</span>
          {/if}
        </div>
        <div class="h-1.5 w-full overflow-hidden rounded-full bg-surface-2">
          <div
            class="h-full rounded-full bg-accent transition-[width] duration-150"
            style="width: {barPercent}%"
          ></div>
        </div>
      </div>
      {#if m.status?.cancelable}
        <Button size="sm" variant="ghost" disabled={m.canceling} onclick={() => void cancelServiceUpdate(key)}>{m.canceling ? 'Canceling…' : 'Cancel update'}</Button>
      {/if}
    {:else}
      {#if !latestVersion && phase !== 'error' && phase !== 'canceled'}
        <p class="text-[0.75rem] text-fg-muted">Up to date.</p>
      {/if}

      <!-- Hidden during a flow so a second, conflicting install can't be
           started; canInstallSelectedRelease() is the guard otherwise. -->
      <VersionPicker
        versions={m.releases}
        loaded={m.releasesLoaded}
        loading={m.releasesLoading}
        error={m.releasesError}
        selectedTag={m.selectedTag}
        {canInstall}
        onOpen={() => void loadServiceReleases(key)}
        onSelect={(tag) => selectServiceRelease(key, tag)}
        onInstall={(tag) => void requestServiceUpdate(key, tag)}
      />
    {/if}
  {/if}
</section>
