<script lang="ts">
  // Hover-revealed right-side row actions. The row's right-hand slot swaps
  // the relative time out and these buttons in via group-hover on the
  // parent row; see ThreadRow.svelte for the fade mechanics.
  //
  // Other row actions (Rename, Fork, Mark Unread, Copy Path, Copy ID,
  // Delete) live in ThreadContextMenu so the hover affordance stays compact
  // at narrow ~200px sidebar widths.

  import Archive from 'lucide-svelte/icons/archive';
  import ArchiveRestore from 'lucide-svelte/icons/archive-restore';
  import Icon from '../primitives/Icon.svelte';
  import type { Thread } from '../../types/models';
  import type { ThreadActionCtx } from './threadRowActions';
  import ThreadRowPinButton from './ThreadRowPinButton.svelte';

  let {
    thread,
    onArchive,
    onUnarchive,
    showPin = false,
    isPinned = false,
    buildCtx,
  }: {
    thread: Thread;
    onArchive: (e: MouseEvent) => void;
    onUnarchive: (e: MouseEvent) => void;
    showPin?: boolean;
    isPinned?: boolean;
    buildCtx: () => ThreadActionCtx;
  } = $props();

  const btnClass =
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40';
</script>

{#if showPin}
  <ThreadRowPinButton {isPinned} {buildCtx} />
{/if}

{#if thread.archived}
  <button
    type="button"
    onclick={onUnarchive}
    data-testid="thread-row-unarchive"
    class={btnClass}
    aria-label="Unarchive Thread"
    title="Unarchive Thread"
  >
    <Icon icon={ArchiveRestore} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{:else}
  <button
    type="button"
    onclick={onArchive}
    data-testid="thread-row-archive"
    class={btnClass}
    aria-label="Archive Thread"
    title="Archive Thread"
  >
    <Icon icon={Archive} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{/if}
