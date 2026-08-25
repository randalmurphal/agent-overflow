<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import Bot from '@lucide/svelte/icons/bot';
  import MessageSquare from '@lucide/svelte/icons/message-square';
  import XCircle from '@lucide/svelte/icons/x-circle';
  import Play from '@lucide/svelte/icons/play';
  import Clock from '@lucide/svelte/icons/clock';
  import CheckCircle2 from '@lucide/svelte/icons/check-circle-2';
  import PanelRightOpen from '@lucide/svelte/icons/panel-right-open';
  import Icon from '../primitives/Icon.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import type { Item } from '../../types/models';
  import type {
    PaneSession,
    RowUiRegistry,
    PaneDoors,
    TimelineSource,
    ScrollHost,
  } from '../../stores/threadPaneRoles';
  import { formatTimeOfDay } from '../../utils/format';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { importUnavailableLabel } from '../../utils/importUnavailable';
  import {
    codexModelEffortAffix,
    codexSubagentLaunchInfo,
    isCodexSubagentLaunchItem,
  } from '../../utils/subagentLaunch';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';
  import {
    createPayloadExpansion,
    keepExpandedPayloadFresh,
  } from '../../utils/payloadExpansion.svelte';
  import CollabToolRowDetails from './CollabToolRowDetails.svelte';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import ToolRowStatusIndicator from './ToolRowStatusIndicator.svelte';
  import { indicatorStateForItem, rowErrorForStatus } from './rowState';
  import {
    collabCardState,
    collabInputFromMeta,
    collabInteractionLabel,
    collabInteractions,
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
    hostActions,
  }: {
    pane?: PaneDoors & PaneSession & RowUiRegistry & ScrollHost & TimelineSource;
    item: Item;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
    statusItem?: Item;
    durationLabel?: string;
    showTimestamp?: boolean;
    showSpawnStatus?: boolean;
    hostActions?: Snippet;
  } = $props();
  let effectiveStatusItem = $derived(statusItem ?? item);

  // Triage caps the stored list at 32. Showing every one turns a chatty agent's
  // card into a wall, so the tail is what renders and the rest is counted.
  const maxVisibleCollabInteractions = 8;

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
  let activityKind = $derived(stringValue(input, 'activityKind'));
  let prompt = $derived(activityKind ? '' : (spawnInfo?.prompt ?? stringValue(input, 'prompt')));
  let promptPreview = $derived(previewText(prompt));
  let model = $derived(stringValue(input, 'model'));
  let effort = $derived(stringValue(input, 'reasoningEffort'));
  let completionLaunchInfo = $derived.by(() => {
    if (item.kind !== 'tool_completion' || item.toolName !== 'collab_agent' || !item.completionOf) {
      return null;
    }
    // The row's box, not an array scan: the launch row's meta is patched
    // in place, and only the box wakes this derived for it.
    const launch = pane?.getItemById(item.completionOf);
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
  let waitAgentLabel = $derived.by(() => {
    if (receiverDisplayLabels.length === 1) return 'Agent';
    if (receiverDisplayLabels.length > 1) return `${receiverDisplayLabels.length} agents`;
    return 'agents';
  });
  let modelAffix = $derived(spawnInfo?.modelAffix ?? codexModelEffortAffix(model, effort));

  let completionPreview = $derived.by(() => {
    if (item.kind !== 'tool_completion' || item.toolName !== 'collab_agent') return '';
    // Payload meta is the only preview source. The old fallback that
    // read the wait carrier's meta.input.agentsStates messages is gone:
    // persisted agentsStates entries are status-only
    // (itemmeta.TrimCollabAgentStateMessages + the v9 store migration),
    // so there is no message left there to read.
    const payloadPreview = typeof payloadMeta?.preview === 'string'
      ? payloadMeta.preview.trim()
      : '';
    return payloadPreview ? previewText(payloadPreview) : '';
  });

  let title = $derived.by(() => {
    if (item.kind === 'tool_completion' && item.toolName === 'collab_agent') {
      return completionLaunchInfo?.agentLabel || item.summary || 'Completed agent';
    }
    if (spawnInfo) return spawnInfo.title;
    // Legacy / imported rows only. A live `send_input` completion no longer
    // mints a top-level row: triage lands it on the owning spawn card as an
    // interaction sub-line (observeCodexCollabInteractionComplete). Rows
    // written before that change still exist in users' databases, so the
    // branch stays rather than degrading them to "Subagent send_input".
    if (tool === 'send_input') return `Sent input to ${agentLabel || 'agent'}`;
    if (tool === 'wait_agent') {
      if (item.kind === 'tool_completion') return 'Finished waiting';
      return `Waiting for ${waitAgentLabel}`;
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
    (isStatusBackgroundedLaunch || showSpawnStatus || !spawnInfo) &&
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
    return 'agent';
  });
  // Interactions and card state live on the SPAWN launch's meta, so they render
  // on the spawn card only — the completion sibling is a different row.
  let interactions = $derived(spawnInfo ? collabInteractions(meta) : []);
  let visibleInteractions = $derived(
    interactions.slice(-maxVisibleCollabInteractions).map((entry) => ({
      id: entry.id,
      kind: entry.kind,
      text: collabInteractionLabel(entry),
    })),
  );
  let earlierInteractionCount = $derived(
    Math.max(0, interactions.length - visibleInteractions.length),
  );
  let cardState = $derived(
    spawnInfo ? collabCardState(meta, spawnInfo.receiverThreadIds) : null,
  );

  let hasOutputShell = $derived(
    item.kind === 'tool_completion' &&
      item.toolName === 'collab_agent',
  );
  // Computed here and PASSED to the details body: the header lives in this
  // component and the body in another, so a second literal there is a
  // cross-file drift waiting to happen (utils/chatDomIds.ts).
  let outputDomId = $derived(chatRowDomId(pane, 'collab-tool-row-output', item.id));
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

  let time = $derived(formatTimeOfDay(effectiveStatusItem.createdAt));

  // The one approved change to a Codex spawn row (user ruling
  // 2026-08-23): the open-in-pane door. Everything else on the row is the
  // pre-card `launched` indicator, and the spawn is never a card. A host
  // that supplies its own actions (the background tray) keeps them.
  let spawnLaunchInfo = $derived(isCodexSubagentLaunchItem(item) ? codexSubagentLaunchInfo(item) : null);
  let opensAgentPane = $derived(
    pane !== undefined && hostActions === undefined && spawnLaunchInfo !== null,
  );
  function openInPane(event: MouseEvent): void {
    event.stopPropagation();
    pane?.openAgentPane(item.id, spawnLaunchInfo?.agentLabel || title);
  }
</script>

{#snippet openPaneAction()}
  <button
    type="button"
    onclick={openInPane}
    title="Open in agent pane"
    aria-label="Open {spawnLaunchInfo?.agentLabel || title} in agent pane"
    data-testid="collab-tool-row-open-pane"
    class="inline-flex items-center justify-center opacity-0 group-hover/tool:opacity-100 focus-visible:opacity-100 rounded p-0.5 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
  >
    <Icon icon={PanelRightOpen} size={12} />
  </button>
{/snippet}

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
  {#if cardState}
    <!-- Outside the truncating span on purpose: the state is the shortest and
         most load-bearing thing on the row, and a long agent label must not be
         allowed to eat it. -->
    <span
      class="ml-1.5 shrink-0 text-[0.6875rem] text-fg-hint"
      data-testid="collab-tool-row-state"
      data-state={cardState}
    >{cardState}</span>
  {/if}
{/snippet}

{#snippet rowActions()}
  <ToolHeaderMeta
    statusSlotTestId="collab-tool-row-status-slot"
    duration={{ testId: 'collab-tool-row-duration', label: durationLabel }}
    timestamp={showTimestamp
      ? { testId: 'collab-tool-row-time', value: effectiveStatusItem.createdAt, label: time }
      : undefined}
    actions={hostActions ?? (opensAgentPane ? openPaneAction : undefined)}
  >
    {#snippet status()}
      <ToolRowStatusIndicator
        item={effectiveStatusItem}
        state={showRunningStatus || badgeStatus === 'failure' ? indicatorState : null}
        testId="collab-tool-row-status"
      />
    {/snippet}
  </ToolHeaderMeta>
{/snippet}

<div class="group/tool px-1 py-1 text-[0.75rem] text-fg-muted" data-testid="collab-tool-row">
  <TranscriptDisclosureHeader
    expanded={expansion?.expanded ?? false}
    expandable={hasExpandableOutput}
    controls={hasExpandableOutput ? outputDomId : undefined}
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
    bodyDomId={outputDomId}
    {promptPreview}
    {rowError}
    {completionPreview}
    expanded={expansion?.expanded ?? false}
    {tool}
    {receiverDisplayLabels}
    interactions={visibleInteractions}
    {earlierInteractionCount}
    expansion={hasExpandableOutput ? expansion : null}
    emptyMessage={importUnavailableLabel(item) ?? 'No stored output for this agent.'}
  />
</div>
