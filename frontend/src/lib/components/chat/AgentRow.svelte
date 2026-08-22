<script lang="ts">
  // Compact agent LEAF row: the background tray's agent entry and the two
  // page-boundary shapes (a depth-cap flattened launch, a completion
  // sibling whose launch is outside the loaded window). The inline
  // timeline renders launches as the shared agent card (SubagentGroup)
  // instead — this row is deliberately header-only.
  //
  // The old expandable body that parsed the payload JSONL back into a
  // pseudo-transcript (ClaudeSubagentTranscript) is DELETED
  // (docs/specs/agent-visibility.md migration table, "ack-text
  // rendering"): the agent's transcript now exists as real rows under the
  // launch — streamed live or backfilled from the task_notification's
  // output_file — and renders in the card body and the agent pane. A
  // failed output-file read/backfill still surfaces here as an inline
  // error line, because a silently incomplete transcript reads exactly
  // like a complete one.

  import type { Snippet } from 'svelte';
  import type { Item } from '../../types/models';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import {
    deriveClaudeSubagentDescription,
    deriveClaudeSubagentLabel,
    deriveClaudeSubagentModelLabel,
    readClaudeSubagentInput,
  } from '../../utils/claudeSubagentLabel';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import RowError from './RowError.svelte';
  import { indicatorStateForItem, rowErrorWithFallback } from './rowState';
  import { createRunningElapsed } from './useRunningElapsed.svelte';

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
  let summaryMeta = $derived(parseJsonObject(effectiveDisplayItem.payloadMeta));
  let displayMeta = $derived(parseJsonObject(effectiveDisplayItem.meta));
  let itemMeta = $derived(parseJsonObject(item.meta));
  let statusMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));
  let agentToolName = $derived(effectiveDisplayItem.toolName === 'Task' ? 'Agent' : (effectiveDisplayItem.toolName ?? 'Agent'));
  let agentInputObject = $derived(readClaudeSubagentInput(summaryMeta, displayMeta));
  let agentLabel = $derived(deriveClaudeSubagentLabel(agentInputObject, agentToolName));
  let modelLabel = $derived(deriveClaudeSubagentModelLabel(agentInputObject, displayMeta, agentToolName));
  let description = $derived(deriveClaudeSubagentDescription(agentInputObject));
  let inputPreview = $derived(
    description ||
      presentToolCardInputPreview(effectiveDisplayItem, summaryMeta, displayMeta, paneWorkspacePath(pane)).text,
  );
  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  let isBackgroundedLaunch = $derived(effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true);
  let isRunning = $derived(effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming');
  const ticker = createRunningElapsed(
    () => isRunning && durationLabel === '' && !isBackgroundedLaunch,
    () => item.createdAt,
  );
  let durationMs = $derived.by<number | null>(() => {
    const d = summaryMeta?.durationMs;
    return typeof d === 'number' && d >= 0 ? d : null;
  });
  // The output-file / transcript-backfill state triage stamped on this row.
  // Only the ERROR is rendered — 'loading' and 'loaded' describe payload
  // availability the card and pane own now.
  let deferredOutputError = $derived.by(() => {
    const state = itemMeta?.notification_output_state ?? itemMeta?.output_file_state;
    if (state !== 'error') return '';
    const error = itemMeta?.notification_output_error ?? itemMeta?.output_file_error;
    return typeof error === 'string' && error ? error : 'Task output could not be read.';
  });
  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusMeta }));
  let rowError = $derived(
    rowErrorWithFallback(effectiveStatusItem, { meta: statusMeta, fallback: 'Agent failed' }),
  );
</script>

<div class="group/tool overflow-hidden" data-testid="agent-row" data-tool-kind="robot">
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="agent-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1"
  >
    {#snippet icon()}<ToolKindIcon kind="robot" ariaLabel="agent" />{/snippet}
    {#snippet label()}<span data-testid="agent-row-label">agent</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[0.75rem] text-fg-muted/75" data-testid="agent-row-preview">
        <span class="text-fg-muted">{agentLabel}</span>{#if modelLabel}<span class="ml-1 text-fg-hint">({modelLabel})</span>{/if}{#if inputPreview}<span class="ml-2">{inputPreview}</span>{/if}
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolDecisionChip decision={item.decision} />
      <ToolHeaderMeta
        statusSlotTestId="agent-row-status-slot"
        duration={{
          testId: 'agent-row-duration',
          label: durationLabel || (durationMs !== null ? formatDurationMs(durationMs) : ticker.label),
        }}
        timestamp={showTimestamp ? { testId: 'agent-row-time', value: effectiveStatusItem.createdAt, label: time } : undefined}
        {trailingActions}
      >
        {#snippet status()}
          <ToolRowStatusIndicator item={effectiveStatusItem} state={indicatorState} testId="agent-row-status" />
        {/snippet}
      </ToolHeaderMeta>
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if rowError}
    <div class="ml-[5.25rem] px-3 pb-1">
      <RowError tone={rowError.tone} msg={rowError.msg} />
    </div>
  {/if}
  {#if deferredOutputError}
    <div class="ml-[5.25rem] px-3 pb-1" data-testid="agent-row-output-error">
      <RowError tone="error" msg={deferredOutputError} />
    </div>
  {/if}
</div>
