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
  import {
    digestThreadOrigin,
    type AgentMessage,
    type AgentMessageKind,
    type AgentMessagePart,
    type WakeThreadFacts,
  } from '../../utils/agentMessage';
  import type { UserMessageOriginThread } from '../../utils/userMessageMeta';
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
  let viewer = $derived(threadMachine(item.threadId, null));
  let headerSender = $derived(senderOf(message.sender, message.senderThread));
  let time = $derived(formatTimeOfDay(item.createdAt));

  function toggle(event: MouseEvent): void {
    void preservePaneScrollAnchor(pane, event, () => {
      pane.setUserMessageExpanded(item.id, !expanded);
    });
  }

  interface Sender {
    label: string;
    open: (() => void) | null;
  }

  /**
   * A sender as the row names it. The origin chip's rules decide both what
   * the name adds and whether it opens: " on <computer>" only when the
   * sender is on another computer than the one showing this row, named as
   * this client names it, and a link only when this client can reach that
   * thread, so a dead one is never offered. A sender with no routable
   * thread is its name, with `computer` when the text named one.
   */
  function senderOf(name: string, thread: UserMessageOriginThread | null, computer = ''): Sender {
    if (!thread) return { label: computer ? `${name} on ${computer}` : name, open: null };
    const chip = agentThreadOriginChip(thread, viewer);
    const named = `from ${thread.title || 'another thread'}`;
    return { label: name + (chip.label.startsWith(named) ? chip.label.slice(named.length) : ''), open: chip.open };
  }

  interface Fact {
    label: string;
    value: string;
  }

  /** What a thread that answered adds: its role and how long ago it answered. */
  function answererFacts(thread: WakeThreadFacts): Fact[] {
    const facts: Fact[] = [];
    if (thread.agentRole !== null) facts.push({ label: 'Role', value: thread.agentRole || 'Standing agent' });
    if (thread.age) facts.push({ label: 'Answered', value: `${thread.age} ago` });
    return facts;
  }

  function tokenFacts(tokens: string[]): Fact[] {
    if (tokens.length > 1) return [{ label: 'Tokens', value: tokens.join(', ') }];
    return tokens.length === 1 ? [{ label: 'Token', value: tokens[0] }] : [];
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
      // A digest's threads each carry their own (the part's threads below).
      const digest = wake.threads.length > 0;
      if (!digest) facts.push(...answererFacts(wake));
      if (wake.stoppedBy) facts.push({ label: 'Stopped by', value: wake.stoppedBy });
      // A request made in an assistant's past thread, answered in its current one.
      if (wake.madeIn) facts.push({ label: 'Made in', value: `${wake.madeIn}, before New Thread, Same Assistant` });
      if (wake.now) facts.push({ label: 'Now in', value: wake.now });
      if (wake.clipped) facts.push({ label: 'Preview', value: `thread_status ${wake.token} returns the whole answer` });
      if (!digest) facts.push(...tokenFacts(wake.tokens));
    }
    return facts;
  }
</script>

{#snippet senderName(sender: Sender, testId: string, extra: string)}
  {#if sender.open}
    <button
      type="button"
      class="{extra} font-medium text-fg-muted hover:text-accent focus-visible:text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 compact:select-none"
      data-testid={testId}
      title={`Open ${sender.label}`}
      onclick={() => sender.open?.()}
    >{sender.label}</button>
  {:else}
    <span class="{extra} font-medium text-fg-muted" data-testid={testId}>{sender.label}</span>
  {/if}
{/snippet}

{#snippet factList(facts: Fact[])}
  {#if facts.length > 0}
    <dl class="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-0.5 text-fg-muted" data-testid="agent-message-facts">
      {#each facts as fact (fact.label)}
        <dt class="text-fg-hint">{fact.label}</dt>
        <dd class="break-words" data-fact={fact.label}>{fact.value}</dd>
      {/each}
    </dl>
  {/if}
{/snippet}

<!-- A message's source, as the header names it: its kind, when it is not the part's, and its sender, who opens when reachable. -->
{#snippet sourceHeading(kindTitle: string, sender: Sender, testId: string)}
  <span class="flex min-w-0 items-baseline gap-1.5" data-testid={testId}>
    {#if kindTitle}<span class="shrink-0 text-fg-hint">{kindTitle}</span>{/if}
    {@render senderName(sender, `${testId}-sender`, 'min-w-0 truncate')}
  </span>
{/snippet}

<div
  class="group/tool overflow-hidden"
  data-testid="agent-message-row"
  data-agent-message-kind={message.kind}
>
  <TranscriptDisclosureHeader
    {expanded}
    controls={bodyDomId}
    testId="agent-message-toggle"
    ariaLabel={`${message.kindTitle} from ${headerSender.label}`}
    interactiveBody
    class="rounded-[var(--radius-control)] px-1 py-1 hover:bg-surface-2/20"
    onToggle={toggle}
  >
    {#snippet icon()}<Icon icon={ICONS[message.kind]} size={13} strokeWidth={2} class="text-fg-hint" />{/snippet}
    {#snippet label()}<span data-testid="agent-message-kind" title={message.kindTitle}>{message.kindLabel}</span>{/snippet}
    {#snippet body()}
      <span class="flex min-w-0 flex-1 items-baseline gap-1.5 text-[0.75rem]">
        {@render senderName(headerSender, 'agent-message-sender', 'max-w-[45%] shrink-0 truncate')}
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
        <section class="flex flex-col gap-1.5" data-testid="agent-message-part" data-agent-message-kind={part.source.kind}>
          <!-- The header names the first message's sender; each message of a joined row names its own. -->
          {#if message.parts.length > 1}
            {@render sourceHeading(part.source.kindTitle, senderOf(part.source.sender, part.source.senderThread), 'agent-message-part-source')}
          {/if}
          {#if part.body}
            <p class="whitespace-pre-wrap break-words text-fg" data-testid="agent-message-text">{part.body}</p>
          {/if}
          {@render factList(partFacts(part))}
          <!-- A digest names each thread it speaks for the same way. -->
          {#each part.wake?.threads ?? [] as thread, threadIndex (threadIndex)}
            <div class="flex flex-col gap-0.5" data-testid="agent-message-thread">
              {@render sourceHeading(
                '',
                senderOf(thread.title || thread.threadId, digestThreadOrigin(thread), thread.computer),
                'agent-message-thread-source',
              )}
              {@render factList([...answererFacts(thread), ...tokenFacts(thread.tokens)])}
            </div>
          {/each}
        </section>
      {/each}
    </div>
  {/if}
</div>
