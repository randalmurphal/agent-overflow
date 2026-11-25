<script lang="ts">
  import SteppedSpinner from '../primitives/SteppedSpinner.svelte';
  import PaneCloseButton from './PaneCloseButton.svelte';
  import PanePromoteButton from './PanePromoteButton.svelte';
  import { untrack } from 'svelte';
  import ChatView from '../chat/ChatView.svelte';
  import CompanionPane from './CompanionPane.svelte';
  import ToolPane from './ToolPane.svelte';
  import TicketPane from './TicketPane.svelte';
  import RenderBoundary from '../shared/RenderBoundary.svelte';
  import {
    focusPane,
    getFocusedPaneId,
    getPane,
  } from '../../stores/panes.svelte';
  import { getMinPaneWidth } from '../../stores/paneDensity.svelte';
  import {
    demotePane,
    getPaneLayoutItems,
    getPromotedPaneId,
    isCompanionKind,
    isSurfaceKind,
    isToolKind,
    paneItemMinWidthPx,
    type PaneLayoutItem,
  } from '../../stores/paneLayout.svelte';
  import { setPaneHostWidth } from '../../stores/layoutMetrics.svelte';
  import { REVEAL_PANE_EVENT } from '../../stores/eventNames';
  import { compactShownPaneId, getCompactScreen, isCompactLayout, noteCompactPaneShown } from '../../stores/layoutMode.svelte';
  import PaneDivider from './PaneDivider.svelte';
  import { measurePane } from './measurePane';
  import { airspaceSurface } from '../../utils/paneAirspace.svelte';
  import { createPaneThreadDrag, PANE_DROP_SEAM_PX } from './usePaneThreadDrag.svelte';
  import { createPanePromotion } from './panePromotion.svelte';
  import { clampStripScrollLeft, createPaneStripReveal } from './paneStripReveal';

  const PANE_SELECTOR = '[data-pane-id]';
  const SLOT_SELECTOR = '[data-pane-slot]';

  let layoutItems = $derived(getPaneLayoutItems());
  let minPaneWidth = $derived(getMinPaneWidth());
  // Compact: every pane is exactly one screen wide and the strip has no
  // dividers, so the existing reveal glide IS the screen switch between
  // a thread and its companions. Nothing is unmounted.
  let compact = $derived(isCompactLayout());
  let threadInert = $derived(compact && getCompactScreen() !== 'thread');
  let focusedPaneId = $derived(getFocusedPaneId());
  // Source panes that currently have a paired take-control terminal pane. Both
  // halves of the pair carry the shared top-border indicator so the user reads
  // them as one bound entity across two panes.
  let pairedSourceIds = $derived(
    new Set(
      layoutItems
        .filter((item) => item.kind === 'take-control' && item.sourcePaneId)
        .map((item) => item.sourcePaneId as string),
    ),
  );
  let stripEl: HTMLDivElement | undefined = $state(undefined);
  let hostEl: HTMLDivElement | undefined = $state(undefined);
  // The sections' offsets and the strip width they were measured at, one
  // snapshot: every offset in it holds for `width`, so a reader at another
  // width (a scroll delivered between a resize and the host's observer)
  // can tell it is stale. Not reactive: consumed imperatively by the
  // compact measurement, and offset churn during resize drags would only
  // feed GC.
  interface StripGeometry {
    width: number;
    offsets: Map<string, number>;
  }
  let stripGeometry: StripGeometry = { width: -1, offsets: new Map() };

  function paneElementById(paneId: string): HTMLElement | null {
    for (const paneEl of hostEl?.querySelectorAll<HTMLElement>(PANE_SELECTOR) ?? []) {
      if (paneEl.dataset.paneId === paneId) return paneEl;
    }
    return null;
  }

  function paneSlotPlaceholder(paneId: string): HTMLElement | null {
    for (const slotEl of hostEl?.querySelectorAll<HTMLElement>(SLOT_SELECTOR) ?? []) {
      if (slotEl.dataset.paneSlot === paneId) return slotEl;
    }
    return null;
  }

  // Where a pane sits in the strip: its section, or while it is promoted
  // the placeholder holding its slot. Every offset and reveal reads this.
  function paneSlotById(paneId: string): HTMLElement | null {
    return paneSlotPlaceholder(paneId) ?? paneElementById(paneId);
  }

  const drag = createPaneThreadDrag({
    getStripEl: () => stripEl,
    getHostEl: () => hostEl,
    getLayoutItems: () => layoutItems,
  });

  // A reveal asked for while a pane is promoted waits for the demote it
  // causes: the strip does not move while a pane is laid over it.
  let revealAfterDemote: string | null = null;
  const reveal = createPaneStripReveal({
    getHostEl: () => hostEl,
    slotOf: paneSlotById,
    held: revealHeldByPromotion,
  });
  const promotion = createPanePromotion({
    getHostEl: () => hostEl,
    sectionOf: paneElementById,
    slotOf: paneSlotPlaceholder,
    flowScrollMax,
    onCapture: () => reveal.cancel(),
    onRelease: () => {
      publishPaneOffsets();
      const paneId = revealAfterDemote;
      revealAfterDemote = null;
      if (paneId) reveal.request(paneId);
    },
  });
  let promotedId = $derived(getPromotedPaneId());
  let shownPromotedId = $derived(promotion.shownId);

  // Revealing the promoted pane has nothing to do: it fills the strip.
  // Revealing any other pane demotes first and reveals once the strip is
  // free, so the pane asked for always ends up on screen.
  function revealHeldByPromotion(paneId: string): boolean {
    const intent = untrack(getPromotedPaneId);
    if (intent === null && promotion.shownId === null) return false;
    if (paneId === intent) return true;
    revealAfterDemote = paneId;
    demotePane();
    return true;
  }

  // A pane's own observer saw it move or resize: republish the whole
  // snapshot rather than patch one offset into a snapshot of another width.
  function handlePaneOffsetChange(paneId: string, offsetLeft: number | null): void {
    if (offsetLeft === null) stripGeometry.offsets.delete(paneId);
    else publishPaneOffsets();
  }

  // Also runs after a divider resize gesture: panes past the boundary
  // shift without resizing, so their per-pane ResizeObservers stay
  // silent and the cached offsets would go stale.
  function publishPaneOffsets(): void {
    const el = hostEl;
    if (!el) return;
    const offsets = new Map<string, number>();
    // A promoted pane's placeholder comes before its section, and its
    // offset is the slot's.
    for (const child of el.children) {
      if (!(child instanceof HTMLElement)) continue;
      const paneId = child.dataset.paneSlot ?? child.dataset.paneId;
      if (paneId && !offsets.has(paneId)) offsets.set(paneId, child.offsetLeft);
    }
    stripGeometry = { width: el.clientWidth, offsets };
  }

  // The strip's scroll range from its flow alone: sections, slots and gap
  // wrappers, not a promoted section laid over it.
  function flowScrollMax(el: HTMLElement): number {
    let right = 0;
    for (const child of el.children) {
      if (!(child instanceof HTMLElement) || child.dataset.panePromoted === 'true') continue;
      if (!child.dataset.paneId && !child.dataset.paneSlot && child.dataset.paneGapIndex === undefined) continue;
      right = Math.max(right, child.offsetLeft + child.offsetWidth);
    }
    return Math.max(0, right - el.clientWidth);
  }

  function reconcilePaneHostGeometry(): void {
    const el = hostEl;
    if (!el) return;

    const nextScrollLeft = clampStripScrollLeft(el, el.scrollLeft);
    if (nextScrollLeft !== el.scrollLeft) el.scrollLeft = nextScrollLeft;
    reveal.resolveAfterLayout();
    publishPaneOffsets();
  }

  let geometryReconcileFrame = 0;
  function schedulePaneHostGeometryReconcile(): void {
    if (geometryReconcileFrame) return;
    geometryReconcileFrame = requestAnimationFrame(() => {
      geometryReconcileFrame = 0;
      reconcilePaneHostGeometry();
    });
  }

  $effect(() => {
    const el = hostEl;
    if (!el) return;
    const publishScrollPosition = (): void => {
      promotion.holdStrip(el);
      measureCompactShownPane();
    };
    // A horizontal wheel/trackpad gesture over the strip is user scroll
    // intent — it wins over an in-flight reveal glide. Vertical wheel
    // (timeline scrolling inside a pane) bubbles here too and must NOT
    // cancel, or reveals would abort whenever content is scrolled mid-glide.
    const cancelGlideOnUserScroll = (event: WheelEvent): void => {
      if (event.deltaX !== 0 || event.shiftKey) reveal.cancelGlide();
    };
    const obs = new ResizeObserver((entries) => {
      const entry = entries[0];
      if (entry) setPaneHostWidth(entry.contentRect.width);
      reconcilePaneHostGeometry();
      promotion.holdStrip(el, flowScrollMax(el));
      realignCompactStrip();
    });
    obs.observe(el);
    setPaneHostWidth(el.getBoundingClientRect().width);
    reconcilePaneHostGeometry();
    el.addEventListener('scroll', publishScrollPosition, { passive: true });
    el.addEventListener('wheel', cancelGlideOnUserScroll, { passive: true });
    return () => {
      el.removeEventListener('scroll', publishScrollPosition);
      el.removeEventListener('wheel', cancelGlideOnUserScroll);
      obs.disconnect();
    };
  });

  $effect(() => {
    const el = hostEl;
    if (!el || typeof window === 'undefined') return;
    const handleReveal = (event: Event): void => {
      const detail = (event as CustomEvent<{ paneId?: string }>).detail;
      if (detail?.paneId) reveal.request(detail.paneId);
    };
    window.addEventListener(REVEAL_PANE_EVENT, handleReveal);
    return () => window.removeEventListener(REVEAL_PANE_EVENT, handleReveal);
  });

  $effect(() => {
    return () => {
      drag.destroy();
      promotion.destroy();
      reveal.cancelGlide();
      // The record measures this strip; it ends with it.
      noteCompactPaneShown(null);
      if (geometryReconcileFrame) cancelAnimationFrame(geometryReconcileFrame);
    };
  });

  // Pane add/remove/reorder changes the scrollable strip without resizing the
  // host itself, so they share the immediate horizontal geometry
  // reconciliation below. An order change can additionally leave an inactive
  // timeline's virtualizer out of sync while its <section> moves via
  // insertBefore; wait for that layout to settle, then ask every mounted
  // timeline to reconcile. The transcript did not change.
  const paneStructureKey = $derived(layoutItems.map((item) => item.paneId).join('|'));
  // The structure effect's FIRST run is the strip's first paint after layout
  // restore: it paints at scrollLeft 0 no matter which pane holds logical
  // focus, so that run snaps the focused pane into view. Every later run is an
  // ordinary structural change and must leave the position alone.
  let stripFirstPaint = true;

  function reconcilePaneHostLayout(): void {
    for (const item of layoutItems) {
      const pane = getPane(item.paneId);
      if (!pane) continue;
      pane.scrollController?.observe('host-layout');
    }
  }

  $effect(() => {
    void paneStructureKey;
    const stripAppeared = stripFirstPaint;
    stripFirstPaint = false;
    // Svelte has flushed the keyed pane sections before this effect runs.
    // Force current scroll geometry now so WebKit cannot paint a frame at an
    // offset beyond the shrunken strip; timeline reconciliation still waits
    // for its separate two-frame settle below.
    reconcilePaneHostGeometry();

    // The strip just appeared at scrollLeft 0. Bring the focused pane
    // into view instantly: there is no prior position worth gliding from,
    // and DOM focus restoration never scrolls (composer initial focus and
    // paneComposerFocus are preventScroll'd), so nothing else reveals it.
    // Untracked — a focus change alone must not re-run this effect.
    if (stripAppeared) {
      const focusTarget = untrack(() => focusedPaneId);
      if (focusTarget) reveal.request(focusTarget, { instant: true });
    }
    // Sections came, went or moved under the strip's position.
    measureCompactShownPane();

    let secondHandle = 0;
    const firstHandle = requestAnimationFrame(() => {
      secondHandle = requestAnimationFrame(reconcilePaneHostLayout);
    });
    return () => {
      cancelAnimationFrame(firstHandle);
      if (secondHandle) cancelAnimationFrame(secondHandle);
    };
  });

  // The strip follows the store's promoted pane. Membership is read so a
  // shown pane that closes ends its promotion in the same flush.
  $effect(() => {
    const next = promotedId;
    const instant = compact;
    void paneStructureKey;
    // A reveal waiting on a demote belongs to the navigation that asked for
    // it; a new promotion, or the same one taken back, supersedes it.
    if (next !== null) revealAfterDemote = null;
    untrack(() => promotion.follow(next, {
      instant,
      present: (paneId) => layoutItems.some((item) => item.paneId === paneId),
    }));
  });

  // Focus moving to another pane demotes: the promoted pane is the only one
  // on screen, and the newly focused one must be seen, so the demote reveals
  // it once it lands (the strip does not move before). Panes under the
  // layer stay tabbable rather than inert: a pane command moves DOM focus
  // into its target in the same task as the focus change (alt+l focuses the
  // next composer), before any attribute could be cleared, and Tab out of a
  // promoted pane is a way to leave it. Until the demote lands the reveal
  // follows focus, so a second move (Tab again) is the one revealed. A focus
  // change with no promotion still reveals nothing.
  $effect(() => {
    const focused = focusedPaneId;
    untrack(() => {
      if (focused === null) return;
      const intent = getPromotedPaneId();
      if (intent !== null) {
        if (focused === intent) return;
        revealAfterDemote = focused;
        demotePane();
        return;
      }
      if (promotion.shownId !== null) revealAfterDemote = focused;
    });
  });

  // A horizontal wheel over a promoted pane would scroll the strip under
  // it; take it unless something inside the pane scrolls that way. Bound
  // only while a pane is shown so ordinary strip scrolling stays passive.
  $effect(() => {
    const el = hostEl;
    const shown = shownPromotedId;
    if (!el || shown === null) return;
    const blockStripWheel = (event: WheelEvent): void => {
      // Shift turns a vertical wheel into a horizontal scroll.
      const dx = event.shiftKey && event.deltaX === 0 ? event.deltaY : event.deltaX;
      if (dx === 0) return;
      if (!event.shiftKey && Math.abs(event.deltaX) < Math.abs(event.deltaY)) return;
      if (scrollsHorizontallyWithin(event.target, paneElementById(shown), dx)) return;
      event.preventDefault();
    };
    el.addEventListener('wheel', blockStripWheel, { passive: false });
    return () => el.removeEventListener('wheel', blockStripWheel);
  });

  function scrollsHorizontallyWithin(target: EventTarget | null, root: HTMLElement | null, dx: number): boolean {
    if (!root || !(target instanceof Element) || !root.contains(target)) return false;
    for (let node: Element | null = target; node && node !== root; node = node.parentElement) {
      if (node.scrollWidth <= node.clientWidth + 1) continue;
      const overflowX = getComputedStyle(node).overflowX;
      if (overflowX !== 'auto' && overflowX !== 'scroll') continue;
      const room = dx > 0 ? node.scrollWidth - node.clientWidth - node.scrollLeft : node.scrollLeft;
      if (room > 0.5) return true;
    }
    return false;
  }

  // Compact shows one pane, and the layout store's record of which one
  // (`isPaneOnScreen`) is a measurement of this strip, written only here:
  // the pane whose section holds the strip's centre. It runs wherever the
  // strip's position or its sections change: every scroll event (each
  // glide frame, a glide's landing, a snap, a native scroll-into-view that
  // abandons a glide), a structure change and entering compact. So it never
  // names a pane the strip moved away from. A reveal does not write it: the
  // reveal moves the strip and the record follows. Reads the published
  // snapshot, no DOM walk, and the store write is a no-op unless the pane
  // under the centre changed. A snapshot from another width says nothing
  // about this position: the strip resized and its observer has not run,
  // and that observer realigns to the pane recorded before the resize.
  function measureCompactShownPane(): void {
    const el = hostEl;
    if (!el || !untrack(() => compact)) return;
    // Compact sections are each the strip's full width.
    const { width, offsets } = stripGeometry;
    if (el.clientWidth !== width) return;
    const centre = el.scrollLeft + width / 2;
    let shown: string | null = null;
    for (const [paneId, left] of offsets) {
      if (left <= centre && centre < left + width) {
        shown = paneId;
        break;
      }
    }
    noteCompactPaneShown(shown);
  }

  // A resize (a rotation, the keyboard opening) keeps the pane the record
  // measured before it, the one place the record leads the strip: the old
  // offset can put another pane, or two halves, under the resized strip.
  // The snap's scroll event measures the result. A reveal on its way keeps
  // its glide, which `reconcilePaneHostGeometry` re-resolved to its pane.
  function realignCompactStrip(): void {
    if (!untrack(() => compact)) return;
    const before = untrack(compactShownPaneId);
    const paneEl = before === null || reveal.active ? null : paneSlotById(before);
    if (paneEl) reveal.snap(paneEl);
  }

  // Entering compact keeps what the reflowed strip shows, as the transition
  // always has, aligned to that pane so it is the one on screen. Leaving it
  // ends the record: the desktop shows every pane.
  // Initialized to the mode at mount: the strip's first paint places the
  // focused pane, so mounting in compact is not a transition into it.
  let wasCompact = untrack(() => compact);
  $effect(() => {
    const now = compact;
    const entering = now && !wasCompact;
    const leaving = !now && wasCompact;
    wasCompact = now;
    untrack(() => {
      if (leaving) noteCompactPaneShown(null);
      if (!entering) return;
      // Compact shows one pane already; a promotion does not survive it.
      demotePane();
      reconcilePaneHostGeometry();
      measureCompactShownPane();
      const shown = compactShownPaneId();
      const paneEl = shown === null ? null : paneSlotById(shown);
      if (paneEl) reveal.snap(paneEl);
    });
  });

  // Pointer focus: reveal only on an actual focus TRANSITION (clicking an
  // unfocused, partially visible pane slides it fully into view). Clicking
  // inside the already-focused pane never moves the strip — selecting text
  // or grabbing a scrollbar in a half-visible pane must not snap it.
  function handlePanePointerDown(paneId: string): void {
    const isTransition = focusedPaneId !== paneId;
    focusPane(paneId);
    if (isTransition) reveal.request(paneId);
  }

  // DOM focus tracks logical focus but NEVER reveals: window re-activation
  // and focus-trap restores re-fire focusin on the previously focused
  // element, and scrolling on those yanked the strip away from wherever
  // the user had scrolled (they usually re-assert the same pane anyway).
  function handlePaneFocusIn(paneId: string): void {
    focusPane(paneId);
  }

  // flex-grow = widthPx stretches panes proportionally to their base
  // widths when the window is wider than their sum; shrink-0 in the
  // class list keeps a narrower window as horizontal scroll instead of
  // squeezing below the stored width.
  function paneSectionStyle(item: PaneLayoutItem): string {
    if (compact) return 'flex:0 0 100%;min-width:100%';
    return `flex-grow:${item.widthPx};flex-basis:${item.widthPx}px;min-width:${paneItemMinWidthPx(item, minPaneWidth)}px`;
  }

  // A promoted section is laid over the strip's visible area: the host's
  // padding box at the locked scroll position. Its slot keeps the flex
  // style above. Above the dividers (z-20), below drop indicators (z-40)
  // and the drag shield. Every section is `isolate`, its own stacking
  // context, so a layer inside a pane (the message rail's z-30) orders
  // only within that pane and never paints over the promoted one.
  function sectionStyle(item: PaneLayoutItem): string {
    if (shownPromotedId !== item.paneId) return paneSectionStyle(item);
    return `position:absolute;top:0;bottom:0;left:${promotion.lockedLeft}px;width:100%;z-index:30`;
  }

