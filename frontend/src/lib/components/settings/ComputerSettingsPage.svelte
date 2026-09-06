<script lang="ts">
  import { onMount, untrack, type Component } from 'svelte';
  import type { BackendKey } from '../../transport/backendKey';
  import { loadSettings, hasComputerSettings } from '../../stores/settings.svelte';
  import { provideSettingsComputer } from './settingsComputer';
  import { PRIMARY_BUTTON_CLASS } from './styles';

  let { backend, Page, needsComputer, hasDeviceControls = false }: {
    backend: BackendKey; Page: Component; needsComputer: boolean; hasDeviceControls?: boolean;
  } = $props();
  const computer = untrack(() => backend);
  provideSettingsComputer(computer);
  let loading = $state(untrack(() => needsComputer && !hasComputerSettings(computer)));
  let failed = $state(false);
  async function load(): Promise<void> {
    loading = !hasComputerSettings(computer);
    failed = !await loadSettings(computer);
    loading = false;
  }
  onMount(() => { if (needsComputer) void load(); });
</script>

{#if loading && !hasDeviceControls}
  <p role="status" class="text-sm text-fg-muted">Loading computer settings…</p>
{:else if failed && !hasComputerSettings(computer) && !hasDeviceControls}
  <div class="flex flex-col items-start gap-3">
    <p role="alert" class="text-sm text-fg-muted">Could not load this computer’s settings.</p>
    <button class={PRIMARY_BUTTON_CLASS} onclick={() => void load()}>Retry</button>
  </div>
{:else}
  {#if loading}<p role="status" class="mb-3 text-xs text-fg-muted">Loading computer settings…</p>{/if}
  {#if failed}<p role="status" class="mb-3 text-xs text-fg-muted">Computer unavailable. Showing its last saved settings.</p>{/if}
  <Page />
{/if}
