<script lang="ts">
  import type { Snippet } from 'svelte';
  import CopyFooter from './CopyFooter.svelte';
  import { untrack } from 'svelte';
  import type { CommandOutputMeta, Item } from '../../types/models';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import DevServerChip from './DevServerChip.svelte';
  import { loopbackDevServerURL } from '../../utils/externalLinks';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import {
    commandErrorForItem,
    displayCommandForItem,
  } from './commandDisplay';
  import { createRunningElapsed } from './useRunningElapsed.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  let {
    pane,
    item,
    meta,
    payloadId,
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
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
  });
  let expansion = $derived(expansionRef.current!);
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
  // One derived id for both halves of the disclosure (utils/chatDomIds.ts):
  // the header's `controls` and the body's `id` must be one string.
  let outputDomId = $derived(chatRowDomId(pane, 'cmd-output', payloadId || item.id));
  let hasBody = $derived(
    hasPayload || deferredOutputState === 'loading' || deferredOutputState === 'error',
  );
  let displayCommand = $derived(displayCommandForItem(effectiveDisplayItem, meta));
  let isBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );

  const BASH_ELAPSED_THRESHOLD_MS = 3_000;

  let isRunning = $derived(
    effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming',
  );

  const ticker = createRunningElapsed(
    () => isRunning && durationLabel === '' && !isBackgroundedLaunch,
    () => effectiveStatusItem.createdAt,
    BASH_ELAPSED_THRESHOLD_MS,
  );

  let completedDurationMs = $derived.by<number | null>(() => {
    if (isRunning || isBackgroundedLaunch) return null;
    const created = effectiveStatusItem.createdAt;
    const updated = effectiveStatusItem.updatedAt;
    if (!created || !updated || updated <= created) return null;
    const elapsed = updated - created;
    if (elapsed < BASH_ELAPSED_THRESHOLD_MS) return null;
    return elapsed;
  });

  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  // payloadMeta is the canonical status source. Callers may pass a
  // normalized CommandOutputMeta that intentionally contains only the
  // display fields, while raw payloadMeta can also carry snake-case
  // exit/error fields from provider-specific paths.
  let completionStatus = $derived(
    deriveCompletionStatus(effectiveStatusItem, {
      meta: statusMeta ?? (meta as unknown as Record<string, unknown> | undefined),
    }),
  );
  let commandError = $derived.by(() => {
    if (completionStatus !== 'failure') return rowErrorForStatus(effectiveStatusItem.status, 'Command failed');
    const statusError = rowErrorForStatus(effectiveStatusItem.status, 'Command failed');
    if (statusError && effectiveStatusItem.status !== 'errored') return statusError;
    const error = commandErrorForItem(item, meta, itemMeta, statusMeta ?? payloadMeta);
    return { tone: 'error' as const, ...error };
  });
  let indicatorState = $derived(
    indicatorStateForItem(effectiveStatusItem, {
      meta: statusMeta ?? (meta as unknown as Record<string, unknown> | undefined),
    }),
  );
  // Dev-server affordance (internal/triage/dev_server_url.go). Streaming
  // payload meta is rebuilt from each 100ms flush window, so a startup
  // banner is only present in the window that carried it — the chip would
  // blink out the moment the server logged its first request. The
  // completion rebuild recomputes the field over the cumulative output and
  // persists it; between those two points the row keeps the first
  // detection. This is last-known-value smoothing of a jittering SERVER
  // field, not remembered reader intent, which is why it lives here rather
  // than in a pane registry: a windowing remount re-reads meta and the
  // persisted value takes over at settle, so nothing is durably lost.
  let detectedDevServerURL = $state<string | null>(null);
  let liveDevServerURL = $derived(
    loopbackDevServerURL(meta?.devServerUrl ?? (payloadMeta?.devServerUrl as string | undefined)),
  );
  $effect(() => {
    const next = liveDevServerURL;
    if (next && next !== untrack(() => detectedDevServerURL)) detectedDevServerURL = next;
  });

  let compactCollapsedPreview = $derived.by(() => {
    const normalized = collapsedPreview.replace(/\s+/g, ' ').trim();
    if (normalized.length <= 160) return normalized;
    return `${normalized.slice(0, 160).trimEnd()}...`;
  });

  keepExpandedPayloadFresh(() => expansion, () => hasPayload);
