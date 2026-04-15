<script lang="ts">
  interface Message {
    id: string;
    role: "user" | "assistant";
    content: string;
    timestamp: number;
  }

  interface Props {
    messages: Message[];
  }

  let { messages }: Props = $props();

  let container: HTMLDivElement | undefined = $state();

  $effect(() => {
    // Auto-scroll on new messages (append-only)
    if (messages.length && container) {
      container.scrollTop = container.scrollHeight;
    }
  });
</script>

<div
  bind:this={container}
  class="flex-1 overflow-y-auto px-4 py-6 space-y-4"
>
  {#each messages as msg (msg.id)}
    <div
      class="max-w-2xl mx-auto px-4 py-3 rounded-lg {msg.role === 'user'
        ? 'bg-surface-2 ml-auto'
        : 'bg-surface-1'}"
    >
      <p class="text-sm text-text-secondary mb-1">
        {msg.role === "user" ? "You" : "Assistant"}
      </p>
      <p class="text-text-primary whitespace-pre-wrap">{msg.content}</p>
    </div>
  {/each}

  {#if messages.length === 0}
    <div class="flex items-center justify-center h-full text-text-secondary">
      <p>Start a conversation</p>
    </div>
  {/if}
</div>
