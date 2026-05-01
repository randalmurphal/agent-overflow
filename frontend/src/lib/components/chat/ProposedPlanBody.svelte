<script lang="ts">
  import ChatMarkdown from './ChatMarkdown.svelte';

  interface Props {
    markdown: string;
    previewOnly?: boolean;
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in the plan markdown. */
    workspacePath?: string;
  }

  let { markdown, previewOnly = false, workspacePath = '' }: Props = $props();
</script>

<div class="mt-4">
  <div class:overflow-y-auto={previewOnly} class:max-h-96={previewOnly} class="relative">
    <ChatMarkdown source={markdown} {workspacePath} />
  </div>
</div>