</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<!-- data-popover-clip-boundary: popups anchored in strip content (composer
     pickers, header menus) clip at this element's edge as the strip scrolls,
     so they slide behind the sidebar with their trigger instead of painting
     over it. See utils/popoverOwnership.ts#resolvePopoverClipBoundary. -->
<div
  bind:this={stripEl}
  class="compact-screen compact-screen-thread relative flex flex-1 min-w-0 min-h-0"
  data-testid="pane-strip"
  ondragover={drag.onHostDragOver}
  ondrop={drag.onHostDrop}
  ondragleave={drag.onHostDragLeave}
>
<div
  bind:this={hostEl}
  class={[
    'relative flex-1 flex min-w-0 min-h-0 overflow-x-auto overflow-y-hidden compact:overflow-x-hidden',
    shownPromotedId !== null ? 'pane-host-promoted' : '',
  ].join(' ')}
  inert={threadInert}
  data-popover-clip-boundary
  data-testid="pane-host"
>
  {#if layoutItems.length === 0}
    <section
      class={[
        'flex h-full min-w-full flex-1 items-center justify-center px-8',
        drag.dropTarget ? 'bg-accent/10 ring-2 ring-accent/70 ring-inset' : 'bg-transparent',
      ].join(' ')}
      data-testid="pane-host-empty"
      data-drop-kind={drag.dropTarget?.kind}
    >
      <p class="text-sm text-fg-muted">Select a thread or create a new one to get started.</p>
    </section>
  {:else}
    {#each layoutItems as item, index (item.id)}
      {#if shownPromotedId === item.paneId}
        <!-- The promoted pane's slot: same flex style, so no other pane
             moves while the section is laid over the strip. Offsets and
             reveals read it in place of the section (paneSlotById). -->
        <div
          class="min-h-0 min-w-0 shrink-0"
          style={paneSectionStyle(item)}
          data-pane-slot={item.paneId}
          data-testid="pane-promoted-slot"
          aria-hidden="true"
        ></div>
      {/if}
      {@const companion = isCompanionKind(item.kind)}
      {@const pane = companion || isSurfaceKind(item.kind) ? undefined : getPane(item.paneId)}
      {@const promoted = shownPromotedId === item.paneId}
      {#if !companion && !isSurfaceKind(item.kind) && !pane}
        <section
          style={sectionStyle(item)}
          style:clip-path={promotion.clipFor(item.paneId)}
          class="isolate flex min-h-0 min-w-0 shrink-0 items-center justify-center bg-surface-0 text-sm text-error"
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-missing="true"
        >
          Pane unavailable.
        </section>
      {:else}
        <!-- One section for every pane kind. Only a thread, tool or ticket
             pane leads a layout block, so only they take reorder drops and
             show the drag states; a thread pane with a take-control
             terminal shares the top border that marks the pair on both
             halves. -->
        {@const paired = pane !== undefined && pairedSourceIds.has(item.paneId)}
        {@const onPaneDragStart = promoted ? undefined : (event: DragEvent) => drag.onPaneDragStart(event, item.paneId)}
        <!-- svelte-ignore a11y_no_static_element_interactions -->
        <!-- No tabindex: a click inside never focuses the section, so a
             keyboard scroll starts where it did. A keyboard pane move to a
             pane with neither a composer nor a primary scroller focuses it
             for that moment (paneComposerFocus#carryCaretIntoPane). -->
        <section
          use:measurePane={{ paneId: item.paneId, onOffsetChange: handlePaneOffsetChange, promoted }}
          style={sectionStyle(item)}
          style:clip-path={promotion.clipFor(item.paneId)}
          class={[
            'isolate flex min-h-0 min-w-0 shrink-0 flex-col overflow-hidden border-r border-border-subtle/70 focus:outline-none',
            item.kind === 'take-control' || paired ? 'take-control-pair-top' : '',
            promoted ? 'bg-surface-0' : focusedPaneId === item.paneId ? 'bg-surface-0/40' : '',
            !companion && drag.draggingPaneId === item.paneId ? 'opacity-55' : '',
            pane && drag.duplicateDropPaneId === item.paneId ? 'ring-2 ring-accent/70 ring-inset' : '',
          ].join(' ')}
          data-pane-id={item.paneId}
          data-pane-kind={item.kind}
          data-pane-min-width={paneItemMinWidthPx(item, minPaneWidth)}
          data-pane-width={item.widthPx}
          data-pane-focused={focusedPaneId === item.paneId}
          data-pane-promoted={promoted}
          data-pane-paired={pane ? paired : undefined}
          onpointerdown={() => handlePanePointerDown(item.paneId)}
          onfocusin={() => handlePaneFocusIn(item.paneId)}
        >
          {#if isCompanionKind(item.kind)}
            <!-- Companion panes (plan/review/browser/agent panels + the
                 take-control PTY mirror) are not ThreadPanes: they hold
                 their own logical focus, so pane-scoped commands
                 (close/move) act on the companion and thread-scoped
                 commands resolve to the source via getFocusedPaneOrNull.
                 Every pane body sits in its own boundary: a render throw in
                 one pane tears down and reports that pane alone, never the
                 strip. -->
            <RenderBoundary label="This panel" testId="pane-render-error">
              {#if item.kind === 'side-chat' || item.kind === 'agent-thread'}
                <!-- A side chat and an agent thread companion are companions by
                     lifetime, not by surface: each hosts an ordinary ChatView,
                     over a side chat's own scratch thread or over the thread an
                     Agents row went to, registered in the pane registry under
                     this companion pane id. -->
                {@const hostedPane = getPane(item.paneId)}
                {#if hostedPane?.threadId}
                  <ChatView pane={hostedPane} />
                {:else}
                  <div class="flex justify-end gap-1 border-b border-border-subtle p-2">
                    <PanePromoteButton paneId={item.paneId} />
                    <PaneCloseButton paneId={item.paneId} />
                  </div>
                  <div role="status" aria-busy="true" class="flex flex-1 items-center justify-center gap-2 px-4 text-sm text-fg-muted" data-testid="{item.kind}-preparing">
                    <SteppedSpinner /> {item.kind === 'side-chat' ? 'Preparing side chat…' : 'Opening thread…'}
                  </div>
                {/if}
              {:else if item.kind === 'take-control'}
                <!-- Lazy: TakeControlPane pulls the xterm stack; a static
                     import here would drag the terminal chunks into the eager
                     startup graph (see the TerminalView mount in ChatView). -->
                {#await import('../takecontrol/TakeControlPane.svelte')}
                  <div class="flex h-full items-center justify-center text-xs text-fg-muted">Loading terminal...</div>
                {:then { default: TakeControlPane }}
                  <TakeControlPane paneId={item.paneId} />
                {:catch err}
                  <div class="flex h-full items-center justify-center text-xs text-error" data-testid="take-control-load-error">
                    Failed to load terminal: {err instanceof Error ? err.message : String(err)}
                  </div>
                {/await}
              {:else}
                <CompanionPane paneId={item.paneId} kind={item.kind} sourcePaneId={item.sourcePaneId!} />
              {/if}
            </RenderBoundary>
          {:else if isToolKind(item.kind)}
            <!-- Tool panes (Pull requests, Workflows) are app-wide surfaces
                 with no ThreadPane. ToolPane owns the render boundary. -->
            {@const toolKind = item.kind}
            <ToolPane paneId={item.paneId} kind={toolKind} {onPaneDragStart} />
          {:else if item.kind === 'ticket'}
            <!-- A ticket in a pane of its own, with no ThreadPane. TicketPane
                 owns the render boundary. -->
            <TicketPane paneId={item.paneId} {onPaneDragStart} />
          {:else if pane}
            <RenderBoundary label="This thread pane" testId="pane-render-error">
              <ChatView {pane} {onPaneDragStart} />
            </RenderBoundary>
          {/if}
        </section>
      {/if}
      {#if index < layoutItems.length - 1 && !compact}
        <!-- `flex` is load-bearing: it stretches the divider to the
             full strip height. Without it the divider is a block box
             whose only child is absolutely positioned, so it collapses
             to 0px tall and there is nothing to hover or grab. -->
        <div data-pane-gap-index={index + 1} class="flex shrink-0">
          <PaneDivider
            leftPaneId={item.paneId}
            rightPaneId={layoutItems[index + 1].paneId}
            leftPaneWidthPx={item.widthPx}
            getHostEl={() => hostEl}
            onDragEnd={schedulePaneHostGeometryReconcile}
          />
        </div>
      {/if}
    {/each}
    <!-- End handle: the far-right pane has no divider on its right, so
         the strip's right edge itself is draggable to resize it. The
         `flex` wrapper stretches it to full height (see above). -->
    {#if !compact}
      <div data-pane-gap-index={layoutItems.length} class="flex shrink-0">
        <PaneDivider
          leftPaneId={layoutItems[layoutItems.length - 1].paneId}
          leftPaneWidthPx={layoutItems[layoutItems.length - 1].widthPx}
          getHostEl={() => hostEl}
          onDragEnd={schedulePaneHostGeometryReconcile}
        />
      </div>
    {/if}
    <!-- Where a dragged thread, tool or pane lands: a bar on the seam, and
         the half of the block under the pointer that chose it, fading away
         from the bar. -->
    {#if drag.dropTarget && drag.dropIndicator}
      {@const lit = drag.dropIndicator.lit}
      {#if lit}
        <div
          class="pointer-events-none absolute top-0 bottom-0 z-40 from-accent/25 to-accent/0 {lit.seam === 'left' ? 'bg-linear-to-r' : 'bg-linear-to-l'}"
          style:left={`${lit.left}px`}
          style:width={`${lit.width}px`}
          data-testid="pane-drop-half"
          data-seam-side={lit.seam}
        ></div>
      {/if}
      <div
        class="pointer-events-none absolute top-0 bottom-0 z-40 rounded-full bg-accent"
        style:left={`${drag.dropIndicator.seamLeft}px`}
        style:width={`${PANE_DROP_SEAM_PX}px`}
        data-testid="pane-drop-seam"
        data-drop-kind={drag.dropTarget.kind}
        data-insert-index={drag.dropTarget.insertIndex}
      ></div>
    {/if}
  {/if}
</div>
<!-- Takes a pane-placing drag off the panes from its start to its end
     (usePaneThreadDrag `shielding`). Outside the scroller, so the drag is
     over nothing the browser can scroll. An airspace surface, so a native
     browser view, which paints above the page, is hidden under it. -->
{#if drag.shielding}
  <div
    class="absolute inset-0 z-50"
    role="presentation"
    data-testid="pane-drag-shield"
    use:airspaceSurface
    ondragleave={drag.onShieldDragLeave}
  ></div>
{/if}
</div>
