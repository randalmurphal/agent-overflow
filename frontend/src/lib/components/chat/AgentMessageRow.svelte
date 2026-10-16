<script lang="ts">
  // A message another agent wrote into this thread (utils/agentMessage.ts):
  // a thread tool's request or the answer coming back, a loop or watch wake,
  // a worker's question, a provider agent's message. It is a user-role turn
  // on the wire and a `user_text` row in the store, but it is not the
  // person speaking, so it renders in the register of a tool row rather
  // than as the user bubble: icon, gutter label, one header line naming the
  // sender, folded body. The body is the message with its routing parsed
  // into facts; the footer and status-line text the agent reads never
  // render raw.
  //
  // Expansion is the pane's per-item user-message state, so it survives
  // virtualization and feeds the size priors like the bubble's own clamp.
  import type { Component } from 'svelte';
  import AlarmClock from '@lucide/svelte/icons/alarm-clock';
  import Bot from '@lucide/svelte/icons/bot';
  import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
  import CircleX from '@lucide/svelte/icons/circle-x';
  import ClockAlert from '@lucide/svelte/icons/clock-alert';
  import Eye from '@lucide/svelte/icons/eye';
  import GitBranchPlus from '@lucide/svelte/icons/git-branch-plus';
  import Inbox from '@lucide/svelte/icons/inbox';
  import Monitor from '@lucide/svelte/icons/monitor';
  import OctagonX from '@lucide/svelte/icons/octagon-x';
  import Repeat from '@lucide/svelte/icons/repeat';
  import Reply from '@lucide/svelte/icons/reply';
  import Terminal from '@lucide/svelte/icons/terminal';
  import Workflow from '@lucide/svelte/icons/workflow';
  import UserRound from '@lucide/svelte/icons/user-round';
  import SquarePen from '@lucide/svelte/icons/square-pen';
  import type { Item } from '../../types/models';
  import type { PaneSession, RowUiRegistry, ScrollHost } from '../../stores/threadPaneRoles';
  import { threadMachine } from '../../stores/attachedBackends.svelte';
  import { chatRowDomId } from '../../utils/chatDomIds';
  import { formatTimeOfDay } from '../../utils/format';
  import type { AgentMessage, AgentMessageKind, AgentMessagePart } from '../../utils/agentMessage';
  import Icon from '../primitives/Icon.svelte';
  import { agentThreadOriginChip } from './agentThreadOrigin';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import ToolHeaderMeta from './ToolHeaderMeta.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

  let {
    pane,
    item,
    message,
  }: {
    pane: PaneSession & RowUiRegistry & ScrollHost;
    item: Item;
    message: AgentMessage;
  } = $props();

  const ICONS: Record<AgentMessageKind, Component> = {
    spawn: GitBranchPlus,
    request: Inbox,
    question: CircleQuestionMark,
    reply: Reply,
    finished: Reply,
    errored: CircleX,
    cancelled: OctagonX,
    interrupted: OctagonX,
    expired: ClockAlert,
    reminder: AlarmClock,
    settled: Reply,
    loop: Repeat,
    watch: Eye,
    script: Terminal,
    opening: UserRound,
    handoff: SquarePen,
    'worker-question': CircleQuestionMark,
    'worker-approval': CircleQuestionMark,
    job: Monitor,
    workflow: Workflow,
    message: Bot,
    task: Bot,
    'final-answer': Bot,
  };

  let expanded = $derived(pane.isUserMessageExpanded(item.id));
  let bodyDomId = $derived(chatRowDomId(pane, 'agent-message-body', item.id));
  // The origin chip's rules decide both what the sender adds and whether it
  // opens: " on <computer>" only when the sender is on another computer than
  // the one showing this row, named as this client names it, and a link
  // only when this client can reach that thread, so a dead one is never
  // offered.
  let chip = $derived(
    message.senderThread ? agentThreadOriginChip(message.senderThread, threadMachine(item.threadId, null)) : null,
  );
  let open = $derived(chip?.open ?? null);
  let senderLabel = $derived.by(() => {
    const named = `from ${message.senderThread?.title || 'another thread'}`;
    const where = chip && chip.label.startsWith(named) ? chip.label.slice(named.length) : '';
    return message.sender + where;
  });
  let time = $derived(formatTimeOfDay(item.createdAt));

  function toggle(event: MouseEvent): void {
    void preservePaneScrollAnchor(pane, event, () => {
      pane.setUserMessageExpanded(item.id, !expanded);
    });
  }


  interface Fact {
    label: string;
    value: string;
  }

  function partFacts(part: AgentMessagePart): Fact[] {
    const facts: Fact[] = [];
    const footer = part.footer;
    if (footer) {
      facts.push({ label: 'Waiting', value: footer.waiting ? 'Yes, for an answer' : 'No' });
      facts.push({
        label: 'Reply',
        value: footer.reachable
          ? 'This thread can read the sender and answer it'
          : "The sender's computer is not reachable from here",
      });
      if (footer.agentRole !== null) facts.push({ label: 'Role', value: footer.agentRole || 'Standing agent' });
      if (footer.routesPrompts) facts.push({ label: 'Prompts', value: "Go to the sender, which spawned this thread" });
      if (footer.userMessage) facts.push({ label: 'User said', value: footer.userMessage });
      facts.push({ label: 'Token', value: footer.token });
    }
    const wake = part.wake;
    if (wake) {
      // The header names the sender; a joined part from another thread
      // names its own.
      if (wake.title && wake.threadId !== message.senderThread?.threadId) {
        facts.push({ label: 'From', value: wake.computer ? `${wake.title} on ${wake.computer}` : wake.title });
      }
      if (wake.agentRole !== null) facts.push({ label: 'Role', value: wake.agentRole || 'Standing agent' });
      if (wake.age) facts.push({ label: 'Answered', value: `${wake.age} ago` });
      if (wake.stoppedBy) facts.push({ label: 'Stopped by', value: wake.stoppedBy });
      if (wake.clipped) facts.push({ label: 'Preview', value: `thread_status ${wake.token} returns the whole answer` });
      if (wake.token) facts.push({ label: 'Token', value: wake.token });
    }
    return facts;
  }
