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
  // 2. A Popover inside a Modal costs two things, and both are settled
  //    in the primitives — `stopPropagation` pays neither.
  //
  //    ESCAPE is automatic: Modal declines the press while a popover it
  //    OWNS is open (the anchor chain reaches its panel — see
  //    `utils/popoverOwnership.ts`), so the topmost surface handles it.
  //    That is Modal's guard, not an ordering trick — Modal's handler
  //    sits on the backdrop and runs BEFORE this component's
  //    document-level listener, so nothing here could pre-empt it.
  //
  //    FOCUS is opt-in via props, because it only applies to a popover
  //    hosted inside a focus trap. Portaling lifts the floating element
  //    out of the Modal panel, so it is outside the dialog's trap: Tab
  //    from inside the popover walks into the page behind, and a close
  //    that leaves focus on a removed node drops it to <body>. Pass
  //    `claimTab` and `restoreFocusTo={trigger}` and both are handled
  //    here; `import/SessionImportProjectMenu.svelte` is the worked
  //    example.
  //
  // 3. Callers do NOT re-parent the floating element themselves.
  //    Popover moves it to <body> on mount and removes it on
  //    teardown; a third party moving it would desync the cleanup
  //    path and leak nodes between tests.

  import type { Snippet } from 'svelte';
  import {
    hasOpenPopoverOwnedBy,
    popoverAnchorChainReaches,
    type PopoverFloatingEl,
  } from '../../utils/popoverOwnership';

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
    /**
     * Take Tab while open: suppress the move and close instead. For a
     * popover hosted inside a focus trap this is the one exit that would
     * otherwise strand focus outside the trap (constraint #2 above).
     */
    claimTab?: boolean;
    /**
     * Where focus goes when a close would leave it on the removed floating
     * element — normally the trigger. Only applies when focus was still
     * inside the popover: a close caused by an outside click has already
     * put focus where the user asked for it.
     */
    restoreFocusTo?: HTMLElement;
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
    claimTab = false,
    restoreFocusTo,
    children,
  }: Props = $props();

  let floatingEl: HTMLDivElement | undefined = $state(undefined);
  let top = $state(0);
  let left = $state(0);
  let width: number | undefined = $state(undefined);
  let maxHeight: number | undefined = $state(undefined);
  // Once we've measured the floating element we set resolvedPlacement to
  // the placement we actually used after flip logic. The initial `null`
  // means "not yet positioned" — the floating div is kept invisible so
  // there's no first-frame jump at the viewport's origin.
  let resolvedPlacement = $state<Placement | null>(null);

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
  // ancestor popovers (and a hosting Modal) can walk their way back
  // through the DOM at click time — after portaling, DOM ancestry
  // can't answer "did I spawn this?" because every popover is a
  // sibling under <body>, but every popover's anchor is still a DOM
  // descendant of whatever opened it, so the walk chases anchors.
  // Without this, clicking a menu item inside a nested
  // submenu would close the parent popover first (both are body
  // children after portaling, so the parent sees the click as
  // "outside"), then the click event would never fire on the now-
  // detached row — visible symptom: "menu opens but selections do
  // nothing".
  $effect(() => {
    if (!floatingEl) return;
    (floatingEl as PopoverFloatingEl).__popoverAnchor = anchor;
  });

  // "Is that popover my descendant?" — I spawned it directly, or I
  // spawned an intermediate popover that spawned it, etc. Used by both
  // outside-mousedown (don't close me when a child swallows the click)
  // and Escape (let the deepest popover handle the press). The walk
  // itself is shared with Modal, which asks the same question about the
  // pickers mounted inside its panel.
  function isDescendantPopoverClick(target: Element): boolean {
    const other = target.closest('[data-popover]') as PopoverFloatingEl | null;
    if (!other || !floatingEl || other === floatingEl) return false;
    return popoverAnchorChainReaches(other, floatingEl);
  }

  function hasOpenDescendantPopover(): boolean {
    if (!floatingEl) return false;
    return hasOpenPopoverOwnedBy(floatingEl);
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

  function overflowsPrimaryAxis(
    pos: { top: number; left: number },
    floatRect: { width: number; height: number },
    p: Placement,
  ): boolean {
    if (typeof window === 'undefined') return false;
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    switch (p) {
      case 'bottom-start':
      case 'bottom-end':
        return pos.top + floatRect.height > vh;
      case 'top-start':
      case 'top-end':
        return pos.top < 0;
      case 'right-start':
        return pos.left + floatRect.width > vw;
      case 'left-start':
        return pos.left < 0;
    }
  }

  const VIEWPORT_MARGIN = 8;

  function clamp(value: number, min: number, max: number): number {
    if (max < min) return min;
    return Math.min(Math.max(value, min), max);
  }

  function clampToViewport(
    pos: { top: number; left: number },
    floatRect: { width: number; height: number },
  ): { top: number; left: number; maxHeight: number | undefined } {
    if (typeof window === 'undefined') {
      return { ...pos, maxHeight: undefined };
    }

    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const minLeft = VIEWPORT_MARGIN;
    const minTop = VIEWPORT_MARGIN;
    const maxLeft = vw - floatRect.width - VIEWPORT_MARGIN;
    const maxTop = vh - floatRect.height - VIEWPORT_MARGIN;
    const clampedTop = clamp(pos.top, minTop, maxTop);
    const clampedLeft = clamp(pos.left, minLeft, maxLeft);
    const availableHeight = Math.max(0, vh - (VIEWPORT_MARGIN * 2));
    const needsHeightLimit = floatRect.height > availableHeight || pos.top !== clampedTop;

    return {
      top: clampedTop,
      left: clampedLeft,
      maxHeight: needsHeightLimit ? availableHeight : undefined,
    };
  }

  // Where the floating element sits RELATIVE to the anchor, captured each
  // time a fit runs. Anchor movement re-applies it verbatim (followAnchor),
  // so the popover stays glued to its trigger between fits.
  let fitOffset = { dx: 0, dy: 0 };

  // Full placement pass: preferred placement, flip, viewport clamp,
  // max-height. Runs at open and when GEOMETRY changes — viewport resize,
  // anchor resize, floating-content growth — never on mere anchor
  // movement. Returns the anchor rect it measured so callers can seed
  // movement tracking from the same read.
  function fitPosition(): DOMRect | undefined {
    if (!anchor || !floatingEl) return undefined;
    const rect = anchor.getBoundingClientRect();
    const floatRect = {
      width: floatingEl.offsetWidth,
      height: Math.max(floatingEl.offsetHeight, floatingEl.scrollHeight),
    };
    if (matchAnchorWidth) {
      width = rect.width;
    } else {
      width = undefined;
    }

    let chosen: Placement = placement;
    let pos = placePopover(rect, floatRect, chosen);
    if (overflowsPrimaryAxis(pos, floatRect, chosen)) {
      const alt = oppositeOf(chosen);
      const altPos = placePopover(rect, floatRect, alt);
      if (!overflowsPrimaryAxis(altPos, floatRect, alt)) {
        chosen = alt;
        pos = altPos;
      }
    }
    const clamped = clampToViewport(pos, floatRect);
    top = clamped.top;
    left = clamped.left;
    maxHeight = clamped.maxHeight;
    resolvedPlacement = chosen;
    fitOffset = { dx: clamped.left - rect.left, dy: clamped.top - rect.top };
    return rect;
  }

  // The anchor MOVED (pane scroll, drag auto-scroll, transform shift):
  // re-apply the fitted offset rigidly, no re-clamp. Re-clamping here is
  // what made popovers "ride the viewport edge" while their trigger
  // scrolled away — following rigidly, the popover moves with the pane
  // content and clips at the viewport edge exactly like its trigger does,
  // until the trigger is fully gone and the tracker closes it.
  function followAnchor(rect: DOMRect): void {
    top = rect.top + fitOffset.dy;
    left = rect.left + fitOffset.dx;
  }

  // Whether focus was last seen inside my floating element. Lives out here
  // rather than inside the effect that maintains it, because the restore
  // below has to read it AFTER that effect has torn down.
  let focusInside = false;

  // Attach positioning + lifecycle listeners when open. Everything
  // unwinds cleanly when `open` flips to false (the {#if} below
  // unmounts the floating div and this effect's cleanup runs).
  $effect(() => {
    if (!open) {
      resolvedPlacement = null;
      maxHeight = undefined;
      return;
    }
    if (!anchor || !floatingEl) return;

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
      if (claimTab && e.key === 'Tab') {
        // Unlike Escape this is NOT gated on being the deepest popover.
        // Escape dismisses one layer; a Tab claim exists to keep focus
        // inside the surface hosting me, and collapsing the stack back
        // onto my trigger is the right answer at every depth.
        e.preventDefault();
        onClose();
        return;
      }
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
    // Scroll moves the anchor without resizing anything → follow. Resize
    // changes the clamp bounds themselves → refit. (The per-frame tracker
    // below would catch both a frame later; these give same-frame response.)
    const handleScroll = () => {
      if (anchor) followAnchor(anchor.getBoundingClientRect());
    };
    const handleResize = () => refit();

    // Feed `focusInside` (see its declaration): by the time the close is
    // observable the floating element is gone and focus has fallen back to
    // <body>, so whether it was ever mine is only answerable from what we
    // watched while open. Menu taking focus on mount is the first such event.
    const floating = floatingEl;
    focusInside = floating.contains(document.activeElement);
    const handleFocusIn = (e: FocusEvent) => {
      focusInside = e.target instanceof Node && floating.contains(e.target);
    };

    // Per-frame anchor tracking. Scroll/resize listeners miss anchor
    // movement that fires no event at the moved element — programmatic
    // scrollLeft writes (PaneHost drag auto-scroll), transform-driven
    // layout shifts — leaving the popover stranded at a stale viewport
    // position. While open, re-measure the anchor each frame (one
    // getBoundingClientRect per frame; popovers are open briefly and one
    // at a time, so this is cheap) and split what changed:
    //   - size changed → refit (a resize shifts where the popover FITS);
    //   - position changed → followAnchor (rigid, see its comment);
    //   - left the DOM, or scrolled fully out of the viewport → close.
    let lastAnchorPos = '';
    let lastAnchorSize = '';
    let anchorWasVisible = false;
    const refit = () => {
      const r = fitPosition();
      if (r) {
        lastAnchorPos = `${r.top}:${r.left}`;
        lastAnchorSize = `${r.width}:${r.height}`;
      }
    };
    let rafId = 0;
    const trackAnchor = () => {
      if (anchor && floatingEl) {
        if (!anchor.isConnected) {
          onClose();
          return;
        }
        const r = anchor.getBoundingClientRect();
        // An anchor that has SCROLLED fully out of the viewport closes the
        // popover, same as one that left the DOM — followAnchor has carried
        // the floating element to the edge with it, and there is nothing
        // left on screen for it to belong to. Transition-gated on having
        // been seen visible, because "never visible" is not "scrolled
        // away": a zero-rect environment (happy-dom) or an anchor whose
        // geometry hasn't materialized yet must not self-close.
        const visible =
          r.right > 0 && r.bottom > 0 && r.left < window.innerWidth && r.top < window.innerHeight;
        if (visible) {
          anchorWasVisible = true;
        } else if (anchorWasVisible) {
          onClose();
          return;
        }
        const sizeKey = `${r.width}:${r.height}`;
        const posKey = `${r.top}:${r.left}`;
        if (sizeKey !== lastAnchorSize) {
          refit();
        } else if (posKey !== lastAnchorPos) {
          lastAnchorPos = posKey;
          followAnchor(r);
        }
      }
      rafId = requestAnimationFrame(trackAnchor);
    };
    refit();
    rafId = requestAnimationFrame(trackAnchor);

    document.addEventListener('mousedown', handleMouseDown);
    document.addEventListener('keydown', handleKeydown);
    document.addEventListener('focusin', handleFocusIn);
    window.addEventListener('scroll', handleScroll, { passive: true, capture: true });
    window.addEventListener('resize', handleResize, { passive: true });

    const anchorObserver = new ResizeObserver(() => refit());
    anchorObserver.observe(anchor);

    // Observe the floating element too: its content may grow (e.g. async
    // menu items load) and shift the correct flip decision.
    const floatObserver = new ResizeObserver(() => refit());
    floatObserver.observe(floatingEl);

    return () => {
      cancelAnimationFrame(rafId);
      document.removeEventListener('mousedown', handleMouseDown);
      document.removeEventListener('keydown', handleKeydown);
      document.removeEventListener('focusin', handleFocusIn);
      window.removeEventListener('scroll', handleScroll, true);
      window.removeEventListener('resize', handleResize);
      anchorObserver.disconnect();
      floatObserver.disconnect();
    };
  });

  // Put focus back where the caller asked when a close would otherwise
  // strand it. A separate effect rather than the teardown above: Svelte
  // serves signal reads inside a teardown from the values that effect last
  // RAN with, so `open` reads true there and a close is indistinguishable
  // from a re-subscribe (a placement or anchor change). Keying an effect on
  // `open` asks the question at the one moment it has an answer — and it
  // deliberately does not fire on unmount, where the host is tearing down
  // and owns focus itself.
  $effect(() => {
    if (open) return;
    if (focusInside) restoreFocusTo?.focus();
    focusInside = false;
  });

  // If the `anchor` prop changes while open (e.g. parent re-creates the
  // trigger), re-run the initial measurement. The outer $effect's
  // ResizeObserver is bound to the old anchor, so it would miss this.
  $effect(() => {
    if (open && anchor && floatingEl) {
      fitPosition();
    }
  });

  // Derived style string so the template stays declarative. Until we've
  // measured we paint the div with `visibility: hidden` so there's no
  // flash at (0,0) before the first layout frame.
  let floatingStyle = $derived.by(() => {
    const widthRule = width !== undefined ? `width: ${width}px;` : '';
    const maxHeightRule = maxHeight !== undefined ? `max-height: ${maxHeight}px; overflow-y: auto;` : '';
    const visibility = resolvedPlacement === null ? 'visibility: hidden;' : '';
    return `position: fixed; top: ${top}px; left: ${left}px; ${widthRule} ${maxHeightRule} ${visibility}`.trim();
  });

  // `role="none"` tells AT consumers to ignore the wrapper. Opt in ONLY
  // when the popover's children carry no role element of their own (the
  // composer listbox popovers render bare `role="option"` rows, so the
  // wrapper is the listbox). A caller composing the Menu primitive keeps
  // "none": Menu renders its own `role="menu"`, and a wrapper role there
  // nests menu-inside-menu — an AT tree where the outer menu's only child
  // is another menu.
  let appliedRole = $derived(role === 'none' ? undefined : role);
</script>

{#if open}
  <!-- z-[80]: the transient-surface layer ContextMenu already sits on, above
       Modal's z-[60] backdrop. A popover is opened by the topmost surface the
       user is interacting with, so it must paint above every persistent layer
       — at z-50 a picker inside a modal rendered BEHIND the backdrop blur. -->
  <div
    bind:this={floatingEl}
    role={appliedRole}
    aria-label={ariaLabel}
    style={floatingStyle}
    data-popover
    data-placement={resolvedPlacement ?? placement}
    class="z-[80]"
  >
    {@render children()}
  </div>
{/if}
