<script lang="ts">
  import { slide } from 'svelte/transition';
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import type { Item } from '../../types/models';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';

  let { item }: { item: Item } = $props();

  let classification = $derived(classifyToolName(item.toolName ?? item.summary));
  const expansion = createPayloadExpansion(() => item.payloadId, () => item.threadId);

  $effect(() => {
    item.id;
    item.payloadId;
    expansion.reset();
  });

  function parseObject(raw: string | undefined): Record<string, unknown> | null {
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (parsed && typeof parsed === 'object') return parsed as Record<string, unknown>;
    } catch {
      return null;
    }
    return null;
  }

  let summaryMeta = $derived(parseObject(item.payloadMeta));
  let itemMeta = $derived(parseObject(item.meta));

  let time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  let inputPreview = $derived.by<string>(() => {
    const fromSummary = (item.summary ?? '').trim();
    if (fromSummary) return fromSummary;
    if (summaryMeta) {
      const title = summaryMeta.title;
      if (typeof title === 'string' && title.trim()) return title.trim();
    }
    return classification.displayName;
  });

  let exitCode = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const code = summaryMeta.exitCode;
    return typeof code === 'number' ? code : null;
  });

  let durationMs = $derived.by<number | null>(() => {
    if (!summaryMeta) return null;
    const d = summaryMeta.durationMs;
    if (typeof d === 'number' && d >= 0) return d;
    return null;
  });

  let isBackgroundedRunning = $derived(
    item.kind === 'tool_call' && item.isBackground === true && item.status === 'running',
  );

  let statusLabel = $derived.by(() => {
    if (isBackgroundedRunning) return '…';
    if (item.status === 'running' || item.status === 'streaming') return 'running';
    if (item.status === 'errored') return 'failed';
    if (item.status === 'killed') return 'stopped';
    return 'done';
  });

  let statusClass = $derived.by(() => {
    if (item.status === 'running' || item.status === 'streaming') return 'text-accent';
    if (item.status === 'errored') return 'text-error';
    if (item.status === 'killed') return 'text-text-secondary';
    return 'text-success';
  });

  let exitBadgeClass = $derived.by(() => {
    if (exitCode === null) return '';
    return exitCode === 0 ? 'bg-success/20 text-success' : 'bg-error/20 text-error';
  });

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
  <ToolDecisionChip decision={item.decision} />
  <span
    class="shrink-0 text-[10px] {statusClass} opacity-70 transition-opacity group-hover/tool:opacity-100"
    data-testid="tool-call-card-status"
    data-status={item.status}
    title={isBackgroundedRunning ? 'Running in background' : undefined}
    aria-label={isBackgroundedRunning ? 'Backgrounded' : undefined}
  >
    {statusLabel}
  </span>
  {#if exitCode !== null}
    <span
      class="shrink-0 rounded-[var(--radius-field)] px-1.5 py-0.5 text-[10px] font-medium {exitBadgeClass} opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid="tool-call-card-exit"
    >
      exit {exitCode}
    </span>
  {:else if durationMs !== null}
    <span
      class="shrink-0 tabular-nums text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
      data-testid="tool-call-card-duration"
    >
      {formatDuration(durationMs)}
    </span>
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
        <p class="px-3 py-2 text-[11px] text-error" role="alert">
          Failed to load: {expansion.error}
        </p>
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
