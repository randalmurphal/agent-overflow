<script lang="ts" module>
  import {
    GetWSLDistroPreference,
    IsWSL,
    ListWSLDistros,
    type WSLDistro,
  } from '../../stores/bindings';

  // SettingsView destroys/recreates this component each time the user
  // tabs back to the Network panel. The module-scoped Promise caches
  // the cold-load result so the wsl.exe spawn + JSON read happen once
  // per app session — subsequent mounts re-await the cached value.
  type LoadResult = {
    supported: boolean;
    distros: WSLDistro[];
    saved: string;
  };
  let cachedLoad: Promise<LoadResult> | null = null;

  export function resetWSLSectionCache(): void {
    cachedLoad = null;
  }

  async function loadOnce(): Promise<LoadResult> {
    const isWsl = !!(await IsWSL());
    if (!isWsl) {
      return { supported: false, distros: [], saved: '' };
    }
    const [listResult, currentResult] = await Promise.allSettled([
      ListWSLDistros(),
      GetWSLDistroPreference(),
    ]);
    if (listResult.status === 'rejected') {
      throw listResult.reason;
    }
    return {
      supported: true,
      distros: listResult.value ?? [],
      // A failed preference read is not fatal: render the radio list
      // with no current selection rather than blocking the whole panel.
      saved: currentResult.status === 'fulfilled' ? currentResult.value ?? '' : '',
    };
  }

  function ensureLoad(): Promise<LoadResult> {
    if (!cachedLoad) {
      cachedLoad = loadOnce();
    }
    return cachedLoad;
  }
</script>

<script lang="ts">
  import { SetWSLDistroPreference } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import { errString } from '../../utils/errors';
  import { isClientMode } from '../../transport/runMode';
  import SettingsHeader from './SettingsHeader.svelte';

  const clientMode = isClientMode();

  let loading = $state(true);
  let supported = $state(false);
  let distros = $state<WSLDistro[]>([]);
  let saved = $state<string>('');
  let selected = $state<string>('');
  let saving = $state(false);

  async function load(): Promise<void> {
    if (clientMode) {
      loading = false;
      return;
    }
    loading = true;
    try {
      const result = await ensureLoad();
      supported = result.supported;
      distros = result.distros;
      saved = result.saved;
      selected = saved;
    } catch (err) {
      // Drop the cache on failure so the next mount retries instead
      // of replaying the same rejection forever.
      resetWSLSectionCache();
      addToast('error', `Failed to load WSL distros: ${errString(err)}`);
    } finally {
      loading = false;
    }
  }

  async function selectDistro(name: string): Promise<void> {
    if (saving || name === '' || name === saved) return;
    const previous = saved;
    saving = true;
    selected = name;
    try {
      const persisted = await SetWSLDistroPreference(name);
      saved = persisted;
      selected = persisted;
    } catch (err) {
      selected = previous;
      addToast('error', `Failed to set WSL distro: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }

  $effect(() => {
    void load();
  });
</script>

{#if clientMode}
  <!-- Hidden in --connect mode entirely; nothing to render. -->
{:else if loading}
  <section data-testid="wsl-section-loading">
    <SettingsHeader title="WSL Distro" />
    <p class="text-[0.75rem] text-fg-hint" role="status" aria-live="polite">
      Detecting WSL…
    </p>
  </section>
{:else if !supported}
  <!-- Not running under WSL — nothing to render. -->
{:else}
  <section data-testid="wsl-section">
    <SettingsHeader
      title="WSL Distro"
      description="Pick which WSL distribution the launcher boots into the next time you open Agent Overflow. The change takes effect on next launch — this session keeps running in its current distro until you close and reopen the app."
    />

    {#if distros.length === 0}
      <p
        class="text-[0.75rem] text-fg-hint"
        data-testid="wsl-section-no-distros"
      >
        No WSL distros reported by
        <code class="font-mono text-[0.6875rem]">wsl.exe</code>. Install one from
        PowerShell with
        <code class="font-mono text-[0.6875rem]">wsl --install -d Ubuntu</code> and
        reopen Settings to refresh the list.
      </p>
    {:else}
      <fieldset
        class="flex flex-col gap-0.5"
        role="radiogroup"
        aria-label="Preferred WSL distro"
        data-testid="wsl-section-radiogroup"
      >
        {#each distros as distro (distro.name)}
          <label
            class="flex items-center gap-2.5 rounded-[var(--radius-field)] px-2 py-1.5 cursor-pointer transition-colors hover:bg-surface-2/30"
            data-testid="wsl-option-{distro.name}"
          >
            <input
              type="radio"
              name="wsl-distro-preference"
              value={distro.name}
              checked={selected === distro.name}
              disabled={saving}
              onchange={() => void selectDistro(distro.name)}
              class="accent-accent"
            />
            <span class="text-[0.8125rem] font-medium text-fg">{distro.name}</span>
            {#if distro.default}
              <span class="text-[0.6875rem] text-fg-hint">(default)</span>
            {/if}
            {#if distro.state && distro.state !== 'Running'}
              <span class="text-[0.6875rem] italic text-fg-hint">({distro.state})</span>
            {/if}
            {#if distro.version === 1}
              <span class="text-[0.6875rem] text-warning">(WSL1 — agent-overflow needs WSL2)</span>
            {/if}
          </label>
        {/each}
      </fieldset>

      {#if saved && selected !== saved}
        <p
          class="mt-2 text-[0.6875rem] text-fg-hint"
          aria-live="polite"
          data-testid="wsl-section-pending-hint"
        >
          Currently saved: <code class="font-mono">{saved}</code>
        </p>
      {/if}
    {/if}
  </section>
{/if}
