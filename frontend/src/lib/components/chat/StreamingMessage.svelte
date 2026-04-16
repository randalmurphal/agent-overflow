<script lang="ts">
  import Markdown from '../shared/Markdown.svelte';

  let { content }: { content: string } = $props();

  let completed = $derived.by(() => {
    const lastBreak = content.lastIndexOf('\n\n');
    if (lastBreak === -1) return '';
    return content.slice(0, lastBreak);
  });

  let inProgress = $derived.by(() => {
    const lastBreak = content.lastIndexOf('\n\n');
    if (lastBreak === -1) return content;
    return content.slice(lastBreak + 2);
  });
</script>

<div class="flex justify-start mb-3" aria-live="polite" aria-relevant="additions" role="log">
  <div class="max-w-[85%] rounded-lg px-4 py-2.5 bg-surface-2 text-text-primary">
    {#if completed}
      <Markdown content={completed} />
    {/if}
    {#if inProgress}
      <p class="whitespace-pre-wrap text-sm leading-relaxed">{inProgress}<span class="inline-block w-1.5 h-4 bg-accent animate-pulse ml-0.5 align-text-bottom" aria-hidden="true"></span></p>
    {:else}
      <span class="inline-block w-1.5 h-4 bg-accent animate-pulse align-text-bottom" aria-hidden="true"></span>
    {/if}
  </div>
</div>
