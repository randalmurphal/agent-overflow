<script lang="ts">
  import { untrack } from 'svelte';
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { decodeToolCardPreview, toolCardInputPreview } from './toolCardPreview';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import EditorLink from '../common/EditorLink.svelte';

  let { pane, item }: { pane?: ThreadPane; item: Item } = $props();

  let classification = $derived(classifyToolName(item.toolName ?? item.summary));
  // Use the pane's per-itemId registry when available so expand/collapse
  // and loaded chunks survive virtua's overscan eviction. Falls back to
  // local state when rendered outside a pane (e.g. unit tests or surfaces
  // that haven't been plumbed yet). The registry reads through to the
  // live Item, so payload_id enrichment is picked up without a reset.
  // pane is stable across a row's lifetime; intentionally read once via
  // `untrack` so the local fallback is created exactly when needed.
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

  let summaryMeta = $derived(parseJsonObject(item.payloadMeta));
  let itemMeta = $derived(parseJsonObject(item.meta));

  let time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  let inputPreview = $derived.by<string>(() => {
    return toolCardInputPreview(item, classification, summaryMeta, itemMeta);
  });

  // When the preview leads with a path, surface a sibling EditorLink
  // so the row is launchable without forcing the user to expand it.
  // The detection is a leading-only match — see decodeToolCardPreview
  // for why we don't linkify mid-sentence path tokens.
  let previewDecoded = $derived(decodeToolCardPreview(inputPreview));

  let durationMs = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  // Backgrounded launch rows are stable transcript records — they keep
  // the `…` affordance for the lifetime of the row regardless of any
  // status drift. The actual completion lands as a sibling
  // tool_completion row that runs through `deriveCompletionStatus` and
  // renders the unified badge.
  let isBackgroundedLaunch = $derived(
    item.kind === 'tool_call' && item.isBackground === true,
  );

  let runningLabel = $derived.by<string | null>(() => {
    if (isBackgroundedLaunch) return '…';
    if (item.status === 'running' || item.status === 'streaming') return 'running';
    return null;
  });

  let completionStatus = $derived(deriveCompletionStatus(item, { meta: summaryMeta }));

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

  let hasExpandableBody = $derived(
    Boolean(item.payloadId) || deferredOutputState === 'loading' || deferredOutputState === 'error',
  );

  keepExpandedPayloadFresh(
    () => expansion,
    () => Boolean(item.payloadId),
  );

  function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    const seconds = ms / 1000;
    if (seconds < 60) return `${seconds.toFixed(1)}s`;
    const minutes = Math.floor(seconds / 60);
    const remSec = Math.round(seconds - minutes * 60);
    return `${minutes}m ${remSec}s`;
  }

  async function toggle() {
    await expansion.toggle();
  }

  function handleKeydown(evt: KeyboardEvent) {
    if (evt.key === 'Enter' || evt.key === ' ') {
      evt.preventDefault();
      toggle();
    }
  }
</script>

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
  <ToolKindIcon kind={classification.icon} ariaLabel={classification.label} />
  <span class="text-[11px] font-medium text-fg-muted shrink-0 uppercase tracking-[0.04em]" data-testid="tool-call-card-label">
    {classification.label}
  </span>
  <span class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75" data-testid="tool-call-card-preview">
    {inputPreview}
  </span>
  {#if previewDecoded.path}
    <EditorLink
      path={previewDecoded.path.path}
      line={previewDecoded.path.line ?? 0}
      col={previewDecoded.path.col ?? 0}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100"
    />
  {/if}
  <ToolDecisionChip decision={item.decision} />
  {#if runningLabel !== null}
    <span
      class="shrink-0 text-[10px] text-accent opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid="tool-call-card-status"
      data-status={item.status}
      title={isBackgroundedLaunch ? 'Running in background' : undefined}
      aria-label={isBackgroundedLaunch ? 'Backgrounded' : undefined}
    >
      {runningLabel}
    </span>
  {/if}
  {#if durationMs !== null}
    <span
      class="shrink-0 tabular-nums text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid="tool-call-card-duration"
    >
      {formatDuration(durationMs)}
    </span>
  {/if}
  {#if runningLabel === null && completionStatus !== null}
    <CompletionBadge
      status={completionStatus}
      class="opacity-80 transition-opacity group-hover/tool:opacity-100"
    />
  {/if}
  <time
    class="shrink-0 tabular-nums text-[10px] text-fg-hint"
    datetime={new Date(item.createdAt).toISOString()}
    data-testid="tool-call-card-time"
  >
    {time}
  </time>
{/snippet}

<div
  class="group/tool mb-1.5 overflow-hidden"
  data-testid="tool-call-card"
  data-tool-kind={classification.icon}
>
  {#if hasExpandableBody}
    <button
      type="button"
      class="flex w-full items-center gap-2 rounded-[var(--radius-control)] px-1 py-1 text-left hover:bg-surface-2/20 cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors"
      onclick={toggle}
      onkeydown={handleKeydown}
      aria-expanded={expansion.expanded}
      aria-controls="tool-call-card-body-{item.id}"
      data-testid="tool-call-card-toggle"
    >
      {@render headerContent(true)}
    </button>
  {:else}
    <div
      class="flex w-full items-center gap-2 rounded-[var(--radius-control)] px-1 py-1 text-left"
      data-testid="tool-call-card-row"
    >
      {@render headerContent(false)}
    </div>
  {/if}

  {#if hasExpandableBody && expansion.expanded}
    <div
      id="tool-call-card-body-{item.id}"
      transition:slide={{ duration: 150 }}
      class="ml-5 border-l border-border-subtle bg-surface-0/35"
      data-testid="tool-call-card-body"
    >
      {#if expansion.loading}
        <p class="px-3 py-2 text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">
          Loading…
        </p>
      {:else if expansion.error}
        <div class="space-y-2 px-3 py-2">
          <p class="text-[11px] text-error" role="alert">
            Failed to load: {expansion.error}
          </p>
          <button
            type="button"
            class="text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.retry()}
            data-testid="tool-call-card-retry"
          >
            Retry
          </button>
        </div>
      {:else if expansion.displayData !== null}
        <div
          class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[11px] leading-relaxed text-fg-muted"
          data-testid="tool-call-card-output"
        >
          <AnsiText source={expansion.displayData} />
        </div>
        {#if expansion.hasMore}
          <button
            type="button"
            class="mx-3 mb-3 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.showFull()}
            data-testid="tool-call-card-show-full"
          >
            Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
        {#if expansion.displayData}
          <CopyFooter text={expansion.displayData} label="Copy output" />
        {/if}
      {:else if deferredOutputState === 'loading'}
        <p class="px-3 py-2 text-[11px] text-fg-subtle animate-pulse" role="status" aria-live="polite">
          Loading…
        </p>
      {:else if deferredOutputState === 'error'}
        <p class="px-3 py-2 text-[11px] text-error" role="alert">
          Failed to load: {deferredOutputError || 'Background output could not be loaded.'}
        </p>
      {:else}
        <p class="px-3 py-2 text-[11px] text-fg-subtle italic">
          No stored payload for this tool result.
        </p>
      {/if}
    </div>
  {/if}
</div>
