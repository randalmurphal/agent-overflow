<script lang="ts">
  import type { Snippet } from 'svelte';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import Icon from '../primitives/Icon.svelte';
  import Menu from '../primitives/Menu.svelte';
  import Popover from '../primitives/Popover.svelte';
  import { restorePickerFocus } from '../panes/paneComposerFocus';
  import type { PopoverCloseReason } from '../../utils/popoverOwnership';

  // A toolbar dropdown for the review pane: a bordered trigger (the
  // current value and a chevron) over the shared Menu. One shape for the
  // scope, base-branch, commit and edit pickers, so the toolbar reads as
  // one row of controls. The trigger never truncates on its own; a caller
  // whose value can be long passes it already shortened and puts the
  // whole value in `title`. A pick closes the menu.

  interface Props {
    /** The trigger's accessible name. */
    ariaLabel: string;
    /** The trigger's hover text; defaults to the accessible name. */
    title?: string;
    disabled?: boolean;
    testId?: string;
    /** Wider menus for long rows (commit subjects). */
    minWidthClass?: string;
    /** The trigger's content. */
    label: Snippet;
    /** Rows above the items, outside keyboard navigation (a stepper). */
    header?: Snippet<[close: () => void]>;
    /** The menu's items; `close` ends the menu after a pick. */
    children: Snippet<[close: () => void]>;
  }

  let {
    ariaLabel,
    title,
    disabled = false,
    testId,
    minWidthClass = 'min-w-[200px]',
    label,
    header,
    children,
  }: Props = $props();

  let open = $state(false);
  let triggerEl: HTMLButtonElement | undefined = $state(undefined);

  function close(reason?: PopoverCloseReason): void {
    open = false;
    restorePickerFocus(reason, { triggerEl });
  }

  function pick(): void {
    open = false;
  }
</script>

<button
  bind:this={triggerEl}
  type="button"
  class="inline-flex h-7 max-w-full shrink-0 items-center gap-1.5 rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 text-xs text-fg hover:border-border disabled:opacity-50 compact:h-9 compact:select-none {open ? 'border-border' : ''}"
  aria-label={ariaLabel}
  aria-haspopup="menu"
  aria-expanded={open}
  title={title ?? ariaLabel}
  data-testid={testId}
  {disabled}
  onclick={() => { open = !open; }}
>
  {@render label()}
  <Icon icon={ChevronDown} size={12} class="shrink-0 text-fg-subtle" />
</button>

<Popover anchor={triggerEl} {open} onClose={close} placement="bottom-start" role="none">
  <Menu {ariaLabel} onClose={close} {minWidthClass}>
    {#if header}{@render header(pick)}{/if}
    <!-- Bounded: a branch list or a long commit range scrolls inside. -->
    <div class="max-h-[min(60vh,28rem)] overflow-y-auto" data-testid={testId ? `${testId}-items` : undefined}>
      {@render children(pick)}
    </div>
  </Menu>
</Popover>
