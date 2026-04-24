<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';

  let {
    item,
    meta,
    payloadId,
    threadId,
  }: {
    item?: Item;
    meta: CommandOutputMeta;
    payloadId: string;
    threadId?: string;
  } = $props();

  const expansion = createPayloadExpansion(() => payloadId, () => item?.threadId ?? threadId);

  $effect(() => {
    item?.threadId;
    threadId;
    payloadId;
    expansion.reset();
  });

  let exitBadgeClasses = $derived(
    meta.exitCode === 0
      ? 'bg-success/20 text-success'
      : 'bg-error/20 text-error'
  );
</script>

<div class="mb-1.5 rounded-[var(--radius-control)] border border-border-subtle bg-card/25 overflow-hidden">
  <!-- Header -->
  <button
    class="w-full px-2.5 py-1.5 flex items-center gap-2 text-[13px] cursor-pointer hover:bg-surface-2/25 transition-colors"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="cmd-output-{payloadId}"
    aria-label="Toggle command output: {meta.command}"
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expansion.expanded}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    <span class="font-mono text-[12px] text-fg-muted truncate">{meta.command}</span>
    <ToolDecisionChip decision={item?.decision} />
    <span class="px-1.5 py-0.5 rounded-[var(--radius-field)] text-[10px] font-medium {exitBadgeClasses}">
      exit {meta.exitCode}
    </span>
    <span class="ml-auto text-[10px] text-fg-hint shrink-0 tabular-nums">
      {meta.lineCount} lines
    </span>
  </button>

  <!-- Output content -->
  {#if expansion.expanded}
    <div id="cmd-output-{payloadId}" transition:slide={{ duration: 150 }} class="border-t border-border-subtle bg-surface-0/50 px-3 py-2 overflow-x-auto">
      {#if expansion.loading}
        <p class="text-[11px] text-fg-subtle" role="status" aria-live="polite">Loading full output…</p>
      {:else if expansion.error}
        <p class="text-[11px] text-error" role="alert">Failed to load output: {expansion.error}</p>
      {:else}
        <AnsiText source={expansion.displayData ?? ''} class="text-[11px] whitespace-pre text-fg-muted leading-relaxed" />
        {#if expansion.hasMore}
          <button
            type="button"
            class="mt-2 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded"
            onclick={() => expansion.showFull()}
            data-testid="command-output-show-full"
          >
            Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
      {/if}
    </div>
  {/if}
</div>
