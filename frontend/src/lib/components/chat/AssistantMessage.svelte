<script lang="ts">
  import type { Item } from '../../types/models';

  let { item }: { item: Item } = $props();

  // highlightedContent is populated by the server on every write path;
  // pre-v19 rows (or streaming edge cases where the render failed)
  // leave it empty. Rather than render an empty bubble we fall through
  // to the raw summary as escaped text content. Using `{ }` here — not
  // `{@html}` — because summary is untrusted markdown.
</script>

<div class="flex justify-start mb-3">
  <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
    {#if item.highlightedContent}
      <div class="markdown-body">{@html item.highlightedContent}</div>
    {:else}
      <p class="whitespace-pre-wrap text-sm leading-relaxed">{item.summary}</p>
    {/if}
  </div>
</div>
