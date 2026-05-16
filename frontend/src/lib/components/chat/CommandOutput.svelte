<script lang="ts">
  import type { Snippet } from 'svelte';
  import Terminal from 'lucide-svelte/icons/terminal';
  import Icon from '../primitives/Icon.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import { untrack } from 'svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    completionBadgeTitleForStatus,
    deriveCompletionStatus,
  } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import {
    commandErrorLineForItem,
    commandTextForItem,
    displayCommandForItem,
  } from './commandDisplay';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';

  let {
    pane,
    item,
    meta,
    payloadId,
    showCompletionBadge = true,
    displayItem,
    statusItem,
    collapsedPreview = '',
    durationLabel = '',
    showTimestamp = true,
    trailingActions,
  }: {
    pane?: ThreadPane;
    item: Item;
    meta?: CommandOutputMeta | null;
    payloadId?: string;
    /** Suppress the completion badge in surfaces where the parent
     * already renders a status icon (e.g. BackgroundTaskTrayRow's
     * Loader/Check/AlertCircle/Square). Defaults to true so the
     * chat-timeline rendering is unaffected. */
    showCompletionBadge?: boolean;
    /** Item used for command extraction and user-facing command text. */
    displayItem?: Item;
    /** Item used for status/badge derivation. Useful for launch+completion pairs. */
    statusItem?: Item;
    /** Short output preview shown under the collapsed header in scoped timeline surfaces. */
    collapsedPreview?: string;
    /** Optional duration/elapsed label rendered in the metadata area. */
    durationLabel?: string;
    /** Tray surfaces show elapsed time instead of the absolute transcript timestamp. */
    showTimestamp?: boolean;
    /** Optional actions rendered outside the disclosure button. */
    trailingActions?: Snippet;
  } = $props();
  let effectiveDisplayItem = $derived(displayItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);

  // pane is stable across a row's lifetime; read once via `untrack`.
  const localFallback = untrack(() =>
    pane
      ? null
      : createPayloadExpansion(
          () => payloadId,
          () => item.threadId,
          { payloadVersion: () => item.updatedAt },
        ),
  );
  let expansion = $derived(pane ? pane.expansionStateFor(item) : localFallback!);
  let hasPayload = $derived(Boolean(payloadId));
  let itemMeta = $derived(parseJsonObject(item.meta));
  let payloadMeta = $derived(parseJsonObject(item.payloadMeta));
  let statusMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));
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
  let rawCommand = $derived(commandTextForItem(effectiveDisplayItem, meta));
  let displayCommand = $derived(displayCommandForItem(effectiveDisplayItem, meta));
  let isBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );

  let time = $derived(
    new Date(effectiveStatusItem.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  // payloadMeta is the canonical status source. Callers may pass a
  // normalized CommandOutputMeta that intentionally contains only the
  // display fields, while raw payloadMeta can also carry snake-case
  // exit/error fields from provider-specific paths.
  let completionStatus = $derived(
    deriveCompletionStatus(effectiveStatusItem, {
      meta: statusMeta ?? (meta as unknown as Record<string, unknown> | undefined),
    }),
  );
  let completionTitle = $derived(completionBadgeTitleForStatus(effectiveStatusItem.status));
  let errorLine = $derived(
    completionStatus === 'failure' &&
      effectiveStatusItem.status !== 'killed' &&
      effectiveStatusItem.status !== 'declined'
      ? commandErrorLineForItem(item, meta, itemMeta, statusMeta ?? payloadMeta)
      : '',
  );
  let isForegroundRunning = $derived(
    !isBackgroundedLaunch && (effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming'),
  );
  let statusSlotHasContent = $derived(
    isBackgroundedLaunch || isForegroundRunning || (showCompletionBadge && completionStatus !== null),
  );
  let compactCollapsedPreview = $derived.by(() => {
    const normalized = collapsedPreview.replace(/\s+/g, ' ').trim();
    if (normalized.length <= 160) return normalized;
    return `${normalized.slice(0, 160).trimEnd()}...`;
  });

  keepExpandedPayloadFresh(() => expansion, () => hasPayload);
</script>

<div class="group/tool overflow-hidden" data-testid="command-output-row">
  {#snippet headerContent()}
    <span class="flex size-3.5 shrink-0 items-center justify-center text-text-secondary" data-testid="command-output-icon" aria-hidden="true">
      <Icon icon={Terminal} size={14} strokeWidth={2} class="opacity-75" />
    </span>
    <span class="shrink-0 text-[11px] font-medium text-fg-muted uppercase tracking-[0.04em]" data-testid="command-output-label">
      Bash
    </span>
    <span class="min-w-0 flex-1 truncate font-mono text-[12px] text-fg-muted" data-testid="command-output-command">{displayCommand}</span>
  {/snippet}

  {#snippet headerActions()}
    <ToolDecisionChip decision={effectiveDisplayItem.decision} />
    <ToolHeaderMeta
      statusSlotTestId="command-output-status-slot"
      duration={{ testId: 'command-output-duration', label: durationLabel }}
      timestamp={showTimestamp
        ? { testId: 'command-output-time', value: effectiveStatusItem.createdAt, label: time }
        : undefined}
      {trailingActions}
    >
      {#snippet status()}
        {#if statusSlotHasContent}
          {#if isBackgroundedLaunch}
            <span
              class="shrink-0 text-[20px] leading-none text-accent opacity-90 transition-opacity"
              data-testid="command-output-status"
              title="Running in background"
              aria-label="Backgrounded"
            >
              …
            </span>
          {:else if isForegroundRunning}
            <span
              class="shrink-0 text-[20px] leading-none text-accent opacity-90 animate-pulse"
              data-testid="command-output-status"
              title="Running"
              aria-label="Running"
            >
              …
            </span>
          {:else if showCompletionBadge && completionStatus !== null}
            <CompletionBadge status={completionStatus} title={completionTitle} />
          {/if}
        {/if}
      {/snippet}
    </ToolHeaderMeta>
  {/snippet}

  <!-- Header -->
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasBody}
    controls={hasBody ? `cmd-output-${payloadId || item.id}` : undefined}
    ariaLabel={`Toggle Command Output: ${rawCommand}`}
    testId="command-output-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 text-[12px] {hasBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={() => expansion.toggle()}
  >
    {@render headerContent()}
    {#snippet actions()}
      {@render headerActions()}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if errorLine}
    <div class="ml-5 border-l border-border-subtle px-3 pb-1">
      <p class="text-[11px] leading-relaxed text-error" data-testid="command-output-error">
        {errorLine}
      </p>
    </div>
  {/if}
  {#if compactCollapsedPreview && !expansion.expanded}
    <div class="ml-5 truncate px-3 pb-1 text-[11px] text-fg-subtle" data-testid="command-output-preview">
      └ {compactCollapsedPreview}
    </div>
  {/if}

  <!-- Output content -->
  {#if hasBody && expansion.expanded}
    <div id="cmd-output-{payloadId || item.id}" class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <div class="max-h-96 overflow-auto px-3 py-2">
        {#if hasPayload && expansion.loading}
          <p class="text-[11px] text-fg-subtle" role="status" aria-live="polite">Loading output…</p>
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
          {#if expansion.hasMore}
            <button
              type="button"
              class="mt-2 inline-flex items-center rounded-[var(--radius-control)] border border-border-subtle px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
              onclick={() => void expansion.showFull()}
              data-testid="command-output-show-full"
            >
              Show more output ({formatPayloadSize(expansion.totalSize)})
            </button>
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
        <CopyFooter text={expansion.displayData} label={expansion.hasMore ? 'Copy visible output' : 'Copy output'} />
      {/if}
    </div>
  {/if}
</div>
