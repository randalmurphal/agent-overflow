<script lang="ts">
  import { untrack } from 'svelte';
  import CopyFooter from './CopyFooter.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import type { Item } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
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
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatElapsedSeconds } from '../../utils/format';

  // Threshold (ms) at which the running row starts displaying elapsed
  // time in the duration slot. Sub-2s tools (most Read/Edit/Write/etc.)
  // would just churn the digits up to "1s" and finish before anyone
  // could read it; gating on >=2s keeps the transcript quiet for the
  // common case while still surfacing progress on slow Bash/MultiEdit/
  // network-bound tools.
  const RUNNING_ELAPSED_THRESHOLD_MS = 2_000;

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

  // Wall-clock ticker, paused when the row isn't running. Mirrors the
  // SubagentGroup elapsed-display pattern: the interval clears on
  // running → done (because the effect re-runs when runningLabel
  // flips) and on virtua remount (effect cleanup). Only one timer per
  // visible running row; rows scrolled past `bufferSize=900` stop
  // ticking entirely.
  let now = $state(Date.now());
  $effect(() => {
    if (runningLabel === null) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  // Elapsed seconds string while the tool is running, gated on
  // RUNNING_ELAPSED_THRESHOLD_MS so quick tools don't flash a "1s"
  // before completing. Empty when running but under threshold so the
  // reserved-width duration slot stays visually empty in that window.
  let runningElapsedLabel = $derived.by<string>(() => {
    if (runningLabel === null) return '';
    const start = item.createdAt;
    if (!Number.isFinite(start) || start <= 0) return '';
    const elapsedMs = now - start;
    if (elapsedMs < RUNNING_ELAPSED_THRESHOLD_MS) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
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

  // TaskOutput is Claude's "retrieve background-task output" tool. Its
  // tool_result body is an XML envelope wrapping the same stdout/stderr
  // already shown on the originating Bash row, so the dropdown is
  // redundant noise. Render the row collapsed-only — the completion
  // badge and timestamp still convey "model checked the output".
  let suppressBodyExpansion = $derived(item.toolName === 'TaskOutput');

  let hasExpandableBody = $derived(
    !suppressBodyExpansion &&
      (Boolean(item.payloadId) ||
        deferredOutputState === 'loading' ||
        deferredOutputState === 'error'),
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

</script>

{#snippet headerContent()}
  <ToolKindIcon kind={classification.icon} ariaLabel={classification.label} />
  <span class="text-[11px] font-medium text-fg-muted shrink-0 uppercase tracking-[0.04em]" data-testid="tool-call-card-label">
    {classification.label}
  </span>
  <span class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75" data-testid="tool-call-card-preview">
    {inputPreview}
  </span>
{/snippet}

{#snippet headerActions()}
  {#if previewDecoded.path}
    <EditorLink
      path={previewDecoded.path.path}
      line={previewDecoded.path.line ?? 0}
      col={previewDecoded.path.col ?? 0}
      workspacePath={paneWorkspacePath(pane)}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100"
    />
  {/if}
  <ToolDecisionChip decision={item.decision} />
  <!-- Reserved-width status slot. Mirrors SubagentGroup so the running
       label and the completion badge swap without shifting the duration
       and time chrome to their right on the running → done transition.
       `min-w-[3.5rem]` covers the wider of the two ("running" text);
       `justify-end` anchors both variants to the slot's right edge so
       the visible right-of-status boundary stays put. -->
  <span
    class="inline-flex shrink-0 items-center justify-end min-w-[3.5rem]"
    data-testid="tool-call-card-status-slot"
  >
    {#if runningLabel !== null}
      {#if isBackgroundedLaunch}
        <span
          class="text-[20px] leading-none text-accent opacity-90 transition-opacity group-hover/tool:opacity-100"
          data-testid="tool-call-card-status"
          data-status={item.status}
          title="Running in background"
          aria-label="Backgrounded"
        >
          …
        </span>
      {:else}
        <span
          class="text-[10px] text-accent opacity-70 transition-opacity group-hover/tool:opacity-100"
          data-testid="tool-call-card-status"
          data-status={item.status}
        >
          {runningLabel}
        </span>
      {/if}
    {:else if completionStatus !== null}
      <CompletionBadge
        status={completionStatus}
        class="opacity-80 transition-opacity group-hover/tool:opacity-100"
      />
    {/if}
  </span>
  <!-- Always-rendered duration slot. While running, shows wall-clock
       elapsed once the tool has been alive for >= 2s (sub-2s tools
       complete before the digits would update). On completion, swaps
       to the provider-stamped exact `summaryMeta.durationMs` so the
       transcript shows precise final duration. The reserved width
       keeps the slot from materializing on completion and shoving
       the time chip leftward. -->
  <span
    class="shrink-0 inline-block min-w-[3rem] text-right tabular-nums text-[10px] text-fg-hint opacity-70 transition-opacity group-hover/tool:opacity-100"
    data-testid="tool-call-card-duration"
  >
    {durationMs !== null ? formatDuration(durationMs) : runningElapsedLabel}
  </span>
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
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasExpandableBody}
    controls={hasExpandableBody ? `tool-call-card-body-${item.id}` : undefined}
    testId="tool-call-card-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 {hasExpandableBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={() => toggle()}
  >
    {@render headerContent()}
    {#snippet actions()}
      {@render headerActions()}
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if hasExpandableBody && expansion.expanded}
    <div
      id="tool-call-card-body-{item.id}"
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
