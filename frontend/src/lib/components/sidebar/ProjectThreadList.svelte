<script lang="ts">
  // Nested thread list for a single project. Renders ThreadRow at indent=1
  // so rows visually line up under the project chevron. We deliberately
  // do NOT virtualize here — if a single project ever accumulates enough
  // threads to need it, we'll reconsider; today 50-thread projects render
  // flat at full 60fps.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ThreadRow from './ThreadRow.svelte';

  interface Props {
    threads: Thread[];
    pane: ThreadPane;
    onStartDiscussion?: (thread: Thread) => void;
  }

  let { threads, pane, onStartDiscussion }: Props = $props();
</script>

{#if threads.length === 0}
  <p
    class="ml-9 mr-3 my-1 text-[11px] text-text-secondary/60 italic select-none"
    data-testid="project-thread-list-empty"
  >
    No threads yet
  </p>
{:else}
  <div
    class="flex flex-col"
    role="list"
    aria-label="Project threads"
    data-testid="project-thread-list"
  >
    {#each threads as thread (thread.id)}
      <div role="listitem" class="px-2">
        <ThreadRow {thread} {pane} {onStartDiscussion} indent={1} />
      </div>
    {/each}
  </div>
{/if}
