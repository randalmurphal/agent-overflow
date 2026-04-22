<script lang="ts">
  // Hover-revealed archive button. The row's right-hand slot swaps the
  // relative time out and this button in via group-hover on the parent
  // row; see ThreadRow.svelte for the fade mechanics.
  //
  // Fork, Start-discussion, and Delete moved into ThreadContextMenu —
  // the row's hover affordance is intentionally reduced to a single
  // archive/unarchive icon so the compact layout stays unambiguous at
  // 200px sidebar widths.

  import Archive from 'lucide-svelte/icons/archive';
  import ArchiveRestore from 'lucide-svelte/icons/archive-restore';
  import Icon from '../primitives/Icon.svelte';
  import type { Thread } from '../../types/models';

  let {
    thread,
    onArchive,
    onUnarchive,
  }: {
    thread: Thread;
    onArchive: (e: MouseEvent) => void;
    onUnarchive: (e: MouseEvent) => void;
  } = $props();

  const btnClass =
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40';
</script>

{#if thread.archived}
  <button
    type="button"
    onclick={onUnarchive}
    data-testid="thread-row-unarchive"
    class={btnClass}
    aria-label="Unarchive thread"
    title="Unarchive thread"
  >
    <Icon icon={ArchiveRestore} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{:else}
  <button
    type="button"
    onclick={onArchive}
    data-testid="thread-row-archive"
    class={btnClass}
    aria-label="Archive thread"
    title="Archive thread"
  >
    <Icon icon={Archive} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{/if}
