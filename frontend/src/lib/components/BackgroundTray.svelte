<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let expanded = $state(true);
  let taskCount = $derived(pane.backgroundTasks.size);
  let visible = $derived(taskCount > 0);
  let taskEntries = $derived([...pane.backgroundTasks.entries()]);

  // Collapse automatically when all tasks complete.
  $effect(() => {
    if (taskCount === 0) {
      expanded = true;
    }
  });
</script>

{#if visible}
  <div class="border-t border-border bg-surface-1">
    <button
      type="button"
      onclick={() => expanded = !expanded}
      class="w-full px-4 py-2 text-xs text-text-secondary flex items-center justify-between cursor-pointer hover:bg-surface-2/40"
    >
      <span>Background ({taskCount} running)</span>
      <svg
        class="w-3 h-3 transition-transform duration-150"
        class:rotate-180={expanded}
        viewBox="0 0 12 12"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
      >
        <path d="M3 5l3 3 3-3" />
      </svg>
    </button>

    {#if expanded}
      <div class="pb-2">
        {#each taskEntries as [id] (id)}
          <div class="px-4 py-1 text-xs text-text-secondary flex items-center gap-2">
            <span class="w-1.5 h-1.5 rounded-full bg-accent animate-pulse shrink-0"></span>
            <span class="truncate">{id}</span>
          </div>
        {/each}
      </div>
    {/if}
  </div>
{/if}
