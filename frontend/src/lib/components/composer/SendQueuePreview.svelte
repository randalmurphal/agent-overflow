<script lang="ts">
  // Send-queue overlay. Renders inside the composerOverlay
  // (ChatView.svelte) above the composer card. Pending messages stay
  // here until the provider echoes the user message as visible context;
  // only then does the row move into chat history.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    getFlushedForThread,
    getQueueForThread,
    type FlushedLifecycle,
  } from '../../stores/sendQueue.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  // Hover copy for a provider delivery ack. Undefined lifecycle — the
  // only state an older Claude CLI or a Codex thread ever has — gets the
  // unchanged pre-ack wording, so this channel can only ever add detail.
  function lifecycleTitle(lifecycle: FlushedLifecycle | undefined): string {
    if (!lifecycle) return 'Sent to the agent, waiting for it to enter context';
    switch (lifecycle.state) {
      case 'queued':
        return 'The agent has this message queued';
      case 'started':
        if (lifecycle.delivery === 'mid_turn') return 'Delivered into the running turn';
        if (lifecycle.delivery === 'new_turn') return 'Started as its own turn';
        return 'The agent has picked this message up';
      case 'completed':
        return 'The agent finished with this message';
      case 'cancelled':
        return 'The agent will not deliver this message';
    }
  }

  // Terse trailing hint. Only the two states the user can act on say
  // anything: cancelled (it is never arriving) and a mid-turn delivery
  // (the running turn just changed course). Everything else stays silent
  // — the row itself already means "pending".
  function lifecycleHint(lifecycle: FlushedLifecycle | undefined): string {
    if (!lifecycle) return '';
    if (lifecycle.state === 'cancelled') return 'not delivered';
    if (lifecycle.state === 'started' && lifecycle.delivery === 'mid_turn') return 'steering';
    return '';
  }

  let queued = $derived(getQueueForThread(pane.threadId ?? ''));
  let flushed = $derived(getFlushedForThread(pane.threadId ?? ''));
  let pending = $derived([
    ...flushed.map((item) => ({
      key: item.userItemId,
      message: item.message,
      state: 'flushed' as const,
      userItemId: item.userItemId,
      queueId: null as string | null,
      lifecycle: item.lifecycle,
      title: lifecycleTitle(item.lifecycle),
      hint: lifecycleHint(item.lifecycle),
    })),
    ...queued.map((item) => ({
      key: item.id,
      message: item.message,
      state: 'queued' as const,
      userItemId: null as string | null,
      queueId: item.id,
      lifecycle: undefined as FlushedLifecycle | undefined,
      title: 'Queued — not sent to the agent yet',
      hint: '',
    })),
  ]);
</script>

{#if pending.length > 0 && pane.threadId}
  <div
    class="mb-2 flex flex-col gap-0.5 pl-1.5 text-[0.6875rem] leading-snug"
    data-testid="send-queue-preview"
    aria-label="Pending user messages"
  >
    <ul class="flex flex-col gap-0.5">
      {#each pending as item (item.key)}
        <li
          class="flex items-start gap-1.5"
          data-testid="send-queue-preview-row"
          data-state={item.state}
          data-lifecycle={item.lifecycle?.state}
          data-delivery={item.lifecycle?.delivery}
          data-user-item-id={item.userItemId}
          data-queue-id={item.queueId}
          title={item.title}
        >
          <span
            class="select-none pt-px font-mono text-fg-hint/60"
            class:animate-pulse={item.state === 'flushed' && item.lifecycle?.state !== 'cancelled'}
            aria-hidden="true"
          >→</span>
          <span
            class="line-clamp-3 flex-1 italic text-fg-muted/85"
            class:line-through={item.lifecycle?.state === 'cancelled'}
            class:opacity-60={item.lifecycle?.state === 'cancelled'}
          >
            {item.message}
          </span>
          {#if item.hint}
            <span class="shrink-0 pt-px text-fg-hint/70">{item.hint}</span>
          {/if}
        </li>
      {/each}
    </ul>
  </div>
{/if}
