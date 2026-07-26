<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import type { Item } from '../../types/models';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import ToolDecisionChip from './ToolDecisionChip.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import { classifyToolName } from './toolCardHeader';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { presentToolCardInputPreview } from './toolCardPreview';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import AnsiText from './AnsiText.svelte';
  import ClaudeSubagentTranscript from './ClaudeSubagentTranscript.svelte';
  import ExpandablePayloadBody from './ExpandablePayloadBody.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import { formatDurationMs, formatTimeOfDay } from '../../utils/format';
  import { parseClaudeSubagentTranscript } from '../../utils/claudeSubagentTranscript';
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
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { createRunningElapsed } from './useRunningElapsed.svelte';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

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
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
  });
  const expansion = $derived(expansionRef.current!);
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
  let deferredOutputState = $derived.by(() => {
    const state = itemMeta?.notification_output_state ?? itemMeta?.output_file_state;
    return typeof state === 'string' ? state : '';
  });
  let deferredOutputError = $derived.by(() => {
    const error = itemMeta?.notification_output_error ?? itemMeta?.output_file_error;
    return typeof error === 'string' ? error : '';
  });
    // One derived id for both halves of the disclosure: the header's
  // `controls` and the body's `id` must be the same string, and pane-scoped
  // (utils/chatDomIds.ts).
  let bodyDomId = $derived(chatRowDomId(pane, 'agent-row-body', item.id));
let hasExpandableBody = $derived(Boolean(item.payloadId) || deferredOutputState === 'loading' || deferredOutputState === 'error');
  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusMeta }));
  let rowError = $derived(
    rowErrorWithFallback(effectiveStatusItem, { meta: statusMeta, fallback: 'Agent failed' }),
  );
  let transcriptEntries = $derived.by(() => {
    if (expansion.displayData === null) return null;
    return parseClaudeSubagentTranscript(expansion.displayData);
  });

  keepExpandedPayloadFresh(() => expansion, () => Boolean(item.payloadId));
</script>

<div class="group/tool overflow-hidden" data-testid="agent-row" data-tool-kind="robot">
  <TranscriptDisclosureHeader
    expanded={expansion.expanded}
    expandable={hasExpandableBody}
    controls={hasExpandableBody ? bodyDomId : undefined}
    testId="agent-row-toggle"
    class="rounded-[var(--radius-control)] px-1 py-1 {hasExpandableBody ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, () => expansion.toggle())}
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

  {#if hasExpandableBody && expansion.expanded}
    <ExpandablePayloadBody
      {pane}
      {expansion}
      id={bodyDomId}
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
    <div class="max-h-80 overflow-auto" data-testid={testId} use:nestedScroll>
      <ClaudeSubagentTranscript entries={transcriptEntries} />
    </div>
  {:else}
    <div
      class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[0.6875rem] leading-relaxed text-fg-muted"
      data-testid={testId}
      use:nestedScroll
    >
      <AnsiText source={data} />
    </div>
  {/if}
{/snippet}
