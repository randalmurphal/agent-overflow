<script lang="ts">
  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreads } from '../../stores/threads.svelte';
  import ThreadRow from './ThreadRow.svelte';

  let {
    pane,
    onStartDiscussion,
  }: {
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
  } = $props();

  let threads = $derived(getThreads());
</script>

<div class="flex-1 overflow-y-auto px-2 py-2 space-y-1" role="list" aria-label="Threads">
  {#each threads as thread (thread.id)}
    <div role="listitem">
      <ThreadRow {thread} {pane} {onStartDiscussion} />
    </div>
  {/each}

  {#if threads.length === 0}
    <div class="mx-1 mt-3 rounded-2xl border border-dashed border-border/70 bg-surface-0/45 px-4 py-8 text-center shadow-[inset_0_1px_0_rgba(255,255,255,0.02)]">
      <div class="mx-auto flex h-10 w-10 items-center justify-center rounded-2xl border border-border/60 bg-surface-1/80 text-text-secondary/70">
        <svg class="h-4 w-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
        </svg>
      </div>
      <p class="mt-3 text-sm font-medium text-text-primary">No threads yet</p>
      <p class="mt-1 text-xs text-text-secondary/70">Create a new thread to start a session in this workspace.</p>
    </div>
  {/if}
</div>
