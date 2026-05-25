<script lang="ts">
  import Archive from 'lucide-svelte/icons/archive';
  import Icon from '../primitives/Icon.svelte';

  let {
    onArchive,
  }: {
    onArchive: (e: MouseEvent) => void;
  } = $props();

  const btnClass =
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 ' +
    'cursor-pointer text-fg-subtle hover:text-fg hover:bg-surface-2/40 ' +
    'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40';
</script>

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
