<script lang="ts">
  // Nested thread list for a single project. The row's own padding
  // (8px base + 12px per indent level) handles visual alignment under
  // the project chevron; we don't add a `px-2` wrapper any more —
  // it stacked with the row's own padding and ate the usable width
  // at narrow sidebar widths. We deliberately do NOT virtualize here
  // — if a single project ever accumulates enough threads to need it,
  // we'll reconsider; today 50-thread projects render flat at 60fps.

  import type { Thread } from '../../types/models';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import ThreadRow from './ThreadRow.svelte';

  interface Props {
    threads: Thread[];
    pane: ThreadPane;
  }

  let { threads, pane }: Props = $props();
</script>

{#if threads.length === 0}
  <p
    class="ml-6 mr-2 my-1 text-[11px] text-fg-hint italic select-none"
    data-testid="project-thread-list-empty"
  >
    No threads yet
  </p>
{:else}
  <div
    class="flex flex-col gap-px px-1"
    role="list"
    aria-label="Project threads"
    data-testid="project-thread-list"
  >
    {#each threads as thread (thread.id)}
      <div role="listitem">
        <ThreadRow {thread} {pane} indent={1} />
      </div>
    {/each}
  </div>
{/if}