</script>

<div class="group/tool overflow-hidden" data-testid="command-output-row">
  {#snippet headerIcon()}
    <span data-testid="command-output-icon"><ToolKindIcon kind="terminal" ariaLabel="bash" /></span>
  {/snippet}

  {#snippet headerLabel()}
    <span data-testid="command-output-label">bash</span>
  {/snippet}

  {#snippet headerBody()}
    <span
      class="min-w-0 flex-1 truncate font-mono text-[0.75rem] text-fg-muted"
      title={displayCommand || undefined}
      data-testid="command-output-command"
    >
      {displayCommand}
    </span>
  {/snippet}

  {#snippet headerActions()}
    {#if detectedDevServerURL}
      <DevServerChip url={detectedDevServerURL} />
    {/if}
    <ToolDecisionChip decision={effectiveDisplayItem.decision} />
    <ToolHeaderMeta
      statusSlotTestId="command-output-status-slot"
      duration={{
        testId: 'command-output-duration',
        label: durationLabel || (completedDurationMs !== null ? formatDurationMs(completedDurationMs) : ticker.label),
      }}
      timestamp={showTimestamp
        ? { testId: 'command-output-time', value: effectiveStatusItem.createdAt, label: time }
        : undefined}
      {trailingActions}
    >
      {#snippet status()}
        <ToolRowStatusIndicator item={effectiveStatusItem} state={indicatorState} testId="command-output-status" />
      {/snippet}
    </ToolHeaderMeta>
  {/snippet}

  <!-- Header -->
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasBody}
    controls={hasBody ? outputDomId : undefined}
    ariaLabel={`Toggle Command Output: ${displayCommand}`}
    testId="command-output-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 text-[0.75rem] {hasBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, () => expansion.toggle())}
  >
    {#snippet icon()}{@render headerIcon()}{/snippet}
    {#snippet label()}{@render headerLabel()}{/snippet}
    {#snippet body()}{@render headerBody()}{/snippet}
    {#snippet actions()}
      {@render headerActions()}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if commandError}
    <div class="ml-[5.25rem] px-3 pb-1" data-testid="command-output-error">
      <RowError tone={commandError.tone} code={commandError.code} msg={commandError.msg} />
    </div>
  {/if}
  {#if compactCollapsedPreview && !expansion.expanded}
    <div class="ml-5 truncate px-3 pb-1 text-[0.6875rem] text-fg-subtle" data-testid="command-output-preview">
      └ {compactCollapsedPreview}
    </div>
  {/if}

  <!-- Output content -->
  {#if hasBody && expansion.expanded}
    <div id={outputDomId} class="ml-5 border-l border-border-subtle bg-surface-0/35">
      <div class="max-h-96 overflow-auto px-3 py-2" use:nestedScroll>
        {#if hasPayload && expansion.loading}
          <p class="text-[0.6875rem] text-fg-subtle" role="status" aria-live="polite">Loading output…</p>
        {:else if hasPayload && expansion.error}
          <div class="space-y-2">
            <p class="text-[0.6875rem] text-error" role="alert">Failed to load output: {expansion.error}</p>
            <button
              type="button"
              class="text-[0.6875rem] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded"
              onclick={() => expansion.retry()}
              data-testid="command-output-retry"
            >
              Retry
            </button>
          </div>
        {:else if hasPayload}
          <AnsiText source={expansion.displayData ?? ''} class="text-[0.6875rem] whitespace-pre text-fg-muted leading-relaxed" />
          {#if expansion.hasMore}
            <button
              type="button"
              class="mt-2 inline-flex items-center rounded-[var(--radius-control)] border border-border-subtle px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
              onclick={(event) => preservePaneScrollAnchor(pane, event, () => expansion.showFull())}
              data-testid="command-output-show-full"
            >
              Show more output ({formatPayloadSize(expansion.totalSize)})
            </button>
          {/if}
        {:else if deferredOutputState === 'loading'}
          <p class="text-[0.6875rem] text-fg-subtle animate-pulse" role="status" aria-live="polite">
            Loading…
          </p>
        {:else if deferredOutputState === 'error'}
          <p class="text-[0.6875rem] text-error" role="alert">
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
