<script lang="ts">
  import ChatMarkdown from './ChatMarkdown.svelte';

  interface Props {
    markdown: string;
    capped?: boolean;
    loading?: boolean;
    error?: string | null;
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in the plan markdown. */
    workspacePath?: string;
  }

  let {
    markdown,
    capped = false,
    loading = false,
    error = null,
    workspacePath = '',
  }: Props = $props();
</script>

<div class="mt-4">
  <div
    class:h-96={capped}
    class:overflow-y-auto={capped}
    class="relative"
    aria-busy={loading ? 'true' : undefined}
  >
    <ChatMarkdown source={markdown} {workspacePath} />
    {#if error}
      <p class="mt-2 text-xs text-error" role="alert">
        Failed to load full plan.
      </p>
    {/if}
  </div>
</div>
