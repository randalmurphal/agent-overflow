<script lang="ts">
  // An incoming row of the activity rail's Agents body: a message another
  // thread's agent, a wake or a worker queued for this thread, while it is
  // held in the queue. It names the sender and the
  // kind; clicking it opens the sending thread in the pane's agent thread
  // companion; Stop withdraws it from the
  // queue, and its sender is told (StopThreadAgentIncoming).
  import type { BackendKey } from '../../transport/backendKey';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import { formatElapsed } from '../../utils/backgroundTray';
  import {
    agentWakeOriginLabel,
    userMessageOriginThread,
    userMessageOriginWake,
  } from '../../utils/userMessageMeta';
  import { agentThreadOriginChip } from '../chat/agentThreadOrigin';
  import type { IncomingAgentMessage } from './activityRailAgents.svelte';
  import type { Thread } from '../../types/models';
  import Icon from '../primitives/Icon.svelte';
  import Inbox from '@lucide/svelte/icons/inbox';

  interface Props {
    message: IncomingAgentMessage;
    /** The computer showing the thread, which names a sender elsewhere. */
    viewer: BackendKey;
    /** The rail's clock, Unix milliseconds. */
    now: number;
    isStopping: boolean;
    onStop: (message: IncomingAgentMessage) => void;
    /** Where the sending thread opens. */
    place: (thread: Thread) => Promise<unknown>;
  }

  let { message, viewer, now, isStopping, onStop, place }: Props = $props();

  let originThread = $derived(userMessageOriginThread(message.item));
  let chip = $derived(originThread ? agentThreadOriginChip(originThread, viewer, place) : null);
  let sender = $derived.by(() => {
    if (chip) return chip.label;
    const wake = userMessageOriginWake(message.item);
    if (wake) return agentWakeOriginLabel(wake);
    return message.kind === 'wake' ? 'Reminder' : 'From another thread';
  });
  let elapsed = $derived(formatElapsed(Math.max(0, now - message.item.enqueuedAt)));
  let compact = $derived(isCompactLayout());
</script>

<div
  class="rounded-[var(--radius-control)] border border-dashed border-border-subtle bg-transparent px-1 py-1"
  data-testid="agent-incoming-row"
  data-send-id={message.item.sendId ?? ''}
  data-kind={message.kind}
>
  <div class="flex w-full items-center gap-2 compact:gap-1">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-2 rounded-[var(--radius-control)] bg-transparent px-1 py-0.5 text-left focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:min-h-9 compact:select-none {chip?.open ? 'cursor-pointer hover:bg-surface-2/20' : 'cursor-default'}"
      onclick={() => chip?.open?.()}
      tabindex={chip?.open ? undefined : -1}
      aria-disabled={!chip?.open}
      aria-label={`Incoming ${message.kind} ${sender}`}
      data-testid="agent-incoming-row-open"
    >
      <span class="flex size-3.5 shrink-0 items-center justify-center text-fg-hint">
        <Icon icon={Inbox} size={13} strokeWidth={2} />
      </span>
      <span class="shrink-0 text-[0.6875rem] text-fg-hint">incoming</span>
      <span class="shrink-0 rounded-[var(--radius-field)] bg-surface-2/50 px-1 text-[0.625rem] text-fg-muted" data-testid="agent-incoming-row-kind">{message.kind}</span>
      <span class="min-w-8 flex-1 {compact ? 'line-clamp-2 [overflow-wrap:anywhere]' : 'truncate'} text-[0.75rem] text-fg-muted" title={sender} data-testid="agent-incoming-row-sender">{sender}</span>
      <span class="shrink-0 font-mono text-[0.6875rem] tabular-nums text-fg-hint" data-testid="agent-incoming-row-elapsed">{elapsed}</span>
    </button>
    <button
      type="button"
      class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1.5 py-0.5 text-[0.6875rem] font-medium text-text-secondary transition-colors hover:bg-surface-2/40 hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-50 compact:min-h-9 compact:select-none"
      onclick={() => onStop(message)}
      disabled={isStopping}
      title="Withdraw it from the queue before this thread's agent reads it"
      aria-label={`Withdraw incoming ${message.kind}: ${sender}`}
      data-testid="agent-incoming-row-stop"
    >{isStopping ? 'Stopping…' : 'Stop'}</button>
  </div>
</div>
