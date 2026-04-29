<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import { untrack } from 'svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';

  let {
    pane,
    item,
    meta,
    payloadId,
    allowShowFull = true,
    showCompletionBadge = true,
  }: {
    pane?: ThreadPane;
    item: Item;
    meta: CommandOutputMeta;
    payloadId: string;
    allowShowFull?: boolean;
    /** Suppress the completion badge in surfaces where the parent
     * already renders a status icon (e.g. BackgroundTaskTrayRow's
     * Loader/Check/AlertCircle/Square). Defaults to true so the
     * chat-timeline rendering is unaffected. */
    showCompletionBadge?: boolean;
  } = $props();

  // pane is stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    pane ? null : createPayloadExpansion(() => payloadId, () => item.threadId),
  );
  const expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);

  let time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  // Pass the parent's typed meta inline so the helper does not re-parse
  // payloadMeta. The runtime object retains is_error / exit_code keys
  // even when the typed view drops them.
  let completionStatus = $derived(
    deriveCompletionStatus(item, { meta: meta as unknown as Record<string, unknown> }),
  );
</script>

<div class="mb-1.5 overflow-hidden">
  <!-- Header -->
  <button
    class="w-full rounded-[var(--radius-control)] px-1 py-1 flex items-center gap-2 text-[12px] cursor-pointer hover:bg-surface-2/20 transition-colors"
    onclick={() => expansion.toggle()}
    aria-expanded={expansion.expanded}
    aria-controls="cmd-output-{payloadId}"
    aria-label="Toggle Command Output: {meta.command}"
  >
    <span
      class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
      class:rotate-90={expansion.expanded}
      aria-hidden="true"
    >
      <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
    </span>
    <span class="font-mono text-[12px] text-fg-muted truncate">{meta.command}</span>
    <ToolDecisionChip decision={item.decision} />
    {#if showCompletionBadge && completionStatus !== null}
      <CompletionBadge status={completionStatus} />
    {/if}
    <time
      class="ml-auto text-[10px] text-fg-hint shrink-0 tabular-nums"
      datetime={new Date(item.createdAt).toISOString()}
      data-testid="command-output-time"
    >
      {time}
    </time>
  </button>

  <!-- Output content -->
  {#if expansion.expanded}
    <div id="cmd-output-{payloadId}" transition:slide={{ duration: 150 }} class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <div class="px-3 py-2 overflow-x-auto">
        {#if expansion.loading}
          <p class="text-[11px] text-fg-subtle" role="status" aria-live="polite">Loading full output…</p>
        {:else if expansion.error}
          <p class="text-[11px] text-error" role="alert">Failed to load output: {expansion.error}</p>
        {:else}
          <AnsiText source={expansion.displayData ?? ''} class="text-[11px] whitespace-pre text-fg-muted leading-relaxed" />
          {#if expansion.hasMore && allowShowFull}
            <button
              type="button"
              class="mt-2 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded"
              onclick={() => expansion.showFull()}
              data-testid="command-output-show-full"
            >
              Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
            </button>
          {:else if expansion.hasMore}
            <p class="mt-2 text-[11px] text-fg-subtle">
              Preview truncated ({formatPayloadSize(expansion.totalSize)})
            </p>
          {/if}
        {/if}
      </div>
      {#if !expansion.loading && !expansion.error && expansion.displayData}
        <CopyFooter text={expansion.displayData} label="Copy output" />
      {/if}
    </div>
  {/if}
</div>
