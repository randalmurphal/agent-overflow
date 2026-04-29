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
  import {
    commandLabelForStatus,
    commandTextForItem,
    displayCommandForItem,
  } from './commandDisplay';
  import { parseJsonObject } from '../../utils/parseJsonObject';

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
    meta?: CommandOutputMeta | null;
    payloadId?: string;
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
  let expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);
  let hasPayload = $derived(Boolean(payloadId));
  let itemMeta = $derived(parseJsonObject(item.meta));
  let payloadMeta = $derived(parseJsonObject(item.payloadMeta));
  let deferredOutputState = $derived.by(() => {
    if (!itemMeta) return '';
    const state = itemMeta.notification_output_state ?? itemMeta.output_file_state;
    return typeof state === 'string' ? state : '';
  });
  let deferredOutputError = $derived.by(() => {
    if (!itemMeta) return '';
    const error = itemMeta.notification_output_error ?? itemMeta.output_file_error;
    return typeof error === 'string' ? error : '';
  });
  let hasBody = $derived(
    hasPayload || deferredOutputState === 'loading' || deferredOutputState === 'error',
  );
  let rawCommand = $derived(commandTextForItem(item, meta));
  let displayCommand = $derived(displayCommandForItem(item, meta));
  let commandLabel = $derived(commandLabelForStatus(item.status));
  let isBackgroundedLaunch = $derived(item.kind === 'tool_call' && item.isBackground === true);

  let time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  // payloadMeta is the canonical status source. Callers may pass a
  // normalized CommandOutputMeta that intentionally contains only the
  // display fields, while raw payloadMeta can also carry snake-case
  // exit/error fields from provider-specific paths.
  let completionStatus = $derived(
    deriveCompletionStatus(item, {
      meta: payloadMeta ?? (meta as unknown as Record<string, unknown> | undefined),
    }),
  );
</script>

<div class="mb-1.5 overflow-hidden">
  {#snippet headerContent(showChevron: boolean)}
    {#if showChevron}
      <span
        class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150"
        class:rotate-90={expansion.expanded}
        aria-hidden="true"
      >
        <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
      </span>
    {/if}
    <span class="shrink-0 text-[12px] font-medium text-fg-muted" data-testid="command-output-label">
      {commandLabel}:
    </span>
    <span class="min-w-0 flex-1 truncate font-mono text-[12px] text-fg-muted" data-testid="command-output-command">{displayCommand}</span>
    <ToolDecisionChip decision={item.decision} />
    {#if isBackgroundedLaunch}
      <span
        class="shrink-0 text-[10px] text-accent opacity-70 transition-opacity"
        data-testid="command-output-status"
        title="Running in background"
        aria-label="Backgrounded"
      >
        …
      </span>
    {/if}
    <time
      class="ml-auto text-[10px] text-fg-hint shrink-0 tabular-nums"
      datetime={new Date(item.createdAt).toISOString()}
      data-testid="command-output-time"
    >
      {time}
    </time>
    {#if showCompletionBadge && !isBackgroundedLaunch && completionStatus !== null}
      <CompletionBadge status={completionStatus} />
    {/if}
  {/snippet}

  <!-- Header -->
  {#if hasBody}
    <button
      class="w-full rounded-[var(--radius-control)] px-1 py-1 flex items-center gap-2 text-[12px] cursor-pointer hover:bg-surface-2/20 transition-colors"
      onclick={() => expansion.toggle()}
      aria-expanded={expansion.expanded}
      aria-controls="cmd-output-{payloadId || item.id}"
      aria-label="Toggle Command Output: {rawCommand}"
      data-testid="command-output-toggle"
    >
      {@render headerContent(true)}
    </button>
  {:else}
    <div
      class="w-full rounded-[var(--radius-control)] px-1 py-1 flex items-center gap-2 text-[12px]"
      data-testid="command-output-row"
    >
      {@render headerContent(false)}
    </div>
  {/if}

  <!-- Output content -->
  {#if hasBody && expansion.expanded}
    <div id="cmd-output-{payloadId || item.id}" transition:slide={{ duration: 150 }} class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <div class="px-3 py-2 overflow-x-auto">
        {#if hasPayload && expansion.loading}
          <p class="text-[11px] text-fg-subtle" role="status" aria-live="polite">Loading full output…</p>
        {:else if hasPayload && expansion.error}
          <div class="space-y-2">
            <p class="text-[11px] text-error" role="alert">Failed to load output: {expansion.error}</p>
            <button
              type="button"
              class="text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded"
              onclick={() => expansion.retry()}
              data-testid="command-output-retry"
            >
              Retry
            </button>
          </div>
        {:else if hasPayload}
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
        {:else if deferredOutputState === 'loading'}
          <p class="text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">
            Loading…
          </p>
        {:else if deferredOutputState === 'error'}
          <p class="text-[11px] text-error" role="alert">
            Failed to load: {deferredOutputError || 'Background output could not be loaded.'}
          </p>
        {/if}
      </div>
      {#if !expansion.loading && !expansion.error && expansion.displayData}
        <CopyFooter text={expansion.displayData} label="Copy output" />
      {/if}
    </div>
  {/if}
</div>
