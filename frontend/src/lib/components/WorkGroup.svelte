<script lang="ts">
  import type { ThreadPane } from '../stores/thread.svelte';
  import WorkEntry, { type WorkEntryData } from './WorkEntry.svelte';

  let { entries, pane }: { entries: WorkEntryData[]; pane: ThreadPane } = $props();

  let hasRunning = $derived(entries.some((e) => e.status === 'running'));
  let completedCount = $derived(entries.filter((e) => e.status === 'completed').length);

  // Default: expanded when anything is running, collapsed when all done.
  let expanded: boolean = $state(false);

  // Track whether the user has manually toggled. If not, follow the auto rule.
  let userToggled: boolean = $state(false);

  let isExpanded = $derived(userToggled ? expanded : hasRunning);

  function toggle() {
    userToggled = true;
    expanded = !isExpanded;
  }
</script>

{#if entries.length > 0}
  <div class="mb-3">
    <button
      onclick={toggle}
      class="w-full bg-surface-1 rounded px-3 py-2 cursor-pointer flex items-center justify-between text-sm text-text-secondary hover:text-text-primary"
    >
      <span class="flex items-center gap-2">
        <span
          class="inline-block transition-transform duration-150
            {isExpanded ? 'rotate-90' : 'rotate-0'}"
        >
          &#9654;
        </span>
        <span>Operations ({entries.length})</span>
      </span>
      <span class="text-xs">
        {completedCount}/{entries.length} completed
      </span>
    </button>

    {#if isExpanded}
      <div class="mt-1 flex flex-col gap-1 pl-2">
        {#each entries as entry (entry.id)}
          <WorkEntry {entry} />
        {/each}
      </div>
    {/if}
  </div>
{/if}
