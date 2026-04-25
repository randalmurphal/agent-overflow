<script lang="ts">
  // Per-row pin/unpin affordance. Lives left-of-chevron in the row's
  // leading slot. Visible-when-pinned (filled icon, always shown) so
  // pin state reads at a glance; hover-revealed when unpinned (outline
  // icon) so the action is discoverable without cluttering the row at
  // rest. Click toggles the persisted pin via threadRowActions.
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
    'flex items-center justify-center w-4 h-4 rounded text-fg-subtle hover:text-fg hover:bg-surface-2/30 shrink-0 cursor-pointer transition-opacity focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 ' +
    (isPinned
      ? 'opacity-100 text-accent'
      : 'opacity-0 group-hover/thread-row:opacity-100 focus-visible:opacity-100')
  }
>
  <Icon
    icon={Pin}
    size={11}
    strokeWidth={2}
    class={isPinned ? 'opacity-100 fill-current' : 'opacity-90'}
  />
</button>
