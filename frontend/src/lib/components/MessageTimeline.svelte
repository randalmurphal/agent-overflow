<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';
  import UserMessage from './UserMessage.svelte';
  import AssistantMessage from './AssistantMessage.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let scrollContainer: HTMLDivElement | undefined = $state(undefined);

  // Auto-scroll when items change or streaming content updates.
  $effect(() => {
    // Touch reactive values to track them.
    pane.items.length;
    pane.streamingContent;

    if (scrollContainer) {
      // Use requestAnimationFrame so DOM has updated before scrolling.
      requestAnimationFrame(() => {
        scrollContainer!.scrollTop = scrollContainer!.scrollHeight;
      });
    }
  });
</script>

<div bind:this={scrollContainer} class="flex-1 overflow-y-auto px-4 py-4">
  {#each pane.items as item (item.id)}
    {#if item.role === 'user'}
      <UserMessage {item} />
    {:else}
      <AssistantMessage {item} />
    {/if}
  {/each}

  {#if pane.streamingContent}
    <div class="flex justify-start mb-3">
      <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
        <p class="whitespace-pre-wrap text-sm leading-relaxed">{pane.streamingContent}</p>
        <span class="inline-block w-1.5 h-4 bg-accent animate-pulse ml-0.5 align-text-bottom"></span>
      </div>
    </div>
  {/if}

  {#if pane.items.length === 0 && !pane.streamingContent}
    <div class="flex items-center justify-center h-full text-text-secondary text-sm">
      No messages yet. Send a message to get started.
    </div>
  {/if}
</div>
