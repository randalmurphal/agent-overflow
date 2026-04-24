<script lang="ts">
  import type { Item } from '../../types/models';
  import ChatMarkdown from './ChatMarkdown.svelte';

  let { item }: { item: Item } = $props();

  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

<div class="group mb-6" data-item-kind={item.kind}>
  <div
    class="text-fg-muted"
    data-testid="assistant-message-body"
    data-render-mode="client-markdown"
  >
    <ChatMarkdown source={item.summary} streaming={item.status === 'streaming'} />
  </div>
  <time
    class="mt-1.5 block text-[10px] text-fg-hint opacity-0 transition-opacity duration-150 group-hover:opacity-100"
    datetime={new Date(item.createdAt).toISOString()}
  >
    {time}
  </time>
</div>
