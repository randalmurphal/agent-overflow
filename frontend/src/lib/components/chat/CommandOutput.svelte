<script lang="ts">
  import { slide } from 'svelte/transition';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

  let { item, meta, payloadId }: { item?: Item; meta: CommandOutputMeta; payloadId: string } = $props();

  const expansion = createPayloadExpansion(() => payloadId);

  $effect(() => {
    payloadId;
    expansion.reset();
  });

  let exitBadgeClasses = $derived(
    meta.exitCode === 0
      ? 'bg-success/20 text-success'
      : 'bg-error/20 text-error'
  );
</script>

<div class="bg-surface-1 rounded border border-border overflow-hidden mb-2">
  <!-- Header -->
  <button
    class="w-full px-3 py-2 flex items-center gap-2 text-sm cursor-pointer hover:bg-surface-2/40"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="cmd-output-{payloadId}"
    aria-label="Toggle command output: {meta.command}"
  >
    <span class="text-xs text-text-secondary select-none" aria-hidden="true">{expansion.expanded ? '▼' : '▶'}</span>
    <span class="font-mono text-xs text-text-primary truncate">{meta.command}</span>
    <ToolDecisionChip decision={item?.decision} />
    <span class="px-1.5 py-0.5 rounded-full text-xs {exitBadgeClasses}">
      exit {meta.exitCode}
    </span>
    <span class="ml-auto text-xs text-text-secondary shrink-0">
      {meta.lineCount} lines
    </span>
  </button>

  <!-- Output content -->
  {#if expansion.expanded}
    <div id="cmd-output-{payloadId}" transition:slide={{ duration: 150 }} class="border-t border-border bg-surface-0 px-3 py-2 overflow-x-auto">
      {#if expansion.loading}
        <p class="text-xs text-text-secondary" role="status" aria-live="polite">Loading full output…</p>
      {:else if expansion.error}
        <p class="text-xs text-error" role="alert">Failed to load output: {expansion.error}</p>
      {:else}
        <pre class="ansi-body font-mono text-xs whitespace-pre text-text-secondary">{@html expansion.displayHtml ?? ''}</pre>
        {#if expansion.hasMore}
          <button
            type="button"
            class="mt-2 text-xs text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.showFull()}
            data-testid="command-output-show-full"
          >
            Show full output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
