<script lang="ts">
  import { GetProviderStatuses } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderStatus } from '../../types/settings';
  import { onMount } from 'svelte';

  let { currentProvider, onSelect }: {
    currentProvider: 'claude' | 'codex';
    onSelect: (provider: string) => void;
  } = $props();

  let statuses = $state<Map<string, ProviderStatus>>(new Map());

  onMount(async () => {
    try {
      const result = await GetProviderStatuses();
      const map = new Map<string, ProviderStatus>();
      for (const s of (result ?? []) as ProviderStatus[]) {
        map.set(s.provider, s);
      }
      statuses = map;
    } catch (err) {
      console.error('Failed to fetch provider statuses:', err);
      addToast('error', 'Failed to load provider statuses');
    }
  });

  function statusDotColor(provider: string): string {
    const s = statuses.get(provider);
    if (!s) return 'bg-text-secondary/30';
    switch (s.status) {
      case 'ready': return 'bg-green-400';
      case 'error': return 'bg-red-400';
      default: return 'bg-text-secondary/30';
    }
  }

  function isDisabled(provider: string): boolean {
    const s = statuses.get(provider);
    return s ? s.status === 'not_found' : false;
  }
</script>

<div class="flex gap-1">
  {#each ['claude', 'codex'] as provider}
    <button
      onclick={() => { if (!isDisabled(provider)) onSelect(provider); }}
      disabled={isDisabled(provider)}
      class="flex-1 flex items-center justify-center gap-1.5 text-xs py-1.5 rounded cursor-pointer
        {currentProvider === provider
          ? 'bg-accent text-surface-0 font-medium'
          : 'bg-surface-2 text-text-secondary hover:text-text-primary'}
        {isDisabled(provider) ? 'opacity-30 cursor-not-allowed' : ''}"
    >
      <span class="w-1.5 h-1.5 rounded-full {statusDotColor(provider)} shrink-0"></span>
      {provider === 'claude' ? 'Claude' : 'Codex'}
    </button>
  {/each}
</div>
