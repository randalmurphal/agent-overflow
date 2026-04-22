<script lang="ts">
  // Right-edge hover action buttons for a thread row. Extracted from
  // ThreadRow.svelte to keep the row under 300 lines. Each button is a
  // pure onclick forwarder — no local state, no fetches.

  import MessageSquare from 'lucide-svelte/icons/message-square';
  import GitFork from 'lucide-svelte/icons/git-fork';
  import Trash2 from 'lucide-svelte/icons/trash-2';
  import Archive from 'lucide-svelte/icons/archive';
  import ArchiveRestore from 'lucide-svelte/icons/archive-restore';
  import Icon from '../primitives/Icon.svelte';
  import type { Thread } from '../../types/models';

  let {
    thread,
    onStartDiscussion,
    canStartDiscussion,
    onFork,
    onDelete,
    onArchive,
    onUnarchive,
  }: {
    thread: Thread;
    onStartDiscussion?: (e: MouseEvent) => void;
    canStartDiscussion: boolean;
    onFork: (e: MouseEvent) => void;
    onDelete: (e: MouseEvent) => void;
    onArchive: (e: MouseEvent) => void;
    onUnarchive: (e: MouseEvent) => void;
  } = $props();

  const btnClass =
    'opacity-0 group-hover:opacity-100 focus-visible:opacity-100 transition-opacity duration-100 ' +
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40';
</script>

{#if onStartDiscussion && canStartDiscussion}
  <button
    onclick={onStartDiscussion}
    class="{btnClass} text-fg-subtle hover:text-fg hover:bg-surface-2/30"
    aria-label="Start discussion on thread"
    title="Start discussion"
  >
    <Icon icon={MessageSquare} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{/if}
<button
  onclick={onFork}
  disabled={!thread.sessionRef}
  class="{btnClass} text-fg-subtle hover:text-fg hover:bg-surface-2/30 disabled:cursor-not-allowed disabled:opacity-0"
  aria-label="Fork thread"
  title={thread.sessionRef ? 'Fork thread' : 'Fork available after the thread has provider state'}
>
  <Icon icon={GitFork} size={12} strokeWidth={2} class="opacity-90" />
</button>
<button
  onclick={onDelete}
  class="{btnClass} text-error/60 hover:text-error hover:bg-error/10"
  aria-label="Delete thread"
>
  <Icon icon={Trash2} size={12} strokeWidth={2} class="opacity-90" />
</button>
{#if thread.archived}
  <button
    onclick={onUnarchive}
    data-testid="thread-row-unarchive"
    class="{btnClass} text-fg-subtle hover:text-fg hover:bg-surface-2/30"
    aria-label="Unarchive thread"
    title="Unarchive thread"
  >
    <Icon icon={ArchiveRestore} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{:else}
  <button
    onclick={onArchive}
    data-testid="thread-row-archive"
    class="{btnClass} text-fg-subtle hover:text-fg hover:bg-surface-2/30"
    aria-label="Archive thread"
  >
    <Icon icon={Archive} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{/if}
