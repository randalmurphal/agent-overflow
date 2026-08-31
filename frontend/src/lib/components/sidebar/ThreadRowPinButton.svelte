<script lang="ts">
  // Per-row pin/unpin affordance. Sits in the row's leading pin slot
  // (the gutter between the project rail and the row content). Pinned
  // state is rendered at rest as a filled pin; unpinned rows reveal an
  // outlined pin only on row hover or keyboard focus so the gutter reads
  // empty until the user signals intent. Keyboard reveal keys off
  // `group-has-[:focus-visible]/thread-row` (a focus-VISIBLE descendant),
  // NOT `:focus-within` — the row button is tabindex=0, so a plain mouse
  // click focuses it and `:focus-within` would leave the pin stuck on.
  //
  // Render-time guard: top-level rows only (indent ≤ 1). Discussion
  // children don't pin individually — the parent thread is the pin
  // target for that whole subtree. The guard lives in ThreadRow.

  import Pin from '@lucide/svelte/icons/pin';
  import Icon from '../primitives/Icon.svelte';
  import {
    PIN_GROUP_BACK,
    PIN_GROUP_FRONT,
    pinThreadAction,
    setThreadPinGroupAction,
    unpinThreadAction,
    type ThreadActionCtx,
  } from './threadRowActions';

  interface Props {
    isPinned: boolean;
    pinGroup?: number;
    /** Builds the action context lazily so we don't capture stale
     *  refs across re-renders. */
    buildCtx: () => ThreadActionCtx;
  }

  let { isPinned, pinGroup, buildCtx }: Props = $props();

  let isBackBurner = $derived(isPinned && pinGroup === PIN_GROUP_BACK);

  function handleToggle(e: MouseEvent): void {
    e.stopPropagation();
    if (isPinned) {
      void unpinThreadAction(buildCtx());
    } else {
      void pinThreadAction(buildCtx());
    }
  }

  function handleContextMenu(e: MouseEvent): void {
    // An unpinned affordance keeps the row's normal context menu. Once
    // pinned, right-click is the direct two-tier toggle promised by the
    // pin control itself.
    if (!isPinned) return;
    e.preventDefault();
    e.stopPropagation();
    void setThreadPinGroupAction(
      buildCtx(),
      isBackBurner ? PIN_GROUP_FRONT : PIN_GROUP_BACK,
    );
  }
</script>

<button
  type="button"
  onclick={handleToggle}
  oncontextmenu={handleContextMenu}
  data-testid="thread-row-pin"
  data-pin-group={isPinned ? (isBackBurner ? 'back' : 'front') : undefined}
  aria-label={isPinned ? 'Unpin Thread' : 'Pin Thread'}
  aria-pressed={isPinned}
  title={isPinned ? 'Unpin Thread' : 'Pin Thread'}
  class={
    'flex items-center justify-center h-4 w-4 rounded-[var(--radius-field)] shrink-0 cursor-pointer ' +
    'hover:text-fg hover:bg-surface-2/40 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent/40 ' +
    'transition-opacity duration-150 ' +
    (isPinned
      ? `${isBackBurner ? 'text-fg-muted' : 'text-accent'} opacity-100 pointer-events-auto`
      : 'text-fg-subtle opacity-0 pointer-events-none group-hover/thread-item:opacity-100 group-hover/thread-item:pointer-events-auto group-has-[:focus-visible]/thread-row:opacity-100 group-has-[:focus-visible]/thread-row:pointer-events-auto')
  }
>
  <Icon
    icon={Pin}
    size={12}
    strokeWidth={2}
    class={isPinned ? 'opacity-100 fill-current' : 'opacity-90'}
  />
</button>
