<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import Bot from 'lucide-svelte/icons/bot';
  import MessageSquare from 'lucide-svelte/icons/message-square';
  import XCircle from 'lucide-svelte/icons/x-circle';
  import Play from 'lucide-svelte/icons/play';
  import Clock from 'lucide-svelte/icons/clock';
  import CheckCircle2 from 'lucide-svelte/icons/check-circle-2';
  import Icon from '../primitives/Icon.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import {
    codexSubagentLaunchInfo,
    isCodexSubagentLaunchItem,
  } from '../../utils/subagentLaunch';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import CollabToolRowDetails from './CollabToolRowDetails.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import Indicator from './Indicator.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import {
    collabInputFromMeta,
    collabSpawnInfo,
    collabToolName,
    previewText,
    receiverIdsForTool,
    receiverLabelMap,
    stringValue,
    usesRequestedWaitReceivers as usesRequestedWaitReceiversForTool,
  } from './collabToolRowData';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';

  let {
    pane,
    item,
    codexSubagentReceiverLabels = new Map<string, string>(),
    statusItem,
    durationLabel = '',
    showTimestamp = false,
    showSpawnStatus = false,
    trailingActions,
  }: {
    pane?: ThreadPane;
    item: Item;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    showSpawnStatus?: boolean;
    trailingActions?: Snippet;
  } = $props();
  let effectiveStatusItem = $derived(statusItem ?? item);

  const localFallback = untrack(() =>
    createPayloadExpansion(
      () => item.payloadId,
      () => item.threadId,
      { payloadVersion: () => item.updatedAt },
    ),
  );
  let meta = $derived(parseJsonObject(item.meta));
  let payloadMeta = $derived(parseJsonObject(item.payloadMeta));
  let statusPayloadMeta = $derived(parseJsonObject(effectiveStatusItem.payloadMeta));
  let input = $derived(collabInputFromMeta(meta, payloadMeta));
  let spawnInfo = $derived(collabSpawnInfo(item));
  let tool = $derived(collabToolName(item, input));
  let receivers = $derived(receiverIdsForTool(tool, input, spawnInfo));
  let usesRequestedWaitReceivers = $derived(usesRequestedWaitReceiversForTool(tool, input));
  let labelByReceiver = $derived(receiverLabelMap(input, usesRequestedWaitReceivers));
  let prompt = $derived(spawnInfo?.prompt ?? stringValue(input, 'prompt'));
  let promptPreview = $derived(previewText(prompt));
  let model = $derived(stringValue(input, 'model'));
  let effort = $derived(stringValue(input, 'reasoningEffort'));
  let completionLaunchInfo = $derived.by(() => {
    if (item.kind !== 'tool_completion' || item.toolName !== 'collab_agent' || !item.completionOf) {
      return null;
    }
    const launch = pane?.items.find((candidate) => candidate.id === item.completionOf);
    if (!launch || !isCodexSubagentLaunchItem(launch)) return null;
    return codexSubagentLaunchInfo(launch);
  });
  function receiverLabel(id: string): string {
    return labelByReceiver.get(id) ?? codexSubagentReceiverLabels.get(id) ?? 'Agent';
  }

  let receiverDisplayLabels = $derived.by(() => receivers.map((id) => receiverLabel(id)));
  let agentLabel = $derived.by(() => {
    if (receiverDisplayLabels.length === 1) return receiverDisplayLabels[0];
    if (receiverDisplayLabels.length > 1) return `${receiverDisplayLabels.length} agents`;
    return '';
  });
  let modelAffix = $derived(spawnInfo?.modelAffix ?? [model, effort].filter(Boolean).join(' '));

  let agentsStates = $derived.by<Record<string, unknown>>(() => {
    const raw = input.agentsStates;
    return raw && typeof raw === 'object' && !Array.isArray(raw)
      ? raw as Record<string, unknown>
      : {};
  });

  function statusLine(id: string): string {
    const label = receiverLabel(id);
    const raw = agentsStates[id];
    if (typeof raw === 'string') return `${label}: ${raw}`;
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return label;
    const record = raw as Record<string, unknown>;
    const status = typeof record.status === 'string' ? record.status : '';
    const message = typeof record.message === 'string' ? record.message.trim() : '';
    return `${label}: ${[status, message].filter(Boolean).join(' - ') || 'unknown'}`;
  }

  function waitCarrierIDForCompletion(): string {
    const carrierID = meta?.wait_carrier_id ?? meta?.waitCarrierID;
    return typeof carrierID === 'string' ? carrierID.trim() : '';
  }

  let completionPreview = $derived.by(() => {
    if (item.kind !== 'tool_completion' || item.toolName !== 'collab_agent') return '';

    const payloadPreview = typeof payloadMeta?.preview === 'string'
      ? payloadMeta.preview.trim()
      : '';
    if (payloadPreview) return previewText(payloadPreview);

    const waitCarrierID = waitCarrierIDForCompletion();
    const receiverThreadIds = completionLaunchInfo?.receiverThreadIds ?? [];
    if (!pane || !waitCarrierID || receiverThreadIds.length === 0) return '';

    const waitCarrier = pane.items.find((candidate) => candidate.id === waitCarrierID);
    const waitMeta = parseJsonObject(waitCarrier?.meta);
    const waitInput = waitMeta?.input;
    if (!waitInput || typeof waitInput !== 'object' || Array.isArray(waitInput)) return '';

    const waitStates = (waitInput as Record<string, unknown>).agentsStates;
    if (!waitStates || typeof waitStates !== 'object' || Array.isArray(waitStates)) return '';

    const parts = receiverThreadIds
      .map((id) => {
        const raw = (waitStates as Record<string, unknown>)[id];
        if (typeof raw === 'string') return raw.trim();
        if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return '';
        const message = (raw as Record<string, unknown>).message;
        return typeof message === 'string' ? message.trim() : '';
      })
      .filter(Boolean);
    return parts.length > 0 ? previewText(parts.join(' | ')) : '';
  });

  let title = $derived.by(() => {
    if (item.kind === 'tool_completion' && item.toolName === 'collab_agent') {
      return completionLaunchInfo?.agentLabel || item.summary || 'Completed agent';
    }
    if (spawnInfo) return spawnInfo.title;
    if (tool === 'send_input') return `Sent input to ${agentLabel || 'agent'}`;
    if (tool === 'wait_agent') {
      if (item.kind === 'tool_completion') return 'Finished waiting';
      return `Waiting for ${agentLabel || 'agents'}`;
    }
    if (tool === 'close_agent') return `Closed ${agentLabel || 'agent'}`;
    if (tool === 'resume_agent') {
      return item.kind === 'tool_completion'
        ? `Resumed ${agentLabel || 'agent'}`
        : `Resuming ${agentLabel || 'agent'}`;
    }
    return `Subagent ${tool || item.toolName || 'agent'}`;
  });

  let icon = $derived.by(() => {
    if (tool === 'send_input') return MessageSquare;
    if (tool === 'wait_agent') return item.kind === 'tool_completion' ? CheckCircle2 : Clock;
    if (tool === 'close_agent') return XCircle;
    if (tool === 'resume_agent') return Play;
    return Bot;
  });

  let badgeStatus = $derived.by<'success' | 'failure' | null>(() => {
    if (tool === 'wait_agent' && item.kind === 'tool_call') return null;
    return deriveCompletionStatus(effectiveStatusItem, { meta: statusPayloadMeta });
  });
  let isStatusBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );
  let showRunningStatus = $derived(
    (showSpawnStatus || !spawnInfo) &&
      tool !== 'wait_agent' &&
      (effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming'),
  );
  let indicatorState = $derived(indicatorStateForItem(effectiveStatusItem, { meta: statusPayloadMeta }));
  let rowError = $derived.by(() => {
    if (badgeStatus !== 'failure') return null;
    return rowErrorForStatus(effectiveStatusItem.status, 'Agent operation failed') ?? {
      tone: 'error' as const,
      msg: 'Agent operation failed',
    };
  });
  let gutterLabel = $derived.by(() => {
    if (tool === 'send_input') return 'send';
    if (tool === 'wait_agent') return item.kind === 'tool_completion' ? 'waited' : 'waiting';
    if (tool === 'close_agent') return 'closed';
    if (tool === 'resume_agent') return 'resume';
    return 'spawn';
  });
  let hasOutputShell = $derived(
    item.kind === 'tool_completion' &&
      item.toolName === 'collab_agent',
  );
  let hasExpandableOutput = $derived(hasOutputShell && Boolean(item.payloadId));
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => localFallback,
    enabled: () => hasExpandableOutput,
  });
  const expansion = $derived(expansionRef.current);

  keepExpandedPayloadFresh(
    () => expansion ?? localFallback,
    () => hasExpandableOutput && expansion !== null,
  );

  async function toggle() {
    if (expansion === null) return;
    await expansion.toggle();
  }

  let time = $derived(
    new Date(effectiveStatusItem.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

{#snippet rowIcon()}
  <Icon {icon} size={13} strokeWidth={2} class="shrink-0 opacity-75" />
{/snippet}

{#snippet rowLabel()}
  <span data-testid="collab-tool-row-label">{gutterLabel}</span>
{/snippet}

{#snippet rowBody()}
  <span class="min-w-0 flex-1 truncate">
    {title}{#if modelAffix}<span class="ml-1 text-fg-hint">({modelAffix})</span>{/if}
  </span>
{/snippet}

{#snippet rowActions()}
  <ToolHeaderMeta
    statusSlotTestId="collab-tool-row-status-slot"
    duration={{ testId: 'collab-tool-row-duration', label: durationLabel }}
    timestamp={showTimestamp
      ? { testId: 'collab-tool-row-time', value: effectiveStatusItem.createdAt, label: time }
      : undefined}
    {trailingActions}
  >
    {#snippet status()}
      <Indicator state={showRunningStatus || badgeStatus === 'failure' ? indicatorState : null} />
    {/snippet}
  </ToolHeaderMeta>
{/snippet}

<div class="group/tool px-1 py-1 text-[0.75rem] text-fg-muted" data-testid="collab-tool-row">
  <TranscriptDisclosureHeader
    expanded={expansion?.expanded ?? false}
    expandable={hasExpandableOutput}
    controls={hasExpandableOutput ? `collab-tool-row-output-${item.id}` : undefined}
    testId="collab-tool-row-toggle"
    class="rounded-[var(--radius-control)] py-1 {hasExpandableOutput ? 'hover:bg-surface-2/20' : ''}"
    onToggle={(event) => preservePaneScrollAnchor(pane, event, toggle)}
  >
    {#snippet icon()}{@render rowIcon()}{/snippet}
    {#snippet label()}{@render rowLabel()}{/snippet}
    {#snippet body()}{@render rowBody()}{/snippet}
    {#snippet actions()}
      {@render rowActions()}
    {/snippet}
  </TranscriptDisclosureHeader>
  <CollabToolRowDetails
    {pane}
    itemId={item.id}
    {promptPreview}
    {rowError}
    {completionPreview}
    expanded={expansion?.expanded ?? false}
    {tool}
    isCompletion={item.kind === 'tool_completion'}
    {receivers}
    {receiverDisplayLabels}
    {statusLine}
    expansion={hasExpandableOutput ? expansion : null}
  />
</div>
