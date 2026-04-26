<script lang="ts">
  import ChatMarkdown from './ChatMarkdown.svelte';

  interface Props {
    markdown: string;
    canCollapse: boolean;
    expanded: boolean;
    onToggleExpanded: () => void | Promise<void>;
  }

  let { markdown, canCollapse, expanded, onToggleExpanded }: Props = $props();
</script>

<div class="mt-4">
  <div class:overflow-hidden={canCollapse && !expanded} class:max-h-104={canCollapse && !expanded} class="relative">
    <ChatMarkdown source={markdown} />
    {#if canCollapse && !expanded}
      <div class="pointer-events-none absolute inset-x-0 bottom-0 h-24 bg-linear-to-t from-surface-1 via-surface-1/80 to-transparent"></div>
    {/if}
  </div>
  {#if canCollapse}
    <div class="mt-4 flex justify-center">
      <button
        onclick={() => void onToggleExpanded()}
        class="rounded-md border border-border px-3 py-1.5 text-sm text-text-secondary hover:text-text-primary hover:border-text-secondary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      >
        {expanded ? 'Collapse plan' : 'Expand plan'}
      </button>
    </div>
  {/if}
</div>
