<script lang="ts">
  import GitFork from 'lucide-svelte/icons/git-fork';
  import type { Thread } from '../../types/models';
  import Icon from '../primitives/Icon.svelte';

  let {
    forkParent,
    onJumpToParent,
  }: {
    forkParent: Thread | undefined;
    onJumpToParent: (e: MouseEvent) => void | Promise<void>;
  } = $props();

  let title = $derived(
    forkParent
      ? `Forked from "${forkParent.title || 'Untitled'}" — click to open parent`
      : 'Forked thread (parent not loaded in sidebar)',
  );
  let ariaLabel = $derived(
    forkParent
      ? `Open fork parent ${forkParent.title || 'Untitled'}`
      : 'Fork parent not loaded',
  );
</script>

<button
  type="button"
  data-testid="thread-row-fork-lineage"
  onclick={onJumpToParent}
  disabled={!forkParent}
  class="flex h-4 w-4 shrink-0 items-center justify-center rounded-[var(--radius-field)] text-fg-subtle transition-colors hover:bg-surface-2/30 hover:text-fg focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 disabled:cursor-not-allowed disabled:opacity-45 disabled:hover:bg-transparent disabled:hover:text-fg-subtle"
  {title}
  aria-label={ariaLabel}
>
  <Icon icon={GitFork} size={12} strokeWidth={2.2} class="opacity-85" />
</button>
