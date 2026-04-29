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

  let tool = $derived(stringValue(input, 'tool') || item.toolName || '');
  let receivers = $derived(stringArray(input, 'receiverThreadIds'));
  let prompt = $derived(stringValue(input, 'prompt'));
  let model = $derived(stringValue(input, 'model'));
  let effort = $derived(stringValue(input, 'reasoningEffort'));
  let agentLabel = $derived.by(() => {
    if (receivers.length === 1) return receivers[0];
    if (receivers.length > 1) return `${receivers.length} agents`;
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
    const raw = agentsStates[id];
    if (typeof raw === 'string') return `${id}: ${raw}`;
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return id;
    const record = raw as Record<string, unknown>;
    const status = typeof record.status === 'string' ? record.status : '';
    const message = typeof record.message === 'string' ? record.message.trim() : '';
    return `${id}: ${[status, message].filter(Boolean).join(' - ') || 'unknown'}`;
  }

  let title = $derived.by(() => {
    if (tool === 'send_input') return `Sent input to ${agentLabel || 'agent'}`;
    if (tool === 'wait_agent') return item.kind === 'tool_completion' ? 'Finished waiting' : `Waiting for ${agentLabel || 'agents'}`;
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
</script>

<div class="mb-1.5 px-1 py-1 text-[12px] text-fg-muted" data-testid="collab-tool-row">
  <div class="flex items-center gap-2">
    <Icon {icon} size={13} strokeWidth={2} class="shrink-0 opacity-75" />
    <span class="min-w-0 flex-1 truncate">
      {title}{#if modelAffix}<span class="ml-1 text-fg-hint">({modelAffix})</span>{/if}
    </span>
    {#if item.status === 'running' || item.status === 'streaming'}
      <span class="shrink-0 text-[10px] text-accent opacity-70">running</span>
    {:else if completionStatus}
      <CompletionBadge status={completionStatus} class="opacity-80" />
    {/if}
  </div>
  {#if prompt}
    <div class="ml-5 mt-0.5 truncate text-[11px] text-fg-subtle">└ {prompt}</div>
  {/if}
  {#if tool === 'wait_agent' && receivers.length > 0}
    <div class="ml-5 mt-0.5 space-y-0.5 text-[11px] text-fg-subtle">
      {#each receivers as id}
        <div class="truncate">└ {item.kind === 'tool_completion' ? statusLine(id) : id}</div>
      {/each}
    </div>
  {/if}
</div>
