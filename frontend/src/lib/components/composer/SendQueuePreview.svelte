<script lang="ts">
  // Send-queue overlay. Renders inside the composerOverlay
  // (ChatView.svelte) above the composer card. Pending messages stay
  // here until the provider echoes the user message as visible context;
  // only then does the row move into chat history.

  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    getFlushedForThread,
    getQueueForThread,
  } from '../../stores/sendQueue.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let queued = $derived(getQueueForThread(pane.threadId ?? ''));
  let flushed = $derived(getFlushedForThread(pane.threadId ?? ''));
  let pending = $derived([
    ...flushed.map((item) => ({
      key: item.userItemId,
      message: item.message,
      state: 'flushed' as const,
      userItemId: item.userItemId,
      queueId: null as string | null,
    })),
    ...queued.map((item) => ({
      key: item.id,
      message: item.message,
      state: 'queued' as const,
      userItemId: null as string | null,
      queueId: item.id,
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
          data-user-item-id={item.userItemId}
          data-queue-id={item.queueId}
        >
          <span
            class="select-none pt-px font-mono text-fg-hint/60"
            class:animate-pulse={item.state === 'flushed'}
            aria-hidden="true"
          >→</span>
          <span class="line-clamp-3 flex-1 italic text-fg-muted/85">
            {item.message}
          </span>
        </li>
      {/each}
    </ul>
  </div>
{/if}
