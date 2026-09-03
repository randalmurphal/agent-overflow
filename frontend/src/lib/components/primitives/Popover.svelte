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
  //
  // 4. `data-popover-clip-boundary` belongs ONLY on an element whose
  //    overflowing content is occluded by a SIBLING surface (the pane
  //    strip sliding under the sidebar) — it makes every popover
  //    anchored inside clip and auto-close at that edge, so declaring
  //    it on an ordinary container silently cuts popovers that are
  //    legitimately allowed to overflow it. A surface that sits inside
  //    such a subtree but escapes its scrolling (Modal's fixed panel)
  //    opts back out with `data-popover-clip-boundary="none"`.
  //    Division of labour on close: this component's `restoreFocusTo`
  //    only catches focus that would be STRANDED on the removed
  //    floating element (dialog focus-trap repair); the caller-side
  //    `restorePickerFocus` decides where focus should GO on a close,
  //    gated by the close reason.

  import type { Snippet } from 'svelte';
  import { airspaceSurface } from '../../utils/paneAirspace.svelte';
  import { isCompactLayout } from '../../stores/layoutMode.svelte';
  import {
    hasOpenPopoverOwnedBy,
    popoverAnchorChainReaches,
    resolvePopoverClipBoundary,
    type PopoverCloseReason,
    type PopoverFloatingEl,
  } from '../../utils/popoverOwnership';
  import {
    clampPopoverPosition,
    clipPathRule,
    intersectClipBoundary,
    oppositeOf,
    overflowsPrimaryAxis,
    placePopover,
    type EdgeRect,
    type PopoverPlacement as Placement,
  } from '../../utils/popoverGeometry';

  type PopoverRole = 'dialog' | 'menu' | 'listbox' | 'none';

  interface Props {
    anchor: HTMLElement | undefined;
    open: boolean;
    /**
     * Close request, with WHY it happened. Callers that restore focus on
     * close must skip the restore for 'outside-click' and 'anchor-gone'
     * (see PopoverCloseReason) — and must never let that restore scroll:
     * a bare `.focus()` on a trigger scrolled out of the pane strip
     * snaps the strip back to it, hijacking whatever navigation caused
     * the close. `restorePickerFocus` in panes/paneComposerFocus.ts is
     * the canonical implementation.
     */
    onClose: (reason: PopoverCloseReason) => void;
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
    /**
     * Under the compact layout the popover is a bottom sheet: full width,
     * pinned to the bottom edge, no anchor geometry. Menus and pickers
     * want that; a completion list that must sit ON the caret's textarea
     * (the composer's mention and slash popovers) opts out.
     */
    sheet?: boolean;
    /**
     * Treat a mousedown on the anchor as an outside click. The default
     * exemption exists for a TRIGGER anchor, whose own click handler
     * toggles the popover and would reopen what this closed. A menu
     * opened by right-click is anchored to the row it describes, and
     * that row's left-click does something else (expand, a header
     * button): the menu must get out of the way of it.
     */
    dismissOnAnchorClick?: boolean;
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
    sheet = true,
    dismissOnAnchorClick = false,
    children,
  }: Props = $props();

  let asSheet = $derived(sheet && isCompactLayout());

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

  // The anchor's clip boundary (see resolvePopoverClipBoundary), as the
  // viewport-intersected rect the floating element must stay inside. The
  // popover belongs to the boundary's content plane, so it is cut at the
  // plane's edge (clip-path below) instead of painting over whatever
  // surface sits beside it — a composer picker following its trigger
  // behind the sidebar slides under the sidebar, not in front of it.
  //
  // Two representations on purpose, and the split is a reactivity hazard,
  // not a style choice: `clipRectUntracked` is the plain mirror the
  // open-effect, `fitPosition`, and the rAF tracker read, so those
  // imperative reads never register the clip as a dependency of the
  // effect that WRITES it (which would re-run the effect off its own
  // write); `clipRect` is the $state the style derived consumes.
  // `setClipRect` is the one writer and keeps them identical
  // (field-compared, so per-frame refreshes of an unchanged boundary
  // write nothing and allocate nothing).
  let clipRect = $state<EdgeRect | null>(null);
  let clipRectUntracked: EdgeRect | null = null;
  function setClipRect(next: EdgeRect | null): void {
    const cur = clipRectUntracked;
    if (cur === next) return;
    if (
      cur !== null && next !== null
      && cur.top === next.top && cur.left === next.left
      && cur.right === next.right && cur.bottom === next.bottom
    ) return;
    clipRectUntracked = next;
    clipRect = next;
  }

  // Painted size of the floating element, captured at each fit — the clip
  // insets need the box, and reading offsetWidth/Height per frame would
  // force layout for a value that only changes on refit anyway.
  let floatingWidth = $state(0);
  let floatingHeight = $state(0);

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
    const vw = window.innerWidth;
    const vh = window.innerHeight;
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
    let pos = placePopover(rect, floatRect, chosen, offset);
    if (overflowsPrimaryAxis(pos, floatRect, chosen, vw, vh)) {
      const alt = oppositeOf(chosen);
      const altPos = placePopover(rect, floatRect, alt, offset);
      if (!overflowsPrimaryAxis(altPos, floatRect, alt, vw, vh)) {
        chosen = alt;
        pos = altPos;
      }
    }
    // The clamp bounds fold in the clip boundary (a frame stale at worst —
    // refit() refreshes it first), so an open lands the popover inside the
    // plane it will be clipped to instead of opening pre-cut.
    const clamped = clampPopoverPosition(pos, floatRect, vw, vh, clipRectUntracked);
    top = clamped.top;
    left = clamped.left;
    maxHeight = clamped.maxHeight;
    // Clip wants the box as PAINTED: content height capped by the
    // maxHeight just computed — offsetHeight still measures the
    // pre-cap box until that style lands, a frame too tall.
    floatingWidth = floatRect.width;
    floatingHeight = clamped.maxHeight !== undefined
      ? Math.min(floatRect.height, clamped.maxHeight)
      : floatRect.height;
    resolvedPlacement = chosen;
    fitOffset = { dx: clamped.left - rect.left, dy: clamped.top - rect.top };
    return rect;
  }

  // The anchor MOVED (pane scroll, drag auto-scroll, transform shift):
  // re-apply the fitted offset rigidly, no re-clamp. Re-clamping here is
  // what made popovers "ride the viewport edge" while their trigger
  // scrolled away — following rigidly, the popover moves with the pane
  // content and is cut at the clip boundary (or viewport edge) exactly
  // like its trigger is, until the trigger is fully gone and the tracker
  // closes it.
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
      setClipRect(null);
      return;
    }
    if (!anchor || !floatingEl) {
      // A re-run that lost its anchor mid-open must not leave the last
      // clip frozen on a floating element that no longer tracks anything.
      setClipRect(null);
      return;
    }

    const updateClipRect = (): void => {
      // Re-resolved per call rather than once per open, deliberately: a
      // nested popover's chain link (`__popoverAnchor` on the hosting
      // floating element) is stamped by the HOST's own effect, and a
      // popover opening in the same tick as its host runs this effect
      // BEFORE that stamp lands — a one-time resolution here saw an
      // unstamped chain and froze "no boundary" for the popover's whole
      // lifetime. The walk is a couple of closest() calls on a shallow
      // chain, noise next to the per-frame rect reads around it.
      const boundaryEl = anchor !== undefined ? resolvePopoverClipBoundary(anchor) : null;
      if (!boundaryEl) {
        setClipRect(null);
        return;
      }
      const b = boundaryEl.getBoundingClientRect();
      setClipRect(intersectClipBoundary(b, window.innerWidth, window.innerHeight));
    };

    const handleMouseDown = (e: MouseEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (floatingEl?.contains(target)) return;
      if (!dismissOnAnchorClick && anchor?.contains(target)) return;
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
      onClose('outside-click');
    };
    const handleKeydown = (e: KeyboardEvent) => {
      if (claimTab && e.key === 'Tab') {
        // Unlike Escape this is NOT gated on being the deepest popover.
        // Escape dismisses one layer; a Tab claim exists to keep focus
        // inside the surface hosting me, and collapsing the stack back
        // onto my trigger is the right answer at every depth.
        e.preventDefault();
        onClose('tab');
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
      onClose('escape');
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
      updateClipRect();
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
          onClose('anchor-gone');
          return;
        }
        // The boundary's rect only moves on layout changes (sidebar
        // resize, window resize), but those don't all reach the refit
        // listeners — refresh it on the same per-frame cadence as the
        // anchor. setClipRect no-ops when nothing changed.
        updateClipRect();
        const r = anchor.getBoundingClientRect();
        // An anchor that has SCROLLED fully out of view closes the
        // popover, same as one that left the DOM — followAnchor has carried
        // the floating element to the edge with it, and there is nothing
        // left on screen for it to belong to. "Out of view" is judged
        // against the clip boundary when the anchor has one (a composer
        // trigger fully behind the sidebar is gone, even though its rect
        // still intersects the window), else against the viewport.
        // Transition-gated on having been seen visible, because "never
        // visible" is not "scrolled away": a zero-rect environment
        // (happy-dom) or an anchor whose geometry hasn't materialized yet
        // must not self-close.
        const bounds = clipRectUntracked;
        const visible = bounds !== null
          ? r.right > bounds.left && r.bottom > bounds.top
            && r.left < bounds.right && r.top < bounds.bottom
          : r.right > 0 && r.bottom > 0
            && r.left < window.innerWidth && r.top < window.innerHeight;
        if (visible) {
          anchorWasVisible = true;
        } else if (anchorWasVisible) {
          onClose('anchor-gone');
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
    // preventScroll: the restore target can sit inside a scrolled-away
    // region (the pane strip); putting the caret back must not yank the
    // viewport to it.
    if (focusInside) restoreFocusTo?.focus({ preventScroll: true });
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
    if (asSheet) {
      return 'position: fixed; left: 0; right: 0; bottom: 0; max-height: 70vh; overflow-y: auto; padding-bottom: env(safe-area-inset-bottom);';
    }
    const widthRule = width !== undefined ? `width: ${width}px;` : '';
    const maxHeightRule = maxHeight !== undefined ? `max-height: ${maxHeight}px; overflow-y: auto;` : '';
    const visibility = resolvedPlacement === null ? 'visibility: hidden;' : '';
    // Cut the floating element at its clip boundary (see clipRect): the
    // strip content plane ends where the sidebar begins, and a popover
    // riding a scrolled anchor must be occluded there like its trigger
    // is, not painted over the neighbouring surface. Width is the wider
    // of the requested and measured box — a matchAnchorWidth menu can
    // overflow the request via its own min-width, and under-counting
    // the box under-clips its right edge.
    let clipRule = '';
    if (clipRect !== null && resolvedPlacement !== null) {
      clipRule = clipPathRule(
        clipRect, top, left, Math.max(width ?? 0, floatingWidth), floatingHeight,
      );
    }
    return `position: fixed; top: ${top}px; left: ${left}px; ${widthRule} ${maxHeightRule} ${clipRule} ${visibility}`.trim();
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
    data-popover-sheet={asSheet ? '' : undefined}
    data-placement={asSheet ? 'sheet' : (resolvedPlacement ?? placement)}
    class="z-[80]"
    use:airspaceSurface
  >
    {@render children()}
  </div>
{/if}
