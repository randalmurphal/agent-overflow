<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import CopyFooter from './CopyFooter.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import type { Item } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import {
    completionBadgeTitleForStatus,
    deriveCompletionStatus,
  } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { decodeToolCardPreview, toolCardInputPreview } from './toolCardPreview';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import ClaudeSubagentTranscript from './ClaudeSubagentTranscript.svelte';
  import EditorLink from '../common/EditorLink.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { isCodexSubagentLaunchItem } from '../../utils/subagentLaunch';
  import { parseClaudeSubagentTranscript } from '../../utils/claudeSubagentTranscript';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentLabel,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';

  // Threshold (ms) at which the running row starts displaying elapsed
  // time in the duration slot. Sub-2s tools (most Read/Edit/Write/etc.)
  // would just churn the digits up to "1s" and finish before anyone
  // could read it; gating on >=2s keeps the transcript quiet for the
  // common case while still surfacing progress on slow Bash/MultiEdit/
  // network-bound tools.
  const RUNNING_ELAPSED_THRESHOLD_MS = 2_000;

  let {
    pane,
    item,
    displayItem,
    statusItem,
    durationLabel = '',
    showTimestamp = true,
    trailingActions,
  }: {
    pane?: ThreadPane;
    item: Item;
    /** Item used for the tool label/input preview. */
    displayItem?: Item;
    /** Item used for running/completion status. */
    statusItem?: Item;
    /** Optional duration/elapsed label rendered in the metadata slot. */
    durationLabel?: string;
    /** Tray surfaces show elapsed time instead of the absolute transcript timestamp. */
    showTimestamp?: boolean;
    /** Optional actions rendered outside the disclosure button. */
    trailingActions?: Snippet;
  } = $props();
  let effectiveDisplayItem = $derived(displayItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);

  let classification = $derived(classifyToolName(effectiveDisplayItem.toolName ?? effectiveDisplayItem.summary));
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

  let summaryMeta = $derived(parseJsonObject(effectiveDisplayItem.payloadMeta));
  let displayMeta = $derived(parseJsonObject(effectiveDisplayItem.meta));
  let itemMeta = $derived(parseJsonObject(item.meta));
  let statusMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));

  let time = $derived(
    new Date(effectiveStatusItem.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  // Claude Agent rows reuse SubagentGroup's title-case label, model
  // affix, and description treatment via the shared util so the
  // chat/tray surface matches the inline card. Non-Agent rows
  // short-circuit through isClaudeAgent.
  let isClaudeAgent = $derived((effectiveDisplayItem.toolName ?? '') === 'Agent');
  let agentInputObject = $derived(
    isClaudeAgent ? readClaudeSubagentInput(summaryMeta, displayMeta) : null,
  );
  let subagentLabel = $derived(
    isClaudeAgent ? deriveClaudeSubagentLabel(agentInputObject, 'Agent') : '',
  );
  let subagentModelLabel = $derived(
    isClaudeAgent ? deriveClaudeSubagentModelLabel(agentInputObject, displayMeta, 'Agent') : '',
  );
  let subagentDescription = $derived(
    isClaudeAgent ? deriveClaudeSubagentDescription(agentInputObject) : '',
  );
  // subagentLabel is always non-empty when isClaudeAgent (the helper
  // falls back through subagent_type → "Agent"), so the gate is just
  // isClaudeAgent.
  let displayLabel = $derived(isClaudeAgent ? subagentLabel : classification.label);

  let inputPreview = $derived.by<string>(() => {
    if (isClaudeAgent && subagentDescription) return subagentDescription;
    return toolCardInputPreview(effectiveDisplayItem, classification, summaryMeta, displayMeta);
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

  // Backgrounded launch rows are stable transcript records. Regular
  // backgrounded tools keep the `…` affordance for the lifetime of the
  // row; Codex spawn_agent rows are informational child-thread markers,
  // so their running state lives in the tray instead.
  let isBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );
  let isCodexSubagentLaunch = $derived(isCodexSubagentLaunchItem(effectiveStatusItem));

  let runningLabel = $derived.by<string | null>(() => {
    if (isCodexSubagentLaunch) return null;
    if (isBackgroundedLaunch) return '…';
    if (effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming') return 'running';
    return null;
  });

  let completionStatus = $derived(deriveCompletionStatus(effectiveStatusItem, { meta: statusMeta }));
  let completionTitle = $derived(completionBadgeTitleForStatus(effectiveStatusItem.status));
  // Backgrounded launch rows are stable transcript records — they keep
  // the `…` affordance for the row's lifetime, but a live wall-clock
  // ticker in chat history is misleading (the user can't act on it
  // from the chat row; the tray is the live-status surface). Suppress
  // the internal ticker; the launch shows just its timestamp while
  // running, the eventual completion appears as its own sibling row
  // with its own durationMs. The tray surface passes a non-empty
  // `durationLabel` so its live elapsed display is unaffected.
  let shouldTickElapsed = $derived(
    runningLabel !== null && durationLabel === '' && !isBackgroundedLaunch,
  );

  // Wall-clock ticker, paused when the row isn't running. Mirrors the
  // SubagentGroup elapsed-display pattern: the interval clears on
  // running → done (because the effect re-runs when runningLabel
  // flips) and on virtua remount (effect cleanup). Only one timer per
  // visible running row; rows scrolled past `bufferSize=900` stop
  // ticking entirely.
  let now = $state(Date.now());
  $effect(() => {
    if (!shouldTickElapsed) return;
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
    if (!shouldTickElapsed) return '';
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
  let subagentTranscriptEntries = $derived.by(() => {
    if (item.toolName !== 'Agent') return null;
    const data = expansion.displayData;
    if (data === null) return null;
    return parseClaudeSubagentTranscript(data);
  });

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
    {displayLabel}{#if subagentModelLabel}<span class="ml-1 text-fg-hint normal-case tracking-normal" data-testid="tool-call-card-model">({subagentModelLabel})</span>{/if}
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
  <ToolHeaderMeta
    statusSlotTestId="tool-call-card-status-slot"
    duration={{
      testId: 'tool-call-card-duration',
      label: durationLabel || (durationMs !== null ? formatDuration(durationMs) : runningElapsedLabel),
    }}
    timestamp={showTimestamp
      ? { testId: 'tool-call-card-time', value: effectiveStatusItem.createdAt, label: time }
      : undefined}
    {trailingActions}
  >
    {#snippet status()}
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
          title={completionTitle}
          class="opacity-80 transition-opacity group-hover/tool:opacity-100"
        />
      {/if}
    {/snippet}
  </ToolHeaderMeta>
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
        {#if subagentTranscriptEntries !== null}
          <div class="max-h-80 overflow-auto" data-testid="tool-call-card-output">
            <ClaudeSubagentTranscript entries={subagentTranscriptEntries} />
          </div>
        {:else}
          <div
            class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[11px] leading-relaxed text-fg-muted"
            data-testid="tool-call-card-output"
          >
            <AnsiText source={expansion.displayData} />
          </div>
        {/if}
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
