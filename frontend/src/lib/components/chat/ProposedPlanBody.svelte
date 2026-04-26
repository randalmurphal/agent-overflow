<script lang="ts">
  import ChatMarkdown from './ChatMarkdown.svelte';

  interface Props {
    markdown: string;
    previewOnly?: boolean;
  }

  let { markdown, previewOnly = false }: Props = $props();
</script>

<div class="mt-4">
  <div class:overflow-hidden={previewOnly} class:max-h-80={previewOnly} class="relative">
    <ChatMarkdown source={markdown} />
    {#if previewOnly}
      <div class="pointer-events-none absolute inset-x-0 bottom-0 h-24 bg-linear-to-t from-surface-0 via-surface-0/80 to-transparent"></div>
    {/if}
  </div>
</div>
