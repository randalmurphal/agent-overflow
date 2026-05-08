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
  //
  // CONSTRAINTS / CALLER CONTRACTS (load-bearing — violate and things
  // break in subtle ways):
  //
  // 1. Anchor DOM identity is stable across opens. When a nested
  //    popover's anchor lives inside an ancestor popover's floating
  //    element, the `__popoverAnchor` property we stamp is what
  //    ancestor popovers use to detect descendant-click and
  //    descendant-Escape. `other.__popoverAnchor` pointing at a
  //    stale DOM node silently breaks the chain. Every current
  //    consumer binds the anchor once and reuses the reference;
  //    new callers must do the same.
  //
  // 2. Popovers are NOT rendered inside Modals today. Modal runs its
  //    own focus trap and Escape handler on the backdrop; a Popover
  //    inside would duplicate the Escape path and bypass the focus
  //    trap (the portal to document.body lifts children out of the
  //    Modal's panel subtree). If a future flow needs a picker-in-
  //    dialog, coordinate: either route that picker through Modal as
  //    a sub-dialog, or gate Popover's document Escape with a modal-
  //    stack check. Do not simply rely on `stopPropagation`.
  //
  // 3. Callers do NOT re-parent the floating element themselves.
  //    Popover moves it to <body> on mount and removes it on
  //    teardown; a third party moving it would desync the cleanup
  //    path and leak nodes between tests.

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

  // Popover-on-floating-element property bag. Holding the anchor
  // reference here lets other popovers reconstruct the "is this a
  // descendant popover?" check at mousedown time: after portaling,
  // DOM ancestry can't answer that question because every popover is
  // a sibling under <body>, but every popover's anchor is still a DOM
  // descendant of whichever popover opened it — so we chase the anchor.
  type PopoverFloatingEl = HTMLDivElement & { __popoverAnchor?: HTMLElement };

  // Portal the floating element into `document.body` on mount. Without
  // this, a `position: fixed` popover rendered inside an ancestor that
  // creates a new containing block (anything with `backdrop-filter`,
  // `transform`, `filter`, `perspective`, `contain: paint`, etc.) would
  // position relative to that ancestor's padding box instead of the
  // viewport — and any `overflow: hidden` on the chain would then clip
  // it out of sight. The composer card's `overflow-hidden` clip is the
  // closest in-tree concern; portaling preempts both that and any
  // other containing-block ancestor in the chain.
  //
  // Cleanup explicitly `.remove()`s the node so it goes away with the
  // component even if the caller's tree gets torn down externally
  // (testing-library cleanup, manual unmount) — Svelte otherwise only
  // knows to remove the node from its ORIGINAL parent, which no longer
  // holds it after we portaled.
  $effect(() => {
    if (!floatingEl) return;
    const node = floatingEl;
    document.body.appendChild(node);
    return () => {
      node.remove();
    };
  });

  // Keep the anchor reference attached to the floating element so
  // ancestor popovers can walk their way back through the DOM at
  // click time. Without this, clicking a menu item inside a nested
  // submenu would close the parent popover first (both are body
  // children after portaling, so the parent sees the click as
  // "outside"), then the click event would never fire on the now-
  // detached row — visible symptom: "menu opens but selections do
  // nothing".
  $effect(() => {
    if (!floatingEl) return;
    (floatingEl as PopoverFloatingEl).__popoverAnchor = anchor;
  });

  // Walk the anchor chain upward from a clicked popover to determine
  // whether it is "my descendant" — i.e. I spawned it directly, or I
  // spawned an intermediate popover that spawned it, etc. Used by
  // both outside-mousedown (don't close me when a child swallows the
  // click) and Escape (let the deepest popover handle the press).
  //
  // Max depth guard is defensive against a malformed cycle; real
  // popover trees are rarely more than 2–3 deep.
  function isPopoverDescendantOfMe(
    other: PopoverFloatingEl,
  ): boolean {
    if (!floatingEl) return false;
    if (other === floatingEl) return false;
    let cur: PopoverFloatingEl | null = other;
    for (let i = 0; i < 16 && cur; i++) {
      const curAnchor = cur.__popoverAnchor;
      if (!curAnchor) return false;
      if (floatingEl.contains(curAnchor)) return true;
      const next = curAnchor.closest('[data-popover]') as PopoverFloatingEl | null;
      if (!next || next === cur) return false;
      cur = next;
    }
    return false;
  }

  function isDescendantPopoverClick(target: Element): boolean {
    const other = target.closest('[data-popover]') as PopoverFloatingEl | null;
    if (!other) return false;
    return isPopoverDescendantOfMe(other);
  }

  function hasOpenDescendantPopover(): boolean {
    if (!floatingEl) return false;
    const open = document.querySelectorAll<PopoverFloatingEl>('[data-popover]');
    for (const p of open) {
      if (p === floatingEl) continue;
      if (isPopoverDescendantOfMe(p)) return true;
    }
    return false;
  }

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
      // The click landed outside both my floating element and my
      // anchor — but it might be inside a DESCENDANT popover whose
      // anchor (or whose anchor's anchor, etc.) lives inside my
      // floating element. Portaling turned every popover into a
      // sibling under <body>, so DOM-contains can't see the parent/
      // child relationship anymore. Walk the anchor chain to
      // reconstruct it: each popover we visit has an `__popoverAnchor`
      // pointer; if the anchor lives inside me, the clicked popover
      // is my descendant (maybe transitively).
      if (target instanceof Element && isDescendantPopoverClick(target)) return;
      onClose();
    };
    const handleKeydown = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      // Only the topmost open popover responds to Escape. When nested
      // popovers are open (root menu → Codex submenu), both register
      // document-level keydown listeners; `stopPropagation` does not
      // stop sibling listeners on the same target. Without this check
      // one Escape press would collapse the whole stack. Ask the DOM:
      // does any open popover have me as an ancestor? If yes, it's
      // deeper than me — let it handle this press; I'll handle the
      // next one after it closes.
      if (hasOpenDescendantPopover()) return;
      e.stopPropagation();
      onClose();
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