</script>

<div
  class="group/tool overflow-hidden"
  data-testid="agent-message-row"
  data-agent-message-kind={message.kind}
>
  <TranscriptDisclosureHeader
    {expanded}
    controls={bodyDomId}
    testId="agent-message-toggle"
    ariaLabel={`${message.kindTitle} from ${senderLabel}`}
    interactiveBody
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={toggle}
  >
    {#snippet icon()}<Icon icon={ICONS[message.kind]} size={13} strokeWidth={2} class="text-fg-hint" />{/snippet}
    {#snippet label()}<span data-testid="agent-message-kind" title={message.kindTitle}>{message.kindLabel}</span>{/snippet}
    {#snippet body()}
      <span class="flex min-w-0 flex-1 items-baseline gap-1.5 text-[0.75rem]">
        {#if open}
          <button
            type="button"
            class="max-w-[45%] shrink-0 truncate font-medium text-fg-muted hover:text-accent focus-visible:text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none"
            data-testid="agent-message-sender"
            title={`Open ${senderLabel}`}
            onclick={() => open?.()}
          >{senderLabel}</button>
        {:else}
          <span class="max-w-[45%] shrink-0 truncate font-medium text-fg-muted" data-testid="agent-message-sender">{senderLabel}</span>
        {/if}
        {#if message.senderRole}
          <span class="max-w-[30%] shrink truncate text-fg-hint" data-testid="agent-message-role" title={message.senderRole}>{message.senderRole}</span>
        {/if}
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <span
          class="min-w-0 flex-1 cursor-pointer truncate text-fg-muted/75"
          data-testid="agent-message-first-line"
          onclick={toggle}
        >{message.firstLine}</span>
      </span>
    {/snippet}
    {#snippet actions()}
      <ToolHeaderMeta
        statusSlotTestId="agent-message-status-slot"
        timestamp={{ testId: 'agent-message-time', value: item.createdAt, label: time }}
      />
    {/snippet}
  </TranscriptDisclosureHeader>

  {#if expanded}
    <div
      id={bodyDomId}
      class="ml-[5.25rem] compact:ml-5 flex flex-col gap-3 px-3 pb-2 pt-1 text-[0.75rem] leading-5"
      data-testid="agent-message-body"
    >
      {#each message.parts as part, index (index)}
        {@const facts = partFacts(part)}
        <section class="flex flex-col gap-1.5" data-testid="agent-message-part">
          {#if message.parts.length > 1}
            <span class="text-[0.625rem] uppercase tracking-wide text-fg-hint">Message {index + 1} of {message.parts.length}</span>
          {/if}
          {#if part.body}
            <p class="whitespace-pre-wrap break-words text-fg" data-testid="agent-message-text">{part.body}</p>
          {/if}
          {#if facts.length > 0}
            <dl class="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-fg-muted" data-testid="agent-message-facts">
              {#each facts as fact (fact.label)}
                <dt class="text-fg-hint">{fact.label}</dt>
                <dd class="break-words" data-fact={fact.label}>{fact.value}</dd>
              {/each}
            </dl>
          {/if}
        </section>
      {/each}
    </div>
  {/if}
</div>
