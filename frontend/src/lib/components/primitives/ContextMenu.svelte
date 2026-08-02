<script lang="ts">
  /**
   * Cursor-anchored menu shell. Owns the three things every right-click
   * menu needs and nothing else: viewport clamping, click-outside, and
   * Escape. Callers supply the rows as `<MenuItem>` children and keep
   * ownership of what the rows do.
   *
   * Paired with `Menu` (roles + roving tabindex + arrow nav) rather than
   * `Popover`, which anchors to an element rather than a point.
   */

  import { onMount, type Snippet } from 'svelte';
  import Menu from './Menu.svelte';

  interface Props {
    /** Viewport coordinates of the invoking pointer event. */
    x: number;
    y: number;
    ariaLabel: string;
    onDismiss: () => void;
    minWidthClass?: string;
    children: Snippet;
  }

  let { x, y, ariaLabel, onDismiss, minWidthClass, children }: Props = $props();

  const MARGIN_PX = 4;

  // Menu dimensions are only known once it is in the DOM, so the clamp runs
  // in an effect rather than at init: effects flush after the insert and
  // before paint, so the menu is never seen at the unclamped position — and,
  // unlike a queued microtask, an effect cannot outlive the component and
  // read an anchor its host has already torn down.
  let menuEl: HTMLDivElement | undefined = $state(undefined);
  let adjustedX = $state(0);
  let adjustedY = $state(0);

  $effect(() => {
    const rect = menuEl?.getBoundingClientRect();
    const maxX = window.innerWidth - (rect?.width ?? 0) - MARGIN_PX;
    const maxY = window.innerHeight - (rect?.height ?? 0) - MARGIN_PX;
    adjustedX = Math.max(MARGIN_PX, Math.min(x, maxX));
    adjustedY = Math.max(MARGIN_PX, Math.min(y, maxY));
  });

  onMount(() => {
    // Capture phase so dismissal runs before any child click handler.
    // A right-click elsewhere dismisses and re-opens at the new spot —
    // the owning host re-raises with fresh coordinates.
    const handleDocPointer = (e: MouseEvent): void => {
      if (menuEl && e.target instanceof Node && !menuEl.contains(e.target)) {
        onDismiss();
      }
    };
    // Escape anywhere in the document closes the menu. Without it a user
    // who opened via keyboard (or tabbed in) could only leave by picking
    // an action, which violates the WAI-ARIA menu pattern.
    const handleDocKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onDismiss();
    };
    document.addEventListener('mousedown', handleDocPointer, true);
    document.addEventListener('keydown', handleDocKey, true);
    return () => {
      document.removeEventListener('mousedown', handleDocPointer, true);
      document.removeEventListener('keydown', handleDocKey, true);
    };
  });
</script>

<div
  bind:this={menuEl}
  class="fixed z-[80]"
  style:left="{adjustedX}px"
  style:top="{adjustedY}px"
>
  <Menu {ariaLabel} onClose={onDismiss} {minWidthClass}>
    {@render children()}
  </Menu>
</div>
