<script lang="ts">
  import type { ProviderStatus } from '../../types/settings';
  import { GetProviderStatuses } from '../../stores/bindings';
  import { getSettings } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';

  let { currentProvider, onSelect }: {
    currentProvider: 'claude' | 'codex';
    onSelect: (provider: string) => void;
  } = $props();

  let statuses = $state<Map<string, ProviderStatus>>(new Map());
  let loadGeneration = 0;

  // Re-fetch provider statuses when binary paths change in settings.
  $effect(() => {
    const settings = getSettings();
    settings.claudeBinaryPath;
    settings.codexBinaryPath;

    const generation = ++loadGeneration;
    void (async () => {
      try {
        const result = await GetProviderStatuses();
        if (generation !== loadGeneration) return;
        const map = new Map<string, ProviderStatus>();
        for (const s of (result ?? []) as ProviderStatus[]) {
          map.set(s.provider, s);
        }
        statuses = map;
      } catch (err) {
        if (generation !== loadGeneration) return;
        console.error('Failed to fetch provider statuses:', err);
        addToast('error', 'Failed to load provider statuses');
      }
    })();
  });

  function statusDotColor(provider: string): string {
    const s = statuses.get(provider);
    if (!s) return 'bg-text-secondary/30';
    switch (s.status) {
      case 'ready': return 'bg-success';
      case 'error': return 'bg-error';
      default: return 'bg-text-secondary/30';
    }
  }

  function isDisabled(provider: string): boolean {
    const s = statuses.get(provider);
    const settings = getSettings();
    const isEnabled = provider === 'claude' ? settings.claudeEnabled : settings.codexEnabled;
    if (!isEnabled) return true;
    return s ? s.status === 'not_found' : false;
  }
</script>

<div class="flex gap-1" role="radiogroup" aria-label="Provider">
  {#each ['claude', 'codex'] as provider}
    {@const label = provider === 'claude' ? 'Claude' : 'Codex'}
    {@const s = statuses.get(provider)}
    {@const statusText = s ? s.status : 'unknown'}
    <button
      onclick={() => { if (!isDisabled(provider)) onSelect(provider); }}
      disabled={isDisabled(provider)}
      role="radio"
      aria-checked={currentProvider === provider}
      aria-label="{label} — {statusText}"
      class="flex-1 flex items-center justify-center gap-1.5 text-xs py-1.5 rounded cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50
        {currentProvider === provider
          ? 'bg-accent text-surface-0 font-medium'
          : 'bg-surface-2 text-text-secondary hover:text-text-primary'}
        {isDisabled(provider) ? 'opacity-30 cursor-not-allowed' : ''}"
    >
      <span class="w-1.5 h-1.5 rounded-full {statusDotColor(provider)} shrink-0" aria-hidden="true"></span>
      {label}
    </button>
  {/each}
</div>
