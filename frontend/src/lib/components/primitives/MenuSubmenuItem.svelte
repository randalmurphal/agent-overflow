<script lang="ts">
  // Menu row that expands a nested Popover+Menu on hover / ArrowRight.
  // The child menu re-uses the Menu primitive so arrow-nav, typeahead,
  // and tabindex rules all come for free inside the submenu.
  //
  // Hover delay (150ms) prevents the submenu from thrashing open when
  // the user's cursor passes over this row on its way somewhere else.
  // The same delay applies to close so diagonal pointer paths between
  // the trigger and the open submenu don't flicker.

  import type { Snippet } from 'svelte';
  import Popover from './Popover.svelte';
  import Menu from './Menu.svelte';
  import {
    popoverCloseRestoresFocus,
    type PopoverCloseReason,
  } from '../../utils/popoverOwnership';

  interface Props {
    label: string;
    icon?: Snippet;
    children: Snippet;
    disabled?: boolean;
  }

  let { label, icon, children, disabled = false }: Props = $props();

  let buttonEl: HTMLButtonElement | undefined = $state(undefined);
  let open = $state(false);
  let openTimer: ReturnType<typeof setTimeout> | null = null;
  let closeTimer: ReturnType<typeof setTimeout> | null = null;

  const HOVER_DELAY_MS = 150;

  function cancelTimers(): void {
    if (openTimer) { clearTimeout(openTimer); openTimer = null; }
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
  }

  function scheduleOpen(): void {
    if (disabled) return;
    if (open) return;
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
    if (openTimer) return;
    openTimer = setTimeout(() => {
      openTimer = null;
      open = true;
    }, HOVER_DELAY_MS);
  }

  function scheduleClose(): void {
    if (openTimer) { clearTimeout(openTimer); openTimer = null; }
    if (!open) return;
    if (closeTimer) return;
    closeTimer = setTimeout(() => {
      closeTimer = null;
      open = false;
    }, HOVER_DELAY_MS);
  }

  function openImmediate(): void {
    if (disabled) return;
    cancelTimers();
    open = true;
  }

  function closeImmediate(): void {
    cancelTimers();
    open = false;
  }

  function handlePointerEnter(): void {
    scheduleOpen();
  }

  function handlePointerLeave(): void {
    scheduleClose();
  }

  function handleKeydown(e: KeyboardEvent): void {
    if (disabled) return;
    switch (e.key) {
      case 'ArrowRight':
      case 'Enter':
      case ' ':
        e.preventDefault();
        e.stopPropagation();
        openImmediate();
        return;
      case 'ArrowLeft':
        if (open) {
          e.preventDefault();
          e.stopPropagation();
          closeImmediate();
        }
        return;
      case 'Escape':
        if (open) {
          // Close the submenu first; don't let the parent menu also see
          // this Escape (which would close the whole menu stack).
          e.preventDefault();
          e.stopPropagation();
          closeImmediate();
          buttonEl?.focus({ preventScroll: true });
        }
        return;
    }
  }

  function handleSubmenuPointerEnter(): void {
    // Cursor entered the open submenu — cancel any pending close so the
    // submenu stays up while the user hovers over it.
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
  }

  function handleSubmenuPointerLeave(): void {
    scheduleClose();
  }

  function handlePopoverClose(reason?: PopoverCloseReason): void {
    closeImmediate();
    // On explicit dismissals, return focus to the trigger so parent
    // Menu's roving tabindex picks up where the user left off. A close
    // the user caused by engaging something else (outside click, the
    // trigger scrolling away) leaves focus where they put it — and the
    // restore never scrolls, so a trigger carried out of the pane strip
    // can't snap the strip back. (This lives in primitives/, so it can't
    // use panes/' restorePickerFocus; the gate is the shared predicate.)
    if (!popoverCloseRestoresFocus(reason)) return;
    buttonEl?.focus({ preventScroll: true });
  }

  // Bubbled menuitem-select from a descendant item inside the submenu
  // closes the submenu. Combined with the parent Menu's own handler for
  // the same event, the full stack collapses on selection.
  // The custom event isn't part of HTML's typed handler list, so we
  // attach it via addEventListener once the wrapper mounts.
  let submenuWrapperEl: HTMLDivElement | undefined = $state(undefined);

  function handleInnerMenuSelect(): void {
    closeImmediate();
  }

  $effect(() => {
    if (!submenuWrapperEl) return;
    const listener = () => handleInnerMenuSelect();
    submenuWrapperEl.addEventListener('menuitem-select', listener);
    return () => submenuWrapperEl?.removeEventListener('menuitem-select', listener);
  });

  // Ensure pending timers don't fire after unmount.
  $effect(() => {
    return () => cancelTimers();
  });
</script>

<!-- svelte-ignore a11y_click_events_have_key_events — onkeydown is handled -->
<button
  bind:this={buttonEl}
  type="button"
  role="menuitem"
  aria-haspopup="menu"
  aria-expanded={open}
  aria-disabled={disabled ? 'true' : undefined}
  data-menuitem
  data-submenu-trigger
  tabindex={-1}
  onclick={openImmediate}
  onkeydown={handleKeydown}
  onpointerenter={handlePointerEnter}
  onpointerleave={handlePointerLeave}
  class={[
    'w-full flex items-center gap-2 px-3 py-1.5 text-sm text-left text-text-primary',
    'cursor-pointer select-none',
    'focus-visible:outline-none',
    'hover:bg-surface-2/60 focus:bg-surface-2/60',
    disabled ? 'opacity-50 cursor-not-allowed hover:bg-transparent focus:bg-transparent' : '',
  ].join(' ')}
>
  {#if icon}
    <span class="flex h-4 w-4 items-center justify-center text-text-secondary" aria-hidden="true">
      {@render icon()}
    </span>
  {/if}
  <span class="flex-1 truncate">{label}</span>
  <span class="text-text-secondary/70" aria-hidden="true">&#9656;</span>
</button>

<Popover
  anchor={buttonEl}
  {open}
  onClose={handlePopoverClose}
  placement="right-start"
  offset={2}
  role="none"
>
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    bind:this={submenuWrapperEl}
    onpointerenter={handleSubmenuPointerEnter}
    onpointerleave={handleSubmenuPointerLeave}
  >
    <Menu ariaLabel={label} onClose={handlePopoverClose}>
      {@render children()}
    </Menu>
  </div>
</Popover>
