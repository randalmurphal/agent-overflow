<script lang="ts">
  import Bot from 'lucide-svelte/icons/bot';
  import MessageSquare from 'lucide-svelte/icons/message-square';
  import XCircle from 'lucide-svelte/icons/x-circle';
  import Play from 'lucide-svelte/icons/play';
  import Clock from 'lucide-svelte/icons/clock';
  import CheckCircle2 from 'lucide-svelte/icons/check-circle-2';
  import Icon from '../primitives/Icon.svelte';
  import CompletionBadge from './CompletionBadge.svelte';
  import type { Item } from '../../types/models';
  import { parseJsonObject } from '../../utils/parseJsonObject';
  import { codexSubagentDisplayLabel } from '../../utils/subagentLaunch';
  import { deriveCompletionStatus } from '../../utils/toolCompletionStatus';

  let { item }: { item: Item } = $props();

  let meta = $derived(parseJsonObject(item.meta));
  let payloadMeta = $derived(parseJsonObject(item.payloadMeta));
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

  interface ReceiverAgentLabel {
    threadId: string;
    label: string;
  }

  function labelForAgentRecord(record: Record<string, unknown>): ReceiverAgentLabel | null {
    const threadId = stringValue(record, 'threadId') || stringValue(record, 'thread_id');
    if (!threadId) return null;
    const nickname = stringValue(record, 'newAgentNickname') || stringValue(record, 'agentNickname') || stringValue(record, 'agent_nickname') || stringValue(record, 'nickname');
    const role = stringValue(record, 'newAgentRole') || stringValue(record, 'agentRole') || stringValue(record, 'agent_role') || stringValue(record, 'agentType') || stringValue(record, 'agent_type');
    const label = codexSubagentDisplayLabel(nickname, role, role ? 'Agent' : threadId);
    return { threadId, label };
  }

  function receiverAgentLabels(obj: Record<string, unknown>): ReceiverAgentLabel[] {
    const raw = obj.receiverAgents ?? obj.agentStatuses;
    if (!Array.isArray(raw)) return [];
    return raw
      .filter((entry): entry is Record<string, unknown> => Boolean(entry) && typeof entry === 'object' && !Array.isArray(entry))
      .map(labelForAgentRecord)
      .filter((entry): entry is ReceiverAgentLabel => entry !== null);
  }

  let tool = $derived(stringValue(input, 'tool') || item.toolName || '');
  let receivers = $derived(stringArray(input, 'receiverThreadIds'));
  let receiverLabels = $derived(receiverAgentLabels(input));
  let labelByReceiver = $derived(new Map(receiverLabels.map((agent) => [agent.threadId, agent.label])));
  let prompt = $derived(stringValue(input, 'prompt'));
  let model = $derived(stringValue(input, 'model'));
  let effort = $derived(stringValue(input, 'reasoningEffort'));
  let receiverDisplayLabels = $derived.by(() => receivers.map((id) => labelByReceiver.get(id) ?? id));
  let agentLabel = $derived.by(() => {
    if (receiverDisplayLabels.length === 1) return receiverDisplayLabels[0];
    if (receiverDisplayLabels.length > 1) return `${receiverDisplayLabels.length} agents`;
    return '';
  });
  let modelAffix = $derived([model, effort].filter(Boolean).join(' '));

  let agentsStates = $derived.by<Record<string, unknown>>(() => {
    const raw = input.agentsStates;
    return raw && typeof raw === 'object' && !Array.isArray(raw)
      ? raw as Record<string, unknown>
      : {};
  });

  function statusLine(id: string): string {
    const label = labelByReceiver.get(id) ?? id;
    const raw = agentsStates[id];
    if (typeof raw === 'string') return `${label}: ${raw}`;
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return label;
    const record = raw as Record<string, unknown>;
    const status = typeof record.status === 'string' ? record.status : '';
    const message = typeof record.message === 'string' ? record.message.trim() : '';
    return `${label}: ${[status, message].filter(Boolean).join(' - ') || 'unknown'}`;
  }

  let title = $derived.by(() => {
    if (tool === 'send_input') return `Sent input to ${agentLabel || 'agent'}`;
    if (tool === 'wait_agent') {
      if (item.kind === 'tool_completion') return 'Finished waiting';
      if (item.status === 'running' || item.status === 'streaming') return `Waiting for ${agentLabel || 'agents'}`;
      return `Waited for ${agentLabel || 'agents'}`;
    }
    if (tool === 'close_agent') return `Closed ${agentLabel || 'agent'}`;
    if (tool === 'resume_agent') return item.kind === 'tool_completion' ? `Resumed ${agentLabel || 'agent'}` : `Resuming ${agentLabel || 'agent'}`;
    return `Subagent ${tool}`;
  });

  let icon = $derived.by(() => {
    if (tool === 'send_input') return MessageSquare;
    if (tool === 'wait_agent') return item.kind === 'tool_completion' ? CheckCircle2 : Clock;
    if (tool === 'close_agent') return XCircle;
    if (tool === 'resume_agent') return Play;
    return Bot;
  });

  let completionStatus = $derived(deriveCompletionStatus(item, { meta: payloadMeta }));
  let badgeStatus = $derived.by<'success' | 'failure' | null>(() => {
    if (tool === 'wait_agent' && item.kind === 'tool_call') return null;
    return completionStatus;
  });
</script>

<div class="mb-1.5 px-1 py-1 text-[12px] text-fg-muted" data-testid="collab-tool-row">
  <div class="flex items-center gap-2">
    <Icon {icon} size={13} strokeWidth={2} class="shrink-0 opacity-75" />
    <span class="min-w-0 flex-1 truncate">
      {title}{#if modelAffix}<span class="ml-1 text-fg-hint">({modelAffix})</span>{/if}
    </span>
    {#if item.status === 'running' || item.status === 'streaming'}
      <span class="shrink-0 text-[10px] text-accent opacity-70">running</span>
    {:else if badgeStatus}
      <CompletionBadge status={badgeStatus} class="opacity-80" />
    {/if}
  </div>
  {#if prompt}
    <div class="ml-5 mt-0.5 truncate text-[11px] text-fg-subtle">└ {prompt}</div>
  {/if}
  {#if tool === 'wait_agent' && receivers.length > 0}
    <div class="ml-5 mt-0.5 space-y-0.5 text-[11px] text-fg-subtle">
      {#each receivers as id, index}
        <div class="truncate">└ {item.kind === 'tool_completion' ? statusLine(id) : receiverDisplayLabels[index]}</div>
      {/each}
    </div>
  {/if}
</div>
