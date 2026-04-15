<script lang="ts">
  import type { Item } from '../types/models';

  let { item }: { item: Item } = $props();

  /**
   * Split text into segments of plain text and fenced code blocks.
   * Returns an array of { type: 'text' | 'code', content, lang? }.
   */
  function parseContent(text: string): Array<{ type: 'text' | 'code'; content: string; lang?: string }> {
    const segments: Array<{ type: 'text' | 'code'; content: string; lang?: string }> = [];
    const codeBlockRegex = /```(\w*)\n([\s\S]*?)```/g;
    let lastIndex = 0;
    let match: RegExpExecArray | null;

    while ((match = codeBlockRegex.exec(text)) !== null) {
      if (match.index > lastIndex) {
        segments.push({ type: 'text', content: text.slice(lastIndex, match.index) });
      }
      segments.push({ type: 'code', content: match[2], lang: match[1] || undefined });
      lastIndex = match.index + match[0].length;
    }

    if (lastIndex < text.length) {
      segments.push({ type: 'text', content: text.slice(lastIndex) });
    }

    if (segments.length === 0) {
      segments.push({ type: 'text', content: text });
    }

    return segments;
  }

  let segments = $derived(parseContent(item.summary));
</script>

<div class="flex justify-start mb-3">
  <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
    {#each segments as segment}
      {#if segment.type === 'code'}
        <div class="my-2 rounded bg-surface-0 border border-border overflow-x-auto">
          {#if segment.lang}
            <div class="px-3 py-1 text-xs text-text-secondary border-b border-border">{segment.lang}</div>
          {/if}
          <pre class="px-3 py-2 text-sm leading-relaxed"><code>{segment.content}</code></pre>
        </div>
      {:else}
        <p class="whitespace-pre-wrap text-sm leading-relaxed">{segment.content}</p>
      {/if}
    {/each}
  </div>
</div>
