<script lang="ts">
  import type { Snippet } from 'svelte';
  import { untrack } from 'svelte';
  import Bot from 'lucide-svelte/icons/bot';
  import MessageSquare from 'lucide-svelte/icons/message-square';
  import XCircle from 'lucide-svelte/icons/x-circle';
  import Play from 'lucide-svelte/icons/play';
  import Clock from 'lucide-svelte/icons/clock';
  import CheckCircle2 from 'lucide-svelte/icons/check-circle-2';
  import AnsiText from './AnsiText.svelte';
  import CopyFooter from './CopyFooter.svelte';
  import Icon from '../primitives/Icon.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';
  import type { Item } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import {
    codexSubagentDisplayLabel,
    codexSubagentLaunchInfo,
    isCodexSubagentLaunchItem,
  } from '../../utils/subagentLaunch';
  import {
    completionBadgeTitleForStatus,
    deriveCompletionStatus,
  } from '../../utils/toolCompletionStatus';
  import {
    createPayloadExpansion,
    formatPayloadSize,
    keepExpandedPayloadFresh,
  } from './payloadExpansion.svelte';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';

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
    /** Item used for running/completion status. */
    statusItem?: Item;
    /** Optional duration/elapsed label rendered in the metadata area. */
    durationLabel?: string;
    /** Chat collab rows currently omit timestamps; tray can keep that default. */
    showTimestamp?: boolean;
    /** Tray rows need status metadata even for spawn_agent launches. */
    showSpawnStatus?: boolean;
    /** Optional actions rendered outside the row content. */
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
  let input = $derived.by<Record<string, unknown>>(() => {
    const raw = meta?.input ?? payloadMeta?.input;
    return raw && typeof raw === 'object' && !Array.isArray(raw)
      ? raw as Record<string, unknown>
      : {};
  });

  function stringValue(obj: Record<string, unknown>, key: string): string {
    const value = obj[key];
    return typeof value === 'string' ? value.trim() : '';
  }

  function stringArray(obj: Record<string, unknown>, key: string): string[] {
    const value = obj[key];
    return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [];
  }

  function previewText(raw: string, maxLength = 160): string {
    const normalized = raw.replace(/\s+/g, ' ').trim();
    if (normalized.length <= maxLength) return normalized;
    return `${normalized.slice(0, maxLength).trimEnd()}...`;
  }

  interface ReceiverAgentLabel {
    threadId: string;
    label: string;
  }

  function labelForAgentRecord(record: Record<string, unknown>): ReceiverAgentLabel | null {
    const threadId = stringValue(record, 'threadId') || stringValue(record, 'thread_id');
    if (!threadId) return null;
    const nickname =
      stringValue(record, 'newAgentNickname') ||
      stringValue(record, 'agentNickname') ||
      stringValue(record, 'agent_nickname') ||
      stringValue(record, 'nickname');
    const role =
      stringValue(record, 'newAgentRole') ||
      stringValue(record, 'agentRole') ||
      stringValue(record, 'agent_role') ||
      stringValue(record, 'agentType') ||
      stringValue(record, 'agent_type');
    if (!nickname && !role) return null;
    const label = codexSubagentDisplayLabel(nickname, role, 'Agent');
    return { threadId, label };
  }

  function receiverAgentLabels(obj: Record<string, unknown>): ReceiverAgentLabel[] {
    const raw = obj.receiverAgents ?? obj.agentStatuses;
    if (!Array.isArray(raw)) return [];
    return raw
      .filter((entry): entry is Record<string, unknown> =>
        Boolean(entry) && typeof entry === 'object' && !Array.isArray(entry))
      .map(labelForAgentRecord)
      .filter((entry): entry is ReceiverAgentLabel => entry !== null);
  }

  let rawTool = $derived(stringValue(input, 'tool') || item.toolName || '');
  let spawnInfo = $derived.by(() => {
    return isCodexSubagentLaunchItem(item) ? codexSubagentLaunchInfo(item) : null;
  });
  let tool = $derived(spawnInfo ? (spawnInfo.tool || 'spawn_agent') : rawTool);
  let receivers = $derived(spawnInfo?.receiverThreadIds ?? stringArray(input, 'receiverThreadIds'));
  let receiverLabels = $derived(receiverAgentLabels(input));
  let labelByReceiver = $derived.by(() => {
    const labels = new Map<string, string>();
    for (const agent of receiverLabels) {
      labels.set(agent.threadId, agent.label);
    }
    return labels;
  });
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

  let title = $derived.by(() => {
    if (item.kind === 'tool_completion' && item.toolName === 'collab_agent') {
      return completionLaunchInfo?.agentLabel || item.summary || 'Completed agent';
    }
    if (spawnInfo) return spawnInfo.title;
    if (tool === 'send_input') return `Sent input to ${agentLabel || 'agent'}`;
    if (tool === 'wait_agent') {
      if (item.kind === 'tool_completion') return 'Finished waiting';
      if (item.status === 'running' || item.status === 'streaming') return `Waiting for ${agentLabel || 'agents'}`;
      return `Waited for ${agentLabel || 'agents'}`;
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
  let completionTitle = $derived(completionBadgeTitleForStatus(effectiveStatusItem.status));
  let isStatusBackgroundedLaunch = $derived(
    effectiveStatusItem.kind === 'tool_call' && effectiveStatusItem.isBackground === true,
  );
  let showRunningStatus = $derived(
    (showSpawnStatus || !spawnInfo) &&
      tool !== 'wait_agent' &&
      (effectiveStatusItem.status === 'running' || effectiveStatusItem.status === 'streaming'),
  );
  let hasOutputShell = $derived(
    item.kind === 'tool_completion' &&
      item.toolName === 'collab_agent',
  );
  let hasExpandableOutput = $derived(hasOutputShell && Boolean(item.payloadId));
  const expansion = $derived(
    hasExpandableOutput ? (pane ? pane.expansionStateFor(item) : localFallback) : null,
  );

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

{#snippet rowContent()}
  <Icon {icon} size={13} strokeWidth={2} class="shrink-0 opacity-75" />
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
      {#if showRunningStatus}
        <span
          class="shrink-0 {isStatusBackgroundedLaunch ? 'text-[20px] leading-none' : 'text-[10px]'} text-accent opacity-70"
          data-testid="collab-tool-row-status"
          aria-label={isStatusBackgroundedLaunch ? 'Backgrounded' : 'Running'}
        >
          {isStatusBackgroundedLaunch ? '…' : 'running'}
        </span>
      {:else if badgeStatus}
        <CompletionBadge status={badgeStatus} title={completionTitle} class="opacity-80" />
      {/if}
    {/snippet}
  </ToolHeaderMeta>
{/snippet}

<div class="group/tool mb-1.5 px-1 py-1 text-[12px] text-fg-muted" data-testid="collab-tool-row">
  {#if hasOutputShell}
    <TranscriptDisclosureHeader
      expanded={expansion?.expanded ?? false}
      expandable={hasExpandableOutput}
      controls={hasExpandableOutput ? `collab-tool-row-output-${item.id}` : undefined}
      testId="collab-tool-row-toggle"
      class="rounded-[var(--radius-control)] py-1 {hasExpandableOutput ? 'hover:bg-surface-2/20' : ''}"
      onToggle={() => toggle()}
    >
      {@render rowContent()}
      {#snippet actions()}
        {@render rowActions()}
      {/snippet}
    </TranscriptDisclosureHeader>
  {:else}
    <div class="flex items-center gap-2">
      {@render rowContent()}
      {@render rowActions()}
    </div>
  {/if}
  {#if promptPreview}
    <div class="ml-5 mt-0.5 truncate text-[11px] text-fg-subtle">└ {promptPreview}</div>
  {/if}
  {#if tool === 'wait_agent' && receivers.length > 0 && (item.kind === 'tool_completion' || receiverDisplayLabels.length > 1)}
    <div class="ml-5 mt-0.5 space-y-0.5 text-[11px] text-fg-subtle">
      {#each receivers as id, index}
        <div class="truncate">
          └ {item.kind === 'tool_completion' ? statusLine(id) : receiverDisplayLabels[index]}
        </div>
      {/each}
    </div>
  {/if}
  {#if hasExpandableOutput && expansion?.expanded}
    <div
      id="collab-tool-row-output-{item.id}"
      class="ml-5 border-l border-border-subtle bg-surface-0/35"
      data-testid="collab-tool-row-output"
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
            data-testid="collab-tool-row-retry"
          >
            Retry
          </button>
        </div>
      {:else if expansion.displayData !== null}
        <div
          class="ansi-body max-h-60 overflow-auto whitespace-pre-wrap break-words px-3 py-2 text-[11px] leading-relaxed text-fg-muted"
          data-testid="collab-tool-row-output-text"
        >
          <AnsiText source={expansion.displayData} />
        </div>
        {#if expansion.hasMore}
          <button
            type="button"
            class="mx-3 mb-3 text-[11px] text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50 rounded"
            onclick={() => expansion.showFull()}
            data-testid="collab-tool-row-show-full"
          >
            Load more output ({formatPayloadSize(expansion.totalSize)}) ↓
          </button>
        {/if}
        {#if expansion.displayData}
          <CopyFooter text={expansion.displayData} label="Copy output" />
        {/if}
      {:else}
        <p class="px-3 py-2 text-[11px] text-fg-subtle italic">
          No stored output for this agent.
        </p>
      {/if}
    </div>
  {/if}
</div>
