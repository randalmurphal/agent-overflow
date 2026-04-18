<script lang="ts">
  // Anchored floating surface. Computes a position relative to an anchor
  // element, flips away from the viewport edge when the preferred
  // placement would overflow, and closes via caller-owned `onClose` on
  // Escape or an outside mousedown.
  //
  // This primitive does NOT trap focus — Menu popovers explicitly
  // shouldn't trap, dialogs use Modal. Callers compose focus management
  // themselves.
  //
  // The floating element mounts only when `open === true` (via {#if open})
  // so closed popovers contribute zero DOM nodes and zero listeners.

  import type { Snippet } from 'svelte';

  type Placement =
    | 'bottom-start'
    | 'bottom-end'
    | 'top-start'
    | 'top-end'
    | 'right-start'
    | 'left-start';

  type PopoverRole = 'dialog' | 'menu' | 'listbox' | 'none';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    onClose: () => void;
    placement?: Placement;
    offset?: number;
    matchAnchorWidth?: boolean;
    role?: PopoverRole;
    ariaLabel?: string;
    children: Snippet;
  }

  let {
    anchor,
    open,
    onClose,
    placement = 'bottom-start',
    offset = 4,
    matchAnchorWidth = false,
    role = 'none',
    ariaLabel,
    children,
  }: Props = $props();

  let floatingEl: HTMLDivElement | undefined = $state(undefined);
  let top = $state(0);
  let left = $state(0);
  let width: number | undefined = $state(undefined);
  // Once we've measured the floating element we set resolvedPlacement to
  // the placement we actually used after flip logic. The initial `null`
  // means "not yet positioned" — the floating div is kept invisible so
  // there's no first-frame jump at the viewport's origin.
  let resolvedPlacement = $state<Placement | null>(null);

  // Core placement math. Given the anchor rect and the floating rect,
  // return {top,left} for each placement. Written as a pure function so
  // flip can retry with a different placement without mutating state.
  function placePopover(
    rect: DOMRect,
    floatRect: { width: number; height: number },
    p: Placement,
  ): { top: number; left: number } {
    switch (p) {
      case 'bottom-start':
        return { top: rect.bottom + offset, left: rect.left };
      case 'bottom-end':
        return { top: rect.bottom + offset, left: rect.right - floatRect.width };
      case 'top-start':
        return { top: rect.top - floatRect.height - offset, left: rect.left };
      case 'top-end':
        return { top: rect.top - floatRect.height - offset, left: rect.right - floatRect.width };
      case 'right-start':
        return { top: rect.top, left: rect.right + offset };
      case 'left-start':
        return { top: rect.top, left: rect.left - floatRect.width - offset };
    }
  }

  // Flip rules: if the preferred placement overflows the viewport, try
  // its natural opposite. We keep this intentionally simple — two
  // candidates per axis. If both overflow we stick with preferred rather
  // than cascading through every direction (keeps behaviour predictable
  // in tiny viewports).
  function oppositeOf(p: Placement): Placement {
    switch (p) {
      case 'bottom-start': return 'top-start';
      case 'bottom-end':   return 'top-end';
      case 'top-start':    return 'bottom-start';
      case 'top-end':      return 'bottom-end';
      case 'right-start':  return 'left-start';
      case 'left-start':   return 'right-start';
    }
  }

  function overflowsViewport(
    pos: { top: number; left: number },
    floatRect: { width: number; height: number },
  ): boolean {
    if (typeof window === 'undefined') return false;
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    return (
      pos.top < 0 ||
      pos.left < 0 ||
      pos.top + floatRect.height > vh ||
      pos.left + floatRect.width > vw
    );
  }

  function updatePosition(): void {
    if (!anchor || !floatingEl) return;
    const rect = anchor.getBoundingClientRect();
    const floatRect = {
      width: floatingEl.offsetWidth,
      height: floatingEl.offsetHeight,
    };
    if (matchAnchorWidth) {
      width = rect.width;
    } else {
      width = undefined;
    }

    let chosen: Placement = placement;
    let pos = placePopover(rect, floatRect, chosen);
    if (overflowsViewport(pos, floatRect)) {
      const alt = oppositeOf(chosen);
      const altPos = placePopover(rect, floatRect, alt);
      if (!overflowsViewport(altPos, floatRect)) {
        chosen = alt;
        pos = altPos;
      }
    }
    top = pos.top;
    left = pos.left;
    resolvedPlacement = chosen;
  }

  // Attach positioning + lifecycle listeners when open. Everything
  // unwinds cleanly when `open` flips to false (the {#if} below
  // unmounts the floating div and this effect's cleanup runs).
  $effect(() => {
    if (!open) {
      resolvedPlacement = null;
      return;
    }
    if (!anchor || !floatingEl) return;

    updatePosition();

    const handleMouseDown = (e: MouseEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (floatingEl?.contains(target)) return;
      if (anchor?.contains(target)) return;
      onClose();
    };
    const handleKeydown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
      }
    };
    const handleScroll = () => updatePosition();
    const handleResize = () => updatePosition();

    document.addEventListener('mousedown', handleMouseDown);
    document.addEventListener('keydown', handleKeydown);
    window.addEventListener('scroll', handleScroll, { passive: true, capture: true });
    window.addEventListener('resize', handleResize, { passive: true });

    const anchorObserver = new ResizeObserver(() => updatePosition());
    anchorObserver.observe(anchor);

    // Observe the floating element too: its content may grow (e.g. async
    // menu items load) and shift the correct flip decision.
    const floatObserver = new ResizeObserver(() => updatePosition());
    floatObserver.observe(floatingEl);

    return () => {
      document.removeEventListener('mousedown', handleMouseDown);
      document.removeEventListener('keydown', handleKeydown);
      window.removeEventListener('scroll', handleScroll, true);
      window.removeEventListener('resize', handleResize);
      anchorObserver.disconnect();
      floatObserver.disconnect();
    };
  });

  // If the `anchor` prop changes while open (e.g. parent re-creates the
  // trigger), re-run the initial measurement. The outer $effect's
  // ResizeObserver is bound to the old anchor, so it would miss this.
  $effect(() => {
    if (open && anchor && floatingEl) {
      updatePosition();
    }
  });

  // Derived style string so the template stays declarative. Until we've
  // measured we paint the div with `visibility: hidden` so there's no
  // flash at (0,0) before the first layout frame.
  let floatingStyle = $derived.by(() => {
    const widthRule = width !== undefined ? `width: ${width}px;` : '';
    const visibility = resolvedPlacement === null ? 'visibility: hidden;' : '';
    return `position: fixed; top: ${top}px; left: ${left}px; ${widthRule} ${visibility}`.trim();
  });

  // `role="none"` tells AT consumers to ignore the wrapper; callers who
  // want AT semantics (Menu, listbox, dialog) opt in via the prop.
  let appliedRole = $derived(role === 'none' ? undefined : role);
</script>

{#if open}
  <div
    bind:this={floatingEl}
    role={appliedRole}
    aria-label={ariaLabel}
    style={floatingStyle}
    data-popover
    data-placement={resolvedPlacement ?? placement}
    class="z-50"
  >
    {@render children()}
  </div>
{/if}
