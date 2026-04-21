<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { Item } from '../../types/models';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

  let { item }: { item: Item } = $props();

  const expansion = createPayloadExpansion(() => item.payloadId);

  let preview = $derived(
    item.summary.length > 200 ? item.summary.slice(0, 200) + '...' : item.summary,
  );

  $effect(() => {
    item.id;
    item.payloadId;
    expansion.reset();
  });
</script>

<div class="mb-2 bg-surface-1 rounded border border-border overflow-hidden">
  <button
    class="w-full px-3 py-2 flex items-center gap-2 text-left cursor-pointer hover:bg-surface-2/40"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="thinking-{item.id}"
    aria-label="Toggle thinking block"
  >
    <span class="text-xs text-text-secondary select-none" aria-hidden="true">{expansion.expanded ? '▼' : '▶'}</span>
    <span class="text-xs text-text-secondary font-medium">Thinking</span>
    {#if !expansion.expanded}
      <span class="text-xs text-text-secondary/60 truncate flex-1 italic">{preview}</span>
    {/if}
  </button>

  {#if expansion.expanded}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div id="thinking-{item.id}" transition:slide={{ duration: 150 }} class="border-t border-border px-3 py-2 max-h-80 overflow-y-auto" tabindex="0" role="region" aria-label="Thinking content">
      {#if expansion.loading}
        <p class="text-xs text-text-secondary animate-pulse" role="status" aria-live="polite">Loading thinking content...</p>
      {:else if expansion.error}
        <p class="text-xs text-error" role="alert">Failed to load: {expansion.error}</p>
      {:else}
        <pre class="ansi-body text-xs text-text-secondary whitespace-pre-wrap font-mono leading-relaxed italic">{@html expansion.displayHtml ?? item.highlightedContent}</pre>
        {#if expansion.hasMore}
          <button
            type="button"
            class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.showFull()}
            data-testid="thinking-show-full"
          >
            Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
