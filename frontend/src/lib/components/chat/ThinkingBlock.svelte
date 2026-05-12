<script lang="ts">
  import { untrack } from 'svelte';
  import CopyFooter from './CopyFooter.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  // pane is stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => item.payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  const expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);
  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  let preview = $derived(
    item.summary.length > 200 ? item.summary.slice(0, 200) + '...' : item.summary,
  );

  const copyText = $derived(expansion.displayData ?? item.summary);
</script>

<div class="group/thinking mb-1.5 overflow-hidden">
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    controls={`thinking-${item.id}`}
    ariaLabel="Toggle Thinking Block"
    testId="thinking-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={() => expansion.toggle()}
  >
    <span class="text-[11px] text-fg-muted font-medium uppercase tracking-[0.04em] shrink-0">Thinking</span>
    {#if !expansion.expanded}
      <span class="text-[12px] text-fg-muted/70 truncate flex-1 italic">{preview}</span>
    {/if}
  </TranscriptDisclosureHeader>

  {#if expansion.expanded}
    <div class="ml-5 border-l border-border-subtle bg-surface-0/35">
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
