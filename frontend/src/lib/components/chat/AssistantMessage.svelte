<script lang="ts">
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();

  // highlightedContent is populated by the server on every write path;
  // pre-v19 rows (or streaming edge cases where the render failed)
  // leave it empty. Rather than render empty chrome we fall through to
  // the raw summary as escaped text content. Using `{ }` here — not
  // `{@html}` — because summary is untrusted markdown.

  const time = $derived(
    new Date(item.createdAt).toLocaleTimeString(undefined, {
      hour: 'numeric',
      minute: '2-digit',
    }),
  );
</script>

<div class="group mb-6" data-item-kind={item.kind}>
  {#if item.highlightedContent}
    <div class="markdown-body text-fg-muted">{@html item.highlightedContent}</div>
  {:else}
    <p class="whitespace-pre-wrap text-[13px] leading-[1.65] text-fg-muted">{item.summary}</p>
  {/if}
  <time
    class="mt-1.5 block text-[10px] text-fg-hint opacity-0 transition-opacity duration-150 group-hover:opacity-100"
    datetime={new Date(item.createdAt).toISOString()}
  >
    {time}
  </time>
</div>
