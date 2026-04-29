<script lang="ts">
  import ChatMarkdown from './ChatMarkdown.svelte';

  interface Props {
    markdown: string;
    previewOnly?: boolean;
  }

  let { markdown, previewOnly = false }: Props = $props();
</script>

<div class="mt-4">
  <div class:overflow-y-auto={previewOnly} class:max-h-96={previewOnly} class="relative">
    <ChatMarkdown source={markdown} />
  </div>
</div>
