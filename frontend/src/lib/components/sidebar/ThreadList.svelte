<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import ThreadRow from './ThreadRow.svelte';

  let { pane }: { pane: ThreadPane } = $props();

  let threads = $derived(getThreads());
</script>

<div class="flex-1 overflow-y-auto px-2 py-1 space-y-0.5">
  {#each threads as thread (thread.id)}
    <ThreadRow {thread} {pane} />
  {/each}

  {#if threads.length === 0}
    <p class="text-xs text-text-secondary/50 text-center py-4">No threads yet</p>
  {/if}
</div>
