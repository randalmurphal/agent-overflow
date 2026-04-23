<script lang="ts">
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();

  // highlightedContent is populated by the server on every write path.
  // Keep the outer body node stable while streaming so a throttled HTML
  // render arriving mid-message updates content without swapping the whole
  // message surface. The fallback branch uses escaped text because summary is
  // untrusted markdown.

  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

<div class="group mb-6" data-item-kind={item.kind}>
  <div
    class="markdown-body text-fg-muted"
    data-testid="assistant-message-body"
    data-render-mode={item.highlightedContent ? 'html' : 'text'}
  >
    {#if item.highlightedContent}
      {@html item.highlightedContent}
    {:else}
      <p class="whitespace-pre-wrap text-[13px] leading-[1.65]">{item.summary}</p>
    {/if}
  </div>
  <time
    class="mt-1.5 block text-[10px] text-fg-hint opacity-0 transition-opacity duration-150 group-hover:opacity-100"
    datetime={new Date(item.createdAt).toISOString()}
  >
    {time}
  </time>
</div>
