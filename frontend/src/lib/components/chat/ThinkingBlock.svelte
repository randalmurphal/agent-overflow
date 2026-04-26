<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import type { Item } from '../../types/models';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';

  let { item }: { item: Item } = $props();

  const expansion = createPayloadExpansion(() => item.payloadId, () => item.threadId);

  let preview = $derived(
    item.summary.length > 200 ? item.summary.slice(0, 200) + '...' : item.summary,
  );

  const copyText = $derived(expansion.displayData ?? item.summary);

  $effect(() => {
    item.id;
    item.threadId;
    item.payloadId;
    expansion.reset();
  });
</script>

<div class="mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/20 overflow-hidden">
  <button
    class="w-full px-2.5 py-1.5 flex items-center gap-2 text-left cursor-pointer hover:bg-surface-2/20 transition-colors"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="thinking-{item.id}"
    aria-label="Toggle Thinking Block"
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expansion.expanded}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    <span class="text-[11px] text-fg-subtle font-medium uppercase tracking-[0.04em]">Thinking</span>
    {#if !expansion.expanded}
      <span class="text-[12px] text-fg-muted/70 truncate flex-1 italic">{preview}</span>
    {/if}
  </button>

  {#if expansion.expanded}
    <div transition:slide={{ duration: 150 }} class="border-t border-border-subtle bg-surface-0/50">
      <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
      <div id="thinking-{item.id}" class="px-3 py-2 max-h-80 overflow-y-auto" tabindex="0" role="region" aria-label="Thinking Content">
        {#if expansion.loading}
          <p class="text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">Loading thinking content...</p>
        {:else if expansion.error}
          <p class="text-[11px] text-error" role="alert">Failed to load: {expansion.error}</p>
        {:else}
          <AnsiText source={copyText} class="text-[11px] text-fg-muted whitespace-pre-wrap leading-relaxed italic" />
          {#if expansion.hasMore}
            <button
              type="button"
              class="mt-2 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
              onclick={() => expansion.showFull()}
              data-testid="thinking-show-full"
            >
              Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
            </button>
          {/if}
        {/if}
      </div>
      {#if !expansion.loading && !expansion.error && copyText}
        <CopyFooter text={copyText} label="Copy thinking" />
      {/if}
    </div>
  {/if}
</div>
