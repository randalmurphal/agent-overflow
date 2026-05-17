<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import { type ThreadPane } from '../../stores/thread.svelte';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { toolCardInputPreview } from './toolCardPreview';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import ClaudeSubagentTranscript from './ClaudeSubagentTranscript.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatElapsedSeconds } from '../../utils/format';
  import { parseClaudeSubagentTranscript } from '../../utils/claudeSubagentTranscript';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentLabel,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import Indicator from './Indicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorAriaLabel, indicatorStateForItem, rowErrorForStatus } from './rowState';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

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
    displayItem?: Item;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    trailingActions?: Snippet;
  } = $props();

  let effectiveDisplayItem = $derived(displayItem ?? item);
  let effectiveStatusItem = $derived(statusItem ?? item);
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
  let classification = $derived(classifyToolName(effectiveDisplayItem.toolName ?? 'Agent'));
  let agentToolName = $derived(effectiveDisplayItem.toolName === 'Task' ? 'Agent' : (effectiveDisplayItem.toolName ?? 'Agent'));
  let agentInputObject = $derived(readClaudeSubagentInput(summaryMeta, displayMeta));
  let agentLabel = $derived(deriveClaudeSubagentLabel(agentInputObject, agentToolName));
  let modelLabel = $derived(deriveClaudeSubagentModelLabel(agentInputObject, displayMeta, agentToolName));
  let description = $derived(deriveClaudeSubagentDescription(agentInputObject));
  let inputPreview = $derived(description || toolCardInputPreview(effectiveDisplayItem, summaryMeta, displayMeta));
  let time = $derived(
    new Date(effectiveStatusItem.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );

  let isBackgroundedLaunch = $derived(effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true);
  let isRunning = $derived(effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming');
  let shouldTickElapsed = $derived(isRunning && durationLabel === '' && !isBackgroundedLaunch);
  let now = $state(Date.now());
  $effect(() => {
    if (!shouldTickElapsed) return;
    now = Date.now();
    const id = setInterval(() => {
      now = Date.now();
    }, 1_000);
    return () => clearInterval(id);
  });

  let runningElapsedLabel = $derived.by<string>(() => {
    if (!shouldTickElapsed) return '';
    if (!Number.isFinite(item.createdAt) || item.createdAt <= 0) return '';
    const elapsedMs = now - item.createdAt;
    if (elapsedMs < RUNNING_ELAPSED_THRESHOLD_MS) return '';
    return formatElapsedSeconds(Math.floor(elapsedMs / 1_000));
  });
  let durationMs = $derived.by<number | null>(() => {
    const d = summaryMeta?.durationMs;
    return typeof d === 'number' && d >= 0 ? d : null;
  });
  let deferredOutputState = $derived.by(() => {
    const state = itemMeta?.notification_output_state ?? itemMeta?.output_file_state;
    return typeof state === 'string' ? state : '';
  });
  let deferredOutputError = $derived.by(() => {
    const error = itemMeta?.notification_output_error ?? itemMeta?.output_file_error;
    return typeof error === 'string' ? error : '';
  });
  let hasExpandableBody = $derived(Boolean(item.payloadId) || deferredOutputState === 'loading' || deferredOutputState === 'error');
  let completionStatus = $derived(deriveCompletionStatus(effectiveStatusItem, { meta: statusMeta }));
  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusMeta }));
  let rowError = $derived.by(() => {
    if (completionStatus !== 'failure') return null;
    return rowErrorForStatus(effectiveStatusItem.status, 'Agent failed') ?? { tone: 'error' as const, msg: 'Agent failed' };
  });
  let transcriptEntries = $derived.by(() => {
    if (expansion.displayData === null) return null;
    return parseClaudeSubagentTranscript(expansion.displayData);
  });

  keepExpandedPayloadFresh(() => expansion, () => Boolean(item.payloadId));

  function formatDuration(ms: number): string {
    if (ms < 1000) return `${ms}ms`;
    const seconds = ms / 1000;
    if (seconds < 60) return `${seconds.toFixed(1)}s`;
    const minutes = Math.floor(seconds / 60);
    const remSec = Math.round(seconds - minutes * 60);
    return `${minutes}m ${remSec}s`;
  }
</script>

<div class="group/tool overflow-hidden" data-testid="agent-row" data-tool-kind="robot">
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasExpandableBody}
    controls={hasExpandableBody ? `agent-row-body-${item.id}` : undefined}
    testId="agent-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 {hasExpandableBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, () => expansion.toggle())}
  >
    {#snippet icon()}<ToolKindIcon kind="robot" ariaLabel="agent" />{/snippet}
    {#snippet label()}<span data-testid="agent-row-label">agent</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75" data-testid="agent-row-preview">
        <span class="text-fg-muted">{agentLabel}</span>{#if modelLabel}<span class="ml-1 text-fg-hint">({modelLabel})</span>{/if}{#if inputPreview}<span class="ml-2">{inputPreview}</span>{/if}
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolDecisionChip decision={item.decision} />
      <ToolHeaderMeta
        statusSlotTestId="agent-row-status-slot"
        duration={{
          testId: 'agent-row-duration',
          label: durationLabel || (durationMs !== null ? formatDuration(durationMs) : runningElapsedLabel),
        }}
        timestamp={showTimestamp ? { testId: 'agent-row-time', value: effectiveStatusItem.createdAt, label: time } : undefined}
        {trailingActions}
      >
        {#snippet status()}
          {#if indicatorState}
            <span
              data-testid="agent-row-status"
              data-status={effectiveStatusItem.status}
              data-state={indicatorState}
              aria-label={indicatorAriaLabel(indicatorState)}
            >
              <Indicator state={indicatorState} />
            </span>
          {/if}
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}

  {#if hasExpandableBody && expansion.expanded}
    <ExpandablePayloadBody
      {pane}
      {expansion}
      id="agent-row-body-{item.id}"
      testPrefix="agent-row"
      emptyMessage="No stored payload for this agent."
      {deferredOutputState}
      {deferredOutputError}
      renderContent={agentBodyContent}
    />
  {/if}
</div>

{#snippet agentBodyContent({ data, testId }: { data: string; testId: string })}
  {#if transcriptEntries !== null}
    <div class="max-h-80 overflow-auto" data-testid={testId}>
      <ClaudeSubagentTranscript entries={transcriptEntries} />
    </div>
  {:else}
    <div
      class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[11px] leading-relaxed text-fg-muted"
      data-testid={testId}
    >
      <AnsiText source={data} />
    </div>
  {/if}
{/snippet}
