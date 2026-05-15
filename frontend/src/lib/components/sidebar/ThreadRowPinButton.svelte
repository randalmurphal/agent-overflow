<script lang="ts">
  // Per-row pin/unpin affordance. Lives in the row's right-side hover
  // action cluster next to archive/unarchive so the leading title column
  // stays aligned.
  //
  // Render-time guard: top-level rows only (indent ≤ 1). Discussion
  // children don't pin individually — the parent thread is the pin
  // target for that whole subtree.

  import Pin from 'lucide-svelte/icons/pin';
  import Icon from '../primitives/Icon.svelte';
  import {
    pinThreadAction,
    unpinThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';

  interface Props {
    isPinned: boolean;
    /** Builds the action context lazily so we don't capture stale
     *  refs across re-renders. */
    buildCtx: () => ThreadActionCtx;
  }

  let { isPinned, buildCtx }: Props = $props();

  function handleToggle(e: MouseEvent): void {
    e.stopPropagation();
    if (isPinned) {
      void unpinThreadAction(buildCtx());
    } else {
      void pinThreadAction(buildCtx());
    }
  }
</script>

<button
  type="button"
  onclick={handleToggle}
  data-testid="thread-row-pin"
  aria-label={isPinned ? 'Unpin Thread' : 'Pin Thread'}
  aria-pressed={isPinned}
  title={isPinned ? 'Unpin Thread' : 'Pin Thread'}
  class={
    'flex items-center justify-center h-5 w-5 rounded-[var(--radius-field)] shrink-0 cursor-pointer ' +
    'text-fg-subtle hover:text-fg hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 ' +
    (isPinned ? 'text-accent' : '')
  }
>
  <Icon
    icon={Pin}
    size={12}
    strokeWidth={2}
    class={isPinned ? 'opacity-100 fill-current' : 'opacity-90'}
  />
</button>
