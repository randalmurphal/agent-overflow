<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { Item } from '../../types/models';
  import { GetPayloadData } from '../../stores/bindings';

  let { item }: { item: Item } = $props();

  let expanded = $state(false);
  let loading = $state(false);
  let fullContent = $state<string | null>(null);
  let loadError = $state<string | null>(null);

  let preview = $derived(
    item.summary.length > 200 ? item.summary.slice(0, 200) + '...' : item.summary,
  );

  async function toggle() {
    if (expanded) {
      expanded = false;
      return;
    }

    expanded = true;

    if (fullContent !== null || !item.payloadId) return;

    loading = true;
    loadError = null;
    try {
      fullContent = await GetPayloadData(item.payloadId);
    } catch (err) {
      loadError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }
</script>

<div class="mb-2 bg-surface-1 rounded border border-border overflow-hidden">
  <button
    class="w-full px-3 py-2 flex items-center gap-2 text-left cursor-pointer hover:bg-surface-2/40"
    onclick={toggle}
  >
    <span class="text-xs text-text-secondary select-none">{expanded ? '▼' : '▶'}</span>
    <span class="text-xs text-text-secondary font-medium">Thinking</span>
    {#if !expanded}
      <span class="text-xs text-text-secondary/60 truncate flex-1 italic">{preview}</span>
    {/if}
  </button>

  {#if expanded}
    <div transition:slide={{ duration: 150 }} class="border-t border-border px-3 py-2 max-h-80 overflow-y-auto">
      {#if loading}
        <p class="text-xs text-text-secondary animate-pulse">Loading thinking content...</p>
      {:else if loadError}
        <p class="text-xs text-error">Failed to load: {loadError}</p>
      {:else if fullContent}
        <pre class="text-xs text-text-secondary whitespace-pre-wrap font-mono leading-relaxed">{fullContent}</pre>
      {:else}
        <pre class="text-xs text-text-secondary whitespace-pre-wrap font-mono leading-relaxed italic">{item.summary}</pre>
      {/if}
    </div>
  {/if}
</div>
