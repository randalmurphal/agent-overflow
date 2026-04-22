<script lang="ts">
  /**
   * Right-click menu for a rendered mermaid diagram. Anchored at the
   * cursor coordinates (clamped to stay on-screen). Actions are
   * issued back to the caller — this component owns positioning,
   * click-outside, and keyboard navigation only.
   */

  import { onMount } from 'svelte';
  import Menu from '../primitives/Menu.svelte';
  import MenuItem from '../primitives/MenuItem.svelte';
  import MenuDivider from '../primitives/MenuDivider.svelte';

  export type DiagramAction = 'copy-png' | 'copy-svg' | 'copy-source' | 'expand' | 'close';

  interface Props {
    x: number;
    y: number;
    context: 'inline' | 'modal';
    onAction: (action: DiagramAction) => void;
    onDismiss: () => void;
  }

  let { x, y, context, onAction, onDismiss }: Props = $props();

  // Menu dimensions are only known after mount; we render at the
  // click coords first, then adjust in an effect so the menu never
  // clips off-screen. The invisible first paint lasts one frame.
  let menuEl: HTMLDivElement | undefined = $state(undefined);
  // Default to 0/0; the $effect below copies the current props into
  // these reactive scalars on first tick. Initialising from `x`/`y`
  // directly would only capture the prop's value at component
  // creation (Svelte's state_referenced_locally warning).
  let adjustedX = $state(0);
  let adjustedY = $state(0);

  $effect(() => {
    adjustedX = x;
    adjustedY = y;
  });

  function adjustPosition(): void {
    if (!menuEl) return;
    const rect = menuEl.getBoundingClientRect();
    const viewportW = window.innerWidth;
    const viewportH = window.innerHeight;
    const margin = 4;
    let nx = x;
    let ny = y;
    if (nx + rect.width + margin > viewportW) {
      nx = Math.max(margin, viewportW - rect.width - margin);
    }
    if (ny + rect.height + margin > viewportH) {
      ny = Math.max(margin, viewportH - rect.height - margin);
    }
    adjustedX = nx;
    adjustedY = ny;
  }

  onMount(() => {
    queueMicrotask(adjustPosition);
    // Dismiss on any click outside the menu (capture phase so this
    // runs before any child click handler). Right-clicks dismiss and
    // re-open at the new spot — handled upstream by the host.
    const handleDocClick = (e: MouseEvent): void => {
      if (menuEl && e.target instanceof Node && !menuEl.contains(e.target)) {
        onDismiss();
      }
    };
    document.addEventListener('mousedown', handleDocClick, true);
    return () => {
      document.removeEventListener('mousedown', handleDocClick, true);
    };
  });

  function pick(action: DiagramAction): void {
    onAction(action);
  }
</script>

<div
  bind:this={menuEl}
  class="fixed z-[80]"
  style:left="{adjustedX}px"
  style:top="{adjustedY}px"
>
  <Menu ariaLabel="Diagram actions" onClose={onDismiss}>
    <MenuItem label="Copy as PNG" onSelect={() => pick('copy-png')} />
    <MenuItem label="Copy as SVG" onSelect={() => pick('copy-svg')} />
    <MenuItem label="Copy source" onSelect={() => pick('copy-source')} />
    <MenuDivider />
    {#if context === 'inline'}
      <MenuItem label="Expand" onSelect={() => pick('expand')} />
    {:else}
      <MenuItem label="Close" onSelect={() => pick('close')} />
    {/if}
  </Menu>
</div>
