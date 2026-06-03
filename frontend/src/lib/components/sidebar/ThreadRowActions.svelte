<script lang="ts">
  // The single row-hover affordance. A normal thread gets Archive; a
  // terminal thread gets Delete (terminals aren't archivable — there's
  // nothing to keep). The parent passes exactly one callback and this
  // renders the matching button, so the row stays a thin shell.
  import Archive from 'lucide-svelte/icons/archive';
  import X from 'lucide-svelte/icons/x';
  import Icon from '../primitives/Icon.svelte';

  let {
    onArchive,
    onDelete,
  }: {
    onArchive?: (e: MouseEvent) => void;
    onDelete?: (e: MouseEvent) => void;
  } = $props();

  const btnClass =
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40';
</script>

{#if onDelete}
  <button
    type="button"
    onclick={onDelete}
    data-testid="thread-row-delete"
    class={btnClass}
    aria-label="Delete Terminal"
    title="Delete Terminal"
  >
    <Icon icon={X} size={12} strokeWidth={2} class="opacity-90" />
  </button>
{:else if onArchive}
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
